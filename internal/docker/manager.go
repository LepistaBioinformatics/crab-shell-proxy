package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// Reconciliation labels stamped on every managed container.
const (
	LabelManaged      = "crab-shell.managed"
	LabelAgent        = "crab-shell.agent" // = role (agent key)
	LabelTenant       = "crab-shell.tenant"
	LabelSubscription = "crab-shell.subscription"
	LabelUser         = "crab-shell.user" // = user account id
	LabelMode         = "crab-shell.mode"
)

// WorkspaceKey identifies one fully isolated per-user workspace/container under
// the tenant→subscription→agent→user layout. Role is the agent key.
type WorkspaceKey struct {
	TenantID  string
	SubsAccID string
	Role      string
	UserAccID string
}

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

// HealthChecker reports whether the harness inside a container is health-ready
// (an HTTP GET returning 2xx on the harness port).
type HealthChecker func(ctx context.Context, name string, port int) error

// hermesAPIPort is the OpenAI-compatible API server port hermes-agent listens on
// (API_SERVER_PORT); the picoclaw port is configurable (cfg.PicoclawPort).
const hermesAPIPort = 8642

// Target is the resolved connection info for a running per-user container.
type Target struct {
	Name string
	// Endpoint is the harness-specific address: ws://<name>:<port>/pico/ws
	// (picoclaw) or http://<name>:8642 (hermes).
	Endpoint string
	// AuthToken is the pico channel token (picoclaw) or API server bearer key
	// (hermes).
	AuthToken string
	// Harness selects which turner runs against this target.
	Harness string
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
	// reg is the model inventory: the single source of truth for which model a
	// workspace uses. Nothing else in this package may decide that.
	reg  *registry.Registry
	logf func(format string, args ...any)

	mu   sync.Mutex
	keys map[string]*keyState

	// The embedded operator-managed skills are materialized once per process to
	// the read-only bind source shared by every container.
	managedOnce sync.Once
	managedErr  error
}

