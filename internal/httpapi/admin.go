package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/authz"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"github.com/google/uuid"
)

// Role slugs for scope discovery (SystemActor Display forms). Authorization
// itself is enforced in internal/authz; these are only used to report which
// scopes a caller may administer (FR-8).
const (
	slugTenantOwner          = "tenant-owner"
	slugTenantManager        = "tenant-manager"
	slugSubscriptionsManager = "subscriptions-manager"
)

// agentTargetAll is the sentinel that addresses the agent-less shared store —
// the content every agent under the scope reads. It is the default, so a request
// that says nothing about agents behaves exactly as it did before
// per-agent-injection-scope.
const agentTargetAll = "all"

// parseAdminScope validates the scope kind + ids and returns the resolved
// docker.Scope, or a non-empty client-error message. UUIDs are normalized to
// their canonical string form (the same form the on-disk layout keys on).
func parseAdminScope(kind, tenantID, subsAccID string) (docker.Scope, string) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return docker.Scope{}, `"tenant_id" is required and must be a UUID`
	}
	switch kind {
	case "tenant":
		return docker.Scope{Kind: docker.ScopeTenant, TenantID: tid.String()}, ""
	case "subscription":
		sid, err := uuid.Parse(subsAccID)
		if err != nil {
			return docker.Scope{}, `"subs_acc_id" is required and must be a UUID`
		}
		return docker.Scope{Kind: docker.ScopeSubscription, TenantID: tid.String(), SubsAccID: sid.String()}, ""
	default:
		return docker.Scope{}, `"scope" must be "tenant" or "subscription"`
	}
}

// resolveAgentTarget maps the request's agent target onto Scope.AgentKey: "" /
// "all" → the all-agents store (empty AgentKey); any other value must be a
// configured agent key, else a client error (FR-2). The target is deliberately
// independent of the routed service — the routed agent is the bearer-guard
// vehicle and cannot express "all agents".
func (s *Server) resolveAgentTarget(raw string) (string, string) {
	if raw == "" || raw == agentTargetAll {
		return "", ""
	}
	if _, ok := s.Cfg.Agents[raw]; !ok {
		return "", `"agent" must be "all" or a configured agent key`
	}
	return raw, ""
}

// adminScope reads scope/tenant_id/subs_acc_id/agent via get (query or form),
// writing a 400 and returning ok=false on a bad value.
func (s *Server) adminScope(w http.ResponseWriter, get func(string) string) (docker.Scope, bool) {
	scope, msg := parseAdminScope(get("scope"), get("tenant_id"), get("subs_acc_id"))
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, errBody(msg))
		return docker.Scope{}, false
	}
	agentKey, msg := s.resolveAgentTarget(get("agent"))
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, errBody(msg))
		return docker.Scope{}, false
	}
	scope.AgentKey = agentKey
	return scope, true
}

