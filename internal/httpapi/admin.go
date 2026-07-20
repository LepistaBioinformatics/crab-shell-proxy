package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/authz"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
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

// adminScope reads scope/tenant_id/subs_acc_id via get (query or form), writing
// a 400 and returning ok=false on a bad value.
func (s *Server) adminScope(w http.ResponseWriter, get func(string) string) (docker.Scope, bool) {
	scope, msg := parseAdminScope(get("scope"), get("tenant_id"), get("subs_acc_id"))
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, errBody(msg))
		return docker.Scope{}, false
	}
	return scope, true
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
		if errors.Is(err, docker.ErrInvalidSecretName) {
			writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
			return
		}
		s.logf("admin: delete shared secret failed scope=%+v: %v", scope, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
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

// --- admin-model-override (CTX-AMO-06: keys never transit this API) ---

// handleAdminModelsList returns the caller agent's SELECTABLE models
// {provider,name} only — never an api key or apiKeyEnv (CTX-AMO-06).
func (s *Server) handleAdminModelsList(w http.ResponseWriter, r *http.Request) {
	agent, _, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	models := make([]map[string]any, 0, len(agent.SelectableModels()))
	for _, mc := range agent.SelectableModels() {
		models = append(models, map[string]any{"provider": mc.Provider, "name": mc.Name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// resolveModelTarget parses+authorizes a model-override target shared by
// GET/PUT/DELETE /v1/admin/model, regardless of whether the ids came from the
// query string or a JSON body. When userAccIDStr is non-empty the target
// addresses one specific user (authorized like adminUserFileKey: authority over
// that user's subscription); otherwise it addresses a tenant/subscription scope
// (authorized like the shared-content handlers). It writes the error response
// and returns ok=false on any failure.
func (s *Server) resolveModelTarget(w http.ResponseWriter, agent config.Agent, ident identity.Identity,
	scopeKind, tenantIDStr, subsAccIDStr, userAccIDStr string) (docker.ModelTarget, docker.Scope, docker.WorkspaceKey, bool) {
	if userAccIDStr != "" {
		tenantID, err := uuid.Parse(tenantIDStr)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" is required and must be a UUID`))
			return docker.ModelTarget{}, docker.Scope{}, docker.WorkspaceKey{}, false
		}
		subsAccID, err := uuid.Parse(subsAccIDStr)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" is required and must be a UUID`))
			return docker.ModelTarget{}, docker.Scope{}, docker.WorkspaceKey{}, false
		}
		userAccID, err := uuid.Parse(userAccIDStr)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errBody(`"user_acc_id" must be a UUID`))
			return docker.ModelTarget{}, docker.Scope{}, docker.WorkspaceKey{}, false
		}
		if !authz.AuthorizeUserManagement(ident.Profile, tenantID.String(), subsAccID.String()) {
			writeJSON(w, http.StatusForbidden, errBody("not authorized to manage this user's model"))
			return docker.ModelTarget{}, docker.Scope{}, docker.WorkspaceKey{}, false
		}
		target := docker.ModelTarget{
			Kind: docker.ScopeSubscription, TenantID: tenantID.String(), SubsAccID: subsAccID.String(),
			Role: agent.Key, UserAccID: userAccID.String(),
		}
		key := docker.WorkspaceKey{
			TenantID: tenantID.String(), SubsAccID: subsAccID.String(), Role: agent.Key, UserAccID: userAccID.String(),
		}
		return target, docker.Scope{}, key, true
	}

	scope, msg := parseAdminScope(scopeKind, tenantIDStr, subsAccIDStr)
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, errBody(msg))
		return docker.ModelTarget{}, docker.Scope{}, docker.WorkspaceKey{}, false
	}
	if !authz.AuthorizeSharedScope(ident.Profile, string(scope.Kind), scope.TenantID, scope.SubsAccID) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to administer this scope"))
		return docker.ModelTarget{}, docker.Scope{}, docker.WorkspaceKey{}, false
	}
	target := docker.ModelTarget{Kind: scope.Kind, TenantID: scope.TenantID, SubsAccID: scope.SubsAccID}
	return target, scope, docker.WorkspaceKey{}, true
}

