package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/authz"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
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

// rejectNonPicoclawAgent writes a 400 and returns true when the routed agent is
// not governed by the model inventory.
//
// The gate lives HERE, in the proxy, because the proxy is the gate (NFR-6) — a
// webapp-only filter leaves this open. hermes agents read their model from the
// proxy's config.yaml (CTX-MR-13), so an assignment or agent-level default written
// for one is a record nothing ever reads: it restarts the container for nothing and
// leaves a phantom workspace referrer that blocks delete and disable of that model
// forever.
func rejectNonPicoclawAgent(w http.ResponseWriter, agent config.Agent) bool {
	if agent.Harness == "" || agent.Harness == config.HarnessPicoclaw {
		return false
	}
	writeJSON(w, http.StatusBadRequest, errBody("agent "+agent.Key+" runs the "+agent.Harness+
		" harness, which reads its model from the proxy configuration; the model inventory "+
		"governs picoclaw agents only"))
	return true
}

// authorizeScopeDefault gates a scope-default operation. global and agent are
// instance-wide, so they need proxy-admin: AuthorizeSharedScope has no level above
// tenant to express, and letting a tenant admin set them would hand them the whole
// instance.
//
// mutating narrows the agent-level check to writes: reading a hermes agent's
// (never-consulted) default is harmless and lets a UI render what is stored, while
// writing one is the operation that does nothing and blocks a model forever.
func (s *Server) authorizeScopeDefault(w http.ResponseWriter, r *http.Request, mutating bool) (registry.ScopeSel, bool) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return registry.ScopeSel{}, false
	}
	sel, err := scopeSelFromQuery(r.URL.Query().Get, agent.Key)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return registry.ScopeSel{}, false
	}
	if mutating && sel.Level == registry.LevelAgent && rejectNonPicoclawAgent(w, agent) {
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
	sel, ok := s.authorizeScopeDefault(w, r, false)
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
	sel, ok := s.authorizeScopeDefault(w, r, true)
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
	s.reapplyForScope(r, sel)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleAdminModelDefaultClear(w http.ResponseWriter, r *http.Request) {
	sel, ok := s.authorizeScopeDefault(w, r, true)
	if !ok {
		return
	}
	if err := s.Reg.ClearScopeDefault(sel); err != nil {
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	s.reapplyForScope(r, sel)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// reapplyForScope re-materializes the workspaces a scope-default change affects.
// A global or agent change has no docker.Scope to express, so it is left to each
// workspace's next start rather than sweeping the whole instance — a fleet-wide
// restart is not something a single admin click should trigger.
func (s *Server) reapplyForScope(r *http.Request, sel registry.ScopeSel) {
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
	// A malformed policy here cannot be reported — the default is already saved
	// and this runs past the response — so fall back to bouncing, which is the
	// behaviour this path always had.
	p, err := restartPolicyFrom(r)
	if err != nil {
		s.logf("model default: bad restart policy, bouncing: %v", err)
		p = RestartPolicy{Mode: PolicyNow}
	}
	if err := s.Mgr.ReapplyModelScope(scope, p.Mode == PolicyNow); err != nil {
		s.logf("model default: reapply scope %+v: %v", scope, err)
	}
	if p.Mode == PolicySchedule {
		s.Mgr.ArmScheduledBounce(scope, *p.ScheduledAt)
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
	if rejectNonPicoclawAgent(w, agent) {
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
	bounce, ok := s.bounceNow(w, r)
	if !ok {
		return
	}
	if err := s.Mgr.SetModelAssignment(key, req.ModelName, bounce); err != nil {
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
	bounce, ok := s.bounceNow(w, r)
	if !ok {
		return
	}
	if err := s.Mgr.ClearModelAssignment(key, bounce); err != nil {
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleAdminModelAssignmentList reports which users under a subscription are
// pinned and to what, so the admin UI can show a pin and tell it apart from a
// cascade (FR-27). Without this read every user renders as "inherited from scope",
// including the pinned ones, and "Unpin" looks like it does nothing.
//
// It spans agents under the pair rather than reporting only the routed one: a
// subscription's users may each have a workspace under a different agent, and a
// routed-agent-only read would show exactly those users as unpinned. The authority
// checked is authority over the (tenant, subscription), which is what
// AuthorizeUserManagement expresses; a model name is not a credential.
func (s *Server) handleAdminModelAssignmentList(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	tenantID, err := uuid.Parse(q.Get("tenant_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" is required and must be a UUID`))
		return
	}
	subsAccID, err := uuid.Parse(q.Get("subs_acc_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" is required and must be a UUID`))
		return
	}
	if !authz.AuthorizeUserManagement(ident.Profile, tenantID.String(), subsAccID.String()) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to manage this subscription's users"))
		return
	}
	entries, err := s.Reg.AssignmentsUnder(tenantID.String(), subsAccID.String())
	if err != nil {
		status, body := registryErrStatus(err)
		writeJSON(w, status, body)
		return
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"agent":        e.Agent,
			"user_acc_id":  e.UserAccID,
			"model_name":   e.ModelName,
			"chain":        e.Chain,
			"source":       e.Source,
			"materialized": e.MaterializedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"assignments": out})
}
