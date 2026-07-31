package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/authz"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/restart"
)

// Bulk instance-config administration (admin-bulk-instance-config): see how ONE
// config.json key varies across every instance of one agent in one subscription,
// then set it everywhere it differs.
//
// This is NOT the content endpoint admin-shared-content FR-7 forbids, for the same
// three reasons the single-instance editor states (admin_instance_config.go), and
// the "no content route here" instructions in admin.go, in the webapp's BFF route
// and in members-panel.tsx all stay in force:
//
//   - FR-7's subject is the set ListUserFiles enumerates, which is the UPLOADS dir
//     alone. config.json is not in it.
//   - FR-7 protects MEMBER-AUTHORED content. config.json is proxy-materialized
//     provisioning state.
//   - These routes take no name/path/file parameter. Do not add one.
//
// FR-6.4, stated rather than buried: config.json carries the agent's sandbox
// boundary — restrict_to_workspace, allow_read_outside_workspace,
// tools.allow_read_paths / allow_write_paths. This endpoint reaches N of those in
// one request where the single-instance editor reaches one. The authority is
// deliberately the same (AuthorizeUserManagement, that feature's DEC-3); what
// makes the reach reconstructable afterwards is that every apply AND every refusal
// emits an audit line.

// maxBulkConfigEnvelope caps the request envelope. This endpoint carries one value
// and a revision map, not a document, so it needs nowhere near the 1 MiB the
// single-instance editor allows for whole files.
const maxBulkConfigEnvelope = 256 << 10

// adminBulkConfigScope resolves the subscription + agent a bulk op targets and
// gates it on user-management authority.
//
// Modelled on adminInstanceKey, NOT on adminScope. adminScope accepts
// scope=tenant|subscription, and this feature's ceiling is one subscription
// (DEC-1) — so there is deliberately no `scope` parameter here at all. The ceiling
// therefore holds because there is no tenant form of the request to make, not
// because a check rejects one; a future edit cannot weaken it by deleting a
// branch.
//
// The returned config.Agent carries Template, which the template endpoints need:
// `template` is a config.yaml field distinct from the agent key, so the agent key
// must never be used as a directory name. That is also why config.TemplatesDir's
// unsanitized segment is safe here — the value comes from the operator's own
// config, never from the request.
func (s *Server) adminBulkConfigScope(
	w http.ResponseWriter, r *http.Request, ident identity.Identity,
) (docker.Scope, config.Agent, bool) {
	q := r.URL.Query()
	tenantID, err := uuid.Parse(q.Get("tenant_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" query parameter is required and must be a UUID`))
		return docker.Scope{}, config.Agent{}, false
	}
	subsAccID, err := uuid.Parse(q.Get("subs_acc_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" query parameter is required and must be a UUID`))
		return docker.Scope{}, config.Agent{}, false
	}
	agentKey, msg := s.resolveAgentTarget(q.Get("agent"))
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, errBody(msg))
		return docker.Scope{}, config.Agent{}, false
	}
	// "all" resolves to "" and is refused: every path in this feature is per-agent
	// (the template is per-agent, and workspacesInScope filters by AgentKey).
	if agentKey == "" {
		writeJSON(w, http.StatusBadRequest,
			errBody(`"agent" query parameter is required and must name one configured agent`))
		return docker.Scope{}, config.Agent{}, false
	}
	if !authz.AuthorizeUserManagement(ident.Profile, tenantID.String(), subsAccID.String()) {
		writeJSON(w, http.StatusForbidden, errBody("not authorized to manage this subscription's configuration"))
		return docker.Scope{}, config.Agent{}, false
	}
	return docker.Scope{
		Kind:      docker.ScopeSubscription,
		TenantID:  tenantID.String(),
		SubsAccID: subsAccID.String(),
		AgentKey:  agentKey,
	}, s.Cfg.Agents[agentKey], true
}

// writeBulkConfigError maps the docker layer's sentinels onto status codes. Both
// key refusals are 400s the UI shows on the key field, not page-level failures.
func (s *Server) writeBulkConfigError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, docker.ErrInvalidConfigKey):
		writeJSON(w, http.StatusBadRequest, errBody("invalid_key: "+err.Error()))
	case errors.Is(err, docker.ErrManagedConfigPath):
		writeJSON(w, http.StatusBadRequest, errBody("managed_path: "+err.Error()))
	case errors.Is(err, docker.ErrInvalidConfigValue):
		writeJSON(w, http.StatusBadRequest, errBody("invalid_value: "+err.Error()))
	case errors.Is(err, docker.ErrNotProvisioned):
		writeJSON(w, http.StatusNotFound, errBody("not_provisioned: "+err.Error()))
	case errors.Is(err, docker.ErrStaleRevision):
		writeJSON(w, http.StatusConflict, errBody("stale_revision: "+err.Error()))
	default:
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
	}
}

