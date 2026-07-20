// Package config loads crab-shell-proxy's agent catalog and runtime settings
// from a YAML file, resolving per-field secrets from environment variables.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from a YAML string like "15m".
type Duration time.Duration

// UnmarshalYAML parses a Go duration string (e.g. "35s", "15m").
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Mode is a container lifecycle policy (see design CTX-05).
type Mode string

const (
	// ModeScaleToZero stops a container after an idle period ("liga-desliga").
	ModeScaleToZero Mode = "scale-to-zero"
	// ModeContinuous keeps a container running (native connectors need it alive).
	ModeContinuous Mode = "continuous"
)

// secret is a value sourced either inline or from an environment variable
// (`{ env: "VAR" }`), mirroring mycelium's own field-level env resolver.
type secret struct {
	Value string
	Env   string
}

// UnmarshalYAML accepts either a bare string or a `{ env: NAME }` mapping.
func (s *secret) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		s.Value = node.Value
		return nil
	}
	var m struct {
		Env string `yaml:"env"`
	}
	if err := node.Decode(&m); err != nil {
		return err
	}
	s.Env = m.Env
	return nil
}

// resolve returns the concrete secret value, reading the environment when the
// field was declared as `{ env: NAME }`.
func (s secret) resolve() (string, error) {
	if s.Env != "" {
		v := os.Getenv(s.Env)
		if v == "" {
			return "", fmt.Errorf("environment variable %q is empty or unset", s.Env)
		}
		return v, nil
	}
	return s.Value, nil
}

// ModelConfig optionally pins the picoclaw LLM provider/model for an agent and
// sources the API key from the environment, so the key lives in env (not on
// disk / not in this file). When set, the proxy writes it into each user's
// picoclaw config/.security.yml at provisioning time.
type ModelConfig struct {
	Provider  string `yaml:"provider"`
	Name      string `yaml:"name"`      // must match a picoclaw model_list model_name
	APIKeyEnv string `yaml:"apiKeyEnv"` // env var holding the API key
	APIKey    string `yaml:"-"`         // resolved from APIKeyEnv at load
}

// Agent is one declared picoclaw agent (e.g. alpha, beta).
type Agent struct {
	// Key is the catalog key (map key), e.g. "alpha".
	Key string `yaml:"-"`
	// ServiceName matches the value mycelium injects as x-mycelium-service-name
	// (e.g. "picoclaw-alpha"). Requests are routed to an agent by this value.
	ServiceName string `yaml:"serviceName"`
	// Token is the bearer token mycelium injects for this agent's routes; the
	// proxy rejects any request whose Authorization does not match.
	Token secret `yaml:"token"`
	// Template is the sub-directory under <dataRoot>/templates holding the
	// config-only seed (config.json + .security.yml) for this agent.
	Template string `yaml:"template"`
	// Mode selects the lifecycle policy.
	Mode Mode `yaml:"mode"`
	// IdleTimeout is the scale-to-zero inactivity window (ignored when continuous).
	IdleTimeout Duration `yaml:"idleTimeout"`
	// Model optionally pins the picoclaw provider/model and injects the API key
	// from the environment into each user's config at provisioning time.
	Model *ModelConfig `yaml:"model"`
	// Models is the selectable model allowlist for admin-model-override (the
	// default Model above stays the fallback). Each entry's APIKey is resolved
	// from its APIKeyEnv exactly like Model, at Load.
	Models []*ModelConfig `yaml:"models"`

	// ResolvedToken is filled by Load from Token.
	ResolvedToken string `yaml:"-"`
}

// modelKey identifies a ModelConfig by its selectable identity.
type modelKey struct{ Provider, Name string }

// SelectableModels returns the agent's selectable model list: the default
// Model (if set) followed by Models, deduped by (provider, name) with the
// first occurrence winning, in stable declaration order.
func (a Agent) SelectableModels() []*ModelConfig {
	seen := map[modelKey]bool{}
	out := []*ModelConfig{}
	add := func(mc *ModelConfig) {
		if mc == nil {
			return
		}
		k := modelKey{mc.Provider, mc.Name}
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, mc)
	}
	add(a.Model)
	for _, mc := range a.Models {
		add(mc)
	}
	return out
}

