package docker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/restart"
)

func testManagerWithRegistry(t *testing.T) (*Manager, *registry.Registry, string) {
	t.Helper()
	root := t.TempDir()
	at := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	reg, err := registry.Open(filepath.Join(root, "model-registry.db"), func() time.Time { return at })
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	m := &Manager{
		cfg:      &config.Config{ContainerDataRoot: root, PicoclawUser: ""},
		reg:      reg,
		logf:     func(string, ...any) {},
		restarts: restart.NewStore(root),
		keys:     map[string]*keyState{},
		sched:    map[string]*time.Timer{},
	}
	return m, reg, root
}

func seedProvisionedWorkspace(t *testing.T, root string, key WorkspaceKey) string {
	t.Helper()
	userDir := config.UserWorkspace(root, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"version":3,"channel_list":{"pico":{"enabled":false}},` +
		`"agents":{"defaults":{"provider":"","model_name":""}},"model_list":[]}`
	if err := os.WriteFile(filepath.Join(userDir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	sec := "channel_list:\n  pico:\n    settings:\n      token: pico-seed\n"
	if err := os.WriteFile(filepath.Join(userDir, ".security.yml"), []byte(sec), 0o600); err != nil {
		t.Fatal(err)
	}
	return userDir
}

func TestResolveAndMaterializeRefusesWhenNoModelResolves(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	userDir := seedProvisionedWorkspace(t, root, key)

	err := m.resolveAndMaterialize(key, userDir)
	// picoclaw fails at startup when agents.defaults.model_name names a model
	// absent from model_list, so provisioning without one would produce a
	// permanently unbootable workspace. Refusing loudly is the only safe answer.
	if !errors.Is(err, registry.ErrNoModelResolvable) {
		t.Fatalf("want ErrNoModelResolvable, got %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(userDir, "config.json"))
	if string(raw) == "" {
		t.Fatal("config.json was emptied by a refused materialization")
	}
	if got := string(raw); got != `{"version":3,"channel_list":{"pico":{"enabled":false}},`+
		`"agents":{"defaults":{"provider":"","model_name":""}},"model_list":[]}` {
		t.Errorf("refused materialization still wrote config.json:\n%s", got)
	}
}

func TestResolveAndMaterializeRecordsAnInheritedAssignment(t *testing.T) {
	m, reg, root := testManagerWithRegistry(t)
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	userDir := seedProvisionedWorkspace(t, root, key)

	if _, err := reg.CreateModel(registry.Model{
		ModelName: "m", Provider: "openai", Model: "gpt-5.4",
		APIBase: "https://api.openai.com/v1", APIKey: "sk-m", Status: registry.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetScopeDefault(registry.ScopeSel{Level: registry.LevelTenant, TenantID: "t1"}, "m"); err != nil {
		t.Fatal(err)
	}

	if err := m.resolveAndMaterialize(key, userDir); err != nil {
		t.Fatalf("resolveAndMaterialize: %v", err)
	}

	a, err := reg.GetAssignment(m.workspaceRef(key))
	if err != nil {
		t.Fatalf("GetAssignment: %v", err)
	}
	if a.ModelName != "m" {
		t.Errorf("assignment model = %q, want m", a.ModelName)
	}
	// Source must be inherited: nothing pinned this user, the tenant default did.
	// Recording it as explicit would freeze the workspace against future scope
	// changes.
	if a.Source != registry.SourceInherited {
		t.Errorf("Source = %q, want inherited", a.Source)
	}
}

func TestResolveAndMaterializePreservesAnExplicitAssignmentSource(t *testing.T) {
	m, reg, root := testManagerWithRegistry(t)
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	userDir := seedProvisionedWorkspace(t, root, key)

	for _, n := range []string{"pinned", "scoped"} {
		if _, err := reg.CreateModel(registry.Model{
			ModelName: n, Provider: "openai", Model: n,
			APIBase: "https://api.openai.com/v1", APIKey: "sk-" + n, Status: registry.StatusActive,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.SetScopeDefault(registry.ScopeSel{Level: registry.LevelTenant, TenantID: "t1"}, "scoped"); err != nil {
		t.Fatal(err)
	}
	ref := m.workspaceRef(key)
	if err := reg.PutAssignment(ref, registry.Assignment{ModelName: "pinned", Source: registry.SourceExplicit}); err != nil {
		t.Fatal(err)
	}

	if err := m.resolveAndMaterialize(key, userDir); err != nil {
		t.Fatalf("resolveAndMaterialize: %v", err)
	}

	a, _ := reg.GetAssignment(ref)
	if a.ModelName != "pinned" || a.Source != registry.SourceExplicit {
		t.Errorf("assignment = %+v, want pinned/explicit — re-materializing must not demote a pin", a)
	}
}

func TestResolveAndMaterializeRecordsTheChain(t *testing.T) {
	m, reg, root := testManagerWithRegistry(t)
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	userDir := seedProvisionedWorkspace(t, root, key)

	if _, err := reg.CreateModel(registry.Model{
		ModelName: "fb", Provider: "anthropic", Model: "claude-sonnet-4-6",
		APIBase: "https://api.anthropic.com/v1", APIKey: "sk-fb", Status: registry.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CreateModel(registry.Model{
		ModelName: "main", Provider: "openai", Model: "gpt-5.4",
		APIBase: "https://api.openai.com/v1", APIKey: "sk-main", Status: registry.StatusActive,
		Fallbacks: []string{"fb"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetScopeDefault(registry.ScopeSel{Level: registry.LevelGlobal}, "main"); err != nil {
		t.Fatal(err)
	}

	if err := m.resolveAndMaterialize(key, userDir); err != nil {
		t.Fatalf("resolveAndMaterialize: %v", err)
	}

	a, _ := reg.GetAssignment(m.workspaceRef(key))
	if len(a.Chain) != 1 || a.Chain[0] != "fb" {
		t.Errorf("Chain = %v, want [fb] so a key edit reaches this workspace", a.Chain)
	}
}

// writeEffectiveOverlay drops a native.yml into the EFFECTIVE secret dir
// resolveAndMaterialize reads — the same path syncEffectiveSecrets writes, which
// is what the admin cascade materializes a scope-level slot into.
func writeEffectiveOverlay(t *testing.T, root string, key WorkspaceKey, slots map[string]string) {
	t.Helper()
	writeNativeOverlay(t, config.EffectiveSecretsDir(root, key.UserAccID, key.Role), slots)
}

func workspaceModelKey(t *testing.T, secPath, name string) string {
	t.Helper()
	sec, err := readSecurityConfig(secPath)
	if err != nil {
		t.Fatalf("readSecurityConfig: %v", err)
	}
	ml, ok := sec["model_list"].(map[string]any)
	if !ok {
		t.Fatalf("no model_list in %s: %#v", secPath, sec)
	}
	entry, ok := ml[name].(map[string]any)
	if !ok {
		t.Fatalf("no model_list.%s in %s: %#v", name, secPath, ml)
	}
	keys, ok := entry["api_keys"].([]any)
	if !ok || len(keys) == 0 {
		t.Fatalf("model_list.%s.api_keys = %#v", name, entry["api_keys"])
	}
	s, _ := keys[0].(string)
	return s
}

// TestNativeKeyOverlayWinsOverTheInventoryKeyOnEveryMaterialization is the FR-32 /
// CTX-MR-12 contract: a scope admin's own credential for a model this workspace
// DOES resolve to overrides the inventory's key, and keeps overriding it.
//
// The second resolveAndMaterialize is the load-bearing assertion. Applying the
// overlay before materialization (the old order) passes the first check and fails
// this one: materializeModels rewrites every model_list entry and prunes the rest,
// so from the next ensure onwards the inventory key silently wins — a credential
// substitution with billing and isolation consequences.
func TestNativeKeyOverlayWinsOverTheInventoryKeyOnEveryMaterialization(t *testing.T) {
	m, reg, root := testManagerWithRegistry(t)
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	userDir := seedProvisionedWorkspace(t, root, key)
	secPath := filepath.Join(userDir, ".security.yml")

	if _, err := reg.CreateModel(registry.Model{
		ModelName: "m", Provider: "openai", Model: "gpt-5.4",
		APIBase: "https://api.openai.com/v1", APIKey: "sk-inventory", Status: registry.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetScopeDefault(registry.ScopeSel{Level: registry.LevelTenant, TenantID: "t1"}, "m"); err != nil {
		t.Fatal(err)
	}
	writeEffectiveOverlay(t, root, key, map[string]string{"model_list.m.api_keys": "sk-scope-admin"})

	if err := m.resolveAndMaterialize(key, userDir); err != nil {
		t.Fatalf("first resolveAndMaterialize: %v", err)
	}
	if got := workspaceModelKey(t, secPath, "m"); got != "sk-scope-admin" {
		t.Errorf("after first materialize, key = %q, want the scope admin's sk-scope-admin", got)
	}

	if err := m.resolveAndMaterialize(key, userDir); err != nil {
		t.Fatalf("second resolveAndMaterialize: %v", err)
	}
	if got := workspaceModelKey(t, secPath, "m"); got != "sk-scope-admin" {
		t.Errorf("after re-materialization, key = %q — the inventory key reclaimed the slot", got)
	}
	// The overlay must not cost the workspace its channel token.
	sec, _ := readSecurityConfig(secPath)
	tok := sec["channel_list"].(map[string]any)["pico"].(map[string]any)["settings"].(map[string]any)["token"]
	if tok != "pico-seed" {
		t.Errorf("pico token = %#v, want pico-seed preserved", tok)
	}
}

// TestMaterializeAppliesAnOverlayForAModelThisWorkspaceDoesNotHave guards the
// FR-32b skip on the new ordering: an inapplicable model slot must not abort the
// merge and take a working web.* slot down with it.
func TestMaterializeAppliesAnOverlayForAModelThisWorkspaceDoesNotHave(t *testing.T) {
	m, reg, root := testManagerWithRegistry(t)
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	userDir := seedProvisionedWorkspace(t, root, key)
	secPath := filepath.Join(userDir, ".security.yml")

	if _, err := reg.CreateModel(registry.Model{
		ModelName: "m", Provider: "openai", Model: "gpt-5.4",
		APIBase: "https://api.openai.com/v1", APIKey: "sk-inventory", Status: registry.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetScopeDefault(registry.ScopeSel{Level: registry.LevelTenant, TenantID: "t1"}, "m"); err != nil {
		t.Fatal(err)
	}
	writeEffectiveOverlay(t, root, key, map[string]string{
		"model_list.elsewhere.api_keys": "sk-other",
		"web.brave":                     "brave-key",
	})

	if err := m.resolveAndMaterialize(key, userDir); err != nil {
		t.Fatalf("resolveAndMaterialize: %v", err)
	}

	sec, _ := readSecurityConfig(secPath)
	web, ok := sec["web"].(map[string]any)
	if !ok || web["brave"] != "brave-key" {
		t.Errorf("web = %#v, want brave applied despite the inapplicable model slot", sec["web"])
	}
	if got := workspaceModelKey(t, secPath, "m"); got != "sk-inventory" {
		t.Errorf("resolved model key = %q, want the inventory's sk-inventory", got)
	}
}
