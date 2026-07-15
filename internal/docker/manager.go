package docker

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/sgelias/crab-shell-proxy/internal/config"
)

// Reconciliation labels stamped on every managed container.
const (
	LabelManaged = "crab-shell.managed"
	LabelAgent   = "crab-shell.agent"
	LabelUser    = "crab-shell.user"
	LabelMode    = "crab-shell.mode"
)

// Docker is the subset of the Engine API the manager needs (interface at the
// consumer so tests can supply a fake).
type Docker interface {
	Inspect(ctx context.Context, name string) (ContainerState, error)
	EnsureImage(ctx context.Context, image string) error
	Create(ctx context.Context, spec CreateSpec) (string, error)
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string, grace time.Duration) error
	Remove(ctx context.Context, name string) error
	List(ctx context.Context, label string) ([]ContainerSummary, error)
}

// HealthChecker reports whether picoclaw inside a container is health-ready.
type HealthChecker func(ctx context.Context, name string, port int) error

// Target is the resolved connection info for a running per-user container.
type Target struct {
	Name       string
	WSEndpoint string // ws://<name>:<port>/pico/ws
	PicoToken  string
}

type keyState struct {
	mu    sync.Mutex  // serializes ensure/stop for one container (single-flight)
	timer *time.Timer // idle timer (scale-to-zero only)
	gen   uint64      // bumped on every (dis)arm; stale fires are ignored
}

// Manager owns per-user container lifecycle.
type Manager struct {
	cfg    *config.Config
	docker Docker
	health HealthChecker
	logf   func(format string, args ...any)

	mu   sync.Mutex
	keys map[string]*keyState
}