// FindModel returns the selectable ModelConfig matching {provider, name}, or
// nil when none match.
func (a Agent) FindModel(provider, name string) *ModelConfig {
	for _, mc := range a.SelectableModels() {
		if mc.Provider == provider && mc.Name == name {
			return mc
		}
	}
	return nil
}

// Config is the full proxy configuration.
type Config struct {
	Listen string `yaml:"listen"`
	// HostDataRoot is the absolute path ON THE HOST of the per-agent data root;
	// it is used as the source of bind mounts handed to the Docker daemon.
	HostDataRoot string `yaml:"hostDataRoot"`
	// ContainerDataRoot is where that same directory is mounted INSIDE this
	// proxy, used to read history and write per-user config templates.
	ContainerDataRoot string `yaml:"containerDataRoot"`
	// Network is the docker network spawned containers join (compose-qualified).
	Network       string `yaml:"network"`
	PicoclawImage string `yaml:"picoclawImage"`
	PicoclawPort  int    `yaml:"picoclawPort"`
	// PicoclawUser is the "uid:gid" the spawned picoclaw containers run as.
	// Empty => root (the image default). Non-root requires relocating HOME
	// (PicoclawHome) because the image's /root is 0700.
	PicoclawUser string `yaml:"picoclawUser"`
	// PicoclawHome is the in-container HOME for spawned picoclaw; the per-user
	// data dir is mounted at <PicoclawHome>/.picoclaw and the config's workspace
	// path is aligned to it. Must be a dir the PicoclawUser can write.
	PicoclawHome    string   `yaml:"picoclawHome"`
	StartupDeadline Duration `yaml:"startupDeadline"`
	TurnTimeout     Duration `yaml:"turnTimeout"`
	// ContainerPrefix prefixes every managed container name (default "picoclaw").
	ContainerPrefix string `yaml:"containerPrefix"`
	// WebhookSecret authenticates POST /v1/accounts (the mycelium
	// subscriptionAccount.created webhook); env-resolvable like agent tokens so
	// it lives in the environment, never in this file.
	WebhookSecret secret           `yaml:"webhookSecret"`
	Agents        map[string]Agent `yaml:"agents"`

	// Media upload caps (media-upload feature). MediaMaxBytes bounds an
	// uploaded file; MediaAllowedExts is the lowercase extension allowlist.
	MediaMaxBytes    int64    `yaml:"mediaMaxBytes"`
	MediaAllowedExts []string `yaml:"mediaAllowedExts"`

	// ResolvedWebhookSecret is filled by Load from WebhookSecret.
	ResolvedWebhookSecret string `yaml:"-"`
}

// Load reads, validates, and env-resolves the config at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyEnvOverrides()
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	// Resolve secrets and stamp the map key onto each agent.
	for key, agent := range cfg.Agents {
		tok, err := agent.Token.resolve()
		if err != nil {
			return nil, fmt.Errorf("agent %q token: %w", key, err)
		}
		agent.Key = key
		agent.ResolvedToken = tok
		if agent.Model != nil && agent.Model.APIKeyEnv != "" {
			// Empty is allowed (e.g. structural tests) — picoclaw will surface an
			// auth error on the first model call rather than failing to boot.
			agent.Model.APIKey = os.Getenv(agent.Model.APIKeyEnv)
		}
		for _, mc := range agent.Models {
			if mc.APIKeyEnv != "" {
				mc.APIKey = os.Getenv(mc.APIKeyEnv)
			}
		}
		cfg.Agents[key] = agent
	}
	sec, err := cfg.WebhookSecret.resolve()
	if err != nil {
		return nil, fmt.Errorf("webhookSecret: %w", err)
	}
	cfg.ResolvedWebhookSecret = sec
	return &cfg, nil
}

