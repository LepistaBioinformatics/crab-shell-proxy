// Package config loads crab-shell-proxy's agent catalog and runtime settings
// from a YAML file, resolving per-field secrets from environment variables.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// Harness kinds select the agent runtime an agent orchestrates.
const (
	// HarnessPicoclaw is the default: a picoclaw container spoken to over the
	// Pico Protocol WebSocket.
	HarnessPicoclaw = "picoclaw"
	// HarnessGanglion is crab-ganglion-harness: this project's own runtime,
	// spoken to over native HTTP with SSE. It is an ALTERNATIVE to picoclaw,
	// not a replacement -- picoclaw stays the default until the exit criteria
	// in .specs/features/crab-ganglion-harness/spec.md are met.
	HarnessGanglion = "ganglion"
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
	Name      string `yaml:"name"`      // a picoclaw model_list model_name
	APIKeyEnv string `yaml:"apiKeyEnv"` // env var (in THIS proxy) holding the API key
	APIKey    string `yaml:"-"`         // resolved from APIKeyEnv at load
	// BaseURL optionally names the provider endpoint. Picoclaw does not read it —
	// its only consumer is the boot migration, which imports it as the model
	// registry's APIBase (migrate_models.go). Empty for every shipped agent, and
	// the migration falls back to the template's model_list definition then.
	BaseURL string `yaml:"baseUrl"`
}