// NewManager builds a Manager. If health is nil, an HTTP /health poller is used.
// reg is required: without the inventory there is no way to resolve a model, and
// a workspace provisioned without one cannot boot.
func NewManager(cfg *config.Config, dkr Docker, health HealthChecker, reg *registry.Registry, logf func(string, ...any)) *Manager {
	if health == nil {
		health = httpHealth
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{cfg: cfg, docker: dkr, health: health, reg: reg, logf: logf, keys: map[string]*keyState{}}
}

// ContainerName is the deterministic name for one workspace's container:
// <prefix>-<role>-<subsAccId>-<userAccId>. subsAccId is a globally-unique
// mycelium account UUID, so the triple is unique without the tenant id.
func (m *Manager) ContainerName(key WorkspaceKey) string {
	// <prefix>-<role>-<hash>. The full tuple has two UUIDs (~88 chars), which
	// exceeds the 63-char DNS label limit and makes the container unresolvable
	// by its own name on the docker network (health-wait dials it by name). Hash
	// the isolation tuple instead; tenant/subscription/user are recovered from
	// the container labels and the .crab-owner.json marker, not the name.
	sum := sha256.Sum256([]byte(key.TenantID + "::" + key.SubsAccID + "::" + key.UserAccID))
	return fmt.Sprintf("%s-%s-%s", m.cfg.ContainerPrefix,
		identity.SanitizeID(key.Role), hex.EncodeToString(sum[:])[:16])
}

// harnessPort is the health/API port for an agent's harness.
func (m *Manager) harnessPort(agent config.Agent) int {
	if agent.Harness == config.HarnessHermes {
		return hermesAPIPort
	}
	return m.cfg.PicoclawPort
}

// startupDeadline is how long to wait for an agent's container to become
// health-ready: the agent's own override when set, else the global.
func (m *Manager) startupDeadline(agent config.Agent) time.Duration {
	if agent.StartupDeadline > 0 {
		return agent.StartupDeadline.Std()
	}
	return m.cfg.StartupDeadline.Std()
}

// endpoint is the address a turner dials for a running container of this agent.
func (m *Manager) endpoint(agent config.Agent, name string) string {
	if agent.Harness == config.HarnessHermes {
		return fmt.Sprintf("http://%s:%d", name, hermesAPIPort)
	}
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

// EnsureRunning guarantees the workspace's container exists, is running, and is
// health-ready, then returns how to reach it. Concurrent calls for the same
// container are serialized (single-flight cold start); the idle timer is
// disarmed on entry so a pending scale-to-zero cannot fire mid-turn.
//
// It NEVER creates the subscription scaffold (only POST /v1/accounts does);
// when SubscriptionRoot is absent it errors without touching any container. It
// MAY create the lazy <role>/users/<u> leaf and the container.
func (m *Manager) EnsureRunning(ctx context.Context, agent config.Agent, key WorkspaceKey, ownerEmail string) (Target, error) {
	name := m.ContainerName(key)
	ks := m.keyState(name)
	ks.mu.Lock()
	defer ks.mu.Unlock()

	// Disarm any pending idle-stop for this container before we touch it.
	m.disarmLocked(ks)

	subsRoot := config.SubscriptionRoot(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID)
	if _, err := os.Stat(subsRoot); err != nil {
		return Target{}, fmt.Errorf("subscription %s/%s not scaffolded (POST /v1/accounts first): %w",
			key.TenantID, key.SubsAccID, err)
	}

	userDir := config.UserWorkspace(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	templateDir := config.TemplatesDir(m.cfg.ContainerDataRoot, agent.Template)
	model := m.resolveModel(agent, key)

	// Provision the per-user data dir and obtain the auth token to reach the
	// container: picoclaw's pico channel token, or hermes' generated API server
	// bearer key. Both are persisted per user so a returning user reuses them.
	var authToken string
	var err error
	if agent.Harness == config.HarnessHermes {
		authToken, err = provisionHermes(userDir, templateDir, m.cfg.PicoclawUser, model)
	} else {
		// Materialize the effective secret view BEFORE provisioning: provision
		// merges the native overlay from it, and native slots now arrive from the
		// admin cascade, not only from the user's own store.
		effDir, syncErr := m.syncEffectiveSecrets(key)
		if syncErr != nil {
			return Target{}, syncErr
		}
		authToken, err = provision(userDir, templateDir, effDir, m.cfg.PicoclawHome, m.cfg.PicoclawUser, model, key, ownerEmail)
	}
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
		var cerr error
		if agent.Harness == config.HarnessHermes {
			cerr = m.createHermes(ctx, agent, key, name, model, authToken)
		} else {
			cerr = m.create(ctx, agent, key, name)
		}
		if cerr != nil {
			return Target{}, cerr
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

	if err := m.waitHealthy(ctx, name, m.harnessPort(agent), m.startupDeadline(agent)); err != nil {
		// Don't leak a half-started container we just created.
		if createdNow {
			_ = m.docker.Stop(context.Background(), name, 5*time.Second)
			_ = m.docker.Remove(context.Background(), name)
		}
		return Target{}, fmt.Errorf("%s %s did not become ready: %w", agent.Harness, name, err)
	}

	return Target{Name: name, Endpoint: m.endpoint(agent, name), AuthToken: authToken, Harness: agent.Harness}, nil
}

func (m *Manager) create(ctx context.Context, agent config.Agent, key WorkspaceKey, name string) error {
	hostDir := config.UserWorkspace(m.cfg.HostDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	// picoclaw keeps its config/workspace under $HOME/.picoclaw; mount the
	// per-user dir there and set HOME so it works for a non-root user too (the
	// image's own /root is 0700 and unusable by a non-root uid).
	mountDest := m.cfg.PicoclawHome + "/.picoclaw"
	// The per-(user, agent) secret store is bind-mounted READ-ONLY into every
	// container of that pair (AC-10): the non-root agent can read the generic
	// sinks (.env / secrets.json / secrets/) but the kernel blocks any write.
	// Ensure the per-user store exists (it is the source of the effective merge)
	// and chown it so the non-root agent can read the files.
	storeContainer := config.StoreDir(m.cfg.ContainerDataRoot, key.UserAccID, key.Role)
	if err := os.MkdirAll(storeContainer, 0o700); err != nil {
		return fmt.Errorf("create secret store dir: %w", err)
	}
	if err := chownTree(storeContainer, m.cfg.PicoclawUser); err != nil {
		return fmt.Errorf("chown secret store dir: %w", err)
	}
	// Mount the EFFECTIVE secret view (shared cascade + user's own, user wins) at
	// .secrets, so shared secrets arrive as sink files like the user's own — live
	// via the mount, no env baking, no recreate needed (FR-5).
	if _, err := m.syncEffectiveSecrets(key); err != nil {
		return err
	}
	effHost := config.EffectiveSecretsDir(m.cfg.HostDataRoot, key.UserAccID, key.Role)
	secretsMount := effHost + ":" + mountDest + "/workspace/.secrets:ro"
	// Cascade the tenant- and subscription-scope shared files READ-ONLY into the
	// workspace (FR-4/NFR-3). Ensure the container-side dirs exist and are
	// readable by the non-root agent, mirroring the secret store above, so the
	// bind source is always present (they are normally pre-created on scaffold).
	sharedMounts := make([]string, 0, 4)
	for _, sm := range sharedFileBinds(m.cfg, key, mountDest) {
		if err := os.MkdirAll(sm.container, 0o700); err != nil {
			return fmt.Errorf("create shared files dir: %w", err)
		}
		if err := chownTree(sm.container, m.cfg.PicoclawUser); err != nil {
			return fmt.Errorf("chown shared files dir: %w", err)
		}
		sharedMounts = append(sharedMounts, sm.bind)
	}
	// Operator-managed content bind-mounted READ-ONLY into the workspace: the
	// shared-content skill (where shared files/secrets are + never copy secrets)
	// and the context-recovery note (how to read the durable transcript back).
	// Materialized once from the proxy's embedded copy; being a root-owned
	// read-only bind, the agent can neither alter them nor keep an edit past a
	// restart (the canonical copy is remounted every start).
	m.managedOnce.Do(func() {
		m.managedErr = materializeManagedContent(config.ManagedSkillsDir(m.cfg.ContainerDataRoot), m.cfg.PicoclawUser)
	})
	if m.managedErr != nil {
		return fmt.Errorf("materialize managed content: %w", m.managedErr)
	}
	managedBase := config.ManagedSkillsDir(m.cfg.HostDataRoot)
	managedSkillMount := filepath.Join(managedBase, managedSkillRel) +
		":" + mountDest + "/workspace/" + managedSkillRel + ":ro"
	managedMemoryMount := filepath.Join(managedBase, managedMemoryRel) +
		":" + mountDest + "/workspace/" + managedMemoryRel + ":ro"
	// Cascade admin shared skills: materialize the (tenant, subscription)
	// effective-skills dir and mount it whole READ-ONLY at picoclaw's global
	// skills root. New/edited/removed skills reach picoclaw on the next
	// stop/start (RestartScope) — no recreate, no transcript loss — mirroring
	// the effective-secrets discipline.
	if err := m.syncEffectiveSkills(key.TenantID, key.SubsAccID, key.Role); err != nil {
		return fmt.Errorf("sync effective skills: %w", err)
	}
	skillsMount := config.EffectiveSkillsDir(m.cfg.HostDataRoot, key.TenantID, key.SubsAccID, key.Role) +
		":" + mountDest + "/skills:ro"
	env := []string{
		"PICOCLAW_GATEWAY_HOST=0.0.0.0",
		"HOME=" + m.cfg.PicoclawHome,
	}
	spec := CreateSpec{
		Name:  name,
		Image: m.cfg.PicoclawImage,
		User:  m.cfg.PicoclawUser,
		Env:   env,
		Labels: map[string]string{
			LabelManaged:      "true",
			LabelAgent:        key.Role,
			LabelTenant:       key.TenantID,
			LabelSubscription: key.SubsAccID,
			LabelUser:         key.UserAccID,
			LabelMode:         string(agent.Mode),
		},
		Binds: append([]string{hostDir + ":" + mountDest, secretsMount},
			append(sharedMounts, managedSkillMount, managedMemoryMount, skillsMount)...),
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

func (m *Manager) waitHealthy(ctx context.Context, name string, port int, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		lastErr = m.health(attemptCtx, name, port)
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

// ScaffoldSubscription idempotently creates the subscription scaffold
// (SubscriptionRoot) for the /v1/accounts webhook and chowns it to picoclawUser
// so lazy per-user provisioning under it can write. It reports whether the
// scaffold was created now (true) or already existed (false).
func (m *Manager) ScaffoldSubscription(tenantID, subsAccID string) (bool, error) {
	dir := config.SubscriptionRoot(m.cfg.ContainerDataRoot, tenantID, subsAccID)
	_, statErr := os.Stat(dir)
	existed := statErr == nil
	// Create the subscription root plus the tenant-scope and subscription-scope
	// shared dirs, so the cascade bind sources always exist (empty) before any
	// container is created (design "Both created (empty) on scaffold").
	dirs := []string{
		dir,
		config.TenantSharedFilesDir(m.cfg.ContainerDataRoot, tenantID),
		config.TenantSharedSecretsDir(m.cfg.ContainerDataRoot, tenantID),
		config.SubscriptionSharedFilesDir(m.cfg.ContainerDataRoot, tenantID, subsAccID),
		config.SubscriptionSharedSecretsDir(m.cfg.ContainerDataRoot, tenantID, subsAccID),
		config.TenantSharedSkillsDir(m.cfg.ContainerDataRoot, tenantID),
		config.SubscriptionSharedSkillsDir(m.cfg.ContainerDataRoot, tenantID, subsAccID),
	}
	// The per-agent layer and the effective-skills view are keyed by agent, so
	// scaffold one of each per configured agent.
	for agentKey := range m.cfg.Agents {
		dirs = append(dirs,
			config.TenantAgentSharedFilesDir(m.cfg.ContainerDataRoot, tenantID, agentKey),
			config.TenantAgentSharedSecretsDir(m.cfg.ContainerDataRoot, tenantID, agentKey),
			config.TenantAgentSharedSkillsDir(m.cfg.ContainerDataRoot, tenantID, agentKey),
			config.SubscriptionAgentSharedFilesDir(m.cfg.ContainerDataRoot, tenantID, subsAccID, agentKey),
			config.SubscriptionAgentSharedSecretsDir(m.cfg.ContainerDataRoot, tenantID, subsAccID, agentKey),
			config.SubscriptionAgentSharedSkillsDir(m.cfg.ContainerDataRoot, tenantID, subsAccID, agentKey),
			config.EffectiveSkillsDir(m.cfg.ContainerDataRoot, tenantID, subsAccID, agentKey),
		)
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return false, err
		}
		if err := chownTree(d, m.cfg.PicoclawUser); err != nil {
			return false, err
		}
	}
	return !existed, nil
}

// SubscriptionScaffolded reports whether the subscription scaffold exists on
// disk (used to 409 an un-scaffolded chat and to annotate discovery).
func (m *Manager) SubscriptionScaffolded(tenantID, subsAccID string) bool {
	_, err := os.Stat(config.SubscriptionRoot(m.cfg.ContainerDataRoot, tenantID, subsAccID))
	return err == nil
}

// ArmIdle re-arms the scale-to-zero idle timer for a container after a turn
// completes. No-op for continuous-mode agents.
func (m *Manager) ArmIdle(agent config.Agent, key WorkspaceKey) {
	if agent.Mode != config.ModeScaleToZero {
		return
	}
	name := m.ContainerName(key)
	ks := m.keyState(name)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	m.armLocked(ks, name, agent.IdleTimeout.Std())
}

// RestartWorkspace stops and restarts the user's agent container so picoclaw
// re-reads its just-injected secrets (CTX-AC-04). It takes the per-container
// lock (serializing with ensure/turn/idle), disarms the idle timer, and — only
// if the container exists and is running — Stop+Start+waits-healthy, then
// re-arms the idle timer for scale-to-zero. When the container isn't running
// (scaled to zero / never created) it is a no-op: the next chat cold-starts with
// the new secret already applied (spec edge case).
func (m *Manager) RestartWorkspace(key WorkspaceKey) error {
	name := m.ContainerName(key)
	ks := m.keyState(name)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	m.disarmLocked(ks)

	ctx := context.Background()
	st, err := m.docker.Inspect(ctx, name)
	if err != nil {
		return err
	}
	if !st.Exists || !st.Running {
		return nil // scaled to zero / never created: next chat cold-starts with it
	}
	if err := m.docker.Stop(ctx, name, 10*time.Second); err != nil {
		return err
	}
	if err := m.docker.Start(ctx, name); err != nil {
		return err
	}
	port := m.cfg.PicoclawPort
	budget := m.cfg.StartupDeadline.Std()
	if agent, ok := m.cfg.Agents[key.Role]; ok {
		port = m.harnessPort(agent)
		budget = m.startupDeadline(agent)
	}
	if err := m.waitHealthy(ctx, name, port, budget); err != nil {
		return fmt.Errorf("container %s did not become ready after restart: %w", name, err)
	}
	if agent, ok := m.cfg.Agents[key.Role]; ok && agent.Mode == config.ModeScaleToZero {
		m.armLocked(ks, name, agent.IdleTimeout.Std())
	}
	m.logf("restarted container %s (secret injection)", name)
	return nil
}

// WriteSecret validates and persists one secret into the per-(user, agent)
// store under the chosen format, then — for native — merges it into the caller's
// current workspace .security.yml. agent supplies the template used to validate a
// native slot when the workspace has not been provisioned yet. Returns
// ErrInvalidSecretName / ErrUnknownNativeSlot for a bad name or slot (the handler
// maps these to 400).
func (m *Manager) WriteSecret(agent config.Agent, key WorkspaceKey, format, name, value string) error {
	if err := validateSecretName(name); err != nil {
		return err
	}
	storeDir := config.StoreDir(m.cfg.ContainerDataRoot, key.UserAccID, key.Role)
	secPath := m.workspaceSecurityPath(agent, key)
	if err := writeSecret(storeDir, secPath, format, name, value); err != nil {
		return err
	}
	if err := chownTree(storeDir, m.cfg.PicoclawUser); err != nil {
		return fmt.Errorf("chown secret store: %w", err)
	}
	if format == FormatNative {
		// Apply immediately to the current workspace when it exists; otherwise the
		// overlay is picked up at the next provision/ensure (design §6).
		if _, err := os.Stat(secPath); err == nil {
			if err := applyNativeSecrets(secPath, storeDir, m.cfg.PicoclawUser); err != nil {
				return err
			}
		}
	}
	// Refresh the mounted effective view so the new secret is picked up on the
	// caller's next stop/start (RestartWorkspace).
	_, err := m.syncEffectiveSecrets(key)
	return err
}

// ListSecrets returns the set secret names per format for the caller's store,
// parsed server-side. It NEVER returns a stored value.
func (m *Manager) ListSecrets(key WorkspaceKey) (SecretNames, error) {
	storeDir := config.StoreDir(m.cfg.ContainerDataRoot, key.UserAccID, key.Role)
	return listSecretNames(storeDir)
}

// DeleteSecret removes one secret from the store (and, for native, unsets the
// slot in the caller's current workspace .security.yml).
func (m *Manager) DeleteSecret(key WorkspaceKey, format, name string) error {
	if err := validateSecretName(name); err != nil {
		return err
	}
	storeDir := config.StoreDir(m.cfg.ContainerDataRoot, key.UserAccID, key.Role)
	secPath := filepath.Join(config.UserWorkspace(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID), ".security.yml")
	if err := deleteSecret(storeDir, secPath, m.cfg.PicoclawUser, format, name); err != nil {
		return err
	}
	if err := chownTree(storeDir, m.cfg.PicoclawUser); err != nil {
		return err
	}
	effDir, err := m.syncEffectiveSecrets(key)
	if err != nil {
		return err
	}
	// deleteSecret unsets the slot in .security.yml outright, but an admin scope
	// layer may still provide it — re-apply the remaining cascade so removing a
	// legacy personal entry never strips the admin's value.
	if format == FormatNative {
		return m.applyNativeToWorkspace(key, effDir)
	}
	return nil
}

// workspaceSecurityPath returns the .security.yml to validate/merge native
// secrets into: the caller's provisioned workspace when it exists, else the
// agent template (so a native slot can be validated before the first chat).
func (m *Manager) workspaceSecurityPath(agent config.Agent, key WorkspaceKey) string {
	ws := filepath.Join(config.UserWorkspace(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID), ".security.yml")
	if _, err := os.Stat(ws); err == nil {
		return ws
	}
	return filepath.Join(config.TemplatesDir(m.cfg.ContainerDataRoot, agent.Template), ".security.yml")
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
