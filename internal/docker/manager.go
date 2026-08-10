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
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/restart"
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
	ImageID(ctx context.Context, image string) (string, error)
	Create(ctx context.Context, spec CreateSpec) (string, error)
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string, grace time.Duration) error
	Remove(ctx context.Context, name string) error
	List(ctx context.Context, label string) ([]ContainerSummary, error)
}

// HealthChecker reports whether the harness inside a container is health-ready
// (an HTTP GET returning 2xx on the harness port).
type HealthChecker func(ctx context.Context, name string, port int) error

// Target is the resolved connection info for a running per-user container.
type Target struct {
	Name string
	// Endpoint is the harness-specific address: ws://<name>:<port>/pico/ws for
	// picoclaw, the only harness.
	Endpoint string
	// AuthToken is the pico channel token.
	AuthToken string
	// Harness records which runtime this target runs, so a caller need not look the
	// agent up again to know what it is talking to.
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
	// restarts holds the restart notices and the per-workspace lastRestartAt
	// markers that make "does this workspace still need a bounce?" a derived
	// question (restart-control design §1).
	restarts *restart.Store

	mu   sync.Mutex
	keys map[string]*keyState

	// schedMu guards the armed scheduled-bounce timers, keyed by scope. Kept
	// separate from mu (which guards keys) because a scheduled bounce takes the
	// per-container locks while it runs.
	schedMu sync.Mutex
	sched   map[string]*time.Timer

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
	return &Manager{
		cfg: cfg, docker: dkr, health: health, reg: reg, logf: logf,
		restarts: restart.NewStore(cfg.ContainerDataRoot),
		keys:     map[string]*keyState{},
		sched:    map[string]*time.Timer{},
	}
}

// Restarts exposes the restart-notice store so the HTTP layer can read a
// member's status and raise notices without reaching through the Manager for
// every accessor.
func (m *Manager) Restarts() *restart.Store { return m.restarts }