// writeModelResult writes the {provider,name,level} response shape shared by
// GET/PUT — never an api key (CTX-AMO-06). A nil model (no override anywhere
// and no agent default) reports empty provider/name at level "default".
func writeModelResult(w http.ResponseWriter, model *config.ModelConfig, level string) {
	resp := map[string]any{"level": level}
	if model != nil {
		resp["provider"] = model.Provider
		resp["name"] = model.Name
	} else {
		resp["provider"] = ""
		resp["name"] = ""
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAdminModelGet returns the current effective model override at a
// scope/user (+ which level set it): tenant|subscription|user|default. NO key.
func (s *Server) handleAdminModelGet(w http.ResponseWriter, r *http.Request) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	target, _, _, ok := s.resolveModelTarget(w, agent, ident, q.Get("scope"), q.Get("tenant_id"), q.Get("subs_acc_id"), q.Get("user_acc_id"))
	if !ok {
		return
	}
	model, level := s.Mgr.EffectiveModel(agent, target)
	writeModelResult(w, model, level)
}

// adminModelRequest is the PUT /v1/admin/model body.
type adminModelRequest struct {
	Scope     string `json:"scope"`
	TenantID  string `json:"tenant_id"`
	SubsAccID string `json:"subs_acc_id"`
	UserAccID string `json:"user_acc_id"`
	Provider  string `json:"provider"`
	Name      string `json:"name"`
}

// handleAdminModelSet sets a model override at a target: validates
// {provider,name} against the agent's selectable allowlist (400 if not
// selectable — nothing is written), writes the override, then re-applies it to
// every established workspace under the target (or the single user) and
// restarts the running ones so picoclaw reloads it.
func (s *Server) handleAdminModelSet(w http.ResponseWriter, r *http.Request) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	var req adminModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	if agent.FindModel(req.Provider, req.Name) == nil {
		writeJSON(w, http.StatusBadRequest, errBody("model is not in the agent's selectable allowlist"))
		return
	}
	target, scope, key, ok := s.resolveModelTarget(w, agent, ident, req.Scope, req.TenantID, req.SubsAccID, req.UserAccID)
	if !ok {
		return
	}
	sel := docker.ModelSel{Provider: req.Provider, Name: req.Name}
	if err := s.Mgr.SetModelOverride(target, sel); err != nil {
		s.logf("admin: set model override failed target=%+v: %v", target, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	if req.UserAccID != "" {
		if err := s.Mgr.ReapplyModelUser(key, agent); err != nil {
			s.logf("admin: reapply model user failed key=%+v: %v", key, err)
		}
	} else if err := s.Mgr.ReapplyModelScope(scope); err != nil {
		s.logf("admin: reapply model scope failed scope=%+v: %v", scope, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "provider": req.Provider, "name": req.Name})
}

// handleAdminModelClear clears a model override at a target (idempotent),
// falls back to the next level (re-applied to every established workspace
// under the target), and restarts the running ones.
func (s *Server) handleAdminModelClear(w http.ResponseWriter, r *http.Request) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	target, scope, key, ok := s.resolveModelTarget(w, agent, ident, q.Get("scope"), q.Get("tenant_id"), q.Get("subs_acc_id"), q.Get("user_acc_id"))
	if !ok {
		return
	}
	if err := s.Mgr.ClearModelOverride(target); err != nil {
		s.logf("admin: clear model override failed target=%+v: %v", target, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	if q.Get("user_acc_id") != "" {
		if err := s.Mgr.ReapplyModelUser(key, agent); err != nil {
			s.logf("admin: reapply model user failed key=%+v: %v", key, err)
		}
	} else if err := s.Mgr.ReapplyModelScope(scope); err != nil {
		s.logf("admin: reapply model scope failed scope=%+v: %v", scope, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "cleared"})
}

// handleAdminModelUsers mirrors handleAdminUsersList but includes each user's
// current effective model {provider,name,level} for the per-user override UI.
// Each user's OWN agent (by role) resolves its model — a subscription can host
// users under several different agents. NO key ever appears (CTX-AMO-06).
func (s *Server) handleAdminModelUsers(w http.ResponseWriter, r *http.Request) {
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
		s.logf("admin: list model users failed tenant=%s subs=%s: %v", tenantID, subsAccID, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		entry := map[string]any{"accId": u.AccID, "role": u.Role, "email": u.Email}
		if userAgent, ok := s.Cfg.Agents[u.Role]; ok {
			target := docker.ModelTarget{
				Kind: docker.ScopeSubscription, TenantID: tenantID.String(), SubsAccID: subsAccID.String(),
				Role: u.Role, UserAccID: u.AccID,
			}
			model, level := s.Mgr.EffectiveModel(userAgent, target)
			entry["level"] = level
			if model != nil {
				entry["provider"] = model.Provider
				entry["name"] = model.Name
			}
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

// --- admin-model-registry (per-agent model definitions + keys) ---

type registeredModelRequest struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Model    string `json:"model"`
	APIBase  string `json:"api_base"`
	APIKey   string `json:"api_key"`
}

type applyRegisteredModelRequest struct {
	TenantID  string `json:"tenant_id"`
	SubsAccID string `json:"subs_acc_id"`
	UserAccID string `json:"user_acc_id"`
	Provider  string `json:"provider"`
	Name      string `json:"name"`
}

// handleAdminRegisteredModelsList returns the agent's registered models (never
// keys). The agent is resolved from the routed picoclaw service, so registration
// is per-agent. Admin-privilege gated (registry is agent-global).
func (s *Server) handleAdminRegisteredModelsList(w http.ResponseWriter, r *http.Request) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	if !ident.Profile.HasAdminPrivileges() {
		writeJSON(w, http.StatusForbidden, errBody("admin privileges required to manage the model registry"))
		return
	}
	models, err := s.Mgr.ListRegisteredModels(agent.Key)
	if err != nil {
		s.logf("registered-models: list failed svc=%s: %v", agent.Key, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// handleAdminRegisteredModelsPost registers/updates a model (definition + key)
// for the routed agent.
func (s *Server) handleAdminRegisteredModelsPost(w http.ResponseWriter, r *http.Request) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	if !ident.Profile.HasAdminPrivileges() {
		writeJSON(w, http.StatusForbidden, errBody("admin privileges required to manage the model registry"))
		return
	}
	var req registeredModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	if req.Provider == "" || req.Name == "" || req.Model == "" || req.APIBase == "" || req.APIKey == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"provider", "name", "model", "api_base" and "api_key" are required`))
		return
	}
	rm := docker.RegisteredModel{
		Provider: req.Provider, Name: req.Name, Model: req.Model, APIBase: req.APIBase, APIKey: req.APIKey,
	}
	if err := s.Mgr.AddRegisteredModel(agent.Key, rm); err != nil {
		s.logf("registered-models: add failed svc=%s: %v", agent.Key, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "provider": req.Provider, "name": req.Name})
}

// handleAdminRegisteredModelsDelete removes a model from the routed agent's registry.
func (s *Server) handleAdminRegisteredModelsDelete(w http.ResponseWriter, r *http.Request) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	if !ident.Profile.HasAdminPrivileges() {
		writeJSON(w, http.StatusForbidden, errBody("admin privileges required to manage the model registry"))
		return
	}
	provider := r.URL.Query().Get("provider")
	name := r.URL.Query().Get("name")
	if provider == "" || name == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"provider" and "name" query parameters are required`))
		return
	}
	if err := s.Mgr.DeleteRegisteredModel(agent.Key, provider, name); err != nil {
		s.logf("registered-models: delete failed svc=%s: %v", agent.Key, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleAdminRegisteredModelApply assigns a registered model to one specific
// user (writes its definition + key into that user's config) and restarts them.
// Authorized like the other per-user admin ops (authority over the subscription).
func (s *Server) handleAdminRegisteredModelApply(w http.ResponseWriter, r *http.Request) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	var req applyRegisteredModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	tenantID, err := uuid.Parse(req.TenantID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" is required and must be a UUID`))
		return
	}
	subsAccID, err := uuid.Parse(req.SubsAccID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" is required and must be a UUID`))
		return
	}
	userAccID, err := uuid.Parse(req.UserAccID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"user_acc_id" is required and must be a UUID`))
		return
	}
	if req.Provider == "" || req.Name == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"provider" and "name" are required`))
		return
	}
	if !authz.AuthorizeUserManagement(ident.Profile, tenantID.String(), subsAccID.String()) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to manage this user"))
		return
	}
	key := docker.WorkspaceKey{
		TenantID: tenantID.String(), SubsAccID: subsAccID.String(), Role: agent.Key, UserAccID: userAccID.String(),
	}
	if err := s.Mgr.ApplyRegisteredModelToUser(agent.Key, key, req.Provider, req.Name); err != nil {
		if errors.Is(err, docker.ErrRegisteredModelNotFound) {
			writeJSON(w, http.StatusNotFound, errBody(err.Error()))
			return
		}
		if errors.Is(err, docker.ErrWorkspaceNotProvisioned) {
			writeJSON(w, http.StatusConflict, errBody(err.Error()))
			return
		}
		s.logf("registered-models: apply failed svc=%s user=%s: %v", agent.Key, userAccID, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	if err := s.Mgr.RestartWorkspace(key); err != nil {
		s.logf("registered-models: restart failed svc=%s user=%s: %v", agent.Key, userAccID, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "provider": req.Provider, "name": req.Name})
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
