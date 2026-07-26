package docker

import (
	"context"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/restart"
)

// RestartStatus is what a member sees: whether their instance still needs a
// bounce, why, when one is scheduled, and whether the container is up right now.
type RestartStatus struct {
	restart.Status
	// Running reports whether the container exists and is up. It is what lets
	// the caller distinguish a real restart from a no-op, and lets the UI avoid
	// promising a bounce that will not happen.
	Running bool `json:"running"`
}

// RestartStatus answers the member's "do I need to restart?" question for one
// workspace. The Docker call is a single cheap Inspect — this sits on the chat
// screen's polling path (NFR-3).
func (m *Manager) RestartStatus(key WorkspaceKey) (RestartStatus, error) {
	st, err := m.restarts.Status(key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if err != nil {
		return RestartStatus{}, err
	}
	out := RestartStatus{Status: st}
	// A daemon hiccup must not break the banner: report not-running and let the
	// pending flag stand on its own.
	if state, ierr := m.docker.Inspect(context.Background(), m.ContainerName(key)); ierr == nil {
		out.Running = state.Exists && state.Running
	} else {
		m.logf("restart status: inspect %s failed: %v", m.ContainerName(key), ierr)
	}
	return out, nil
}

// RaiseRestartNotice records that a scope needs a bounce.
func (m *Manager) RaiseRestartNotice(scope Scope, n restart.Notice) error {
	return m.restarts.Raise(scope.TenantID, scope.SubsAccID, scope.AgentKey, n)
}

// RestartNotice returns the notice recorded at exactly this scope (no cascade),
// which is what an admin screen shows and amends.
func (m *Manager) RestartNotice(scope Scope) (restart.Notice, bool, error) {
	return m.restarts.Get(scope.TenantID, scope.SubsAccID, scope.AgentKey)
}

// WithdrawRestartNotice removes a scope's notice and disarms any schedule it
// carried, so withdrawing is one action rather than two the caller could get
// half-right.
func (m *Manager) WithdrawRestartNotice(scope Scope) error {
	m.CancelScheduledBounce(scope)
	return m.restarts.Withdraw(scope.TenantID, scope.SubsAccID, scope.AgentKey)
}

// RaiseWorkspaceRestartNotice records a notice that concerns exactly one
// workspace: a member's own secret write (DEC-3), or a model re-apply that
// touched this workspace and not its neighbours. It is stored on that
// workspace's own marker, so it never raises a banner for anyone else.
func (m *Manager) RaiseWorkspaceRestartNotice(key WorkspaceKey, reason restart.Reason) error {
	return m.restarts.RaiseWorkspace(key.TenantID, key.SubsAccID, key.Role, key.UserAccID, reason, time.Now().UTC())
}
