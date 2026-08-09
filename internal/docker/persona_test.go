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
	if got, src, err := m.ReadPersona(scope, "AGENT.md"); err != nil || got != "hello" || src != PersonaSourceScope {
		t.Errorf("read = %q source=%q err=%v, want the scope's own injection", got, src, err)
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
		if _, _, err := m.ReadPersona(scope, name); err == nil {
			t.Errorf("read %q should be refused", name)
		}
		if err := m.DeletePersona(scope, name); err == nil {
			t.Errorf("delete %q should be refused", name)
		}
	}
}

// TestReadPersonaResolvesToTemplate is the regression test for the Identity tab
// opening AGENT.md empty. The editor used to read the scope's own injection only,
// so a scope that injects nothing — the normal state — offered a blank page, and
// the first save silently replaced an identity the admin had never seen. The read
// now walks the same precedence the workspace cascade does and reports which layer
// answered.
func TestReadPersonaResolvesToTemplate(t *testing.T) {
	cfg, _, tmpl := personaFixture(t)
	cfg.Agents = map[string]config.Agent{"alpha": {Key: "alpha", Template: "alpha"}}
	m := &Manager{cfg: cfg}
	subScope := Scope{Kind: ScopeSubscription, TenantID: "t1", SubsAccID: "s1", AgentKey: "alpha"}
	tenantScope := Scope{Kind: ScopeTenant, TenantID: "t1", AgentKey: "alpha"}

	mustWrite(t, filepath.Join(tmpl, "workspace", "AGENT.md"), "from the template")

	// Nothing injected anywhere: both scopes preload the agent's real identity.
	for _, sc := range []Scope{tenantScope, subScope} {
		got, src, err := m.ReadPersona(sc, "AGENT.md")
		if err != nil || got != "from the template" || src != PersonaSourceTemplate {
			t.Fatalf("%s scope: read = %q source=%q err=%v, want the template's text", sc.Kind, got, src, err)
		}
	}

	// A tenant injection takes over — and for the SUBSCRIPTION scope it reads as
	// inherited ("tenant"), not as its own.
	if err := m.WritePersona(tenantScope, "AGENT.md", "from the tenant"); err != nil {
		t.Fatal(err)
	}
	if got, src, _ := m.ReadPersona(subScope, "AGENT.md"); got != "from the tenant" || src != PersonaSourceTenant {
		t.Errorf("subscription scope: read = %q source=%q, want the tenant layer", got, src)
	}
	// At the tenant scope that same file IS the scope's own injection.
	if got, src, _ := m.ReadPersona(tenantScope, "AGENT.md"); got != "from the tenant" || src != PersonaSourceScope {
		t.Errorf("tenant scope: read = %q source=%q, want its own injection", got, src)
	}

	// The subscription's own injection wins over both.
	if err := m.WritePersona(subScope, "AGENT.md", "from the subscription"); err != nil {
		t.Fatal(err)
	}
	if got, src, _ := m.ReadPersona(subScope, "AGENT.md"); got != "from the subscription" || src != PersonaSourceScope {
		t.Errorf("subscription scope: read = %q source=%q, want its own injection", got, src)
	}

	// A file no layer provides is still a miss, which the handler turns into a 404
	// rather than showing an empty editor as if it were content.
	if _, _, err := m.ReadPersona(tenantScope, "HEARTBEAT.md"); !os.IsNotExist(err) {
		t.Errorf("err = %v, want a not-exist miss when no layer has the file", err)
	}
}

// An agent the config does not know has no template layer to fall back to: the
// cascade ends at the scope dirs instead of guessing a template path.
func TestReadPersonaWithoutConfiguredAgentHasNoTemplateLayer(t *testing.T) {
	cfg, _, tmpl := personaFixture(t)
	m := &Manager{cfg: cfg} // cfg.Agents is nil
	mustWrite(t, filepath.Join(tmpl, "workspace", "AGENT.md"), "from the template")

	if _, _, err := m.ReadPersona(Scope{Kind: ScopeTenant, TenantID: "t1", AgentKey: "alpha"}, "AGENT.md"); !os.IsNotExist(err) {
		t.Errorf("err = %v, want a miss: an unconfigured agent has no template to resolve", err)
	}
}

