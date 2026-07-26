package httpapi

import (
	"net/http"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/restart"
)

// The three ways an admin action that needs a container bounce can be applied
// (restart-control FR-4).
const (
	// PolicyNow bounces immediately — the behaviour every site had before this
	// feature, and still the default when a client sends nothing.
	PolicyNow = "now"
	// PolicyNotice applies the change and tells members a restart is needed,
	// leaving the moment to them.
	PolicyNotice = "notice"
	// PolicySchedule applies the change and bounces the scope at a chosen time.
	PolicySchedule = "schedule"
)

// RestartPolicy is how the caller wants the bounce delivered.
type RestartPolicy struct {
	Mode        string
	ScheduledAt *time.Time
	Note        string
}

// restartPolicyFrom reads the policy off a request's query string. Query
// parameters rather than a body field so the multipart upload handlers (shared
// files, skills) need no body change, and so a GET-shaped delete can carry it
// too.
//
// An absent `restart` parameter yields PolicyNow, which is what makes this
// change additive: every existing client keeps today's exact behaviour
// (FR-4.2).
func restartPolicyFrom(r *http.Request) (RestartPolicy, error) {
	q := r.URL.Query()
	return parsePolicyFields(q.Get("restart"), q.Get("restart_at"), q.Get("restart_note"))
}

// applyRestartPolicy is the single place an admin-triggered restart is decided.
//
// Propagation always runs: rebuilding the effective secret view and merging the
// native slots is how the change reaches disk, and deferring THAT would mean a
// member pressing "restart now" gets a bounce with nothing new to load (FR-4.1).
// Only the container stop/start is subject to the policy.
//
// PolicyNow deliberately raises no notice. Raising one and immediately bouncing
// would leave a noticeAt newer than the marker of every container that was not
// running, showing those members a banner for a change they will pick up on
// their next cold start anyway.
func (s *Server) applyRestartPolicy(
	scope docker.Scope, reason restart.Reason, p RestartPolicy, by string,
) error {
	if err := s.Mgr.PropagateScope(scope); err != nil {
		return err
	}

	switch p.Mode {
	case PolicyNotice, PolicySchedule:
		notice := restart.Notice{
			NoticeAt:    time.Now().UTC(),
			ScheduledAt: p.ScheduledAt,
			Reason:      reason,
			Note:        p.Note,
			By:          by,
		}
		if err := s.Mgr.RaiseRestartNotice(scope, notice); err != nil {
			return err
		}
		if p.Mode == PolicySchedule {
			s.Mgr.ArmScheduledBounce(scope, *p.ScheduledAt)
		}
		return nil
	default: // PolicyNow
		return s.Mgr.BounceScope(scope)
	}
}

// parseRestartPolicy validates the policy BEFORE the mutation runs. Validating
// it afterwards would 400 a write that already succeeded, telling the caller
// their request failed when only the restart instruction was unusable.
func (s *Server) parseRestartPolicy(w http.ResponseWriter, r *http.Request) (RestartPolicy, bool) {
	p, err := restartPolicyFrom(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return RestartPolicy{}, false
	}
	return p, true
}

// applyParsedRestartPolicy runs after the mutation. A failure is logged, not
// returned: the write already succeeded, and propagation/bounce is best-effort
// exactly as the pre-policy code was.
func (s *Server) applyParsedRestartPolicy(
	scope docker.Scope, reason restart.Reason, p RestartPolicy, by string,
) {
	if err := s.applyRestartPolicy(scope, reason, p, by); err != nil {
		s.logf("restart policy %q on scope=%+v failed: %v", p.Mode, scope, err)
	}
}

// bounceNow reads the restart policy off a request and reduces it to the single
// question the model re-apply paths ask: bounce the affected workspaces now, or
// leave each one a notice? A malformed policy is a 400 and ok=false.
//
// Those paths take no scope — ReapplyModelForModel spans tenants — so a
// scheduled bounce is not offered there: "schedule" behaves as "notice", and the
// admin can arm the window separately via POST /v1/admin/restart.
func (s *Server) bounceNow(w http.ResponseWriter, r *http.Request) (bool, bool) {
	p, err := restartPolicyFrom(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return false, false
	}
	return p.Mode == PolicyNow, true
}
