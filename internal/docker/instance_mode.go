package docker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// Per-instance lifecycle mode.
//
// `agents.<key>.mode` in config.yaml is the DEFAULT for every instance of that
// agent. This file is the per-instance override, and the two layers are the
// whole model -- there is deliberately no third, global one, because a global
// default would have to reconcile with the per-agent validation that
// scale-to-zero requires a positive idleTimeout.
//
// WHY IT IS WORTH HAVING AT ALL
//
// Scheduled tasks run from timers inside the container, so a scale-to-zero
// instance fires none. Today that is decided for a whole agent: either every
// member pays for a container that never stops, or nobody gets a working
// schedule. One member who needs a daily report is a reason to keep ONE
// container up, not forty.
//
// EVERY READER GOES THROUGH ModeFor
//
// There were five places branching on agent.Mode, and an override that reached
// four of them would produce an instance that is scale-to-zero for the idle
// timer and continuous for the reconciler -- a container stopped and restarted
// in a loop, with nothing naming the cause. The chokepoint is the design.

// instanceMode is the on-disk shape. One field, because anything else about an
// instance belongs in its own file rather than accreting here.
type instanceMode struct {
	Mode config.Mode `json:"mode"`
}

// ModeFor resolves the lifecycle mode of ONE instance: its override if it has a
// valid one, otherwise its agent's default.
//
// An unreadable or malformed file falls back to the agent default rather than
// failing. The alternative is refusing to run a container because a dotfile got
// truncated, and the agent default is always a safe answer -- it is what every
// instance used before this file existed.
func (m *Manager) ModeFor(agent config.Agent, key WorkspaceKey) config.Mode {
	path := config.UserModeOverrideFile(m.cfg.ContainerDataRoot,
		key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	b, err := os.ReadFile(path)
	if err != nil {
		return agent.Mode
	}
	var im instanceMode
	if json.Unmarshal(b, &im) != nil {
		m.logf("instance mode %s is malformed, using the %q default", path, agent.Mode)
		return agent.Mode
	}
	switch im.Mode {
	case config.ModeScaleToZero:
		// The same rule config.Load enforces on an agent default, applied here
		// because this file can also arrive by hand. An idle timer armed with a
		// non-positive duration fires immediately, so the container would be
		// stopped the instant it came up, forever, and the logs would show a
		// restart loop with no cause.
		if agent.IdleTimeout.Std() <= 0 {
			m.logf("instance mode %s asks for %s but agent %q has no idleTimeout; using %q",
				path, config.ModeScaleToZero, agent.Key, agent.Mode)
			return agent.Mode
		}
		return im.Mode
	case config.ModeContinuous:
		return im.Mode
	default:
		// Includes the empty string, which is how an override is CLEARED: the
		// file may exist and name nothing, and that means "follow the agent".
		return agent.Mode
	}
}

// ModeOverride reports the override an instance carries, or "" when it follows
// its agent. Distinct from ModeFor because the admin screen has to show which
// of the two the current value is -- "continuous (inherited)" and "continuous
// (set here)" differ in what happens when the agent default changes.
func (m *Manager) ModeOverride(key WorkspaceKey) config.Mode {
	b, err := os.ReadFile(config.UserModeOverrideFile(m.cfg.ContainerDataRoot,
		key.TenantID, key.SubsAccID, key.Role, key.UserAccID))
	if err != nil {
		return ""
	}
	var im instanceMode
	if json.Unmarshal(b, &im) != nil {
		return ""
	}
	switch im.Mode {
	case config.ModeScaleToZero, config.ModeContinuous:
		return im.Mode
	}
	return ""
}

// SetMode writes an instance's override and moves its idle timer to match.
//
// The write and the timer transition are ONE operation on purpose. Three of the
// four transitions fail silently if they come apart:
//
//   - continuous -> scale-to-zero with no timer armed: the container runs
//     forever, and nothing ever says the setting did not take.
//   - scale-to-zero -> continuous with a timer still armed: the container is
//     stopped once more after the change. Reconcile brings it back, but on the
//     reconcile interval, not now.
//   - either direction with no container: harmless, and the only one that is.
//
// Passing an empty mode clears the override, so the instance follows its agent
// again.
func (m *Manager) SetMode(agent config.Agent, key WorkspaceKey, mode config.Mode) error {
	switch mode {
	case "", config.ModeScaleToZero, config.ModeContinuous:
	default:
		return fmt.Errorf("mode %q: want %q, %q, or empty to inherit",
			mode, config.ModeScaleToZero, config.ModeContinuous)
	}
	if mode == config.ModeScaleToZero && agent.IdleTimeout.Std() <= 0 {
		// The same rule config.Load enforces for an agent default. Without it,
		// an override could arm a zero-duration timer.
		return fmt.Errorf("agent %q has no idleTimeout, so its instances cannot be set to %s",
			agent.Key, config.ModeScaleToZero)
	}

	path := config.UserModeOverrideFile(m.cfg.ContainerDataRoot,
		key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("instance mode dir: %w", err)
	}
	b, err := json.Marshal(instanceMode{Mode: mode})
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write instance mode: %w", err)
	}
	if err := chownTree(path, m.cfg.PicoclawUser); err != nil {
		// Not fatal: the file is read by THIS process, not by the container.
		// Chowned only to match its siblings in a directory the agent's uid
		// owns.
		m.logf("chown %s: %v", path, err)
	}

	m.applyModeTimer(agent, key, m.ModeFor(agent, key))
	return nil
}

// applyModeTimer arms or cancels the idle timer to match a just-changed mode.
func (m *Manager) applyModeTimer(agent config.Agent, key WorkspaceKey, mode config.Mode) {
	name := m.ContainerName(key)
	ks := m.keyState(name)
	ks.mu.Lock()
	defer ks.mu.Unlock()

	if mode == config.ModeScaleToZero {
		m.armLocked(ks, name, agent.IdleTimeout.Std())
		m.logf("instance %s set to %s, idle timer armed", name, mode)
		return
	}
	m.disarmLocked(ks)
	m.logf("instance %s set to %s, idle timer cancelled", name, mode)
}
