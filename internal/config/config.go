// Package config loads crab-shell-proxy's agent catalog and runtime settings
// from a YAML file, resolving per-field secrets from environment variables.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

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

	// ResolvedToken is filled by Load from Token.
	ResolvedToken string `yaml:"-"`
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
	StartupDeadline Duration `yaml:"startupDeadline"`
	TurnTimeout     Duration `yaml:"turnTimeout"`
	// ContainerPrefix prefixes every managed container name (default "picoclaw").
	ContainerPrefix string           `yaml:"containerPrefix"`
	Agents          map[string]Agent `yaml:"agents"`
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
		cfg.Agents[key] = agent
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
		c.ContainerDataRoot = "/data/agents"
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
	}
	return nil
}

// SessionsDir is the path INSIDE this proxy to a user's picoclaw session
// transcripts (used by /v1/sessions/history). The same host dir is mounted into
// the picoclaw container at /root/.picoclaw.
func (c *Config) SessionsDir(agentKey, userKey string) string {
	return filepath.Join(c.ContainerDataRoot, agentKey, userKey, "workspace", "sessions")
}

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