// handleAdminAgents lists the configured agent keys so the admin UI can offer an
// agent target instead of hardcoding one (FR-6). Keys only — never an image,
// model or key from the agent config. Gated like /v1/admin/scopes: the caller
// must administer at least one scope.
func (s *Server) handleAdminAgents(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	if !s.hasAnyManageableScope(ident) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to administer any scope"))
		return
	}
	keys := make([]string, 0, len(s.Cfg.Agents))
	for key := range s.Cfg.Agents {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	agents := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		// harness is reported so a client can tell which agents an admin surface
		// applies to. The model inventory governs picoclaw only — hermes reads its
		// model from the proxy's config.yaml (CTX-MR-13) — so a picker that offered
		// a hermes agent would let an admin pin a model nothing reads.
		agents = append(agents, map[string]any{"key": key, "harness": s.Cfg.Agents[key].Harness})
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

// hasAnyManageableScope reports whether the caller holds administrative
// authority over at least one tenant or subscription — Instance tier, or a
// licensed tenant/subscription management role.
func (s *Server) hasAnyManageableScope(ident identity.Identity) bool {
	p := ident.Profile
	if p == nil {
		return false
	}
	if p.HasAdminPrivileges() {
		return true
	}
	if p.LicensedResources == nil {
		return false
	}
	for _, res := range p.LicensedResources.ToLicensesVector() {
		switch res.Role {
		case slugTenantOwner, slugTenantManager, slugSubscriptionsManager:
			return true
		}
	}
	return false
}

// handleAdminScopes reports the flat set of scopes the caller may administer
// (FR-8). A Tenant-tier caller (and Instance) gets the tenant scope AND every
// subscription scope under that tenant (enumerated on disk) — a tenant admin
// manages the members of every subscription below it, so the Members panel must
// be reachable. A subscriptions-manager gets only its own subscription scopes.
// Instance enumerates all tenants + their subscriptions on disk.
func (s *Server) handleAdminScopes(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	p := ident.Profile
	scopes := []map[string]any{}
	seen := map[string]bool{}
	addTenant := func(tenantID string) {
		key := "t:" + tenantID
		if seen[key] {
			return
		}
		seen[key] = true
		scopes = append(scopes, map[string]any{"kind": "tenant", "tenantId": tenantID})
	}
	addSubscription := func(tenantID, subsAccID, accName string) {
		key := "s:" + tenantID + "/" + subsAccID
		if seen[key] {
			return
		}
		seen[key] = true
		entry := map[string]any{"kind": "subscription", "tenantId": tenantID, "subsAccId": subsAccID}
		if accName != "" {
			entry["accName"] = accName
		}
		scopes = append(scopes, entry)
	}
	// enumerateTenant adds the tenant scope plus every subscription under it
	// (on disk) — the authority a Tenant/Instance caller holds.
	enumerateTenant := func(tenantID string) {
		addTenant(tenantID)
		subs, err := s.Mgr.ListTenantSubscriptions(tenantID)
		if err != nil {
			s.logf("admin: list subscriptions for tenant %s failed: %v", tenantID, err)
			return
		}
		for _, sid := range subs {
			addSubscription(tenantID, sid, "")
		}
	}

	if p.HasAdminPrivileges() {
		tenants, err := s.Mgr.ListTenants()
		if err != nil {
			s.logf("admin: list tenants failed: %v", err)
			writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
			return
		}
		for _, tid := range tenants {
			enumerateTenant(tid)
		}
	}
	if p.LicensedResources != nil {
		for _, res := range p.LicensedResources.ToLicensesVector() {
			switch res.Role {
			case slugTenantOwner, slugTenantManager:
				enumerateTenant(res.TenantID.String())
			case slugSubscriptionsManager:
				addSubscription(res.TenantID.String(), res.AccID.String(), res.AccName)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopes": scopes})
}

func (s *Server) handleAdminSharedList(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	scope, ok := s.adminScope(w, r.URL.Query().Get)
	if !ok {
		return
	}
	if !authz.AuthorizeSharedScope(ident.Profile, string(scope.Kind), scope.TenantID, scope.SubsAccID) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to administer this scope"))
		return
	}
	files, err := s.Mgr.ListSharedFiles(scope)
	if err != nil {
		s.logf("admin: list shared files failed scope=%+v: %v", scope, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (s *Server) handleAdminSharedPost(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.Cfg.MediaMaxBytes+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge,
			errBody("file exceeds the size limit"))
		return
	}
	scope, ok := s.adminScope(w, r.FormValue)
	if !ok {
		return
	}
	if !authz.AuthorizeSharedScope(ident.Profile, string(scope.Kind), scope.TenantID, scope.SubsAccID) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to administer this scope"))
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("a `file` part is required"))
		return
	}
	defer file.Close()
	stored, err := s.Mgr.WriteSharedFile(scope, header.Filename, file)
	if err != nil {
		if errors.Is(err, docker.ErrMediaName) {
			writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
			return
		}
		s.logf("admin: write shared file failed scope=%+v: %v", scope, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	// No restart: shared files are read-only bind-mounts, so a new file shows up
	// in every running container under the scope immediately. Recreating the
	// container here would destroy picoclaw's live session and truncate the
	// transcript (the "conversation cut in half after an injection" bug).
	writeJSON(w, http.StatusOK, stored)
}

func (s *Server) handleAdminSharedContent(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	scope, ok := s.adminScope(w, r.URL.Query().Get)
	if !ok {
		return
	}
	if !authz.AuthorizeSharedScope(ident.Profile, string(scope.Kind), scope.TenantID, scope.SubsAccID) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to administer this scope"))
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"name" query parameter is required`))
		return
	}
	rc, meta, err := s.Mgr.ReadSharedFile(scope, name)
	if err != nil {
		if errors.Is(err, docker.ErrMediaName) {
			writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
			return
		}
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, errBody("shared file not found"))
			return
		}
		s.logf("admin: read shared file failed scope=%+v name=%s: %v", scope, name, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+meta.Name+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

func (s *Server) handleAdminSharedDelete(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	scope, ok := s.adminScope(w, r.URL.Query().Get)
	if !ok {
		return
	}
	if !authz.AuthorizeSharedScope(ident.Profile, string(scope.Kind), scope.TenantID, scope.SubsAccID) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to administer this scope"))
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"name" query parameter is required`))
		return
	}
	if err := s.Mgr.DeleteSharedFile(scope, name); err != nil {
		if errors.Is(err, docker.ErrMediaName) {
			writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
			return
		}
		s.logf("admin: delete shared file failed scope=%+v name=%s: %v", scope, name, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	// No restart: the read-only bind reflects the deletion in running containers
	// immediately; recreating would truncate live picoclaw sessions.
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "name": name})
}

// adminSecretRequest is the POST /v1/admin/shared-secrets body.
type adminSecretRequest struct {
	Scope     string `json:"scope"`
	TenantID  string `json:"tenant_id"`
	SubsAccID string `json:"subs_acc_id"`
	Agent     string `json:"agent"`
	Format    string `json:"format"`
	Name      string `json:"name"`
	Value     string `json:"value"`
}

func (s *Server) handleAdminSharedSecretsPost(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	var req adminSecretRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	scope, msg := parseAdminScope(req.Scope, req.TenantID, req.SubsAccID)
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, errBody(msg))
		return
	}
	agentKey, msg := s.resolveAgentTarget(req.Agent)
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, errBody(msg))
		return
	}
	scope.AgentKey = agentKey
	if !authz.AuthorizeSharedScope(ident.Profile, string(scope.Kind), scope.TenantID, scope.SubsAccID) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to administer this scope"))
		return
	}
	if err := s.Mgr.WriteSharedSecret(scope, req.Format, req.Name, req.Value); err != nil {
		if errors.Is(err, docker.ErrInvalidSecretName) {
			writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
			return
		}
		s.logf("admin: write shared secret failed scope=%+v: %v", scope, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	if err := s.Mgr.RestartScope(scope); err != nil {
		s.logf("admin: restart scope after secret write failed scope=%+v: %v", scope, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "format": req.Format, "name": req.Name})
}

func (s *Server) handleAdminSharedSecretsList(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	scope, ok := s.adminScope(w, r.URL.Query().Get)
	if !ok {
		return
	}
	if !authz.AuthorizeSharedScope(ident.Profile, string(scope.Kind), scope.TenantID, scope.SubsAccID) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to administer this scope"))
		return
	}
	names, err := s.Mgr.ListSharedSecrets(scope)
	if err != nil {
		s.logf("admin: list shared secrets failed scope=%+v: %v", scope, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": names})
}

func (s *Server) handleAdminSharedSecretsDelete(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	scope, ok := s.adminScope(w, r.URL.Query().Get)
	if !ok {
		return
	}
	if !authz.AuthorizeSharedScope(ident.Profile, string(scope.Kind), scope.TenantID, scope.SubsAccID) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to administer this scope"))
		return
	}
	format := r.URL.Query().Get("format")
	name := r.URL.Query().Get("name")
	if err := s.Mgr.DeleteSharedSecret(scope, format, name); err != nil {
		if errors.Is(err, docker.ErrInvalidSecretName) || errors.Is(err, docker.ErrUnknownNativeSlot) {
			writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
			return
		}
		s.logf("admin: delete shared secret failed scope=%+v: %v", scope, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	// A native slot lives inside each workspace's .security.yml, and the merge on
	// restart only ever SETS slots — so clear it from the covered workspaces first.
	// The RestartScope below then re-applies whatever lower cascade layer still
	// provides it (native-secrets-admin-only AC-6).
	if format == docker.FormatNative {
		s.Mgr.UnsetNativeSlotForScope(scope, name)
	}
	if err := s.Mgr.RestartScope(scope); err != nil {
		s.logf("admin: restart scope after secret delete failed scope=%+v: %v", scope, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "format": format, "name": name})
}

func (s *Server) handleAdminUsersList(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	tenantID, err := uuid.Parse(r.URL.Query().Get("tenant_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" query parameter is required and must be a UUID`))
		return
	}
	subsAccID, err := uuid.Parse(r.URL.Query().Get("subs_acc_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" query parameter is required and must be a UUID`))
		return
	}
	if !authz.AuthorizeUserManagement(ident.Profile, tenantID.String(), subsAccID.String()) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to manage users under this subscription"))
		return
	}
	users, err := s.Mgr.ListSubscriptionUsers(tenantID.String(), subsAccID.String())
	if err != nil {
		s.logf("admin: list users failed tenant=%s subs=%s: %v", tenantID, subsAccID, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

// adminUserFileKey resolves the (tenant, subscription, role, user) key a
// user-file op targets, gating it on user-management authority. The role is the
// addressed agent (the auth vehicle), so the browsed files are that agent's
// workspace. Writes the error response and returns ok=false on failure.
func (s *Server) adminUserFileKey(w http.ResponseWriter, r *http.Request, agentKey string, ident identity.Identity) (docker.WorkspaceKey, bool) {
	tenantID, err := uuid.Parse(r.URL.Query().Get("tenant_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" query parameter is required and must be a UUID`))
		return docker.WorkspaceKey{}, false
	}
	subsAccID, err := uuid.Parse(r.URL.Query().Get("subs_acc_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" query parameter is required and must be a UUID`))
		return docker.WorkspaceKey{}, false
	}
	userAccID, err := uuid.Parse(r.URL.Query().Get("user_acc_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"user_acc_id" query parameter is required and must be a UUID`))
		return docker.WorkspaceKey{}, false
	}
	if !authz.AuthorizeUserManagement(ident.Profile, tenantID.String(), subsAccID.String()) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to manage this user's files"))
		return docker.WorkspaceKey{}, false
	}
	return docker.WorkspaceKey{
		TenantID:  tenantID.String(),
		SubsAccID: subsAccID.String(),
		Role:      agentKey,
		UserAccID: userAccID.String(),
	}, true
}

// handleAdminUserFilesList returns a user's private-file METADATA only — never
// the bytes (FR-7). There is deliberately no content endpoint.
func (s *Server) handleAdminUserFilesList(w http.ResponseWriter, r *http.Request) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	key, ok := s.adminUserFileKey(w, r, agent.Key, ident)
	if !ok {
		return
	}
	files, err := s.Mgr.ListUserFiles(key)
	if err != nil {
		s.logf("admin: list user files failed key=%+v: %v", key, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// handleAdminUserFilesDelete removes one of a user's private files. It never
// reads the bytes (FR-7).
func (s *Server) handleAdminUserFilesDelete(w http.ResponseWriter, r *http.Request) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	key, ok := s.adminUserFileKey(w, r, agent.Key, ident)
	if !ok {
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"name" query parameter is required`))
		return
	}
	if err := s.Mgr.DeleteUserFile(key, name); err != nil {
		if errors.Is(err, docker.ErrMediaName) {
			writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
			return
		}
		s.logf("admin: delete user file failed key=%+v name=%s: %v", key, name, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "name": name})
}

// --- admin-shared-skills ---

func skillErrStatus(err error) (int, bool) {
	switch {
	case errors.Is(err, docker.ErrInvalidSkillName),
		errors.Is(err, docker.ErrReservedSkillName),
		errors.Is(err, docker.ErrSkillMetadata),
		errors.Is(err, docker.ErrSkillArchive):
		return http.StatusBadRequest, true
	}
	return 0, false
}

func (s *Server) handleAdminSkillsList(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	scope, ok := s.adminScope(w, r.URL.Query().Get)
	if !ok {
		return
	}
	if !authz.AuthorizeSharedScope(ident.Profile, string(scope.Kind), scope.TenantID, scope.SubsAccID) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to administer this scope"))
		return
	}
	skills, err := s.Mgr.ListSharedSkills(scope)
	if err != nil {
		s.logf("admin: list shared skills failed scope=%+v: %v", scope, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": skills})
}

func (s *Server) handleAdminSkillsDoc(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	scope, ok := s.adminScope(w, r.URL.Query().Get)
	if !ok {
		return
	}
	if !authz.AuthorizeSharedScope(ident.Profile, string(scope.Kind), scope.TenantID, scope.SubsAccID) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to administer this scope"))
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"name" query parameter is required`))
		return
	}
	content, meta, err := s.Mgr.ReadSharedSkillDoc(scope, name)
	if err != nil {
		if st, ok := skillErrStatus(err); ok {
			writeJSON(w, st, errBody(err.Error()))
			return
		}
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, errBody("skill not found"))
			return
		}
		s.logf("admin: read skill doc failed scope=%+v name=%s: %v", scope, name, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "content": content, "meta": meta})
}

