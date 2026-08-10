package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// fakeDocker is an in-memory Docker for manager tests.
type fakeDocker struct {
	mu         sync.Mutex
	createN    int32
	startN     int32
	stopN      int32
	running    map[string]bool
	exists     map[string]bool
	lastSpec   CreateSpec
	createHook func() // called inside Create to widen the single-flight race window
	listResult []ContainerSummary
	// The binds each container was created with, so Inspect can report them back
	// the way the real daemon does (HostConfig.Binds). Pre-seed an entry to stand
	// in for a container created by an older image.
	binds   map[string][]string
	removeN int32
	// The resolved image id each container is running, and what PicoclawImage
	// currently resolves to. Modelled because a rebuild under an unchanged tag is
	// invisible in every other field the daemon reports.
	images       map[string]string
	wantImageID  string
	imageIDErr   error
	imageIDCalls int32
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		running: map[string]bool{}, exists: map[string]bool{},
		binds: map[string][]string{}, images: map[string]string{},
	}
}

// ImageID answers what a container created right now would run. "" means the image
// is not present locally, which imageDrift must NOT read as drift.
func (f *fakeDocker) ImageID(context.Context, string) (string, error) {
	atomic.AddInt32(&f.imageIDCalls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wantImageID, f.imageIDErr
}

func (f *fakeDocker) Inspect(_ context.Context, name string) (ContainerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ContainerState{
		Exists: f.exists[name], Running: f.running[name], ID: name,
		Binds: f.binds[name], Image: f.images[name],
	}, nil
}

func (f *fakeDocker) EnsureImage(context.Context, string) error { return nil }

func (f *fakeDocker) Create(_ context.Context, spec CreateSpec) (string, error) {
	atomic.AddInt32(&f.createN, 1)
	if f.createHook != nil {
		f.createHook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exists[spec.Name] = true
	f.binds[spec.Name] = spec.Binds
	f.lastSpec = spec
	return spec.Name, nil
}

func (f *fakeDocker) Start(_ context.Context, name string) error {
	atomic.AddInt32(&f.startN, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[name] = true
	return nil
}

func (f *fakeDocker) Stop(_ context.Context, name string, _ time.Duration) error {
	atomic.AddInt32(&f.stopN, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[name] = false
	return nil
}

func (f *fakeDocker) Remove(_ context.Context, name string) error {
	atomic.AddInt32(&f.removeN, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.exists, name)
	delete(f.running, name)
	delete(f.binds, name)
	return nil
}

func (f *fakeDocker) List(_ context.Context, _ string) ([]ContainerSummary, error) {
	return f.listResult, nil
}

// wk builds a WorkspaceKey under the tenant/subscription testManager scaffolds.
func wk(user string) WorkspaceKey {
	return WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: user}
}

// testManager wires a manager with a fake docker, an always-healthy checker,
// and a temp data root seeded with an alpha template and the t1/s1 subscription
// scaffold (so EnsureRunning, which never creates the scaffold, can proceed).
func testManager(t *testing.T, mode config.Mode, dkr Docker) (*Manager, config.Agent) {
	t.Helper()
	root := t.TempDir()
	tmpl := filepath.Join(root, "templates", "alpha")
	if err := os.MkdirAll(tmpl, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpl, "config.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sec := "channels:\n  pico:\n    settings:\n      token: \"secret-tok\"\n"
	if err := os.WriteFile(filepath.Join(tmpl, ".security.yml"), []byte(sec), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(config.SubscriptionRoot(root, "t1", "s1"), 0o700); err != nil {
		t.Fatal(err)
	}

	agent := config.Agent{Key: "alpha", ServiceName: "picoclaw-alpha", Template: "alpha",
		Mode: mode, IdleTimeout: config.Duration(50 * time.Millisecond), ResolvedToken: "bearer"}
	cfg := &config.Config{
		HostDataRoot: "/host/data", ContainerDataRoot: root, Network: "zombie_net",
		PicoclawImage: "img", PicoclawPort: 18790, StartupDeadline: config.Duration(time.Second),
		TurnTimeout: config.Duration(time.Second), ContainerPrefix: "picoclaw",
		PicoclawUser: "1000:1000", PicoclawHome: "/data",
		Agents: map[string]config.Agent{"alpha": agent},
	}
	healthy := func(context.Context, string, int) error { return nil }

	// A real registry with one resolvable model at LevelGlobal: EnsureRunning
	// now calls resolveAndMaterialize after provision, and a workspace with no
	// resolvable model is refused rather than defaulted (materialize.go).
	reg, err := registry.Open(filepath.Join(root, "model-registry.db"), time.Now)
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	if _, err := reg.CreateModel(registry.Model{
		ModelName: "default", Provider: "openai", Model: "gpt-4o",
		APIBase: "https://api.openai.com/v1", APIKey: "sk-test", Status: registry.StatusActive,
	}); err != nil {
		t.Fatalf("registry.CreateModel: %v", err)
	}
	if err := reg.SetScopeDefault(registry.ScopeSel{Level: registry.LevelGlobal}, "default"); err != nil {
		t.Fatalf("registry.SetScopeDefault: %v", err)
	}

	return NewManager(cfg, dkr, healthy, reg, nil), agent
}

func TestEnsureRunningColdStart(t *testing.T) {
	f := newFakeDocker()
	m, agent := testManager(t, config.ModeScaleToZero, f)

	tgt, err := m.EnsureRunning(context.Background(), agent, wk("hash1"), "test@x")
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	name := m.ContainerName(wk("hash1"))
	if tgt.Name != name {
		t.Errorf("name = %q, want %q", tgt.Name, name)
	}
	if tgt.Endpoint != "ws://"+name+":18790/pico/ws" {
		t.Errorf("endpoint = %q", tgt.Endpoint)
	}
	// seedPicoToken always mints a fresh token on first provision, regardless of
	// whatever placeholder the template shipped (it no longer only regenerates
	// the token when a model happens to be pinned).
	if !strings.HasPrefix(tgt.AuthToken, "pico-") {
		t.Errorf("token = %q, want a pico- prefixed random token", tgt.AuthToken)
	}
	if f.createN != 1 || f.startN != 1 {
		t.Errorf("create=%d start=%d, want 1/1", f.createN, f.startN)
	}
	// Labels carry the full workspace tuple (role/tenant/subscription/user).
	l := f.lastSpec.Labels
	if l[LabelAgent] != "alpha" || l[LabelManaged] != "true" ||
		l[LabelTenant] != "t1" || l[LabelSubscription] != "s1" || l[LabelUser] != "hash1" {
		t.Errorf("labels = %v", l)
	}
	// Bind uses the HOST path (nested layout) as source and <PicoclawHome>/.picoclaw
	// as the mount destination.
	if want := "/host/data/tenants/t1/subscriptions/s1/agents/alpha/users/hash1:/data/.picoclaw"; f.lastSpec.Binds[0] != want {
		t.Errorf("bind = %q, want %q", f.lastSpec.Binds[0], want)
	}
	// Non-root posture: User set and HOME relocated.
	if f.lastSpec.User != "1000:1000" {
		t.Errorf("user = %q, want 1000:1000", f.lastSpec.User)
	}
	hasHome := false
	for _, e := range f.lastSpec.Env {
		if e == "HOME=/data" {
			hasHome = true
		}
	}
	if !hasHome {
		t.Errorf("env missing HOME=/data: %v", f.lastSpec.Env)
	}
	// Per-user workspace leaf was created lazily and seeded from template.
	leaf := config.UserWorkspace(m.cfg.ContainerDataRoot, "t1", "s1", "alpha", "hash1")
	if _, err := os.Stat(filepath.Join(leaf, "config.json")); err != nil {
		t.Errorf("config.json not provisioned: %v", err)
	}
}

func TestCreateAddsReadOnlySecretsBind(t *testing.T) {
	f := newFakeDocker()
	m, agent := testManager(t, config.ModeScaleToZero, f)
	if _, err := m.EnsureRunning(context.Background(), agent, wk("hash1"), "test@x"); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	// The per-(user, agent) EFFECTIVE secret view (shared cascade + user's own) is
	// bind-mounted READ-ONLY under the workspace.
	want := "/host/data/effective-secrets/hash1/alpha:/data/.picoclaw/workspace/.secrets:ro"
	if len(f.lastSpec.Binds) < 2 || f.lastSpec.Binds[1] != want {
		t.Errorf("secrets bind = %v, want [_, %q]", f.lastSpec.Binds, want)
	}
	// The store dir (container-side view) is ensured before create so the merge
	// source exists, and the effective view is materialized as the mount source.
	if _, err := os.Stat(config.StoreDir(m.cfg.ContainerDataRoot, "hash1", "alpha")); err != nil {
		t.Errorf("store dir not ensured before create: %v", err)
	}
	if _, err := os.Stat(config.EffectiveSecretsDir(m.cfg.ContainerDataRoot, "hash1", "alpha")); err != nil {
		t.Errorf("effective secrets dir not materialized before create: %v", err)
	}
}

func TestRestartWorkspaceRestartsAndRearms(t *testing.T) {
	f := newFakeDocker()
	m, agent := testManager(t, config.ModeScaleToZero, f)
	if _, err := m.EnsureRunning(context.Background(), agent, wk("h"), "test@x"); err != nil {
		t.Fatal(err)
	}
	name := m.ContainerName(wk("h"))
	startsBefore, stopsBefore := f.startN, f.stopN

	if err := m.RestartWorkspace(wk("h")); err != nil {
		t.Fatalf("RestartWorkspace: %v", err)
	}
	if f.stopN != stopsBefore+1 || f.startN != startsBefore+1 {
		t.Errorf("restart stop/start = %d/%d, want +1/+1", f.stopN-stopsBefore, f.startN-startsBefore)
	}
	if !f.running[name] {
		t.Error("container not running after restart")
	}
	// Scale-to-zero: the idle timer is re-armed so it can scale back down.
	m.mu.Lock()
	ks := m.keys[name]
	m.mu.Unlock()
	ks.mu.Lock()
	armed := ks.timer != nil
	ks.mu.Unlock()
	if !armed {
		t.Error("idle timer not re-armed after restart (scale-to-zero)")
	}
}

func TestRestartWorkspaceNoopWhenNotRunning(t *testing.T) {
	f := newFakeDocker()
	m, _ := testManager(t, config.ModeScaleToZero, f)
	// A container that was never created: restart is a pure no-op (the next chat
	// cold-starts with the new secret already in the store).
	if err := m.RestartWorkspace(wk("ghost")); err != nil {
		t.Fatalf("no-op restart should not error: %v", err)
	}
	if f.startN != 0 || f.stopN != 0 {
		t.Errorf("start/stop = %d/%d, want 0/0 (no container to restart)", f.startN, f.stopN)
	}
}

func TestEnsureRunningErrorsWhenNotScaffolded(t *testing.T) {
	f := newFakeDocker()
	m, agent := testManager(t, config.ModeScaleToZero, f)
	// A subscription that was never scaffolded (no SubscriptionRoot on disk).
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s-missing", Role: "alpha", UserAccID: "u1"}
	if _, err := m.EnsureRunning(context.Background(), agent, key, "test@x"); err == nil {
		t.Fatal("expected error for un-scaffolded subscription")
	}
	if f.createN != 0 {
		t.Errorf("createN = %d, want 0 (no container for un-scaffolded subscription)", f.createN)
	}
	if _, err := os.Stat(config.UserWorkspace(m.cfg.ContainerDataRoot, "t1", "s-missing", "alpha", "u1")); err == nil {
		t.Error("user leaf must not be created for un-scaffolded subscription")
	}
}

func TestEnsureRunningSingleFlight(t *testing.T) {
	f := newFakeDocker()
	// Widen the create window so concurrent callers would double-create if not
	// serialized.
	f.createHook = func() { time.Sleep(20 * time.Millisecond) }
	m, agent := testManager(t, config.ModeScaleToZero, f)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.EnsureRunning(context.Background(), agent, wk("same"), "test@x"); err != nil {
				t.Errorf("EnsureRunning: %v", err)
			}
		}()
	}
	wg.Wait()
	if f.createN != 1 {
		t.Errorf("createN = %d under concurrency, want exactly 1 (single-flight)", f.createN)
	}
}

func TestEnsureRunningReusesRunning(t *testing.T) {
	f := newFakeDocker()
	m, agent := testManager(t, config.ModeContinuous, f)
	if _, err := m.EnsureRunning(context.Background(), agent, wk("h"), "test@x"); err != nil {
		t.Fatal(err)
	}
	c1, s1 := f.createN, f.startN
	if _, err := m.EnsureRunning(context.Background(), agent, wk("h"), "test@x"); err != nil {
		t.Fatal(err)
	}
	if f.createN != c1 {
		t.Errorf("second call created again: %d -> %d", c1, f.createN)
	}
	if f.startN != s1 {
		t.Errorf("second call started again: %d -> %d", s1, f.startN)
	}
}

func TestScaleToZeroIdleStop(t *testing.T) {
	f := newFakeDocker()
	m, agent := testManager(t, config.ModeScaleToZero, f) // idleTimeout 50ms
	if _, err := m.EnsureRunning(context.Background(), agent, wk("h"), "test@x"); err != nil {
		t.Fatal(err)
	}
	m.ArmIdle(agent, wk("h"))
	name := m.ContainerName(wk("h"))

	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		stopped := !f.running[name]
		f.mu.Unlock()
		if stopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("container was not idle-stopped within deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestContinuousDoesNotArmIdle(t *testing.T) {
	f := newFakeDocker()
	m, agent := testManager(t, config.ModeContinuous, f)
	if _, err := m.EnsureRunning(context.Background(), agent, wk("h"), "test@x"); err != nil {
		t.Fatal(err)
	}
	m.ArmIdle(agent, wk("h"))
	name := m.ContainerName(wk("h"))
	time.Sleep(120 * time.Millisecond) // well past the 50ms idle window
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running[name] {
		t.Error("continuous container must not be idle-stopped")
	}
}

func TestReconcileAdoptsRunningByLabels(t *testing.T) {
	f := newFakeDocker()
	m, _ := testManager(t, config.ModeScaleToZero, f)
	name := "picoclaw-alpha-s1-u1"
	f.listResult = []ContainerSummary{{
		Names: []string{"/" + name}, State: "running",
		Labels: map[string]string{
			LabelManaged: "true", LabelAgent: "alpha", LabelTenant: "t1",
			LabelSubscription: "s1", LabelUser: "u1", LabelMode: "scale-to-zero",
		},
	}}
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// A scale-to-zero container found running must have its idle timer re-armed.
	m.mu.Lock()
	ks := m.keys[name]
	m.mu.Unlock()
	if ks == nil {
		t.Fatal("running container was not adopted")
	}
	ks.mu.Lock()
	armed := ks.timer != nil
	ks.mu.Unlock()
	if !armed {
		t.Error("adopted scale-to-zero container did not get an idle timer")
	}
}

func TestReconcileEnsuresContinuousWorkspaces(t *testing.T) {
	f := newFakeDocker()
	m, _ := testManager(t, config.ModeContinuous, f)
	// A pre-existing user workspace for the continuous agent must be started.
	leaf := config.UserWorkspace(m.cfg.ContainerDataRoot, "t1", "s1", "alpha", "u1")
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if f.createN != 1 || f.startN != 1 {
		t.Errorf("create=%d start=%d, want 1/1 (continuous workspace ensured)", f.createN, f.startN)
	}
	if !f.running[m.ContainerName(wk("u1"))] {
		t.Error("continuous workspace container not running after reconcile")
	}
}

// TestEnsureRunningRecreatesOnPersonaDrift is the end of the reported bug: an
// admin's identity file that never reaches a running instance, restart after
// restart. The container here stands in for one created before the persona mounts
// existed — it is running and healthy, and the only way its workspace can ever see
// AGENT.md is a rebuild.
//
// Also pins the other half: once rebuilt, a further request must NOT recreate
// again. A drift check that never converges would recreate on every single turn,
// truncating the conversation each time — worse than the bug.
func TestEnsureRunningRecreatesOnPersonaDrift(t *testing.T) {
	f := newFakeDocker()
	m, agent := testManager(t, config.ModeContinuous, f)
	key := wk("h")
	name := m.ContainerName(key)

	// A container that exists and runs, with the workspace bind but no persona
	// mount — exactly what an image predating the feature produced.
	f.exists[name] = true
	f.running[name] = true
	f.binds[name] = []string{"/host/ws:" + m.picoclawMountDest()}

	// The admin injected an identity at tenant scope. EnsureRunning materializes the
	// cascade itself, so the effective file (the bind source) appears during the call.
	if err := m.WritePersona(Scope{Kind: ScopeTenant, TenantID: key.TenantID, AgentKey: key.Role},
		"AGENT.md", "# who you are\n"); err != nil {
		t.Fatal(err)
	}

	if _, err := m.EnsureRunning(context.Background(), agent, key, "test@x"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if f.removeN != 1 || f.createN != 1 {
		t.Fatalf("removes=%d creates=%d, want 1/1: a drifted container must be rebuilt, not restarted",
			f.removeN, f.createN)
	}
	var mounted bool
	for _, b := range f.lastSpec.Binds {
		if strings.HasSuffix(b, m.picoclawMountDest()+"/workspace/AGENT.md:ro") {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("rebuilt container still has no AGENT.md mount: %v", f.lastSpec.Binds)
	}

	// Converged: the rebuilt container matches, so nothing further happens.
	if _, err := m.EnsureRunning(context.Background(), agent, key, "test@x"); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if f.removeN != 1 || f.createN != 1 {
		t.Errorf("removes=%d creates=%d after a second call: the check must converge, "+
			"or every turn recreates the container", f.removeN, f.createN)
	}
}

// imageDrift is what makes a harness upgrade land. The agent containers are this
// manager's, not compose's: they outlive a stack redeploy and reuse whatever image
// they were created from, so rebuilding the image and redeploying changes nothing
// until something notices. On 2026-08-10 that cost an afternoon — a picoclaw patch
// was in the image and not in the running binary, and the bug it fixed reproduced
// identically after the deploy.
//
// The comparison is on resolved IDS for a reason this table pins: the tag does not
// change when the image behind it is rebuilt, and a fixed tag is exactly how this
// stack ships its own harness (deploy/picoclaw-glob).
func TestImageDrift(t *testing.T) {
	cases := []struct {
		name      string
		container string // resolved id the container runs
		want      string // resolved id PicoclawImage points at now
		wantErr   error
		drift     bool
		calls     int32
	}{
		{
			name:      "same image",
			container: "sha256:aaa",
			want:      "sha256:aaa",
			drift:     false,
			calls:     1,
		},
		{
			// The reported case: same tag, rebuilt content.
			name:      "rebuilt under the same tag",
			container: "sha256:old",
			want:      "sha256:new",
			drift:     true,
			calls:     1,
		},
		{
			// An older daemon, or a fake that does not model it. Not knowing what the
			// container runs is not evidence that it is wrong.
			name:      "container image unknown",
			container: "",
			want:      "sha256:new",
			drift:     false,
			calls:     0, // asked nothing: there is nothing to compare against
		},
		{
			// Not present locally. create() calls EnsureImage, which is where a pull
			// belongs; recreating here would destroy a live conversation to install
			// nothing.
			name:      "desired image absent locally",
			container: "sha256:old",
			want:      "",
			drift:     false,
			calls:     1,
		},
		{
			name:      "daemon error",
			container: "sha256:old",
			want:      "",
			wantErr:   errors.New("daemon unreachable"),
			drift:     false,
			calls:     1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeDocker()
			f.wantImageID = c.want
			f.imageIDErr = c.wantErr
			m, _ := testManager(t, config.ModeScaleToZero, f)

			got := m.imageDrift(context.Background(), ContainerState{
				Exists: true, Running: true, Image: c.container,
			})
			if got != c.drift {
				t.Errorf("imageDrift = %v, want %v", got, c.drift)
			}
			if n := atomic.LoadInt32(&f.imageIDCalls); n != c.calls {
				t.Errorf("ImageID calls = %d, want %d", n, c.calls)
			}
		})
	}
}

// The wiring, not the predicate: a drift check that EnsureRunning never consults is
// the failure mode this whole feature exists to remove. Chown-dependent like the
// rest of the TestEnsureRunning* family (STATE.md L-001) — it passes as root.
//
// Also pins convergence: once rebuilt, a second request must NOT recreate again. A
// check that never settles would recreate on every turn, which is worse than the
// bug it fixes.
func TestEnsureRunningRecreatesOnImageDrift(t *testing.T) {
	f := newFakeDocker()
	m, agent := testManager(t, config.ModeContinuous, f)
	key := wk("h")
	name := m.ContainerName(key)

	// Running, healthy, correct mounts — and an image nobody builds any more.
	f.exists[name] = true
	f.running[name] = true
	f.images[name] = "sha256:old"
	f.wantImageID = "sha256:new"
	for _, b := range personaBinds(m.cfg, key, m.picoclawMountDest()) {
		f.binds[name] = append(f.binds[name], "/host:"+m.picoclawMountDest()+"/workspace/"+b.name)
	}

	if _, err := m.EnsureRunning(context.Background(), agent, key, "test@x"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if f.removeN != 1 || f.createN != 1 {
		t.Fatalf("removes=%d creates=%d, want 1/1: a stale image must be rebuilt, not restarted",
			f.removeN, f.createN)
	}

	// The recreate installed the new image; the next request must leave it alone.
	f.images[name] = "sha256:new"
	if _, err := m.EnsureRunning(context.Background(), agent, key, "test@x"); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if f.removeN != 1 || f.createN != 1 {
		t.Errorf("removes=%d creates=%d after convergence, want 1/1 (no recreate loop)",
			f.removeN, f.createN)
	}
}
