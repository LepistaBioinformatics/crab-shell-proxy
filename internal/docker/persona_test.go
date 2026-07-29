package docker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

func personaFixture(t *testing.T) (*config.Config, WorkspaceKey, string) {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{ContainerDataRoot: root, HostDataRoot: "/host/data", PicoclawUser: ""}
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	return cfg, key, filepath.Join(root, "templates", "alpha")
}

func TestResolvePersonaSourcesPrecedence(t *testing.T) {
	cfg, key, tmpl := personaFixture(t)
	// The template is the LAST layer.
	mustWrite(t, filepath.Join(tmpl, "workspace", "AGENT.md"), "template")
	mustWrite(t, filepath.Join(tmpl, "workspace", "SOUL.md"), "template")
	// Tenant+agent beats the template.
	mustWrite(t, filepath.Join(
		config.TenantAgentPersonaDir(cfg.ContainerDataRoot, key.TenantID, key.Role), "AGENT.md"), "tenant")
	mustWrite(t, filepath.Join(
		config.TenantAgentPersonaDir(cfg.ContainerDataRoot, key.TenantID, key.Role), "SOUL.md"), "tenant")
	// Subscription+agent beats both.
	mustWrite(t, filepath.Join(
		config.SubscriptionAgentPersonaDir(
			cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role), "AGENT.md"), "subscription")

	got := resolvePersonaSources(cfg, key, tmpl)

	if raw, _ := os.ReadFile(got["AGENT.md"]); string(raw) != "subscription" {
		t.Errorf("AGENT.md resolved to %q, want the subscription layer", raw)
	}
	if raw, _ := os.ReadFile(got["SOUL.md"]); string(raw) != "tenant" {
		t.Errorf("SOUL.md resolved to %q, want the tenant layer", raw)
	}
	// Nothing provides HEARTBEAT.md at any layer: it must be ABSENT, not empty.
	if src, ok := got["HEARTBEAT.md"]; ok {
		t.Errorf("HEARTBEAT.md should resolve to nothing, got %q", src)
	}
}

func TestPersonaBindsOnlyForFilesThatExist(t *testing.T) {
	cfg, key, tmpl := personaFixture(t)
	mustWrite(t, filepath.Join(tmpl, "workspace", "AGENT.md"), "a")
	mustWrite(t, filepath.Join(tmpl, "workspace", "USER.md"), "u")

	m := &Manager{cfg: cfg}
	if err := m.syncEffectivePersona(key, tmpl); err != nil {
		t.Fatalf("sync: %v", err)
	}
	binds := personaBinds(cfg, key, "/ws")

	// AGENT.md exists, so it is mounted. SOUL.md and HEARTBEAT.md do not exist
	// anywhere — and a bind with a missing source makes Docker invent an empty
	// DIRECTORY at the destination, which is worse than the file's absence.
	if len(binds) != 1 {
		t.Fatalf("binds = %+v, want exactly AGENT.md", binds)
	}
	want := "/host/data/effective-persona/t1/s1/alpha/AGENT.md:/ws/workspace/AGENT.md:ro"
	if binds[0].bind != want {
		t.Errorf("bind = %q, want %q", binds[0].bind, want)
	}
}

// USER.md is materialized (it is the seed source) but NEVER mounted: the agent
// accumulates what it learns about the user there, and a read-only mount would
// silently disable that write.
func TestPersonaBindsNeverIncludeUserMD(t *testing.T) {
	cfg, key, tmpl := personaFixture(t)
	for _, name := range PersonaFiles {
		mustWrite(t, filepath.Join(tmpl, "workspace", name), name)
	}
	m := &Manager{cfg: cfg}
	if err := m.syncEffectivePersona(key, tmpl); err != nil {
		t.Fatalf("sync: %v", err)
	}
	eff := config.EffectivePersonaDir(cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role)
	if _, err := os.Stat(filepath.Join(eff, "USER.md")); err != nil {
		t.Errorf("USER.md must be materialized as the seed source: %v", err)
	}
	for _, b := range personaBinds(cfg, key, "/ws") {
		if b.name == "USER.md" {
			t.Error("USER.md must never be bind-mounted")
		}
	}
	if len(personaBinds(cfg, key, "/ws")) != len(PersonaMounted) {
		t.Errorf("every mounted file should be bound, got %d", len(personaBinds(cfg, key, "/ws")))
	}
}

