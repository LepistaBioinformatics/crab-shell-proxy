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

	// Ensure continuous containers are up for every existing per-user data dir.
	for _, agent := range m.cfg.Agents {
		if agent.Mode != config.ModeContinuous {
			continue
		}
		for _, userHash := range m.existingUserDirs(agent.Key) {
			if _, err := m.EnsureRunning(ctx, agent, userHash); err != nil {
				m.logf("reconcile: continuous ensure %s/%s failed: %v", agent.Key, userHash, err)
			} else {
				m.logf("reconcile: continuous %s/%s ensured running", agent.Key, userHash)
			}
		}
	}
	return nil
}

// existingUserDirs lists the userHash sub-directories already materialized for
// an agent (each corresponds to a provisioned per-user container).
func (m *Manager) existingUserDirs(agentKey string) []string {
	entries, err := os.ReadDir(filepath.Join(m.cfg.ContainerDataRoot, agentKey))
	if err != nil {
		return nil
	}
	var hashes []string
	for _, e := range entries {
		if e.IsDir() {
			hashes = append(hashes, e.Name())
		}
	}
	return hashes
}

func trimName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}
