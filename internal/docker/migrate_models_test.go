package docker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