// NewManager builds a Manager. If health is nil, an HTTP /health poller is used.
func NewManager(cfg *config.Config, dkr Docker, health HealthChecker, logf func(string, ...any)) *Manager {
	if health == nil {
		health = httpHealth
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{cfg: cfg, docker: dkr, health: health, logf: logf, keys: map[string]*keyState{}}
}

// ContainerName is the deterministic name for one (agent, user) container.
func (m *Manager) ContainerName(agentKey, userKey string) string {
	return fmt.Sprintf("%s-%s-%s", m.cfg.ContainerPrefix, agentKey, userKey)
}

func (m *Manager) wsEndpoint(name string) string {
	return fmt.Sprintf("ws://%s:%d/pico/ws", name, m.cfg.PicoclawPort)
}

func (m *Manager) keyState(name string) *keyState {
	m.mu.Lock()
	defer m.mu.Unlock()
	ks := m.keys[name]
	if ks == nil {
		ks = &keyState{}
		m.keys[name] = ks
	}
	return ks
}

// EnsureRunning guarantees the (agent, user) container exists, is running, and
// is health-ready, then returns how to reach it. Concurrent calls for the same
// container are serialized (single-flight cold start); the idle timer is
// disarmed on entry so a pending scale-to-zero cannot fire mid-turn.
func (m *Manager) EnsureRunning(ctx context.Context, agent config.Agent, userKey, ownerEmail string) (Target, error) {
	name := m.ContainerName(agent.Key, userKey)
	ks := m.keyState(name)
	ks.mu.Lock()
	defer ks.mu.Unlock()

	// Disarm any pending idle-stop for this container before we touch it.
	m.disarmLocked(ks)

	userDir := filepath.Join(m.cfg.ContainerDataRoot, agent.Key, userKey)
	templateDir := filepath.Join(m.cfg.ContainerDataRoot, "templates", agent.Template)
	token, err := provision(userDir, templateDir, m.cfg.PicoclawHome, m.cfg.PicoclawUser, agent.Model, ownerEmail)
	if err != nil {
		return Target{}, err
	}

	st, err := m.docker.Inspect(ctx, name)
	if err != nil {
		return Target{}, err // daemon unreachable etc. — surfaced as 502 upstream
	}

	createdNow := false
	switch {
	case !st.Exists:
		if err := m.create(ctx, agent, userKey, name); err != nil {
			return Target{}, err
		}
		createdNow = true
		if err := m.docker.Start(ctx, name); err != nil {
			return Target{}, err
		}
	case !st.Running:
		if err := m.docker.Start(ctx, name); err != nil {
			return Target{}, err
		}
	}

	if err := m.waitHealthy(ctx, name); err != nil {
		// Don't leak a half-started container we just created.
		if createdNow {
			_ = m.docker.Stop(context.Background(), name, 5*time.Second)
			_ = m.docker.Remove(context.Background(), name)
		}
		return Target{}, fmt.Errorf("picoclaw %s did not become ready: %w", name, err)
	}

	return Target{Name: name, WSEndpoint: m.wsEndpoint(name), PicoToken: token}, nil
}

func (m *Manager) create(ctx context.Context, agent config.Agent, userKey, name string) error {
	hostDir := filepath.Join(m.cfg.HostDataRoot, agent.Key, userKey)
	// picoclaw keeps its config/workspace under $HOME/.picoclaw; mount the
	// per-user dir there and set HOME so it works for a non-root user too (the
	// image's own /root is 0700 and unusable by a non-root uid).
	mountDest := m.cfg.PicoclawHome + "/.picoclaw"
	spec := CreateSpec{
		Name:  name,
		Image: m.cfg.PicoclawImage,
		User:  m.cfg.PicoclawUser,
		Env: []string{
			"PICOCLAW_GATEWAY_HOST=0.0.0.0",
			"HOME=" + m.cfg.PicoclawHome,
		},
		Labels: map[string]string{
			LabelManaged: "true",
			LabelAgent:   agent.Key,
			LabelUser:    userKey,
			LabelMode:    string(agent.Mode),
		},
		Binds:   []string{hostDir + ":" + mountDest},
		Network: m.cfg.Network,
		Init:    true,
	}
	if err := m.docker.EnsureImage(ctx, m.cfg.PicoclawImage); err != nil {
		return fmt.Errorf("ensure image %s: %w", m.cfg.PicoclawImage, err)
	}
	if _, err := m.docker.Create(ctx, spec); err != nil {
		return err
	}
	m.logf("created container %s (mode=%s)", name, agent.Mode)
	return nil
}

func (m *Manager) waitHealthy(ctx context.Context, name string) error {
	deadline := time.Now().Add(m.cfg.StartupDeadline.Std())
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		lastErr = m.health(attemptCtx, name, m.cfg.PicoclawPort)
		cancel()
		if lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("startup deadline exceeded: %w", lastErr)
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ArmIdle re-arms the scale-to-zero idle timer for a container after a turn
// completes. No-op for continuous-mode agents.
func (m *Manager) ArmIdle(agent config.Agent, userKey string) {
	if agent.Mode != config.ModeScaleToZero {
		return
	}
	name := m.ContainerName(agent.Key, userKey)
	ks := m.keyState(name)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	m.armLocked(ks, name, agent.IdleTimeout.Std())
}

// armLocked schedules an idle stop. Caller holds ks.mu.
func (m *Manager) armLocked(ks *keyState, name string, d time.Duration) {
	m.disarmLocked(ks)
	ks.gen++
	gen := ks.gen
	ks.timer = time.AfterFunc(d, func() { m.onIdle(name, gen) })
}

// disarmLocked cancels any pending idle stop. Caller holds ks.mu.
func (m *Manager) disarmLocked(ks *keyState) {
	if ks.timer != nil {
		ks.timer.Stop()
		ks.timer = nil
	}
	ks.gen++
}

// onIdle stops a container whose idle timer fired, unless it was superseded by
// a newer (dis)arm (gen mismatch) — which is how a request that arrived during
// the timer's wait cancels the stop.
func (m *Manager) onIdle(name string, gen uint64) {
	ks := m.keyState(name)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if ks.gen != gen {
		return // superseded by a newer request; do not stop
	}
	m.logf("idle timeout: stopping %s", name)
	if err := m.docker.Stop(context.Background(), name, 10*time.Second); err != nil {
		m.logf("idle stop of %s failed: %v", name, err)
	}
}

// httpHealth is the default checker: picoclaw's own /health next to the WS port.
func httpHealth(ctx context.Context, name string, port int) error {
	url := fmt.Sprintf("http://%s:%d/health", name, port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("health status %d", resp.StatusCode)
	}
	return nil
}