// handleAdminScopeConfigKeys returns the agent template's dotted leaf paths, which
// is what the key picker offers. It is a suggestion list, not a whitelist: the
// inspect and apply verbs accept any syntactically valid dotted path, because a
// newer picoclaw's field or one added by an earlier repair is legitimately absent
// from the template.
func (s *Server) handleAdminScopeConfigKeys(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	_, agent, ok := s.adminBulkConfigScope(w, r, ident)
	if !ok {
		s.logBulkConfigRefusal(ident, r, "rejected")
		return
	}
	cat, err := s.Mgr.TemplateConfigKeys(agent.Template)
	if err != nil {
		s.writeBulkConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cat)
}

// handleAdminScopeConfigInspect groups the subscription's instances by what they
// hold at one key. This is the half that makes the write safe: without it a bulk
// apply is a blind overwrite, and it is also where the per-instance revisions the
// apply needs come from.
func (s *Server) handleAdminScopeConfigInspect(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	scope, _, ok := s.adminBulkConfigScope(w, r, ident)
	if !ok {
		s.logBulkConfigRefusal(ident, r, "rejected")
		return
	}
	insp, err := s.Mgr.InspectScopeConfigKey(scope, r.URL.Query().Get("key"))
	if err != nil {
		s.writeBulkConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, insp)
}

// scopeConfigApplyResponse embeds the docker layer's result and adds the optional
// template outcome. The two are composed HERE rather than inside
// ApplyScopeConfigKey because the template write is a separate decision on a
// separate document: the docker-layer apply has no business knowing templates
// exist.
type scopeConfigApplyResponse struct {
	docker.ScopeConfigResult
	Template *docker.TemplateResult `json:"template,omitempty"`
	// Subscription is the scoped-seed half: this subscription's FUTURE members.
	// Reported separately from Template because they reach different populations —
	// one subscription versus every subscription on the agent.
	Subscription *docker.OverlayResult `json:"subscription,omitempty"`
}

// handleAdminScopeConfigPut sets one key across every instance that differs.
//
// The restart policy is parsed BEFORE the apply — validating it afterwards would
// 400 a change that already landed — and delivered after, per CHANGED workspace.
func (s *Server) handleAdminScopeConfigPut(w http.ResponseWriter, r *http.Request) {
	_, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	scope, agent, ok := s.adminBulkConfigScope(w, r, ident)
	if !ok {
		// A refused write is audited too, and this is the only place that can do it:
		// adminBulkConfigScope has already answered the request. A 403 on an endpoint
		// that reaches N sandbox boundaries is the line an operator most wants.
		s.logBulkConfigRefusal(ident, r, "rejected")
		return
	}
	bounce, ok := s.bulkConfigBounce(w, r)
	if !ok {
		s.logBulkConfigRefusal(ident, r, "bad_restart_policy")
		return
	}

	// MaxBytesReader rather than io.LimitReader: truncating the body would surface
	// as an unexpected-EOF JSON error, which reads as "you sent malformed JSON"
	// when the truth is "you sent too much". This one distinguishes them.
	var req docker.ScopeConfigChange
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBulkConfigEnvelope)).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errBody("too_large: request body exceeds the limit"))
			s.logBulkConfigRefusal(ident, r, "too_large")
			return
		}
		writeJSON(w, http.StatusBadRequest, errBody("invalid request body: "+err.Error()))
		s.logBulkConfigRefusal(ident, r, "bad_body")
		return
	}

	// Decoded once, here, because both halves need it in different shapes: the
	// instance apply compares and re-encodes the raw bytes, and the template write
	// takes an already-decoded value.
	var decoded any
	if len(req.Value) > 0 {
		if err := json.Unmarshal(req.Value, &decoded); err != nil {
			writeJSON(w, http.StatusBadRequest, errBody("invalid_value: "+err.Error()))
			s.logBulkConfigRefusal(ident, r, "bad_value")
			return
		}
	}

	at := time.Now().UTC()
	by := callerLabel(ident)
	// Set server-side, after the decode, which is the whole point of their json:"-".
	req.By = by
	req.AppliedAt = at
	res, err := s.Mgr.ApplyScopeConfigKey(scope, req)
	if err != nil {
		s.writeBulkConfigError(w, err)
		s.logBulkConfigRefusal(ident, r, "rejected")
		return
	}

	out := scopeConfigApplyResponse{ScopeConfigResult: res}
	if req.AlsoSubscription {
		ov, oerr := s.Mgr.ApplyOverlayConfigKey(scope, req.Key, req.Value, by, at)
		if oerr != nil {
			ov = docker.OverlayResult{OK: false, Detail: oerr.Error()}
		}
		out.Subscription = &ov
		s.logf("admin: bulk config scoped seed by=%s tenant=%s subs=%s agent=%s key=%s ok=%t detail=%s",
			by, scope.TenantID, scope.SubsAccID, scope.AgentKey, req.Key, ov.OK, ov.Detail)
	}
	if req.AlsoTemplate {
		tpl, terr := s.Mgr.ApplyTemplateConfigKey(agent.Template, req.Key, decoded, req.TemplateRevision, by, at)
		if terr != nil {
			tpl = docker.TemplateResult{OK: false, Detail: terr.Error()}
		}
		out.Template = &tpl
		// A separate line because the blast radius is different in kind: the template
		// seeds future members of EVERY subscription, and of every agent declaring
		// the same template.
		s.logf("admin: bulk config template write by=%s template=%s key=%s ok=%t detail=%s",
			by, agent.Template, req.Key, tpl.OK, tpl.Detail)
	}

	s.deliverBulkConfigRestarts(scope, res, bounce)

	// The key, never the value: a hand-typed path could address a
	// credential-bearing field.
	s.logf("admin: bulk config apply by=%s tenant=%s subs=%s agent=%s key=%s outcomes=%v alsoTemplate=%t alsoSubscription=%t restart=%s",
		by, scope.TenantID, scope.SubsAccID, scope.AgentKey, req.Key, res.Summary, req.AlsoTemplate,
		req.AlsoSubscription, restartLabel(bounce))

	writeJSON(w, http.StatusOK, out)
}

