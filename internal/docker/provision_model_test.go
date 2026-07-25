package docker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
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
		cfg:  &config.Config{ContainerDataRoot: root, PicoclawUser: ""},
		reg:  reg,
		logf: func(string, ...any) {},
		keys: map[string]*keyState{},
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