// stampRestart records that a workspace has just applied everything pending. A
// failure here is logged, never propagated: a missing marker reads as "pending",
// which is the safe direction — a spurious banner, never a skipped restart.
func (m *Manager) stampRestart(key WorkspaceKey) {
	if err := m.restarts.Stamp(key.TenantID, key.SubsAccID, key.Role, key.UserAccID, time.Now().UTC()); err != nil {
		m.logf("restart marker for %s failed: %v", m.ContainerName(key), err)
	}
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
	// Provision the per-user data dir and obtain picoclaw's pico channel token,
	// persisted per user so a returning user reuses it.
	var authToken string
	var err error
	// Materialize the effective secret view BEFORE provisioning: it is the
	// bind-mount source, and resolveAndMaterialize reads the native overlay
	// out of it — native slots now arrive from the admin cascade, not only
	// from the user's own store.
	if _, syncErr := m.syncEffectiveSecrets(key); syncErr != nil {
		return Target{}, syncErr
	}
	// The template is the persona cascade's LAST layer, and the cascade is
	// resolved before provisioning — so a missing template has to self-heal
	// first. Left inside provision (where it used to live), a first-ever
	// provision resolved the cascade against a template that did not exist yet
	// and produced a workspace with no identity files at all.
	if tErr := ensurePicoclawTemplate(templateDir, m.cfg.PicoclawUser); tErr != nil {
		return Target{}, tErr
	}
	// Same discipline as the secrets above, and for two reasons: this is the
	// bind-mount source for the read-only identity files, and seedWorkspace
	// reads USER.md out of it (an operator's injection is what a first provision
	// starts from). Both need it materialized before provision runs.
	if syncErr := m.syncEffectivePersona(key, templateDir); syncErr != nil {
		return Target{}, syncErr
	}
	personaDir := config.EffectivePersonaDir(
		m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role)
	authToken, err = provision(userDir, templateDir, personaDir,
		config.SubscriptionAgentConfigOverlay(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role),
		m.cfg.PicoclawHome, m.cfg.PicoclawUser, key, ownerEmail)
	if err == nil {
		// Materialize AFTER seeding, so the template's (now empty) model_list
		// is replaced by the inventory's answer, and the native overlay lands
		// on top of THAT. A workspace with no resolvable model fails here,
		// before any container exists.
		err = m.resolveAndMaterialize(key, userDir)
	}
	if err == nil {
		// The native memory-graph MCP server. Beside resolveAndMaterialize rather
		// than inside it (model resolution and MCP injection are unrelated), and
		// beside it rather than in alignWorkspace, which only ever runs on a
		// first-ever seed — see applyMCPServer's own comment.
		err = m.applyMemoryGraphMCP(key, userDir)
	}
	if err == nil {
		// AFTER syncEffectivePersona (it reads the resolved identity files) and
		// after materialization (which projects the matching agents.list). The
		// workspaces must exist and be chowned before the container starts, or
		// picoclaw creates them itself as root and the non-root agent cannot
		// write its own project.
		err = m.syncProjectWorkspaces(key, userDir)
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
		if cerr := m.create(ctx, agent, key, name); cerr != nil {
			return Target{}, cerr
		}
		createdNow = true
		if err := m.docker.Start(ctx, name); err != nil {
			return Target{}, err
		}
	// An existing container whose persona mounts no longer match the effective set
	// has to be REBUILT, not restarted. Bind sets are fixed at create time, so a
	// container created before a persona file existed — every container created by
	// an image predating the feature included — has no mount for it, and an admin's
	// save can never arrive however many times it is bounced. This is the one place
	// the check belongs: syncEffectivePersona ran a few lines above and returned an
	// error if it failed, so the effective dir it compares against is current.
	//
	// The project .secrets mounts drift for the same structural reason as the
	// persona ones: a project created after this container was built has no mount
	// for its workspace, so its agent runs with no credentials at all. That
	// presents as the model or the tools failing, never as a missing bind, so it
	// has to be caught here rather than diagnosed later.
	// The IMAGE drifts for a third reason, and it is the one that is invisible: a
	// container reuses whatever image it was created from for as long as it exists,
	// so rebuilding the harness image and redeploying the stack changes nothing —
	// the agent containers are not compose's, they are this manager's, and they
	// outlive a redeploy. That cost an afternoon on 2026-08-10, chasing a picoclaw
	// patch that was in the image and not in the running binary.
	case personaBindDrift(m.cfg, key, m.picoclawMountDest(), st.Binds) ||
		m.projectBindDriftFor(key, st.Binds) ||
		m.imageDrift(ctx, st):
		m.logf("container %s: persona/project mounts or harness image stale, recreating (identity, project and image changes cannot reach it otherwise)", name)
		if st.Running {
			if err := m.docker.Stop(ctx, name, 10*time.Second); err != nil {
				return Target{}, err
			}
		}
		if err := m.docker.Remove(ctx, name); err != nil {
			return Target{}, err
		}
		if cerr := m.create(ctx, agent, key, name); cerr != nil {
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

	if createdNow {
		// A container created just now has, by definition, already applied every
		// pending change — everything above (effective secrets, materialize,
		// native overlay) ran before it started. Stamping here is what makes a
		// scaled-to-zero workspace clear its own notice on cold start, with no
		// fan-out at admin-action time (restart-control FR-3.2).
		m.stampRestart(key)
	}

	return Target{Name: name, Endpoint: m.endpoint(agent, name), AuthToken: authToken, Harness: agent.Harness}, nil
}

// ensureManagedContent materializes the proxy's embedded operator-managed tree
// once per process. Called from `create` (the bind source must exist before a
// container references it) and from Reconcile at startup (so a deploy that changes
// the guidance reaches containers that already exist — the skill dir is a directory
// bind, so they read the host copy live).
func (m *Manager) ensureManagedContent() error {
	m.managedOnce.Do(func() {
		m.managedErr = materializeManagedContent(config.ManagedSkillsDir(m.cfg.ContainerDataRoot), m.cfg.PicoclawUser)
	})
	if m.managedErr != nil {
		return fmt.Errorf("materialize managed content: %w", m.managedErr)
	}
	return nil
}

// picoclawMountDest is where the per-user dir is mounted inside a picoclaw
// container. Named because two places need the same answer: `create`, which builds
// the binds, and the drift check, which reads them back.
func (m *Manager) picoclawMountDest() string {
	return m.cfg.PicoclawHome + "/.picoclaw"
}

func (m *Manager) create(ctx context.Context, agent config.Agent, key WorkspaceKey, name string) error {
	hostDir := config.UserWorkspace(m.cfg.HostDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	// picoclaw keeps its config/workspace under $HOME/.picoclaw; mount the
	// per-user dir there and set HOME so it works for a non-root user too (the
	// image's own /root is 0700 and unusable by a non-root uid).
	mountDest := m.picoclawMountDest()
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
	secretsMount := effHost + ":" + mountDest + "/" + config.MainWorkspace + "/.secrets:ro"
	// One more .secrets mount per project, same source. A project inherits its
	// parent's credentials, and restrict_to_workspace means the parent's mount is
	// unreachable from a project's own workspace.
	projectList, err := m.projectStore(key).List()
	if err != nil {
		return fmt.Errorf("read projects: %w", err)
	}
	projectSecrets := projectSecretsBinds(effHost, mountDest, projectList)
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
	if err := m.ensureManagedContent(); err != nil {
		return err
	}
	managedBase := config.ManagedSkillsDir(m.cfg.HostDataRoot)
	// Built by a pure helper so the list is assertable without a container: the memory
	// routing note joins it only when the memory graph is switched on.
	managedMounts := managedContentBinds(
		managedBase, mountDest, m.cfg.ResolvedMCPTokenSecret != "")
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
		// The persona binds come LAST, and per file: each shadows one path inside
		// the workspace bind above it, which is exactly how AGENT.md, SOUL.md and
		// HEARTBEAT.md become unwritable. Only files the effective dir actually
		// holds are bound — a bind with a missing source makes Docker invent an
		// empty directory at the destination (personaBinds).
		Binds: append(
			append([]string{hostDir + ":" + mountDest, secretsMount},
				append(append(append(sharedMounts, managedMounts...), skillsMount), projectSecrets...)...),
			personaBindStrings(m.cfg, key, mountDest)...),
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
		// Scaled to zero / never created: the next chat cold-starts with everything
		// applied, so the pending notice is genuinely resolved — stamp the marker
		// (restart-control FR-1.4) rather than leaving a banner the member can
		// never clear by pressing the button.
		m.stampRestart(key)
		return nil
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
	m.stampRestart(key)
	m.logf("restarted container %s (secret injection)", name)
	return nil
}

// ArmScheduledBounce schedules a scope bounce for `at`, replacing any timer
// already armed for the same scope — re-scheduling never stacks two
// (restart-control FR-6.3). An `at` already in the past fires immediately, which
// is what a schedule that came due while the proxy was down must do (FR-6.2).
func (m *Manager) ArmScheduledBounce(scope Scope, at time.Time) {
	key := scheduleKey(scope)
	d := time.Until(at)
	if d < 0 {
		d = 0
	}
	m.schedMu.Lock()
	defer m.schedMu.Unlock()
	if t, ok := m.sched[key]; ok {
		t.Stop()
	}
	m.sched[key] = time.AfterFunc(d, func() {
		m.schedMu.Lock()
		delete(m.sched, key)
		m.schedMu.Unlock()

		if err := m.BounceScope(scope); err != nil {
			m.logf("scheduled bounce %s failed: %v", key, err)
		}
		// Clear only ScheduledAt: NoticeAt stays so a container that was down at
		// the appointed time still shows the notice until its cold start stamps
		// its marker (FR-6.1).
		if err := m.restarts.ClearSchedule(scope.TenantID, scope.SubsAccID, scope.AgentKey); err != nil {
			m.logf("scheduled bounce %s: clear schedule failed: %v", key, err)
		}
	})
	m.logf("scheduled bounce for %s in %s", key, d)
}

// CancelScheduledBounce disarms a pending scheduled bounce (an admin withdrawing
// the notice). Unknown scopes are a no-op.
func (m *Manager) CancelScheduledBounce(scope Scope) {
	key := scheduleKey(scope)
	m.schedMu.Lock()
	defer m.schedMu.Unlock()
	if t, ok := m.sched[key]; ok {
		t.Stop()
		delete(m.sched, key)
	}
}

// RearmSchedules re-arms every schedule persisted on disk. Called at the end of
// Reconcile so a proxy restart never silently drops an admin's scheduled window
// (FR-6.2).
func (m *Manager) RearmSchedules() {
	refs, err := m.restarts.Schedules()
	if err != nil {
		m.logf("re-arm schedules: %v", err)
		return
	}
	for _, ref := range refs {
		kind := ScopeTenant
		if ref.SubsAccID != "" {
			kind = ScopeSubscription
		}
		m.ArmScheduledBounce(Scope{
			Kind: kind, TenantID: ref.TenantID, SubsAccID: ref.SubsAccID, AgentKey: ref.AgentKey,
		}, ref.At)
	}
}

func scheduleKey(scope Scope) string {
	return scope.TenantID + "|" + scope.SubsAccID + "|" + scope.AgentKey
}

// WriteSecret validates and persists one secret into the per-(user, agent)
// store under the chosen format, then — for native — merges it into the caller's
// current workspace .security.yml. A native model_list slot is validated
// against the model inventory (m.reg), not against agent's template; agent only
// supplies the .security.yml to merge into when the workspace has not been
// provisioned yet. Returns ErrInvalidSecretName / ErrUnknownNativeSlot for a bad
// name or slot (the handler maps these to 400).
func (m *Manager) WriteSecret(agent config.Agent, key WorkspaceKey, format, name, value string) error {
	if err := validateSecretName(name); err != nil {
		return err
	}
	storeDir, err := m.storeDirFor(key)
	if err != nil {
		return err
	}
	secPath := m.workspaceSecurityPath(agent, key)
	if err := writeSecret(m.reg, storeDir, secPath, format, name, value); err != nil {
		return err
	}
	if err := chownTree(storeDir, m.cfg.PicoclawUser); err != nil {
		return fmt.Errorf("chown secret store: %w", err)
	}
	if format == FormatNative {
		// Apply immediately to the current workspace when it exists; otherwise the
		// overlay is picked up at the next provision/ensure (design §6).
		if _, err := os.Stat(secPath); err == nil {
			if err := applyNativeSecrets(secPath, storeDir, m.cfg.PicoclawUser, m.logf); err != nil {
				return err
			}
		}
	}
	// Refresh the mounted effective view so the new secret is picked up on the
	// caller's next stop/start (RestartWorkspace).
	_, err = m.syncEffectiveSecrets(key)
	return err
}

// storeDirFor builds the caller's secret store and refuses one that has somehow
// landed outside the data root. Every path under internal/docker that a request
// can influence is built from an id that identity.SanitizeID has already reduced
// to a single safe segment, so this cannot fire today — which is the point of
// putting it in ONE builder rather than at each of the four call sites: the
// check now costs nothing to keep and cannot be forgotten by the fifth.
func (m *Manager) storeDirFor(key WorkspaceKey) (string, error) {
	return underRoot(m.cfg.ContainerDataRoot,
		config.StoreDir(m.cfg.ContainerDataRoot, key.UserAccID, key.Role))
}

// ListSecrets returns the set secret names per format for the caller's store,
// parsed server-side. It NEVER returns a stored value.
func (m *Manager) ListSecrets(key WorkspaceKey) (SecretNames, error) {
	storeDir, err := m.storeDirFor(key)
	if err != nil {
		return SecretNames{}, err
	}
	return listSecretNames(storeDir)
}

// DeleteSecret removes one secret from the store (and, for native, unsets the
// slot in the caller's current workspace .security.yml).
func (m *Manager) DeleteSecret(key WorkspaceKey, format, name string) error {
	if err := validateSecretName(name); err != nil {
		return err
	}
	storeDir, err := m.storeDirFor(key)
	if err != nil {
		return err
	}
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

// workspaceSecurityPath returns the .security.yml a native secret write merges
// into (secPath, for the immediate-apply check): the caller's provisioned
// workspace when it exists, else the agent template. Model-name validation no
// longer reads this file — that's the inventory now — but the merge target
// still needs somewhere to write before the first chat.
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
