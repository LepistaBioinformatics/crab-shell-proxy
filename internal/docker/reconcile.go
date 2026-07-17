package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/sgelias/crab-shell-proxy/internal/config"
)

// Reconcile brings the manager's in-memory state in line with reality at boot:
//   - adopts already-running managed containers (re-arming scale-to-zero timers),
//   - ensures continuous-mode containers are started (CSP-08/09/10).
//
// Continuous startup can only materialize containers whose per-user data dir
// already exists (a brand-new continuous user still needs one API call to first
// create the dir — documented limitation R3).
func (m *Manager) Reconcile(ctx context.Context) error {
	summaries, err := m.docker.List(ctx, LabelManaged+"=true")
	if err != nil {
		return err
	}
	for _, s := range summaries {
		if s.State != "running" {
			continue
		}
		agentKey := s.Labels[LabelAgent]
		agent, ok := m.cfg.Agents[agentKey]
		if !ok || agent.Mode != config.ModeScaleToZero {
			continue // continuous running containers need no timer; unknown agents left alone
		}
		name := trimName(s.Names)
		if name == "" {
			continue
		}
		ks := m.keyState(name)
		ks.mu.Lock()
		m.armLocked(ks, name, agent.IdleTimeout.Std())
		ks.mu.Unlock()
		m.logf("reconcile: adopted running %s, re-armed idle timer", name)
	}

	// Ensure continuous containers are up for every existing per-user workspace
	// under tenants/*/subscriptions/*/agents/<role>/users/*.
	for _, agent := range m.cfg.Agents {
		if agent.Mode != config.ModeContinuous {
			continue
		}
		for _, key := range m.existingWorkspaces(agent.Key) {
			// Owner email is unknown here (no request); the marker was already
			// written on first provision, and the dir already exists so it isn't
			// re-seeded, so passing "" is fine.
			if _, err := m.EnsureRunning(ctx, agent, key, ""); err != nil {
				m.logf("reconcile: continuous ensure %s/%s/%s/%s failed: %v",
					key.TenantID, key.SubsAccID, key.Role, key.UserAccID, err)
			} else {
				m.logf("reconcile: continuous %s/%s/%s/%s ensured running",
					key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
			}
		}
	}
	return nil
}

// existingWorkspaces walks the nested layout and returns a WorkspaceKey for
// every already-materialized per-user workspace of the given role (each
// corresponds to a provisioned container).
func (m *Manager) existingWorkspaces(role string) []WorkspaceKey {
	pattern := filepath.Join(m.cfg.ContainerDataRoot, "tenants", "*",
		"subscriptions", "*", "agents", role, "users", "*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	var keys []WorkspaceKey
	for _, uw := range matches {
		if fi, err := os.Stat(uw); err != nil || !fi.IsDir() {
			continue
		}
		rel, err := filepath.Rel(m.cfg.ContainerDataRoot, uw)
		if err != nil {
			continue
		}
		// rel = tenants/<t>/subscriptions/<s>/agents/<role>/users/<u>
		parts := strings.Split(rel, string(os.PathSeparator))
		if len(parts) != 8 {
			continue
		}
		keys = append(keys, WorkspaceKey{
			TenantID: parts[1], SubsAccID: parts[3], Role: role, UserAccID: parts[7],
		})
	}
	return keys
}

func trimName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}