// The whole reason writeInPlace exists: a file bind mount pins the inode it was
// created against, so a rename-based write would leave the container reading the
// old content for the life of the container.
func TestSyncEffectivePersonaKeepsTheInode(t *testing.T) {
	cfg, key, tmpl := personaFixture(t)
	agentMD := filepath.Join(tmpl, "workspace", "AGENT.md")
	mustWrite(t, agentMD, "first")

	m := &Manager{cfg: cfg}
	if err := m.syncEffectivePersona(key, tmpl); err != nil {
		t.Fatalf("sync: %v", err)
	}
	eff := filepath.Join(
		config.EffectivePersonaDir(cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role),
		"AGENT.md")
	before, err := os.Stat(eff)
	if err != nil {
		t.Fatal(err)
	}

	mustWrite(t, agentMD, "second, and longer than the first")
	if err := m.syncEffectivePersona(key, tmpl); err != nil {
		t.Fatalf("resync: %v", err)
	}
	after, err := os.Stat(eff)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("the effective file was replaced, not rewritten — a bound container would never see the change")
	}
	if raw, _ := os.ReadFile(eff); string(raw) != "second, and longer than the first" {
		t.Errorf("content = %q, want the new content with no trailing remnant", raw)
	}
}

// A deleted injection with no template fallback must drop the stale copy, or the
// bind would keep serving content nothing provides any more.
func TestSyncEffectivePersonaDropsWhatNothingProvides(t *testing.T) {
	cfg, key, tmpl := personaFixture(t)
	injected := filepath.Join(
		config.TenantAgentPersonaDir(cfg.ContainerDataRoot, key.TenantID, key.Role), "SOUL.md")
	mustWrite(t, injected, "injected")

	m := &Manager{cfg: cfg}
	if err := m.syncEffectivePersona(key, tmpl); err != nil {
		t.Fatalf("sync: %v", err)
	}
	eff := filepath.Join(
		config.EffectivePersonaDir(cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role),
		"SOUL.md")
	if _, err := os.Stat(eff); err != nil {
		t.Fatalf("SOUL.md should be materialized: %v", err)
	}

	if err := os.Remove(injected); err != nil {
		t.Fatal(err)
	}
	if err := m.syncEffectivePersona(key, tmpl); err != nil {
		t.Fatalf("resync: %v", err)
	}
	if _, err := os.Stat(eff); err == nil {
		t.Error("stale effective SOUL.md survived after its injection was deleted")
	}
}

func TestPersonaAdminCRUD(t *testing.T) {
	cfg, _, _ := personaFixture(t)
	m := &Manager{cfg: cfg}
	scope := Scope{Kind: ScopeSubscription, TenantID: "t1", SubsAccID: "s1", AgentKey: "alpha"}

	if list, _ := m.ListPersona(scope); len(list) != 0 {
		t.Errorf("nothing injected yet: %+v", list)
	}
	if err := m.WritePersona(scope, "AGENT.md", "hello"); err != nil {
		t.Fatalf("write: %v", err)
	}
	list, _ := m.ListPersona(scope)
	if len(list) != 1 || list[0].Name != "AGENT.md" {
		t.Fatalf("list = %+v", list)
	}
	if got, err := m.ReadPersona(scope, "AGENT.md"); err != nil || got != "hello" {
		t.Errorf("read = %q err=%v", got, err)
	}
	if err := m.DeletePersona(scope, "AGENT.md"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Idempotent: removing what is not there is the state the caller asked for.
	if err := m.DeletePersona(scope, "AGENT.md"); err != nil {
		t.Errorf("delete should be idempotent: %v", err)
	}
}

// These endpoints write into a workspace ROOT, so an unconstrained name would be
// an arbitrary-file-write primitive reaching every container under the scope.
func TestPersonaRejectsForeignNames(t *testing.T) {
	cfg, _, _ := personaFixture(t)
	m := &Manager{cfg: cfg}
	scope := Scope{Kind: ScopeTenant, TenantID: "t1", AgentKey: "alpha"}

	for _, name := range []string{"", "config.json", "../escape", ".security.yml", "agent.md"} {
		if IsPersonaFile(name) {
			t.Errorf("%q must not count as a persona file", name)
		}
		if err := m.WritePersona(scope, name, "x"); err == nil {
			t.Errorf("write %q should be refused", name)
		}
		if _, err := m.ReadPersona(scope, name); err == nil {
			t.Errorf("read %q should be refused", name)
		}
		if err := m.DeletePersona(scope, name); err == nil {
			t.Errorf("delete %q should be refused", name)
		}
	}
}
