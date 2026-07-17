package docker

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sgelias/crab-shell-proxy/internal/config"
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
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{running: map[string]bool{}, exists: map[string]bool{}}
}

func (f *fakeDocker) Inspect(_ context.Context, name string) (ContainerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ContainerState{Exists: f.exists[name], Running: f.running[name], ID: name}, nil
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
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.exists, name)
	delete(f.running, name)
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
	return NewManager(cfg, dkr, healthy, nil), agent
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
	if tgt.WSEndpoint != "ws://"+name+":18790/pico/ws" {
		t.Errorf("endpoint = %q", tgt.WSEndpoint)
	}
	if tgt.PicoToken != "secret-tok" {
		t.Errorf("token = %q, want secret-tok", tgt.PicoToken)
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
