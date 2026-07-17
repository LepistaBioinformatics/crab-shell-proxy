package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/google/uuid"
	"github.com/sgelias/crab-shell-proxy/internal/authz"
	"github.com/sgelias/crab-shell-proxy/internal/docker"
	"github.com/sgelias/crab-shell-proxy/internal/identity"
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