// applyEnvOverrides lets deploy-specific fields be set from the environment,
// so the committed YAML stays machine-agnostic. The host data root in
// particular is the absolute HOST path (bind-mount source handed to the Docker
// daemon) and differs per machine.
func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("CRAB_HOST_DATA_ROOT"); v != "" {
		c.HostDataRoot = v
	}
	if v := os.Getenv("CRAB_CONTAINER_DATA_ROOT"); v != "" {
		c.ContainerDataRoot = v
	}
	if v := os.Getenv("CRAB_NETWORK"); v != "" {
		c.Network = v
	}
	if v := os.Getenv("CRAB_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("CRAB_PICOCLAW_USER"); v != "" {
		c.PicoclawUser = v
	}
	if v := os.Getenv("CRAB_PICOCLAW_HOME"); v != "" {
		c.PicoclawHome = v
	}
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.ContainerDataRoot == "" {
		c.ContainerDataRoot = "/data"
	}
	if c.PicoclawImage == "" {
		c.PicoclawImage = "docker.io/sipeed/picoclaw:latest"
	}
	if c.PicoclawPort == 0 {
		c.PicoclawPort = 18790
	}
	if c.StartupDeadline == 0 {
		c.StartupDeadline = Duration(35 * time.Second)
	}
	if c.TurnTimeout == 0 {
		c.TurnTimeout = Duration(120 * time.Second)
	}
	if c.ContainerPrefix == "" {
		c.ContainerPrefix = "picoclaw"
	}
	if c.PicoclawHome == "" {
		// Default to a non-root-writable HOME so the default posture (non-root
		// containers) works without the image's 0700 /root getting in the way.
		c.PicoclawHome = "/data"
	}
	if c.MediaMaxBytes == 0 {
		c.MediaMaxBytes = 10 << 20 // 10 MiB
	}
	if len(c.MediaAllowedExts) == 0 {
		c.MediaAllowedExts = []string{
			// images
			"png", "jpg", "jpeg", "webp", "gif",
			// documents (incl. MS Word + OpenDocument)
			"pdf", "txt", "md", "csv", "doc", "docx", "odt",
			// spreadsheets (MS Excel + OpenDocument)
			"xls", "xlsx", "ods",
			// presentations (MS PowerPoint + OpenDocument)
			"ppt", "pptx", "odp",
			// archives
			"zip", "tar", "gz", "tgz", "bz2", "xz", "7z", "rar",
		}
	}
}

func (c *Config) validate() error {
	if c.HostDataRoot == "" {
		return fmt.Errorf("hostDataRoot is required (host path used as bind-mount source)")
	}
	if c.Network == "" {
		return fmt.Errorf("network is required (docker network spawned containers join)")
	}
	if len(c.Agents) == 0 {
		return fmt.Errorf("at least one agent must be declared")
	}
	for key, agent := range c.Agents {
		if agent.ServiceName == "" {
			return fmt.Errorf("agent %q: serviceName is required", key)
		}
		if agent.Template == "" {
			return fmt.Errorf("agent %q: template is required", key)
		}
		switch agent.Mode {
		case ModeScaleToZero, ModeContinuous:
		default:
			return fmt.Errorf("agent %q: mode must be %q or %q, got %q",
				key, ModeScaleToZero, ModeContinuous, agent.Mode)
		}
		if agent.Mode == ModeScaleToZero && agent.IdleTimeout <= 0 {
			return fmt.Errorf("agent %q: idleTimeout must be > 0 for %s", key, ModeScaleToZero)
		}
		if agent.Model != nil {
			if agent.Model.Provider == "" || agent.Model.Name == "" {
				return fmt.Errorf("agent %q: model requires both provider and name", key)
			}
		}
		seen := map[modelKey]bool{}
		for _, mc := range agent.Models {
			if mc.Provider == "" || mc.Name == "" {
				return fmt.Errorf("agent %q: models entries require both provider and name", key)
			}
			k := modelKey{mc.Provider, mc.Name}
			if seen[k] {
				return fmt.Errorf("agent %q: duplicate model {provider: %q, name: %q} in models",
					key, mc.Provider, mc.Name)
			}
			seen[k] = true
		}
	}
	return nil
}

// The layout builders below are the single source of truth for the on-disk
// tenant→subscription→agent→user tree. Each is a free function taking the data
// root as its first argument so the same relative tree is built under both the
// host root (bind-mount source handed to Docker) and the container root (this
// proxy's view); only the prefix differs. Every dynamic segment passes through
// identity.SanitizeID before it reaches the filesystem or a container name.

