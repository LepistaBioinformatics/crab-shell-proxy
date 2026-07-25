package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// seedLegacyWorkspace writes a workspace as the OLD code left it: the model
// declared in its own config.json model_list and its key in .security.yml.
func seedLegacyWorkspace(t *testing.T, root string, key WorkspaceKey, modelName, provider, apiKey string, fallbacks []string) string {
	t.Helper()
	userDir := config.UserWorkspace(root, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	list := []any{map[string]any{
		"model_name": modelName, "provider": provider, "model": modelName,
		"api_base": "https://legacy.example/v1", "enabled": true,
	}}
	defaults := map[string]any{"provider": provider, "model_name": modelName}
	if len(fallbacks) > 0 {
		fb := make([]any, 0, len(fallbacks))
		for _, n := range fallbacks {
			fb = append(fb, n)
			list = append(list, map[string]any{
				"model_name": n, "provider": provider, "model": n,
				"api_base": "https://legacy.example/v1", "enabled": true,
			})
		}
		defaults["model_fallbacks"] = fb
	}
	cfg := map[string]any{
		"version":      3,
		"channel_list": map[string]any{"pico": map[string]any{"enabled": true}},
		"agents":       map[string]any{"defaults": defaults},
		"model_list":   list,
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(userDir, "config.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sec := "channel_list:\n  pico:\n    settings:\n      token: pico-legacy\nmodel_list:\n" +
		"  " + modelName + ":\n    api_keys:\n    - " + apiKey + "\n"
	for _, n := range fallbacks {
		sec += "  " + n + ":\n    api_keys:\n    - sk-" + n + "\n"
	}
	if err := os.WriteFile(filepath.Join(userDir, ".security.yml"), []byte(sec), 0o600); err != nil {
		t.Fatal(err)
	}
	return userDir
}

func TestMigrateImportsRegisteredModelsFile(t *testing.T) {
	m, reg, root := testManagerWithRegistry(t)

	dir := filepath.Join(root, "registered-models")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := `[{"provider":"zhipu","name":"glm-4.7","model":"glm-4.7",
	  "api_base":"https://open.bigmodel.cn/api/paas/v4","api_key":"sk-zhipu"}]`
	if err := os.WriteFile(filepath.Join(dir, "alpha.json"), []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := m.migrateModelRegistry(); err != nil {
		t.Fatalf("migrateModelRegistry: %v", err)
	}

	got, err := reg.GetModel("glm-4.7")
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	// The key must survive: a registered-models entry holds a credential an admin
	// actually typed, which no other source can reproduce.
	if got.APIKey != "sk-zhipu" || got.Provider != "zhipu" {
		t.Errorf("imported = %+v, want the zhipu definition with its key", got)
	}
}

// TestMigrateLaterSourceWinsOnModelNameCollision exercises the ordering
// contract: later sources win on model_name collision, because a
// registered-models entry (a real, admin-typed credential) is imported AFTER
// config.yaml (whose apiKeyEnv may no longer be set). The backfill-only-if-blank
// branch in importLegacyModel is what makes this hold, and until now nothing
// exercised it — every other test uses disjoint names.
func TestMigrateLaterSourceWinsOnModelNameCollision(t *testing.T) {
	m, reg, root := testManagerWithRegistry(t)
	// A config.yaml agent model whose apiKeyEnv is unset: APIKey resolves empty,
	// exactly as config.Load leaves it (internal/config/config.go).
	m.cfg.Agents = map[string]config.Agent{
		"alpha": {
			Key: "alpha",
			Model: &config.ModelConfig{
				Provider: "openai", Name: "shared-name", APIKey: "",
			},
		},
	}

	dir := filepath.Join(root, "registered-models")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := `[{"provider":"openai","name":"shared-name","model":"shared-name",
	  "api_base":"https://api.openai.com/v1","api_key":"sk-real"}]`
	if err := os.WriteFile(filepath.Join(dir, "alpha.json"), []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := m.migrateModelRegistry(); err != nil {
		t.Fatalf("migrateModelRegistry: %v", err)
	}

	got, err := reg.GetModel("shared-name")
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	// config.yaml's keyless entry is imported first (step 1); registered-models'
	// entry with a real key is imported second (step 2) and must backfill it,
	// not be skipped as a duplicate.
	if got.APIKey != "sk-real" {
		t.Errorf("APIKey = %q, want sk-real — the later source's real credential must win", got.APIKey)
	}
}

func TestMigrateImportsScopeAndUserOverrides(t *testing.T) {
	m, reg, root := testManagerWithRegistry(t)
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	seedLegacyWorkspace(t, root, key, "legacy", "openai", "sk-legacy", nil)

	// A tenant-scope override file as admin-model-override wrote it.
	tf := config.TenantModelOverrideFile(root, "t1")
	if err := os.MkdirAll(filepath.Dir(tf), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tf, []byte(`{"provider":"openai","name":"legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A per-user override dotfile.
	uf := config.UserModelOverrideFile(root, "t1", "s1", "alpha", "u1")
	if err := os.WriteFile(uf, []byte(`{"provider":"openai","name":"legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := m.migrateModelRegistry(); err != nil {
		t.Fatalf("migrateModelRegistry: %v", err)
	}

	d, err := reg.GetScopeDefault(registry.ScopeSel{Level: registry.LevelTenant, TenantID: "t1"})
	if err != nil || d.ModelName != "legacy" {
		t.Errorf("tenant default = %+v (err %v), want legacy", d, err)
	}
	a, err := reg.GetAssignment(m.workspaceRef(key))
	if err != nil {
		t.Fatalf("GetAssignment: %v", err)
	}
	// An override file was a deliberate pin, so it must import as EXPLICIT — as
	// inherited it would be silently overridden by the next scope change.
	if a.Source != registry.SourceExplicit || a.ModelName != "legacy" {
		t.Errorf("assignment = %+v, want legacy/explicit", a)
	}
}

func TestMigrateCapturesEveryWorkspacesCurrentModelAndChain(t *testing.T) {
	m, reg, root := testManagerWithRegistry(t)
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	seedLegacyWorkspace(t, root, key, "orphan-primary", "venice", "sk-venice", []string{"orphan-fb"})

	if err := m.migrateModelRegistry(); err != nil {
		t.Fatalf("migrateModelRegistry: %v", err)
	}

	// This is the anti-orphaning step: without it every existing user reads as
	// unassigned and the first scope-default change re-resolves them.
	a, err := reg.GetAssignment(m.workspaceRef(key))
	if err != nil {
		t.Fatalf("GetAssignment: %v", err)
	}
	if a.ModelName != "orphan-primary" || a.Source != registry.SourceInherited {
		t.Errorf("assignment = %+v, want orphan-primary/inherited", a)
	}
	if len(a.Chain) != 1 || a.Chain[0] != "orphan-fb" {
		t.Errorf("Chain = %v, want [orphan-fb]", a.Chain)
	}

	// A model no other source declared is recovered from the workspace itself,
	// key included, and flagged for review.
	prim, err := reg.GetModel("orphan-primary")
	if err != nil {
		t.Fatalf("GetModel primary: %v", err)
	}
	if prim.APIKey != "sk-venice" || !prim.ImportedOrphan {
		t.Errorf("recovered primary = %+v, want the key and ImportedOrphan", prim)
	}
	fb, err := reg.GetModel("orphan-fb")
	if err != nil {
		t.Fatalf("GetModel fallback: %v", err)
	}
	if fb.APIKey != "sk-orphan-fb" {
		t.Errorf("recovered fallback key = %q, want sk-orphan-fb", fb.APIKey)
	}
	// The primary's declared chain is reconstructed from model_fallbacks, so the
	// workspace keeps working after the next re-materialization.
	if len(prim.Fallbacks) != 1 || prim.Fallbacks[0] != "orphan-fb" {
		t.Errorf("recovered Fallbacks = %v, want [orphan-fb]", prim.Fallbacks)
	}
}

// TestMigrateCapturesAWorkspaceWhoseAgentIsNotInConfig guards a deviation from
// the brief's reference code: step 4 walks disk (allExistingWorkspaces), not
// m.cfg.Agents. config.Load deletes a hermes agent from cfg.Agents when its
// token or provider key env is unset (DisabledAgents), and an agent removed
// from config.yaml entirely is likewise absent — in both cases its existing
// workspaces must still be captured, or they are orphaned the instant a scope
// default changes. testManagerWithRegistry's cfg.Agents is empty, so this test
// would fail against a migration that looped m.cfg.Agents instead of disk.
func TestMigrateCapturesAWorkspaceWhoseAgentIsNotInConfig(t *testing.T) {
	m, reg, root := testManagerWithRegistry(t)
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "decommissioned", UserAccID: "u1"}
	seedLegacyWorkspace(t, root, key, "orphaned-agent-model", "openai", "sk-orphaned", nil)

	if err := m.migrateModelRegistry(); err != nil {
		t.Fatalf("migrateModelRegistry: %v", err)
	}

	a, err := reg.GetAssignment(m.workspaceRef(key))
	if err != nil {
		t.Fatalf("GetAssignment: %v", err)
	}
	if a.ModelName != "orphaned-agent-model" {
		t.Errorf("assignment = %+v, want orphaned-agent-model", a)
	}
}

func TestMigrateChangesNoWorkspacesActiveModel(t *testing.T) {
	m, reg, root := testManagerWithRegistry(t)
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	userDir := seedLegacyWorkspace(t, root, key, "keepme", "openai", "sk-keepme", nil)
	before, _ := os.ReadFile(filepath.Join(userDir, "config.json"))

	if err := m.migrateModelRegistry(); err != nil {
		t.Fatalf("migrateModelRegistry: %v", err)
	}

	after, _ := os.ReadFile(filepath.Join(userDir, "config.json"))
	if string(before) != string(after) {
		t.Error("migration rewrote a workspace's config.json; it must only READ workspaces")
	}
	// And the recorded assignment must agree with what is on disk, so the drift
	// check reports clean immediately after.
	a, _ := reg.GetAssignment(m.workspaceRef(key))
	if a.ModelName != "keepme" {
		t.Errorf("assignment = %q, want keepme", a.ModelName)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	m, reg, root := testManagerWithRegistry(t)
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	seedLegacyWorkspace(t, root, key, "once", "openai", "sk-once", nil)

	if err := m.migrateModelRegistry(); err != nil {
		t.Fatalf("first: %v", err)
	}
	v, err := reg.SchemaVersion()
	if err != nil || v != modelRegistrySchemaVersion {
		t.Fatalf("SchemaVersion = %d (err %v), want %d", v, err, modelRegistrySchemaVersion)
	}
	first, _ := reg.GetModel("once")

	// Tamper, then re-run: a second pass must not re-import over the admin's edit.
	if _, err := reg.UpdateModel("once", first.Version, func(mm *registry.Model) error {
		mm.APIBase = "https://edited.example/v1"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.migrateModelRegistry(); err != nil {
		t.Fatalf("second: %v", err)
	}
	after, _ := reg.GetModel("once")
	if after.APIBase != "https://edited.example/v1" {
		t.Errorf("second migration clobbered an admin edit: %+v", after)
	}
}

func TestMigrateSkipsAWorkspaceWithNoActiveModelNamed(t *testing.T) {
	m, reg, root := testManagerWithRegistry(t)
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	userDir := config.UserWorkspace(root, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"version":3,"agents":{"defaults":{"provider":"","model_name":""}},"model_list":[]}`
	if err := os.WriteFile(filepath.Join(userDir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := m.migrateModelRegistry(); err != nil {
		t.Fatalf("migrateModelRegistry: %v", err)
	}
	// Nothing to capture and nothing to invent: a workspace that never had a model
	// gets no assignment, and will resolve normally on its next start.
	if _, err := reg.GetAssignment(m.workspaceRef(key)); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("want ErrNotFound for a model-less workspace, got %v", err)
	}
}

// TestMigrateLogsAnUnreadableWorkspaceConfigInsteadOfSkippingSilently proves the
// review-mandated fix in captureWorkspaceModel is reachable: a genuine read
// failure (permission denied) on a workspace's config.json must be logged, not
// folded into the "never provisioned" no-op. A workspace this pass could not
// read may well have a live model, so treating the failure as "nothing to
// capture" would silently leave it unassigned — the exact failure this task
// exists to prevent, just triggered by an I/O error instead of a stale scope.
func TestMigrateLogsAnUnreadableWorkspaceConfigInsteadOfSkippingSilently(t *testing.T) {
	m, reg, root := testManagerWithRegistry(t)
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	userDir := seedLegacyWorkspace(t, root, key, "unreadable", "openai", "sk-unreadable", nil)
	configPath := filepath.Join(userDir, "config.json")

	if err := os.Chmod(configPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(configPath, 0o600) }) // let TempDir clean up
	if _, err := os.ReadFile(configPath); err == nil || os.IsNotExist(err) {
		t.Skipf("chmod 000 did not produce a non-ENOENT read error in this environment (err=%v); "+
			"likely running as root, where file mode does not gate the owner's own read", err)
	}

	var logs []string
	m.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	if err := m.migrateModelRegistry(); err != nil {
		t.Fatalf("migrateModelRegistry: %v", err)
	}

	found := false
	for _, l := range logs {
		if strings.Contains(l, "read config.json") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no log mentioned the unreadable config.json; logs = %v", logs)
	}
	// The old code returned nil for ANY read error, which reads identically to a
	// never-provisioned workspace: no assignment either way. The fix does not
	// change that outcome (a failed capture still yields no assignment this
	// pass) — it changes whether the failure is visible for a retry instead of
	// being silently indistinguishable from "nothing to do here".
	if _, err := reg.GetAssignment(m.workspaceRef(key)); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("want ErrNotFound (capture failed this pass), got %v", err)
	}
}
