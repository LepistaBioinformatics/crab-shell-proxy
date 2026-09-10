package docker

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

func modeManager(t *testing.T, idle time.Duration) (*Manager, config.Agent, WorkspaceKey) {
	t.Helper()
	root := t.TempDir()
	agent := config.Agent{Key: "gamma", Mode: config.ModeScaleToZero, IdleTimeout: config.Duration(idle)}
	m := &Manager{
		cfg: &config.Config{
			ContainerDataRoot: root,
			HostDataRoot:      root,
			Agents:            map[string]config.Agent{"gamma": agent},
		},
		keys: map[string]*keyState{},
		logf: func(string, ...any) {},
	}
	key := WorkspaceKey{TenantID: "t", SubsAccID: "s", Role: "gamma", UserAccID: "u"}
	if err := os.MkdirAll(config.UserWorkspace(root, key.TenantID, key.SubsAccID, key.Role, key.UserAccID), 0o755); err != nil {
		t.Fatal(err)
	}
	return m, agent, key
}

// No file means no override: every instance behaved this way before the feature
// existed and must keep doing so.
func TestModeForFallsBackToTheAgentDefault(t *testing.T) {
	m, agent, key := modeManager(t, time.Minute)
	if got := m.ModeFor(agent, key); got != config.ModeScaleToZero {
		t.Errorf("ModeFor = %q, want the agent default %q", got, config.ModeScaleToZero)
	}
	if got := m.ModeOverride(key); got != "" {
		t.Errorf("ModeOverride = %q, want empty", got)
	}
}

func TestSetModeOverridesTheAgentDefault(t *testing.T) {
	m, agent, key := modeManager(t, time.Minute)
	if err := m.SetMode(agent, key, config.ModeContinuous); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	if got := m.ModeFor(agent, key); got != config.ModeContinuous {
		t.Errorf("ModeFor = %q, want the override %q", got, config.ModeContinuous)
	}
	if got := m.ModeOverride(key); got != config.ModeContinuous {
		t.Errorf("ModeOverride = %q, want %q", got, config.ModeContinuous)
	}
}