// Agent is one declared picoclaw agent (e.g. alpha, beta).
type Agent struct {
	// Key is the catalog key (map key), e.g. "alpha".
	Key string `yaml:"-"`
	// Harness selects the agent runtime kind. HarnessPicoclaw is the only accepted
	// value; empty defaults to it at Load, so existing configs are unchanged. The
	// field is retained because the admin API publishes it and clients branch on
	// it to decide which per-agent surfaces an agent offers.
	Harness string `yaml:"harness"`
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
	// StartupDeadline optionally overrides the global StartupDeadline for this
	// agent's cold-start health-wait, for an agent whose image takes unusually long
	// to serve its port. Safe to raise well past mycelium's 60s gatewayTimeout
	// because chat is streamed (the 200 is flushed before the cold start).
	// 0 => use the global.
	StartupDeadline Duration `yaml:"startupDeadline"`
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

// ganglionUnprovisioned reports why this environment cannot run the agent, or
// "" when it can. Only ganglion agents are subject to it: a picoclaw agent's
// key is written into a per-user .security.yml at provisioning time and an
// empty one surfaces as an auth error on the first model call, which is the
// behaviour every existing deployment already depends on.
func ganglionUnprovisioned(c Config, a Agent) string {
	if a.Harness != HarnessGanglion {
		return ""
	}
	if c.GanglionImage == "" {
		return "ganglionImage (or CRAB_GANGLION_IMAGE) is unset; it has no default on purpose -- " +
			"set it to an immutable reference, not a moving tag"
	}
	if a.Model != nil && a.Model.APIKeyEnv != "" && a.Model.APIKey == "" {
		// Named, not described. An operator reading "an API key is unset" for
		// an agent they configured has a 404 and nothing to chase; the whole
		// point of disabling instead of exiting is that the log says which
		// variable to set.
		return a.Model.APIKeyEnv + " is unset (the agent's model apiKeyEnv)"
	}
	return ""
}

// DisabledAgent records an agent that was declared but removed at load, and
// why. Reported rather than silently dropped: "that agent does not exist" is a
// legible failure only if something, somewhere, says it was disabled and names
// the missing setting.
type DisabledAgent struct {
	Key    string
	Reason string
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
	PicoclawHome string `yaml:"picoclawHome"`

	// GanglionImage is the crab-ganglion-harness image for agents whose harness
	// is HarnessGanglion.
	//
	// It has NO default, and that is deliberate. picoclawImage defaults to a
	// moving tag, and a moving tag is exactly what left the Dokploy host running
	// a three-week-old binary with two of four patches missing, silently: the
	// harness image is not a compose service, so a redeploy never pulls it, and
	// EnsureImage only pulls what is absent. FR-19 requires an immutable
	// reference, so this must be set explicitly -- ideally to a digest or a
	// per-commit tag.
	GanglionImage string `yaml:"ganglionImage"`
	// GanglionPort is where the harness serves HTTP+SSE inside its container.
	GanglionPort int `yaml:"ganglionPort"`

	// DisabledAgents lists agents removed at Load because this environment

	// GanglionOTLPEndpoint is the collector a ganglion container exports to
	// (FR-10). Empty disables export inside the harness rather than making it
	// log a failed request per turn.
	GanglionOTLPEndpoint string `yaml:"ganglionOtlpEndpoint"`

	// GanglionKeyPassphrase and GanglionKeyFile are the two factors that let a
	// harness resolve an enc:// credential. This proxy never decrypts anything
	// -- it forwards the ciphertext verbatim, because an agent's apiKeyEnv is
	// an opaque string to it -- so these exist only to be handed on.
	//
	// They are deliberately of DIFFERENT KINDS: one arrives as environment,
	// the other as a file bound read-only into the container. Both as
	// environment would mean one `docker inspect` yields the plaintext, and
	// the encryption would be decoration.
	GanglionKeyPassphrase string `yaml:"-"`
	// GanglionKeyFile is a path ON THE HOST. Empty means enc:// values are not
	// in use, and nothing is bound.
	GanglionKeyFile string `yaml:"ganglionKeyFile"`
	// cannot provision them, in key order. Filled by Load, never by YAML.
	DisabledAgents  []DisabledAgent `yaml:"-"`
	StartupDeadline Duration        `yaml:"startupDeadline"`
	// TurnIdleTimeout is how long the harness may stay SILENT before its turn is
	// declared dead. It is not a cap on how long a turn may take: an agentic turn
	// legitimately runs for many minutes while narrating its work, and the total
	// bound belongs to the HTTP layer (httpapi's turnCtx), which has one.
	//
	// It was `turnTimeout` and it did cap total duration, which cut long turns
	// mid-work while picoclaw carried on and persisted the answer -- the "it froze
	// but the reply is there after a reload" bug. Load rejects the old key rather
	// than ignoring it, because the two are not interchangeable.
	TurnIdleTimeout Duration `yaml:"turnIdleTimeout"`
	// ContainerPrefix prefixes every managed container name (default "crabshell").
	// Harness-agnostic: a container's harness is recorded in its labels/config,
	// not its name (the name is <prefix>-<role>-<hash>).
	ContainerPrefix string `yaml:"containerPrefix"`
	// WebhookSecret authenticates POST /v1/accounts (the mycelium
	// subscriptionAccount.created webhook); env-resolvable like agent tokens so
	// it lives in the environment, never in this file.
	WebhookSecret secret           `yaml:"webhookSecret"`
	Agents        map[string]Agent `yaml:"agents"`

	// MCPTokenSecret signs the bearer token a spawned picoclaw container presents
	// to the proxy's native memory-graph MCP endpoint (POST /v1/mcp). The token
	// carries the workspace scope and a MAC over it, so nothing is stored and
	// rotating this value revokes every issued token at once.
	//
	// Env-resolvable like webhookSecret, but UNSET IS NOT FATAL: an empty secret
	// disables the memory graph (the route is not registered and no MCP block is
	// written into any workspace) and the rest of the proxy behaves exactly as it
	// did before the feature existed. A deployment that forgot the secret must get
	// no memory rather than an unauthenticated endpoint on the container network.
	MCPTokenSecret secret `yaml:"mcpTokenSecret"`
	// MCPBaseURL is the origin a SPAWNED CONTAINER uses to reach this proxy; it
	// becomes the `url` of the injected MCP server. The proxy cannot infer the name
	// it is reachable by on the container network, so this is configuration.
	MCPBaseURL string `yaml:"mcpBaseURL"`

	// TelemetryToken authorizes GET /v1/instances, and nothing else. It exists
	// because that route is the first one here that is neither a member's nor an
	// agent's: a watcher asks it which workspaces exist, across every agent.
	//
	// It is NOT an agent token, deliberately. resolveAgent's bearer check is what
	// stops a caller who reached this proxy directly on the container network from
	// asserting whatever accId it likes — the mycelium profile header is decoded,
	// never verified (identity.SDKResolver.Resolve). So an agent token is the gate
	// on chatting AS ANY MEMBER, and a telemetry component must not hold one.
	//
	// Unset is not fatal and follows mcpTokenSecret's rule: an empty value means
	// the route is NOT REGISTERED. Not registered-and-401 — absent. A deployment
	// that has not opted in grows no new surface, and a misconfiguration cannot
	// leave the deployment's whole tenant topology readable without a credential.
	TelemetryToken secret `yaml:"telemetryToken"`

	// MediaMaxBytes bounds an uploaded file (media-upload feature). It is the
	// only thing an upload is checked against: the extension allowlist that used
	// to sit beside it was removed, because a member's file format is not
	// something this proxy has any way to be right about — see
	// .specs/quick/002-drop-media-ext-allowlist.
	MediaMaxBytes int64 `yaml:"mediaMaxBytes"`

	// ResolvedWebhookSecret is filled by Load from WebhookSecret.
	ResolvedWebhookSecret string `yaml:"-"`

	// ResolvedMCPTokenSecret is filled by Load from MCPTokenSecret. Empty means the
	// memory graph is disabled; see MCPTokenSecret.
	ResolvedMCPTokenSecret string `yaml:"-"`

	// ResolvedTelemetryToken is filled by Load from TelemetryToken. Empty means
	// GET /v1/instances is not registered; see TelemetryToken.
	ResolvedTelemetryToken string `yaml:"-"`
}

// Load reads, validates, and env-resolves the config at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	// yaml.Unmarshal ignores unknown keys, so a config still carrying the
	// pre-rename `turnTimeout` would load clean and silently run on the DEFAULT
	// idle window instead of the value written here. The two keys do not mean the
	// same thing (total cap vs. tolerated silence), so this refuses rather than
	// guesses.
	var legacy struct {
		TurnTimeout *Duration `yaml:"turnTimeout"`
	}
	if err := yaml.Unmarshal(raw, &legacy); err == nil && legacy.TurnTimeout != nil {
		return nil, fmt.Errorf(
			"config uses removed key `turnTimeout`: it capped a turn's TOTAL duration and cut long "+
				"turns mid-work. Rename it to `turnIdleTimeout` (%s), which bounds how long the harness "+
				"may stay silent instead", legacy.TurnTimeout.Std())
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
		agent.Key = key
		if agent.Harness == "" {
			agent.Harness = HarnessPicoclaw
		}
		tok, err := agent.Token.resolve()
		if err != nil {
			// For picoclaw this stays fatal: every deployment that exists today
			// declares picoclaw agents it is provisioned for, and silently
			// dropping one would remove a member's access with no boot-time
			// signal beyond a log line nobody reads until they are locked out.
			if agent.Harness != HarnessGanglion {
				return nil, fmt.Errorf("agent %q token: %w", key, err)
			}
			// For ganglion it disables, like a missing image or provider key.
			// This is the THIRD env var whose absence used to take the whole
			// proxy down for one unprovisioned agent; the first two were fixed
			// one at a time, which is how the third survived. All of them now
			// funnel into DisabledAgents.
			cfg.DisabledAgents = append(cfg.DisabledAgents, DisabledAgent{
				Key:    key,
				Reason: err.Error(),
			})
			delete(cfg.Agents, key)
			continue
		}
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
		// FR-18. A ganglion agent whose provider key is not provisioned in THIS
		// environment removes itself instead of taking the proxy down with it.
		//
		// The mechanism existed for the withdrawn Hermes harness and died with
		// it; multi-harness-support/implementation-notes.md §10 recommends
		// bringing it back, and a second harness is when it starts mattering
		// again. The value is that one config can describe several deployments:
		// a shared config declaring a ganglion agent reaches a host with no key
		// for it and degrades to "that agent does not exist" -- a 404 on that
		// agent's routes -- instead of "the proxy will not boot", which takes
		// every other agent down too.
		//
		// Picoclaw agents are deliberately NOT subject to this: their key is
		// written into a per-user .security.yml at provisioning time and an
		// empty one surfaces as an auth error on the first model call, which is
		// the behaviour every existing deployment already depends on.
		if reason := ganglionUnprovisioned(cfg, agent); reason != "" {
			cfg.DisabledAgents = append(cfg.DisabledAgents, DisabledAgent{Key: key, Reason: reason})
			delete(cfg.Agents, key)
			continue
		}
		cfg.Agents[key] = agent
	}
	sort.Slice(cfg.DisabledAgents, func(i, j int) bool {
		return cfg.DisabledAgents[i].Key < cfg.DisabledAgents[j].Key
	})
	sec, err := cfg.WebhookSecret.resolve()
	if err != nil {
		return nil, fmt.Errorf("webhookSecret: %w", err)
	}
	cfg.ResolvedWebhookSecret = sec
	// Deliberately NOT propagated like webhookSecret's error: an unset
	// CRAB_MCP_TOKEN_SECRET disables the memory graph, it does not stop the proxy
	// from booting. resolve() reports an unset env var as an error, which here means
	// "not configured" — the zero value is the correct outcome, and the callers that
	// care (the /v1/mcp registration and the config writer) both key off empty.
	if mcpSec, mcpErr := cfg.MCPTokenSecret.resolve(); mcpErr == nil {
		cfg.ResolvedMCPTokenSecret = mcpSec
	}
	// Same rule, same reason: unset is "not configured", which here means the
	// instances route is never registered. See TelemetryToken.
	if telSec, telErr := cfg.TelemetryToken.resolve(); telErr == nil {
		cfg.ResolvedTelemetryToken = telSec
	}
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
	// CRAB_PICOCLAW_IMAGE exists so the harness image can be swapped without
	// rebuilding this one — config.yaml is COPIED into the proxy image, so it is
	// otherwise a rebuild to change. That matters while the stack runs a locally
	// patched picoclaw (agent-projects needs dispatch-selector globs): rolling
	// forward to a stock upstream release, or back off the patch, should be an
	// edit to compose, not a build.
	if v := os.Getenv("CRAB_PICOCLAW_IMAGE"); v != "" {
		c.PicoclawImage = v
	}
	if v := os.Getenv("CRAB_PICOCLAW_USER"); v != "" {
		c.PicoclawUser = v
	}
	if v := os.Getenv("CRAB_PICOCLAW_HOME"); v != "" {
		c.PicoclawHome = v
	}
	if v := os.Getenv("CRAB_MCP_BASE_URL"); v != "" {
		c.MCPBaseURL = v
	}
	if v := os.Getenv("CRAB_GANGLION_IMAGE"); v != "" {
		c.GanglionImage = v
	}
	if v := os.Getenv("GANGLION_OTLP_ENDPOINT"); v != "" {
		c.GanglionOTLPEndpoint = v
	}
	// Passphrase from the environment only -- never from config.yaml, which is
	// in git.
	if v := os.Getenv("CRAB_GANGLION_KEY_PASSPHRASE"); v != "" {
		c.GanglionKeyPassphrase = v
	}
	if v := os.Getenv("CRAB_GANGLION_KEY_FILE"); v != "" {
		c.GanglionKeyFile = v
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
	if c.MCPBaseURL == "" {
		// The compose service name, which is how a spawned container on zombie_net
		// reaches this proxy in every environment this repo ships.
		c.MCPBaseURL = "http://crab-shell-proxy:8080"
	}
	if c.PicoclawPort == 0 {
		c.PicoclawPort = 18790
	}
	if c.GanglionPort == 0 {
		c.GanglionPort = 18800
	}
	if c.StartupDeadline == 0 {
		c.StartupDeadline = Duration(35 * time.Second)
	}
	if c.TurnIdleTimeout == 0 {
		c.TurnIdleTimeout = Duration(120 * time.Second)
	}
	if c.ContainerPrefix == "" {
		c.ContainerPrefix = "crabshell"
	}
	if c.PicoclawHome == "" {
		// Default to a non-root-writable HOME so the default posture (non-root
		// containers) works without the image's 0700 /root getting in the way.
		c.PicoclawHome = "/data"
	}
	if c.MediaMaxBytes == 0 {
		c.MediaMaxBytes = 10 << 20 // 10 MiB
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
		// An unknown harness fails the load rather than defaulting: a stale config
		// naming a runtime this proxy no longer orchestrates would otherwise hand a
		// user a picoclaw container under a role provisioned for something else.
		switch agent.Harness {
		case "", HarnessPicoclaw:
		case HarnessGanglion:
			// A missing image does NOT fail the load. It disables the agent, the
			// same way a missing provider key does -- see the DisabledAgents
			// block below.
			//
			// This was a hard error until it was tried on a real deployment: one
			// test agent with no image put the proxy in a crash loop and took
			// alpha and beta down with it. FR-19's intent is to fail LOUDLY, not
			// to fail FATALLY, and "agent gamma disabled: CRAB_GANGLION_IMAGE is
			// unset" in the boot log is loud. Refusing to serve every other agent
			// is not a louder version of that -- it is a different, worse
			// failure, and it is exactly what FR-18 exists to prevent.
			//
			// Nothing is silently downgraded: the agent's routes answer 404 and
			// the reason names the variable. The one thing this must never do is
			// invent a default image, because a moving tag left this stack
			// running a three-week-old binary once already.

		default:
			return fmt.Errorf("agent %q: harness must be %q or %q (or omitted), got %q",
				key, HarnessPicoclaw, HarnessGanglion, agent.Harness)
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

// MainWorkspace is the workspace segment of the agent every user gets by
// default — the one picoclaw's agents.defaults.workspace points at.
const MainWorkspace = "workspace"

// ProjectWorkspace is the workspace segment of one project's agent, a SIBLING of
// MainWorkspace rather than a child of it.
//
// The name is not a choice: picoclaw derives a named agent's workspace as
// <defaults.workspace>/../workspace-<id> when the agent config does not set one
// (pkg/agent/instance.go, resolveAgentWorkspace). Writing the path explicitly
// into agents.list keeps the proxy and picoclaw agreeing, but the SHAPE has to
// match, because the agent's own tooling resolves it independently.
//
// It lands inside the per-user dir this proxy mounts at <home>/.picoclaw, so a
// project's files persist in the same volume as everything else the user owns.
func ProjectWorkspace(projectID string) string {
	return "workspace-" + identity.SanitizeID(projectID)
}

// GanglionProjectWorkspace is the same idea for a ganglion agent, and a
// DIFFERENT SHAPE, because the two harnesses reach a project by different means.
//
// picoclaw needs a sibling directory because each project is a separate picoclaw
// AGENT, and an agent resolves its own workspace independently. The ganglion has
// one agent and takes the project as a header, so its projects are children of
// the workspace it already mounts -- which is what lets a project be created
// without adding a bind, and therefore without recreating a container that,
// under scale-to-zero, may not be running.
//
// Kept in step with the harness's own domain.ProjectsDirName. If the two ever
// disagree the proxy reads an empty history for a conversation that exists,
// which is the failure jsonl.go's header already records once.
func GanglionProjectWorkspace(projectID string) string {
	return MainWorkspace + "/projects/" + identity.SanitizeID(projectID)
}

// WorkspaceSegment is the segment for one (harness, project) pair. The empty
// project is the main workspace for both.
func WorkspaceSegment(harness, projectID string) string {
	if projectID == "" {
		return MainWorkspace
	}
	if harness == HarnessGanglion {
		return GanglionProjectWorkspace(projectID)
	}
	return ProjectWorkspace(projectID)
}

// ProjectsFile is the proxy-owned list of a user's projects,
// UserWorkspace/.projects.json.
//
// Deliberately ABOVE workspace/, next to config.json and .crab-owner.json: with
// restrict_to_workspace the agent cannot read or edit it. That matters because
// this file decides which agent identities exist and which dispatch rules get
// written — an agent able to edit it could route a peer's conversations to
// itself, or invent a workspace outside the ones the proxy seeded.
//
// It is also the SOURCE OF TRUTH, not a cache: agents.list and
// agents.dispatch.rules in config.json are re-derived from it on every ensure,
// because materializeModels rewrites that whole file and would otherwise erase
// them on the user's next chat.
func ProjectsFile(root, tenantID, subsAccID, role, userAccID string) string {
	return filepath.Join(UserWorkspace(root, tenantID, subsAccID, role, userAccID),
		".projects.json")
}

// SessionsDir is the path to a user's picoclaw session transcripts (used by
// /v1/sessions/history), under UserWorkspace/<segment>/sessions.
//
// segment is MainWorkspace, or ProjectWorkspace(id) for a project's own agent —
// each picoclaw agent keeps its transcripts under its own workspace, so a
// project's history is not reachable by asking for the main one.
func SessionsDir(root, tenantID, subsAccID, role, userAccID, segment string) string {
	return filepath.Join(UserWorkspace(root, tenantID, subsAccID, role, userAccID),
		segment, "sessions")
}

// CronFile is the path to a user's picoclaw scheduled-job store (used by
// /v1/cron/tasks), under UserWorkspace/MainWorkspace/cron/jobs.json. picoclaw
// owns the file; the proxy only reads it. It is absent until the agent creates
// its first task, which is a normal state, not an error.
//
// IT TAKES NO WORKSPACE SEGMENT, unlike SessionsDir and PublicDir beside it, and
// that is a fact about picoclaw rather than a simplification. picoclaw builds ONE
// CronService per gateway and hands it cfg.WorkspacePath() — the default
// workspace — at pkg/gateway/gateway.go:416 and :664, which composes
// <workspace>/cron/jobs.json at :843. There is no per-agent store and no
// per-agent override, so every agent in the container, projects included, writes
// its jobs into this one file.
//
// A project's jobs are therefore separated by ATTRIBUTION, not by path: each job
// records the conversation that created it in Payload.To, which on the pico
// channel is "pico:" + the session id the proxy stamped (see cron.JobProject).
// An earlier version of this function took a segment and composed
// workspace-<id>/cron/jobs.json — a path picoclaw never writes — so a project's
// panel read an absent file and showed nothing while the global panel showed the
// project's tasks as its own.
func CronFile(root, tenantID, subsAccID, role, userAccID string) string {
	return filepath.Join(UserWorkspace(root, tenantID, subsAccID, role, userAccID),
		MainWorkspace, "cron", "jobs.json")
}

// PublicDirName is the member-facing directory inside a workspace: the only one
// their interface lists, and therefore the only place a produced file can be
// delivered from. The managed FILE_DELIVERY.md memory tells the agent the same
// thing, and `public/attachments/` is where it is told to write.
const PublicDirName = "public"

// LegacyPublicDirName is what PublicDirName used to be called. Kept solely so the
// one-time migration can find a workspace that predates the rename; nothing else
// should reference it.
const LegacyPublicDirName = "uploads"

// PublicDir is where member-visible media lands, inside the agent-readable
// workspace (UserWorkspace/<segment>/public) so a vision model / reader skill can
// open it by the returned "public/<file>" path.
func PublicDir(root, tenantID, subsAccID, role, userAccID, segment string) string {
	return filepath.Join(UserWorkspace(root, tenantID, subsAccID, role, userAccID),
		segment, PublicDirName)
}

// LegacyPublicDir is the pre-rename location of PublicDir. Only MigratePublicDir
// should call it.
func LegacyPublicDir(root, tenantID, subsAccID, role, userAccID, segment string) string {
	return filepath.Join(UserWorkspace(root, tenantID, subsAccID, role, userAccID),
		segment, LegacyPublicDirName)
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

// UserModeOverrideFile is the per-instance lifecycle override:
// UserWorkspace/.crab-mode.json.
//
// A dotfile beside .crab-model.json and .crab-owner.json, ABOVE workspace/ --
// so picoclaw's restrict_to_workspace and the ganglion's Landlock domain both
// keep the agent from reading or editing it. That matters more here than for a
// model selection: an agent able to write this file could keep its own
// container alive indefinitely.
func UserModeOverrideFile(root, tenantID, subsAccID, role, userAccID string) string {
	return filepath.Join(UserWorkspace(root, tenantID, subsAccID, role, userAccID), ".crab-mode.json")
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

// The per-agent shared stores sit one level deeper than the agent-less ones,
// under `shared/agents/<agent>/`. The agent-less dirs above keep their paths and
// mean "all agents", so nothing already published needs migrating
// (per-agent-injection-scope AD-2). `agents` can never collide with stored
// content: the children of `shared/` are the fixed names files/secrets/skills/
// model.json, and file and skill names live inside those.

// TenantAgentSharedFilesDir is the tenant-scope, single-agent shared-files store:
// <root>/tenants/<t>/shared/agents/<agent>/files.
func TenantAgentSharedFilesDir(root, tenantID, agentKey string) string {
	return filepath.Join(tenantAgentSharedRoot(root, tenantID, agentKey), "files")
}

// TenantAgentSharedSecretsDir is the tenant-scope, single-agent shared-secret
// store: <root>/tenants/<t>/shared/agents/<agent>/secrets.
func TenantAgentSharedSecretsDir(root, tenantID, agentKey string) string {
	return filepath.Join(tenantAgentSharedRoot(root, tenantID, agentKey), "secrets")
}

// TenantAgentSharedSkillsDir is the tenant-scope, single-agent shared-skills
// store: <root>/tenants/<t>/shared/agents/<agent>/skills.
func TenantAgentSharedSkillsDir(root, tenantID, agentKey string) string {
	return filepath.Join(tenantAgentSharedRoot(root, tenantID, agentKey), "skills")
}

// TenantAgentPersonaDir is the tenant-scope, single-agent persona store:
// <root>/tenants/<t>/shared/agents/<agent>/persona.
//
// Persona has no agent-less sibling, unlike files/secrets/skills. These files
// ARE the agent's identity, so "the same persona for every agent" is not a thing
// an operator wants to express.
func TenantAgentPersonaDir(root, tenantID, agentKey string) string {
	return filepath.Join(tenantAgentSharedRoot(root, tenantID, agentKey), "persona")
}

// SubscriptionAgentSharedFilesDir is the subscription-scope, single-agent
// shared-files store:
// <root>/tenants/<t>/subscriptions/<s>/shared/agents/<agent>/files.
func SubscriptionAgentSharedFilesDir(root, tenantID, subsAccID, agentKey string) string {
	return filepath.Join(subscriptionAgentSharedRoot(root, tenantID, subsAccID, agentKey), "files")
}

// SubscriptionAgentSharedSecretsDir is the subscription-scope, single-agent
// shared-secret store:
// <root>/tenants/<t>/subscriptions/<s>/shared/agents/<agent>/secrets.
func SubscriptionAgentSharedSecretsDir(root, tenantID, subsAccID, agentKey string) string {
	return filepath.Join(subscriptionAgentSharedRoot(root, tenantID, subsAccID, agentKey), "secrets")
}

// SubscriptionAgentSharedSkillsDir is the subscription-scope, single-agent
// shared-skills store:
// <root>/tenants/<t>/subscriptions/<s>/shared/agents/<agent>/skills.
// SubscriptionAgentConfigOverlay is the subscription-scope, single-agent seed
// overlay for config.json:
// <root>/tenants/<t>/subscriptions/<s>/shared/agents/<agent>/config-overlay.json
//
// It is a FILE, not a dir, because it holds one flat map rather than a store of
// named entries — and it sits beside the other subscription+agent scope stores so
// the scope's contents stay in one place.
func SubscriptionAgentConfigOverlay(root, tenantID, subsAccID, agentKey string) string {
	return filepath.Join(subscriptionAgentSharedRoot(root, tenantID, subsAccID, agentKey),
		"config-overlay.json")
}

func SubscriptionAgentSharedSkillsDir(root, tenantID, subsAccID, agentKey string) string {
	return filepath.Join(subscriptionAgentSharedRoot(root, tenantID, subsAccID, agentKey), "skills")
}

// SubscriptionAgentPersonaDir is the subscription-scope, single-agent persona
// store: <root>/tenants/<t>/subscriptions/<s>/shared/agents/<agent>/persona.
// The most specific layer of the persona cascade.
func SubscriptionAgentPersonaDir(root, tenantID, subsAccID, agentKey string) string {
	return filepath.Join(subscriptionAgentSharedRoot(root, tenantID, subsAccID, agentKey), "persona")
}

// EffectivePersonaDir is the resolved persona set for one workspace's
// (tenant, subscription, agent) — the bind-mount SOURCE for the read-only
// identity files. Same shape as EffectiveSkillsDir, and deliberately without a
// user dimension: an agent's identity does not vary per user.
func EffectivePersonaDir(root, tenantID, subsAccID, agentKey string) string {
	return filepath.Join(root, "effective-persona",
		identity.SanitizeID(tenantID), identity.SanitizeID(subsAccID),
		identity.SanitizeID(agentKey))
}

func tenantAgentSharedRoot(root, tenantID, agentKey string) string {
	return filepath.Join(root, "tenants", identity.SanitizeID(tenantID),
		"shared", "agents", identity.SanitizeID(agentKey))
}

func subscriptionAgentSharedRoot(root, tenantID, subsAccID, agentKey string) string {
	return filepath.Join(root, "tenants", identity.SanitizeID(tenantID),
		"subscriptions", identity.SanitizeID(subsAccID),
		"shared", "agents", identity.SanitizeID(agentKey))
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

// EffectiveSkillsDir is the per-(tenant, subscription, agent) MERGED skills view
// bind-mounted read-only at the container's global skills root
// (<mountDest>/skills). Merge order, later winning by skill name: tenant
// all-agents → tenant this-agent → subscription all-agents → subscription
// this-agent. Materialized whenever a shared skill changes so
// additions/edits/removals reach picoclaw live (via the mount) on the next
// stop/start, without a recreate: <root>/effective-skills/<t>/<s>/<agent>.
func EffectiveSkillsDir(root, tenantID, subsAccID, agentKey string) string {
	return filepath.Join(root, "effective-skills",
		identity.SanitizeID(tenantID), identity.SanitizeID(subsAccID),
		identity.SanitizeID(agentKey))
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

// RestartRoot is where restart notices and per-workspace restart markers live:
// <root>/restart. CRITICAL: this is OUTSIDE the tenant tree because the whole
// UserWorkspace is bind-mounted into the agent container — a marker kept there
// would be readable and writable by the agent itself.
func RestartRoot(root string) string {
	return filepath.Join(root, "restart")
}

// RestartScopeFile is the notice record for one scope:
// <root>/restart/scopes/<t>/<s>.json, or <root>/restart/scopes/<t>/_tenant.json
// when subsAccID is empty. Agent narrowing is a key INSIDE the file (mirroring
// Scope.AgentKey == "" meaning "all agents"), not another path level.
func RestartScopeFile(root, tenantID, subsAccID string) string {
	name := "_tenant.json"
	if subsAccID != "" {
		name = identity.SanitizeID(subsAccID) + ".json"
	}
	return filepath.Join(RestartRoot(root), "scopes", identity.SanitizeID(tenantID), name)
}

// RestartWorkspaceFile is one workspace's lastRestartAt marker, under
// <root>/restart/workspaces/<t>/<s>/<role>/<u>.json. Kept outside the tenant
// tree for the same reason as RestartRoot.
func RestartWorkspaceFile(root, tenantID, subsAccID, role, userAccID string) string {
	return filepath.Join(RestartRoot(root), "workspaces",
		identity.SanitizeID(tenantID), identity.SanitizeID(subsAccID),
		identity.SanitizeID(role), identity.SanitizeID(userAccID)+".json")
}

// WorkspaceSeed is the allowlist of agent template workspace files copied into a
// fresh user workspace on first provision (recursive for directories). CRITICAL:
// sessions/ (conversation history), logs/, and .picoclaw.pid (runtime state) are
// NEVER copied — the isolation invariant that keeps the shared template's
// sessions out of every new user's container.
//
// AGENT.md, SOUL.md and HEARTBEAT.md are NOT here: they are delivered as
// root-owned read-only bind mounts instead (internal/docker/persona.go), so the
// user cannot rewrite the agent's identity or its recurring task list. Copying a
// file the mount always shadows would be dead work, and a stale copy would
// resurface the moment a mount went away.
//
// USER.md stays: the agent accumulates what it learns about the user there, so it
// has to be writable. What an operator controls is the content it is SEEDED from.
var WorkspaceSeed = []string{"USER.md", "memory/", "skills/"}

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