// TestPersonaBindDrift covers the answer to "I saved the file and it never reached
// the instances, even after restarting the containers". A bind set is fixed when a
// container is created and BounceScope is stop+start by design, so a container
// missing the mount can never receive the change — the drift check is what turns
// that into a recreate.
func TestPersonaBindDrift(t *testing.T) {
	cfg, key, _ := personaFixture(t)
	const dest = "/home/pico/.picoclaw"
	eff := config.EffectivePersonaDir(cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role)
	effHost := config.EffectivePersonaDir(cfg.HostDataRoot, key.TenantID, key.SubsAccID, key.Role)
	// The workspace bind every container has, plus an unrelated one: neither is in
	// the persona namespace, so neither may influence the verdict.
	base := []string{
		"/host/data/ws:" + dest,
		"/host/data/eff-secrets:" + dest + "/workspace/.secrets:ro",
	}
	personaBind := func(name string) string {
		return filepath.Join(effHost, name) + ":" + dest + "/workspace/" + name + ":ro"
	}

	// Two files materialized, both mounted: no drift.
	mustWrite(t, filepath.Join(eff, "AGENT.md"), "a")
	mustWrite(t, filepath.Join(eff, "SOUL.md"), "s")
	matching := append(append([]string{}, base...), personaBind("AGENT.md"), personaBind("SOUL.md"))
	if personaBindDrift(cfg, key, dest, matching) {
		t.Error("reported drift while every materialized file is mounted")
	}

	// The reported bug: the container predates the file, so it has no mount for it.
	if !personaBindDrift(cfg, key, dest, append(append([]string{}, base...), personaBind("AGENT.md"))) {
		t.Error("a missing SOUL.md mount is exactly the drift that must be caught")
	}
	// A container from before the feature existed: no persona mounts at all.
	if !personaBindDrift(cfg, key, dest, base) {
		t.Error("a container with no persona mounts at all must read as drifted")
	}

	// The reverse: an injection was cleared and the template has no such file, so
	// the effective copy is gone while the container still binds it. Left alone,
	// Docker recreates that missing source as an empty DIRECTORY in the workspace.
	stale := append(append([]string{}, matching...), personaBind("HEARTBEAT.md"))
	if !personaBindDrift(cfg, key, dest, stale) {
		t.Error("a bind whose effective source no longer exists must read as drifted")
	}

	// Nothing materialized and nothing mounted is a legitimate state (a template
	// that ships no workspace/*.md), not drift.
	empty, emptyKey, _ := personaFixture(t)
	if personaBindDrift(empty, emptyKey, dest, base) {
		t.Error("no expected mounts and none present is not drift")
	}

	// Host-root changes must NOT read as drift: the destination is what decides, so
	// a container bound through an old HostDataRoot is left alone rather than
	// recreating the entire fleet after a settings edit.
	movedHost := []string{
		"/old/host/root/effective-persona/t1/s1/alpha/AGENT.md:" + dest + "/workspace/AGENT.md:ro",
		"/old/host/root/effective-persona/t1/s1/alpha/SOUL.md:" + dest + "/workspace/SOUL.md:ro",
	}
	if personaBindDrift(cfg, key, dest, append(append([]string{}, base...), movedHost...)) {
		t.Error("a different host source path is not persona drift; only the destination counts")
	}
}

// TestSyncEffectivePersonaForScopeReachesTenantWorkspaces exercises the other
// candidate for the same symptom: a tenant-scope write whose fan-out never reaches
// the workspace, leaving the effective file (the bind source) untouched. It builds
// the real on-disk layout config.go defines and proves the fan-out lands.
func TestSyncEffectivePersonaForScopeReachesTenantWorkspaces(t *testing.T) {
	cfg, key, tmpl := personaFixture(t)
	cfg.Agents = map[string]config.Agent{"alpha": {Key: "alpha", Template: "alpha"}}
	m := &Manager{cfg: cfg}
	// The layout a real subscription has: the enumeration walks
	// tenants/<t>/subscriptions/<s>, and the user leaf is what marks it in use.
	mustWrite(t, filepath.Join(config.UserWorkspace(cfg.ContainerDataRoot,
		key.TenantID, key.SubsAccID, key.Role, key.UserAccID), "config.json"), "{}")
	mustWrite(t, filepath.Join(tmpl, "workspace", "AGENT.md"), "from the template")

	tenantScope := Scope{Kind: ScopeTenant, TenantID: key.TenantID, AgentKey: "alpha"}
	if err := m.WritePersona(tenantScope, "AGENT.md", "from the admin"); err != nil {
		t.Fatal(err)
	}
	if err := m.SyncEffectivePersonaForScope(tenantScope); err != nil {
		t.Fatalf("sync: %v", err)
	}

	eff := filepath.Join(config.EffectivePersonaDir(cfg.ContainerDataRoot,
		key.TenantID, key.SubsAccID, key.Role), "AGENT.md")
	raw, err := os.ReadFile(eff)
	if err != nil {
		t.Fatalf("effective AGENT.md not materialized for the subscription under the tenant: %v", err)
	}
	if string(raw) != "from the admin" {
		t.Errorf("effective AGENT.md = %q, want the admin's text (the bind source must carry it)", raw)
	}
}