func (s *Server) handleAdminSkillsArchive(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	scope, ok := s.adminScope(w, r.URL.Query().Get)
	if !ok {
		return
	}
	if !authz.AuthorizeSharedScope(ident.Profile, string(scope.Kind), scope.TenantID, scope.SubsAccID) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to administer this scope"))
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"name" query parameter is required`))
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.zip"`)
	if err := s.Mgr.ArchiveSharedSkill(scope, name, w); err != nil {
		// Headers may already be sent; just log (mirrors the streaming file route).
		s.logf("admin: archive skill failed scope=%+v name=%s: %v", scope, name, err)
	}
}

func (s *Server) handleAdminSkillsPost(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.Cfg.MediaMaxBytes+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, errBody("upload exceeds the size limit"))
		return
	}
	scope, ok := s.adminScope(w, r.FormValue)
	if !ok {
		return
	}
	if !authz.AuthorizeSharedScope(ident.Profile, string(scope.Kind), scope.TenantID, scope.SubsAccID) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to administer this scope"))
		return
	}
	name := r.FormValue("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errBody("a `name` field is required"))
		return
	}
	var writeErr error
	if file, _, err := r.FormFile("file"); err == nil {
		defer file.Close()
		writeErr = s.Mgr.WriteSharedSkillZip(scope, name, file)
	} else {
		body := r.FormValue("body")
		if body == "" {
			writeJSON(w, http.StatusBadRequest, errBody("provide either a `file` (zip) or a `body` (SKILL.md)"))
			return
		}
		writeErr = s.Mgr.WriteSharedSkillDoc(scope, name, body)
	}
	if writeErr != nil {
		if st, ok := skillErrStatus(writeErr); ok {
			writeJSON(w, st, errBody(writeErr.Error()))
			return
		}
		s.logf("admin: write skill failed scope=%+v name=%s: %v", scope, name, writeErr)
		writeJSON(w, http.StatusBadGateway, errBody(writeErr.Error()))
		return
	}
	// Skills reach containers via the merged effective dir, so re-materialize it
	// and restart the scope (stop/start, no recreate) to pick up the change.
	if err := s.Mgr.SyncEffectiveSkillsForScope(scope); err != nil {
		s.logf("admin: sync effective skills failed scope=%+v: %v", scope, err)
	}
	if err := s.Mgr.RestartScope(scope); err != nil {
		s.logf("admin: restart scope after skill write failed scope=%+v: %v", scope, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "name": name})
}

func (s *Server) handleAdminSkillsDelete(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	scope, ok := s.adminScope(w, r.URL.Query().Get)
	if !ok {
		return
	}
	if !authz.AuthorizeSharedScope(ident.Profile, string(scope.Kind), scope.TenantID, scope.SubsAccID) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to administer this scope"))
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"name" query parameter is required`))
		return
	}
	if err := s.Mgr.DeleteSharedSkill(scope, name); err != nil {
		if st, ok := skillErrStatus(err); ok {
			writeJSON(w, st, errBody(err.Error()))
			return
		}
		s.logf("admin: delete skill failed scope=%+v name=%s: %v", scope, name, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	if err := s.Mgr.SyncEffectiveSkillsForScope(scope); err != nil {
		s.logf("admin: sync effective skills failed scope=%+v: %v", scope, err)
	}
	if err := s.Mgr.RestartScope(scope); err != nil {
		s.logf("admin: restart scope after skill delete failed scope=%+v: %v", scope, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "name": name})
}