// An empty mode CLEARS. Without this an admin who set an override could never
// hand the instance back to the agent default, only pin the other value.
func TestSetModeWithAnEmptyValueClearsTheOverride(t *testing.T) {
	m, agent, key := modeManager(t, time.Minute)
	if err := m.SetMode(agent, key, config.ModeContinuous); err != nil {
		t.Fatal(err)
	}
	if err := m.SetMode(agent, key, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := m.ModeOverride(key); got != "" {
		t.Errorf("ModeOverride = %q after a clear, want empty", got)
	}
	if got := m.ModeFor(agent, key); got != agent.Mode {
		t.Errorf("ModeFor = %q after a clear, want the agent default %q", got, agent.Mode)
	}
}

func TestSetModeRejectsAnUnknownValue(t *testing.T) {
	m, agent, key := modeManager(t, time.Minute)
	if err := m.SetMode(agent, key, config.Mode("paused")); err == nil {
		t.Error("an unknown mode was accepted")
	}
	if got := m.ModeOverride(key); got != "" {
		t.Errorf("a rejected write still left %q on disk", got)
	}
}

// config.Load refuses a scale-to-zero AGENT with no idleTimeout. The same rule
// has to hold for an override, or the timer is armed with a non-positive
// duration and fires at once -- the container stopped the instant it comes up,
// forever, with the logs showing a restart loop and no cause.
func TestScaleToZeroIsRefusedWithoutAnIdleTimeout(t *testing.T) {
	m, _, key := modeManager(t, 0)
	agent := config.Agent{Key: "gamma", Mode: config.ModeContinuous}
	if err := m.SetMode(agent, key, config.ModeScaleToZero); err == nil {
		t.Fatal("scale-to-zero was accepted for an agent with no idleTimeout")
	}
}

// The same guard on the READ path, because this file can also arrive by hand.
func TestModeForIgnoresScaleToZeroWithoutAnIdleTimeout(t *testing.T) {
	m, _, key := modeManager(t, 0)
	agent := config.Agent{Key: "gamma", Mode: config.ModeContinuous}
	path := config.UserModeOverrideFile(m.cfg.ContainerDataRoot,
		key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if err := os.WriteFile(path, []byte(`{"mode":"scale-to-zero"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := m.ModeFor(agent, key); got != config.ModeContinuous {
		t.Errorf("ModeFor = %q, want the agent default: the override is unrepresentable", got)
	}
}

// A truncated dotfile must not stop a container from running. The agent default
// is always a safe answer -- it is what the instance used before the file existed.
func TestAMalformedOverrideFallsBackInsteadOfFailing(t *testing.T) {
	m, agent, key := modeManager(t, time.Minute)
	path := config.UserModeOverrideFile(m.cfg.ContainerDataRoot,
		key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := m.ModeFor(agent, key); got != agent.Mode {
		t.Errorf("ModeFor = %q on a malformed file, want the agent default %q", got, agent.Mode)
	}
}

// THE TRANSITION, which is the part that fails silently if the store write and
// the timer move come apart.
func TestSetModeMovesTheIdleTimer(t *testing.T) {
	m, agent, key := modeManager(t, time.Hour)
	name := m.ContainerName(key)

	t.Run("to scale-to-zero arms it", func(t *testing.T) {
		if err := m.SetMode(agent, key, config.ModeScaleToZero); err != nil {
			t.Fatal(err)
		}
		ks := m.keyState(name)
		ks.mu.Lock()
		defer ks.mu.Unlock()
		if ks.timer == nil {
			t.Error("no idle timer armed: the container would run forever despite the setting")
		}
	})

	t.Run("to continuous cancels it", func(t *testing.T) {
		if err := m.SetMode(agent, key, config.ModeContinuous); err != nil {
			t.Fatal(err)
		}
		ks := m.keyState(name)
		ks.mu.Lock()
		defer ks.mu.Unlock()
		if ks.timer != nil {
			t.Error("the idle timer survived: the container would be stopped once more after the change")
		}
	})
}

// The override lives ABOVE workspace/, where neither picoclaw's
// restrict_to_workspace nor the ganglion's Landlock domain lets the agent
// reach it. An agent able to write this file could keep its own container
// alive indefinitely.
func TestTheOverrideIsOutOfTheAgentsReach(t *testing.T) {
	root := "/srv/data"
	key := WorkspaceKey{TenantID: "t", SubsAccID: "s", Role: "gamma", UserAccID: "u"}
	path := config.UserModeOverrideFile(root, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	workspace := filepath.Join(config.UserWorkspace(root, key.TenantID, key.SubsAccID, key.Role, key.UserAccID), config.MainWorkspace)
	if filepath.Dir(path) == workspace || len(path) > len(workspace) && path[:len(workspace)+1] == workspace+"/" {
		t.Errorf("%s is inside the agent's workspace %s", path, workspace)
	}
}

// LabelMode is stamped at CREATE time and a mode change never touches an
// existing container, so the label goes stale the moment an admin sets an
// override. It is still written -- it costs nothing and reads well in
// `docker inspect` -- but nothing may branch on it again.
//
// A source scan, because the failure is a NEW reader appearing, which no
// behavioural test would notice.
func TestNothingBranchesOnTheModeLabel(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	// A glob that matches nothing passes silently, which is the same shape as
	// the bug this test is guarding against.
	if len(files) == 0 {
		t.Fatal("the source scan matched no files, so it proves nothing")
	}
	scanned := 0
	reader := regexp.MustCompile(`Labels\[LabelMode\]`)
	for _, f := range files {
		if len(f) > 8 && f[len(f)-8:] == "_test.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		if reader.Match(b) {
			t.Errorf("%s reads Labels[LabelMode]; it is stale after a per-instance override. Use Manager.ModeFor.", f)
		}
	}
	if scanned == 0 {
		t.Fatal("every candidate was skipped as a test file; the scan proves nothing")
	}
}
