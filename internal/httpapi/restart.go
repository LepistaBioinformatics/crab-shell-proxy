package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/authz"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/restart"
	"github.com/google/uuid"
)

// maxScheduleHorizon bounds how far ahead an admin may schedule a bounce. An
// armed time.AfterFunc lives in memory until it fires, so a mis-typed year must
// not park a timer forever (assumption A-2 in context.md).
const maxScheduleHorizon = 7 * 24 * time.Hour

// --- member surface (restart-control FR-1, FR-2) ---

// handleRestartStatus answers "does my instance still need a restart, and is one
// already scheduled?" for the caller's own workspace.
//
// Read-gated: a member who only holds read on the agent still has to see a
// restart coming, even though they may not trigger one (DEC-1).
func (s *Server) handleRestartStatus(w http.ResponseWriter, r *http.Request) {
	key, ok := s.restartCallerKey(w, r, false)
	if !ok {
		return
	}
	st, err := s.Mgr.RestartStatus(key)
	if err != nil {
		s.logf("restart: status failed key=%+v: %v", key, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleRestartPost restarts the caller's OWN container.
//
// Two things make this safe to expose to a non-admin. The workspace key comes
// from the mycelium profile plus the routed agent — there is no code path here
// that reads a user id from the request, so a body naming someone else is
// ignored rather than honoured (FR-1.1). And the profile chain is re-checked for
// WRITE access in-proxy, so the gate holds even if the gateway route were ever
// declared read-only (FR-1.2).
//
// It calls RestartWorkspace directly, not BounceScope: the member is waiting on
// the answer, so a Docker failure must surface as a 500 carrying the real
// message rather than being swallowed into a log line the way the best-effort
// scope paths do (FR-1.3).
func (s *Server) handleRestartPost(w http.ResponseWriter, r *http.Request) {
	key, ok := s.restartCallerKey(w, r, true)
	if !ok {
		return
	}
	if err := s.Mgr.RestartWorkspace(key); err != nil {
		s.logf("restart: self-restart failed key=%+v: %v", key, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	st, err := s.Mgr.RestartStatus(key)
	if err != nil {
		s.logf("restart: status after restart failed key=%+v: %v", key, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	// "noop" when the container was absent or scaled to zero: nothing was
	// bounced, but the notice is resolved all the same — the next cold start
	// starts from the new state (FR-1.4).
	status := "restarted"
	if !st.Running {
		status = "noop"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        status,
		"lastRestartAt": st.LastRestartAt,
	})
}

// restartCallerKey resolves the caller's own workspace key from the profile and
// the routed agent, enforcing write access when the action mutates.
func (s *Server) restartCallerKey(w http.ResponseWriter, r *http.Request, needWrite bool) (docker.WorkspaceKey, bool) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return docker.WorkspaceKey{}, false
	}
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
	if needWrite {
		return s.authorizeSecret(w, agent, ident, tenantID, subsAccID)
	}
	return s.authorizeRestartRead(w, agent, ident, tenantID, subsAccID)
}

// authorizeRestartRead is authorizeSecret's read-only twin: the caller must hold
// read access on this tenant, for a role named exactly like the resolved agent,
// over this subscription account.
func (s *Server) authorizeRestartRead(
	w http.ResponseWriter, agent config.Agent, ident identity.Identity, tenantID, subsAccID uuid.UUID,
) (docker.WorkspaceKey, bool) {
	if ident.Profile.AccID == subsAccID {
		writeJSON(w, http.StatusForbidden,
			errBody("profile account id must differ from subs_acc_id (act as an individual member)"))
		return docker.WorkspaceKey{}, false
	}
	if _, err := ident.Profile.
		WithReadAccess().
		OnTenant(tenantID).
		WithRoles([]string{agent.Key}).
		OnAccount(subsAccID).
		GetRelatedAccountOrError(); err != nil {
		s.logf("restart: authz denied svc=%s tenant=%s subs=%s user=%s: %v",
			agent.Key, tenantID, subsAccID, ident.AccID, err)
		writeJSON(w, http.StatusForbidden,
			errBody("not licensed to use this subscription for this agent"))
		return docker.WorkspaceKey{}, false
	}
	return docker.WorkspaceKey{
		TenantID:  tenantID.String(),
		SubsAccID: subsAccID.String(),
		Role:      agent.Key,
		UserAccID: ident.AccID,
	}, true
}

// --- admin surface (restart-control FR-5) ---

type adminRestartRequest struct {
	TenantID  string `json:"tenant_id"`
	SubsAccID string `json:"subs_acc_id"`
	AgentKey  string `json:"agent_key"`
	Mode      string `json:"mode"`
	At        string `json:"at"`
	Reason    string `json:"reason"`
	Note      string `json:"note"`
}

// handleAdminRestartGet returns the notice recorded at exactly this scope, so an
// admin screen can show and amend it.
func (s *Server) handleAdminRestartGet(w http.ResponseWriter, r *http.Request) {
	scope, _, ok := s.authorizeRestartScope(w, r, r.URL.Query().Get("tenant_id"),
		r.URL.Query().Get("subs_acc_id"), r.URL.Query().Get("agent_key"))
	if !ok {
		return
	}
	n, found, err := s.Mgr.RestartNotice(scope)
	if err != nil {
		s.logf("admin restart: get failed scope=%+v: %v", scope, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, map[string]any{"notice": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notice": n})
}

// handleAdminRestartPost raises, reschedules or immediately executes a restart
// for a scope. Unlike the policy attached to a content write, this is a
// standalone administrative action: reason defaults to admin-request.
func (s *Server) handleAdminRestartPost(w http.ResponseWriter, r *http.Request) {
	var req adminRestartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	scope, ident, ok := s.authorizeRestartScope(w, r, req.TenantID, req.SubsAccID, req.AgentKey)
	if !ok {
		return
	}

	policy, err := parsePolicyFields(req.Mode, req.At, req.Note)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	}
	reason := restart.Reason(req.Reason)
	if !knownReason(reason) {
		reason = restart.ReasonAdminRequest
	}

	if err := s.applyRestartPolicy(scope, reason, policy, ident.Email); err != nil {
		s.logf("admin restart: apply failed scope=%+v: %v", scope, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "applied", "mode": policy.Mode})
}

// handleAdminRestartDelete withdraws a scope's notice and disarms any schedule.
func (s *Server) handleAdminRestartDelete(w http.ResponseWriter, r *http.Request) {
	scope, _, ok := s.authorizeRestartScope(w, r, r.URL.Query().Get("tenant_id"),
		r.URL.Query().Get("subs_acc_id"), r.URL.Query().Get("agent_key"))
	if !ok {
		return
	}
	if err := s.Mgr.WithdrawRestartNotice(scope); err != nil {
		s.logf("admin restart: withdraw failed scope=%+v: %v", scope, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "withdrawn"})
}

// authorizeRestartScope parses the target scope and applies the same
// authority-over-target check the shared-content endpoints use: a
// subscriptions-manager may act on their own subscription, a tenant manager on
// the whole tenant, instance staff anywhere (FR-5.4).
func (s *Server) authorizeRestartScope(
	w http.ResponseWriter, r *http.Request, rawTenant, rawSubs, agentKey string,
) (docker.Scope, identity.Identity, bool) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return docker.Scope{}, identity.Identity{}, false
	}
	tenantID, err := uuid.Parse(rawTenant)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" is required and must be a UUID`))
		return docker.Scope{}, identity.Identity{}, false
	}
	scope := docker.Scope{Kind: docker.ScopeTenant, TenantID: tenantID.String(), AgentKey: agentKey}
	kind := "tenant"
	if rawSubs != "" {
		subsAccID, err := uuid.Parse(rawSubs)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" must be a UUID when present`))
			return docker.Scope{}, identity.Identity{}, false
		}
		scope.Kind = docker.ScopeSubscription
		scope.SubsAccID = subsAccID.String()
		kind = "subscription"
	}
	if !authz.AuthorizeSharedScope(ident.Profile, kind, scope.TenantID, scope.SubsAccID) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to administer restarts at this scope"))
		return docker.Scope{}, identity.Identity{}, false
	}
	return scope, ident, true
}

func knownReason(r restart.Reason) bool {
	switch r {
	case restart.ReasonSharedSecret, restart.ReasonSharedSkills, restart.ReasonSharedFiles,
		restart.ReasonModel, restart.ReasonOwnSecret, restart.ReasonAdminRequest:
		return true
	}
	return false
}

// parsePolicyFields validates the mode/at/note triple shared by the admin
// restart endpoint and the query-parameter form used on content writes.
func parsePolicyFields(mode, at, note string) (RestartPolicy, error) {
	p := RestartPolicy{Mode: mode, Note: note}
	if p.Mode == "" {
		p.Mode = PolicyNow
	}
	switch p.Mode {
	case PolicyNow, PolicyNotice:
		return p, nil
	case PolicySchedule:
	default:
		return RestartPolicy{}, errors.New(`"mode" must be one of now, notice, schedule`)
	}

	if at == "" {
		return RestartPolicy{}, errors.New(`"at" is required when mode is "schedule"`)
	}
	when, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return RestartPolicy{}, fmt.Errorf(`"at" must be an RFC3339 timestamp: %w`, err)
	}
	when = when.UTC()
	if !when.After(time.Now().UTC()) {
		return RestartPolicy{}, errors.New(`"at" must be in the future`)
	}
	if time.Until(when) > maxScheduleHorizon {
		return RestartPolicy{}, fmt.Errorf(`"at" must be within %d days`, int(maxScheduleHorizon.Hours()/24))
	}
	p.ScheduledAt = &when
	return p, nil
}
