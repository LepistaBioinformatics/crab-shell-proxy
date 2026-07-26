package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/authz"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// scopeSelFromQuery parses ?scope=…&tenant_id=…&subs_acc_id=… into a ScopeSel.
// The agent level takes the agent from the ROUTED service, not a parameter, so a
// caller cannot address another agent's default through their own route.
func scopeSelFromQuery(get func(string) string, routedAgent string) (registry.ScopeSel, error) {
	switch scope := get("scope"); scope {
	case "global":
		return registry.ScopeSel{Level: registry.LevelGlobal}, nil
	case "agent":
		return registry.ScopeSel{Level: registry.LevelAgent, Agent: routedAgent}, nil
	case "tenant":
		return registry.ScopeSel{Level: registry.LevelTenant, TenantID: get("tenant_id")}, nil
	case "subscription":
		return registry.ScopeSel{
			Level: registry.LevelSubscription, TenantID: get("tenant_id"), SubsAccID: get("subs_acc_id"),
		}, nil
	default:
		return registry.ScopeSel{}, fmt.Errorf(`"scope" must be global, agent, tenant or subscription (got %q)`, scope)
	}
}

// authorizeScopeDefault gates a scope-default operation. global and agent are
// instance-wide, so they need proxy-admin: AuthorizeSharedScope has no level above
// tenant to express, and letting a tenant admin set them would hand them the whole
// instance.
func (s *Server) authorizeScopeDefault(w http.ResponseWriter, r *http.Request) (registry.ScopeSel, bool) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return registry.ScopeSel{}, false
	}
	sel, err := scopeSelFromQuery(r.URL.Query().Get, agent.Key)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return registry.ScopeSel{}, false
	}
	switch sel.Level {
	case registry.LevelGlobal, registry.LevelAgent:
		if !ident.Profile.HasAdminPrivileges() {
			writeJSON(w, http.StatusForbidden,
				errBody("admin privileges required to set an instance-wide model default"))
			return registry.ScopeSel{}, false
		}
	default:
		if !authz.AuthorizeSharedScope(ident.Profile, string(sel.Level), sel.TenantID, sel.SubsAccID) {
			writeJSON(w, http.StatusForbidden, errBody("not authorized to administer this scope"))
			return registry.ScopeSel{}, false
		}
	}
	return sel, true
}

func (s *Server) handleAdminModelDefaultGet(w http.ResponseWriter, r *http.Request) {
	sel, ok := s.authorizeScopeDefault(w, r)
	if !ok {
		return
	}
	d, err := s.Reg.GetScopeDefault(sel)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			// Absent is a normal state the UI renders, not an error.
			writeJSON(w, http.StatusOK, map[string]any{"default": nil})
			return
		}
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"default": d})
}

func (s *Server) handleAdminModelDefaultSet(w http.ResponseWriter, r *http.Request) {
	sel, ok := s.authorizeScopeDefault(w, r)
	if !ok {
		return
	}
	var req struct {
		ModelName string `json:"model_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	if err := s.Reg.SetScopeDefault(sel, req.ModelName); err != nil {
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	s.reapplyForScope(sel)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleAdminModelDefaultClear(w http.ResponseWriter, r *http.Request) {
	sel, ok := s.authorizeScopeDefault(w, r)
	if !ok {
		return
	}
	if err := s.Reg.ClearScopeDefault(sel); err != nil {
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	s.reapplyForScope(sel)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// reapplyForScope re-materializes the workspaces a scope-default change affects.
// A global or agent change has no docker.Scope to express, so it is left to each
// workspace's next start rather than sweeping the whole instance — a fleet-wide
// restart is not something a single admin click should trigger.
func (s *Server) reapplyForScope(sel registry.ScopeSel) {
	var scope docker.Scope
	switch sel.Level {
	case registry.LevelTenant:
		scope = docker.Scope{Kind: docker.ScopeTenant, TenantID: sel.TenantID}
	case registry.LevelSubscription:
		scope = docker.Scope{Kind: docker.ScopeSubscription, TenantID: sel.TenantID, SubsAccID: sel.SubsAccID}
	default:
		s.logf("model default %s changed: workspaces pick it up on their next start", sel.Level)
		return
	}
	if err := s.Mgr.ReapplyModelScope(scope); err != nil {
		s.logf("model default: reapply scope %+v: %v", scope, err)
	}
}

type modelAssignmentRequest struct {
	TenantID  string `json:"tenant_id"`
	SubsAccID string `json:"subs_acc_id"`
	UserAccID string `json:"user_acc_id"`
	ModelName string `json:"model_name"`
}

// resolveAssignmentTarget parses and authorizes a per-user assignment, reusing the
// same authority check every other per-user admin operation uses.
func (s *Server) resolveAssignmentTarget(w http.ResponseWriter, r *http.Request) (docker.WorkspaceKey, modelAssignmentRequest, bool) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return docker.WorkspaceKey{}, modelAssignmentRequest{}, false
	}
	var req modelAssignmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return docker.WorkspaceKey{}, req, false
	}
	tenantID, err := uuid.Parse(req.TenantID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" is required and must be a UUID`))
		return docker.WorkspaceKey{}, req, false
	}
	subsAccID, err := uuid.Parse(req.SubsAccID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" is required and must be a UUID`))
		return docker.WorkspaceKey{}, req, false
	}
	userAccID, err := uuid.Parse(req.UserAccID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"user_acc_id" is required and must be a UUID`))
		return docker.WorkspaceKey{}, req, false
	}
	if !authz.AuthorizeUserManagement(ident.Profile, tenantID.String(), subsAccID.String()) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to manage this user"))
		return docker.WorkspaceKey{}, req, false
	}
	return docker.WorkspaceKey{
		TenantID: tenantID.String(), SubsAccID: subsAccID.String(),
		Role: agent.Key, UserAccID: userAccID.String(),
	}, req, true
}

func (s *Server) handleAdminModelAssignmentSet(w http.ResponseWriter, r *http.Request) {
	key, req, ok := s.resolveAssignmentTarget(w, r)
	if !ok {
		return
	}
	m, err := s.Reg.GetModel(req.ModelName)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeJSON(w, http.StatusBadRequest, errBody("model "+req.ModelName+" is not in the inventory"))
			return
		}
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	if m.Status == registry.StatusDisabled {
		writeJSON(w, http.StatusBadRequest,
			errBody("model "+req.ModelName+" is disabled and cannot be assigned"))
		return
	}
	if err := s.Mgr.SetModelAssignment(key, req.ModelName); err != nil {
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "model_name": req.ModelName})
}

func (s *Server) handleAdminModelAssignmentClear(w http.ResponseWriter, r *http.Request) {
	key, _, ok := s.resolveAssignmentTarget(w, r)
	if !ok {
		return
	}
	if err := s.Mgr.ClearModelAssignment(key); err != nil {
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
