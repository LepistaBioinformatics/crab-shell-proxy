package docker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

func TestReapplyModelForModelTouchesOnlyWorkspacesHoldingIt(t *testing.T) {
	m, reg, root := testManagerWithRegistry(t)
	// A no-op docker so the restart pass at the end of a re-apply is inert.
	m.docker = noopDocker{}

	for _, n := range []string{"fb", "other"} {
		if _, err := reg.CreateModel(registry.Model{
			ModelName: n, Provider: "openai", Model: n,
			APIBase: "https://api.openai.com/v1", APIKey: "sk-" + n, Status: registry.StatusActive,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reg.CreateModel(registry.Model{
		ModelName: "main", Provider: "openai", Model: "main",
		APIBase: "https://api.openai.com/v1", APIKey: "sk-main", Status: registry.StatusActive,
		Fallbacks: []string{"fb"},
	}); err != nil {
		t.Fatal(err)
	}

	holder := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "holder"}
	bystander := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "bystander"}
	holderDir := seedProvisionedWorkspace(t, root, holder)
	bystanderDir := seedProvisionedWorkspace(t, root, bystander)

	if err := reg.PutAssignment(m.workspaceRef(holder), registry.Assignment{
		ModelName: "main", Chain: []string{"fb"}, Source: registry.SourceExplicit,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.PutAssignment(m.workspaceRef(bystander), registry.Assignment{
		ModelName: "other", Source: registry.SourceExplicit,
	}); err != nil {
		t.Fatal(err)
	}
	beforeBystander, _ := os.ReadFile(filepath.Join(bystanderDir, "config.json"))

	// Editing fb's key must reach the holder even though fb is only its FALLBACK.
	if err := m.ReapplyModelForModel("fb", true); err != nil {
		t.Fatalf("ReapplyModelForModel: %v", err)
	}

	sec, err := readSecurityConfig(filepath.Join(holderDir, ".security.yml"))
	if err != nil {
		t.Fatal(err)
	}
	ml := sec["model_list"].(map[string]any)
	if _, ok := ml["fb"]; !ok {
		t.Errorf("holder did not get fb's key: %#v", ml)
	}

	afterBystander, _ := os.ReadFile(filepath.Join(bystanderDir, "config.json"))
	if string(beforeBystander) != string(afterBystander) {
		t.Error("a workspace that does not hold the model was re-materialized")
	}
}

func TestReapplyModelScopeLeavesAPinnedWorkspaceCompletelyAlone(t *testing.T) {
	m, reg, root := testManagerWithRegistry(t)
	var restarted []string
	m.docker = noopDocker{}
	m.logf = func(string, ...any) {}
	// recordingDocker reports every container as running, and health always
	// succeeds, so a spurious RestartWorkspace call on the pinned workspace is
	// actually observable via Stop — without this, Inspect would report
	// Exists=false and RestartWorkspace would no-op regardless of whether the
	// pinned workspace was (wrongly) asked to restart, making the assertion below
	// vacuous.
	m.health = func(context.Context, string, int) error { return nil }

	for _, n := range []string{"pinned", "scoped"} {
		if _, err := reg.CreateModel(registry.Model{
			ModelName: n, Provider: "openai", Model: n,
			APIBase: "https://api.openai.com/v1", APIKey: "sk-" + n, Status: registry.StatusActive,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.SetScopeDefault(registry.ScopeSel{
		Level: registry.LevelSubscription, TenantID: "t1", SubsAccID: "s1",
	}, "scoped"); err != nil {
		t.Fatal(err)
	}

	pinned := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "pinned"}
	drifter := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "drifter"}
	pinnedDir := seedProvisionedWorkspace(t, root, pinned)
	seedProvisionedWorkspace(t, root, drifter)
	if err := reg.PutAssignment(m.workspaceRef(pinned), registry.Assignment{
		ModelName: "pinned", Source: registry.SourceExplicit,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.PutAssignment(m.workspaceRef(drifter), registry.Assignment{
		ModelName: "scoped", Source: registry.SourceInherited,
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(pinnedDir, "config.json"))
	rec := &recordingDocker{}
	m.docker = rec

	if err := m.ReapplyModelScope(Scope{Kind: ScopeSubscription, TenantID: "t1", SubsAccID: "s1"}, true); err != nil {
		t.Fatalf("ReapplyModelScope: %v", err)
	}

	after, _ := os.ReadFile(filepath.Join(pinnedDir, "config.json"))
	if string(before) != string(after) {
		t.Error("a pinned workspace was re-materialized by a scope-default change")
	}
	// A no-op rewrite is invisible, but a restart is not: bouncing someone's agent
	// because a sibling's default changed is what "untouched" forbids.
	pinnedName := m.ContainerName(pinned)
	for _, n := range rec.stopped {
		if n == pinnedName {
			t.Errorf("pinned workspace %q was restarted; stopped = %v", pinnedName, rec.stopped)
		}
	}
	restarted = rec.stopped
	if len(restarted) == 0 {
		t.Error("expected the drifter workspace to be restarted (recordingDocker reports every container as running)")
	}
}

func TestReapplyModelUserRewritesFromTheRegistry(t *testing.T) {
	m, reg, root := testManagerWithRegistry(t)
	m.docker = noopDocker{}

	if _, err := reg.CreateModel(registry.Model{
		ModelName: "chosen", Provider: "anthropic", Model: "claude-sonnet-4-6",
		APIBase: "https://api.anthropic.com/v1", APIKey: "sk-chosen", Status: registry.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	userDir := seedProvisionedWorkspace(t, root, key)
	if err := reg.PutAssignment(m.workspaceRef(key), registry.Assignment{
		ModelName: "chosen", Source: registry.SourceExplicit,
	}); err != nil {
		t.Fatal(err)
	}

	if err := m.ReapplyModelUser(key, true); err != nil {
		t.Fatalf("ReapplyModelUser: %v", err)
	}

	raw, _ := os.ReadFile(filepath.Join(userDir, "config.json"))
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	defaults := cfg["agents"].(map[string]any)["defaults"].(map[string]any)
	if defaults["model_name"] != "chosen" {
		t.Errorf("model_name = %#v, want chosen", defaults["model_name"])
	}
}

func TestReapplySkipsAnUnprovisionedWorkspace(t *testing.T) {
	m, reg, _ := testManagerWithRegistry(t)
	m.docker = noopDocker{}

	if _, err := reg.CreateModel(registry.Model{
		ModelName: "m", Provider: "openai", Model: "m",
		APIBase: "https://api.openai.com/v1", APIKey: "sk-m", Status: registry.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "ghost"}
	if err := reg.PutAssignment(m.workspaceRef(key), registry.Assignment{
		ModelName: "m", Source: registry.SourceExplicit,
	}); err != nil {
		t.Fatal(err)
	}

	// No config.json on disk: the workspace was never provisioned, so there is
	// nothing to rewrite. Resolution already applies at its first provision.
	if err := m.reapplyWorkspace(key); err != nil {
		t.Fatalf("reapplyWorkspace on an unprovisioned workspace should be a no-op, got %v", err)
	}
}

// noopDocker satisfies the Docker interface so a re-apply's trailing restart pass
// is inert in a unit test. Only the methods the restart path touches need real
// behaviour; the rest return zero values.
type noopDocker struct{}

func (noopDocker) Inspect(ctx context.Context, name string) (ContainerState, error) {
	return ContainerState{}, nil
}
func (noopDocker) EnsureImage(ctx context.Context, image string) error { return nil }
func (noopDocker) Create(ctx context.Context, spec CreateSpec) (string, error) {
	return "", nil
}
func (noopDocker) Start(ctx context.Context, name string) error { return nil }
func (noopDocker) Stop(ctx context.Context, name string, grace time.Duration) error {
	return nil
}
func (noopDocker) Remove(ctx context.Context, name string) error { return nil }
func (noopDocker) List(ctx context.Context, label string) ([]ContainerSummary, error) {
	return nil, nil
}

// recordingDocker is noopDocker plus a record of which containers were stopped, so
// a test can assert that a workspace was NOT restarted. A silent no-op rewrite is
// invisible in the files; a spurious restart is only visible here. Inspect reports
// every container as already running — RestartWorkspace no-ops on a container it
// believes doesn't exist, so without this every Stop call (the one this double
// exists to catch) would be unreachable and the assertion would be vacuous.
type recordingDocker struct {
	noopDocker
	stopped []string
}

func (r *recordingDocker) Inspect(ctx context.Context, name string) (ContainerState, error) {
	return ContainerState{Exists: true, Running: true}, nil
}

func (r *recordingDocker) Stop(ctx context.Context, name string, grace time.Duration) error {
	r.stopped = append(r.stopped, name)
	return nil
}