// TemplatesDir is the config-only seed dir for an agent template, under
// <root>/templates/<template>.
func TemplatesDir(root, template string) string {
	return filepath.Join(root, "templates", template)
}

// SubscriptionRoot is the subscription scaffold the /v1/accounts webhook
// creates: <root>/tenants/<t>/subscriptions/<s>/agents. The lazy
// <role>/users/<u> leaves are created on first chat under it.
func SubscriptionRoot(root, tenantID, subsAccID string) string {
	return filepath.Join(root, "tenants", identity.SanitizeID(tenantID),
		"subscriptions", identity.SanitizeID(subsAccID), "agents")
}

// UserWorkspace is one user's fully isolated workspace under a subscription's
// agent: SubscriptionRoot/<role>/users/<u>.
func UserWorkspace(root, tenantID, subsAccID, role, userAccID string) string {
	return filepath.Join(SubscriptionRoot(root, tenantID, subsAccID),
		identity.SanitizeID(role), "users", identity.SanitizeID(userAccID))
}

// SessionsDir is the path to a user's picoclaw session transcripts (used by
// /v1/sessions/history), under UserWorkspace/workspace/sessions.
func SessionsDir(root, tenantID, subsAccID, role, userAccID string) string {
	return filepath.Join(UserWorkspace(root, tenantID, subsAccID, role, userAccID),
		"workspace", "sessions")
}

// UploadsDir is where user-uploaded media lands, inside the agent-readable
// workspace (UserWorkspace/workspace/uploads) so a vision model / reader skill
// can open it by the returned "uploads/<file>" path.
func UploadsDir(root, tenantID, subsAccID, role, userAccID string) string {
	return filepath.Join(UserWorkspace(root, tenantID, subsAccID, role, userAccID),
		"workspace", "uploads")
}

// TenantModelOverrideFile is the tenant-scope model override selection file
// (admin-model-override): <root>/tenants/<t>/shared/model.json.
func TenantModelOverrideFile(root, tenantID string) string {
	return filepath.Join(root, "tenants", identity.SanitizeID(tenantID), "shared", "model.json")
}

// SubscriptionModelOverrideFile is the subscription-scope model override
// selection file: <root>/tenants/<t>/subscriptions/<s>/shared/model.json.
func SubscriptionModelOverrideFile(root, tenantID, subsAccID string) string {
	return filepath.Join(root, "tenants", identity.SanitizeID(tenantID),
		"subscriptions", identity.SanitizeID(subsAccID), "shared", "model.json")
}

// UserModelOverrideFile is the per-user model override selection file, a
// dotfile beside .crab-owner.json inside the user's workspace so picoclaw
// ignores it: UserWorkspace/.crab-model.json.
func UserModelOverrideFile(root, tenantID, subsAccID, role, userAccID string) string {
	return filepath.Join(UserWorkspace(root, tenantID, subsAccID, role, userAccID), ".crab-model.json")
}

// TenantSharedFilesDir is the tenant-scope shared-files store, cascaded
// read-only into every user container under the tenant:
// <root>/tenants/<t>/shared/files.
func TenantSharedFilesDir(root, tenantID string) string {
	return filepath.Join(root, "tenants", identity.SanitizeID(tenantID), "shared", "files")
}

// TenantSharedSecretsDir is the tenant-scope shared-secret store (sink formats),
// cascaded as env into every user container under the tenant:
// <root>/tenants/<t>/shared/secrets.
func TenantSharedSecretsDir(root, tenantID string) string {
	return filepath.Join(root, "tenants", identity.SanitizeID(tenantID), "shared", "secrets")
}

// SubscriptionSharedFilesDir is the subscription-scope shared-files store,
// cascaded read-only into every user container under the subscription:
// <root>/tenants/<t>/subscriptions/<s>/shared/files.
func SubscriptionSharedFilesDir(root, tenantID, subsAccID string) string {
	return filepath.Join(root, "tenants", identity.SanitizeID(tenantID),
		"subscriptions", identity.SanitizeID(subsAccID), "shared", "files")
}

