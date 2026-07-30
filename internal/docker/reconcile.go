package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// Reconcile brings the manager's in-memory state in line with reality at boot:
//   - logs any mismatch between a workspace's recorded model assignment and
//     what it is actually running (checkModelDrift),
//   - adopts already-running managed containers (re-arming scale-to-zero timers),
//   - ensures continuous-mode containers are started (CSP-08/09/10).
//
// It does NOT run the model-inventory migration. That has to complete before the
// HTTP server accepts a single request — a chat arriving against an empty
// inventory resolves nothing and fails — whereas everything here is container
// work that may take minutes and must not hold /healthz. So main calls
// MigrateModels synchronously first and Reconcile in the background after.
//
// Continuous startup can only materialize containers whose per-user data dir
// already exists (a brand-new continuous user still needs one API call to first
// create the dir — documented limitation R3).
func (m *Manager) Reconcile(ctx context.Context) error {
	m.checkModelDrift()

	// Refresh the operator-managed content at STARTUP, not only when a container
	// happens to be created. The managed skill dir is a directory bind, so every
	// already-running container reads whatever is on the host right now — which
	// means a deploy that updates that guidance reaches existing workspaces the
	// moment it is written, with no recreate. Left only in `create` (behind a
	// per-process sync.Once), a fully warm deployment would never write it and the
	// new text would reach nobody.
	if err := m.ensureManagedContent(); err != nil {
		m.logf("reconcile: %v", err)
	}

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

	// Re-arm admin-scheduled bounces last. Without this a proxy restart silently
	// drops every scheduled window an admin set (restart-control FR-6.2); one
	// that came due while the proxy was down fires immediately.
	m.RearmSchedules()
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
