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
	running    map[string]bool
	exists     map[string]bool
	lastSpec   CreateSpec
	createHook func() // called inside Create to widen the single-flight race window
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
	return nil, nil
}

// testManager wires a manager with a fake docker, an always-healthy checker,
// and a temp data root seeded with an alpha template.
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

	agent := config.Agent{Key: "alpha", ServiceName: "picoclaw-alpha", Template: "alpha",
		Mode: mode, IdleTimeout: config.Duration(50 * time.Millisecond), ResolvedToken: "bearer"}
	cfg := &config.Config{
		HostDataRoot: "/host/data", ContainerDataRoot: root, Network: "zombie_net",
		PicoclawImage: "img", PicoclawPort: 18790, StartupDeadline: config.Duration(time.Second),
		TurnTimeout: config.Duration(time.Second), ContainerPrefix: "picoclaw",
		Agents: map[string]config.Agent{"alpha": agent},
	}
	healthy := func(context.Context, string, int) error { return nil }
	return NewManager(cfg, dkr, healthy, nil), agent
}

func TestEnsureRunningColdStart(t *testing.T) {
	f := newFakeDocker()
	m, agent := testManager(t, config.ModeScaleToZero, f)

	tgt, err := m.EnsureRunning(context.Background(), agent, "hash1")
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if tgt.Name != "picoclaw-alpha-hash1" {
		t.Errorf("name = %q", tgt.Name)
	}
	if tgt.WSEndpoint != "ws://picoclaw-alpha-hash1:18790/pico/ws" {
		t.Errorf("endpoint = %q", tgt.WSEndpoint)
	}
	if tgt.PicoToken != "secret-tok" {
		t.Errorf("token = %q, want secret-tok", tgt.PicoToken)
	}
	if f.createN != 1 || f.startN != 1 {
		t.Errorf("create=%d start=%d, want 1/1", f.createN, f.startN)
	}
	// Labels + bind use the HOST path, not the container path.
	if f.lastSpec.Labels[LabelAgent] != "alpha" || f.lastSpec.Labels[LabelManaged] != "true" {
		t.Errorf("labels = %v", f.lastSpec.Labels)
	}
	if want := "/host/data/alpha/hash1:/root/.picoclaw"; f.lastSpec.Binds[0] != want {
		t.Errorf("bind = %q, want %q", f.lastSpec.Binds[0], want)
	}
	// Per-user data dir was seeded from template (config-only).
	if _, err := os.Stat(filepath.Join(m.cfg.ContainerDataRoot, "alpha", "hash1", "config.json")); err != nil {
		t.Errorf("config.json not provisioned: %v", err)
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
			if _, err := m.EnsureRunning(context.Background(), agent, "same"); err != nil {
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
	if _, err := m.EnsureRunning(context.Background(), agent, "h"); err != nil {
		t.Fatal(err)
	}
	c1, s1 := f.createN, f.startN
	if _, err := m.EnsureRunning(context.Background(), agent, "h"); err != nil {
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
	if _, err := m.EnsureRunning(context.Background(), agent, "h"); err != nil {
		t.Fatal(err)
	}
	m.ArmIdle(agent, "h")
	name := m.ContainerName("alpha", "h")

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
	if _, err := m.EnsureRunning(context.Background(), agent, "h"); err != nil {
		t.Fatal(err)
	}
	m.ArmIdle(agent, "h")
	name := m.ContainerName("alpha", "h")
	time.Sleep(120 * time.Millisecond) // well past the 50ms idle window
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running[name] {
		t.Error("continuous container must not be idle-stopped")
	}
}