// SubscriptionSharedSecretsDir is the subscription-scope shared-secret store
// (sink formats), cascaded as env into every user container under the
// subscription: <root>/tenants/<t>/subscriptions/<s>/shared/secrets.
func SubscriptionSharedSecretsDir(root, tenantID, subsAccID string) string {
	return filepath.Join(root, "tenants", identity.SanitizeID(tenantID),
		"subscriptions", identity.SanitizeID(subsAccID), "shared", "secrets")
}

// TenantSharedSkillsDir is the tenant-scope shared-skills store, cascaded
// read-only into every user container under the tenant:
// <root>/tenants/<t>/shared/skills. Each skill is a directory <name>/ with a
// SKILL.md (+ optional supporting files).
func TenantSharedSkillsDir(root, tenantID string) string {
	return filepath.Join(root, "tenants", identity.SanitizeID(tenantID), "shared", "skills")
}

// SubscriptionSharedSkillsDir is the subscription-scope shared-skills store:
// <root>/tenants/<t>/subscriptions/<s>/shared/skills.
func SubscriptionSharedSkillsDir(root, tenantID, subsAccID string) string {
	return filepath.Join(root, "tenants", identity.SanitizeID(tenantID),
		"subscriptions", identity.SanitizeID(subsAccID), "shared", "skills")
}

// EffectiveSkillsDir is the per-(tenant, subscription) MERGED skills view
// bind-mounted read-only at the container's global skills root
// (<mountDest>/skills): subscription-scope skills override tenant-scope by name.
// Materialized whenever a shared skill changes so additions/edits/removals reach
// picoclaw live (via the mount) on the next stop/start, without a recreate:
// <root>/effective-skills/<t>/<s>.
func EffectiveSkillsDir(root, tenantID, subsAccID string) string {
	return filepath.Join(root, "effective-skills",
		identity.SanitizeID(tenantID), identity.SanitizeID(subsAccID))
}

// StoreDir is the per-(user, agent) secret store, kept OUTSIDE the
// tenant/subscription tree so the same secret reaches every workspace of that
// pair (CTX-AC-03): <root>/user-secrets/<u>/<role>. Keyed only by the user
// account id and the role (agent key), never the subscription.
func StoreDir(root, userAccID, role string) string {
	return filepath.Join(root, "user-secrets",
		identity.SanitizeID(userAccID), identity.SanitizeID(role))
}

// EffectiveSecretsDir is the per-(user, agent) MERGED secret view bind-mounted
// read-only at workspace/.secrets: tenant- and subscription-shared secrets
// (dotenv/json) cascaded with the user's own store on top (user wins). Keyed by
// user + role like StoreDir so the same merge reaches every workspace of the
// pair: <root>/effective-secrets/<u>/<role>. Rebuilt whenever a user or shared
// secret changes so shared secrets are delivered live (via the mount) without a
// container recreate.
func EffectiveSecretsDir(root, userAccID, role string) string {
	return filepath.Join(root, "effective-secrets",
		identity.SanitizeID(userAccID), identity.SanitizeID(role))
}

// ManagedSkillsDir is the operator-managed, read-only skills root the proxy
// materializes from its embedded copy and bind-mounts into each container's
// workspace skills dir. It lives outside the tenant tree (shared, static
// content) at <root>/managed-skills.
func ManagedSkillsDir(root string) string {
	return filepath.Join(root, "managed-skills")
}

// WorkspaceSeed is the allowlist of agent template workspace files copied into a
// fresh user workspace on first provision (recursive for directories). CRITICAL:
// sessions/ (conversation history), logs/, and .picoclaw.pid (runtime state) are
// NEVER copied — the isolation invariant that keeps the shared template's
// sessions out of every new user's container.
var WorkspaceSeed = []string{"AGENT.md", "SOUL.md", "USER.md", "memory/", "skills/"}

// AgentByServiceName returns the agent whose serviceName matches the value
// mycelium injected as x-mycelium-service-name, and whether it was found.
func (c *Config) AgentByServiceName(serviceName string) (Agent, bool) {
	for _, agent := range c.Agents {
		if agent.ServiceName == serviceName {
			return agent, true
		}
	}
	return Agent{}, false
}