// bulkConfigBounce reduces the restart policy to the one question the delivery
// asks, defaulting an ABSENT parameter to notice rather than now (DEC-9).
//
// The substitution is local on purpose. parsePolicyFields defaults to now and is
// shared by every sibling endpoint, where the target is one workspace or one
// scope's secrets; changing it there would silently flip all of them. Here the
// default would mean "N members lose their running agent at once because nobody
// chose anything", which is not a default worth having. `now` stays available
// explicitly, and `schedule` degrades to notice as it does at every other
// per-workspace site.
func (s *Server) bulkConfigBounce(w http.ResponseWriter, r *http.Request) (bool, bool) {
	q := r.URL.Query()
	mode := q.Get("restart")
	if mode == "" {
		mode = PolicyNotice
	}
	p, err := parsePolicyFields(mode, q.Get("restart_at"), q.Get("restart_note"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return false, false
	}
	return p.Mode == PolicyNow, true
}

// deliverBulkConfigRestarts touches ONLY the workspaces that changed (DEC-8).
//
// applyRestartPolicy is deliberately not used: BounceScope selects containers by
// label, so it cannot know which instances this apply actually wrote — it would
// restart the members reported unchanged, stale and unreadable as well, and it
// would also run PropagateScope (a secrets sync) that a config change does not
// need. The per-workspace reduction is what the targeted model re-apply paths
// already do for the same reason.
func (s *Server) deliverBulkConfigRestarts(scope docker.Scope, res docker.ScopeConfigResult, bounce bool) {
	for _, o := range res.Outcomes {
		if o.Outcome != docker.OutcomeApplied {
			continue
		}
		key := docker.WorkspaceKey{
			TenantID:  scope.TenantID,
			SubsAccID: scope.SubsAccID,
			Role:      scope.AgentKey,
			UserAccID: o.UserAccID,
		}
		if bounce {
			if err := s.Mgr.RestartWorkspace(key); err != nil {
				s.logf("bulk config: restart %s/%s failed: %v", scope.AgentKey, o.UserAccID, err)
			}
			continue
		}
		if err := s.Mgr.RaiseWorkspaceRestartNotice(key, restart.ReasonConfig); err != nil {
			s.logf("bulk config: notice %s/%s failed: %v", scope.AgentKey, o.UserAccID, err)
		}
	}
}

func restartLabel(bounce bool) string {
	if bounce {
		return PolicyNow
	}
	return PolicyNotice
}

// logBulkConfigRefusal audits a request that never reached the apply. Refusals
// matter as much as successes here: FR-6.4 leans on the audit to justify the
// authority tier, and an unlogged 403 is exactly the case that would undermine it.
// The key is unusable at this point, so the raw parameters are what gets recorded.
func (s *Server) logBulkConfigRefusal(ident identity.Identity, r *http.Request, result string) {
	q := r.URL.Query()
	s.logf("admin: bulk config by=%s tenant=%s subs=%s agent=%s key=%s result=%s",
		callerLabel(ident), q.Get("tenant_id"), q.Get("subs_acc_id"), q.Get("agent"), q.Get("key"), result)
}
