package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"gopkg.in/yaml.v3"
)

// hermesKeyFile stores the per-user generated API_SERVER_KEY so a returning user
// (and every restart of their container) authenticates with the same bearer. It
// is a proxy dotfile hermes ignores, beside its config.yaml under /opt/data.
const hermesKeyFile = ".crab-hermes.json"

// hermesMountDest is where the per-user profile is bind-mounted inside a
// hermes-agent container (the image stores config/keys/sessions/skills/memories
// there).
const hermesMountDest = "/opt/data"

// provisionHermes seeds a fresh per-user hermes profile (config.yaml, from the
// agent template or the bundled fallback) on first provision, pins the model,
// and returns the stable API server bearer key to reach the container. The
// provider key and this bearer are injected as container env by createHermes,
// never written to a committed file.
func provisionHermes(userDir, templateDir, user string, model *config.ModelConfig) (string, error) {
	configPath := filepath.Join(userDir, "config.yaml")
	if _, statErr := os.Stat(configPath); statErr != nil {
		// Self-heal: materialize the bundled fallback template if the operator
		// hasn't seeded one, so provisioning succeeds instead of failing.
		if _, tErr := os.Stat(filepath.Join(templateDir, "config.yaml")); tErr != nil {
			if err := materializeDefaultTemplate(templateDir, config.HarnessHermes, user); err != nil {
				return "", fmt.Errorf("materialize default hermes template: %w", err)
			}
		}
		if err := seedHermesTemplate(userDir, templateDir); err != nil {
			return "", err
		}
		if model != nil {
			if err := applyHermesModel(configPath, model); err != nil {
				return "", fmt.Errorf("apply hermes model config: %w", err)
			}
		}
		if err := chownTree(userDir, user); err != nil {
			return "", fmt.Errorf("chown hermes data dir to %q: %w", user, err)
		}
	}
	return ensureHermesAPIKey(userDir, user)
}

// seedHermesTemplate copies the hermes config-only seed (config.yaml + optional
// SOUL.md) into a fresh per-user dir. Runtime/isolation state (sessions/,
// state.db, logs/, cache/, auth.json) is NEVER seeded — the isolation invariant.
func seedHermesTemplate(userDir, templateDir string) error {
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		return fmt.Errorf("create hermes data dir: %w", err)
	}
	for _, name := range []string{"config.yaml", "SOUL.md"} {
		src := filepath.Join(templateDir, name)
		if _, err := os.Stat(src); err != nil {
			continue // SOUL.md is optional; config.yaml is checked below
		}
		if err := copyFile(src, filepath.Join(userDir, name)); err != nil {
			return fmt.Errorf("seed hermes %s: %w", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(userDir, "config.yaml")); err != nil {
		return fmt.Errorf("hermes template %s has no config.yaml", templateDir)
	}
	return nil
}

// applyHermesModel pins model.default/provider/base_url in a hermes config.yaml,
// preserving every other key.
func applyHermesModel(configPath string, model *config.ModelConfig) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	m, _ := cfg["model"].(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	m["default"] = model.Name
	m["provider"] = model.Provider
	if model.BaseURL != "" {
		m["base_url"] = model.BaseURL
	}
	cfg["model"] = m
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, out, 0o600)
}

// ensureHermesAPIKey reads the persisted per-user API server bearer, generating
// and persisting one on first call so it stays stable across restarts.
func ensureHermesAPIKey(userDir, user string) (string, error) {
	path := filepath.Join(userDir, hermesKeyFile)
	if raw, err := os.ReadFile(path); err == nil {
		var v struct {
			APIServerKey string `json:"apiServerKey"`
		}
		if json.Unmarshal(raw, &v) == nil && v.APIServerKey != "" {
			return v.APIServerKey, nil
		}
	}
	key, err := randomHexKey()
	if err != nil {
		return "", err
	}
	b, _ := json.MarshalIndent(map[string]string{"apiServerKey": key}, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return "", err
	}
	_ = chownTree(path, user) // best-effort; keep it consistent with the profile
	return key, nil
}

func randomHexKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil // 48 chars, well over API_SERVER_KEY's 8-char min
}

// splitUser parses "uid:gid" into its parts; returns empties when user is "".
func splitUser(user string) (uid, gid string) {
	if user == "" {
		return "", ""
	}
	parts := strings.SplitN(user, ":", 2)
	uid = parts[0]
	gid = uid
	if len(parts) == 2 {
		gid = parts[1]
	}
	return uid, gid
}

// createHermes creates a per-user hermes-agent container: the image, the profile
// bind-mounted at /opt/data, and the API server + provider key as env. It
// deliberately does NOT get picoclaw's shared secrets/skills/files cascade,
// managed content, native secrets, or model-override mounts — those are all
// .picoclaw/workspace-relative and do not map to hermes' flat /opt/data (P1 scope).
func (m *Manager) createHermes(ctx context.Context, agent config.Agent, key WorkspaceKey, name string, model *config.ModelConfig, apiServerKey string) error {
	hostDir := config.UserWorkspace(m.cfg.HostDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	env := []string{
		"API_SERVER_ENABLED=true",
		"API_SERVER_HOST=0.0.0.0",
		fmt.Sprintf("API_SERVER_PORT=%d", hermesAPIPort),
		"API_SERVER_KEY=" + apiServerKey,
		// Without an allowlist the gateway DENIES every request ("No user
		// allowlists configured. All unauthorized users will be denied"),
		// including API-server turns — so the reply comes back empty. Open access
		// is safe here: each container is one isolated per-(user, agent) sandbox,
		// already behind mycelium + this proxy's auth.
		"GATEWAY_ALLOW_ALL_USERS=true",
	}
	// Provider key under the in-container env name hermes expects (e.g.
	// GLM_API_KEY for provider zai) — provider-specific, not derivable.
	if model != nil && model.KeyEnvName != "" && model.APIKey != "" {
		env = append(env, model.KeyEnvName+"="+model.APIKey)
	}
	// hermes' entrypoint starts as root and drops to the non-root `hermes` user
	// via s6, mapping host ownership from PUID/PGID — so we do NOT set spec.User
	// (that would break the s6 setuidgid step) and we do NOT enable docker --init
	// (s6-overlay is PID 1). The profile was chowned to this uid at provision.
	uid, gid := splitUser(m.cfg.PicoclawUser)
	if uid != "" {
		env = append(env, "PUID="+uid, "PGID="+gid)
	}
	spec := CreateSpec{
		Name:  name,
		Image: m.cfg.HermesImage,
		// Run the persistent gateway (serves the OpenAI-compatible API on :8642),
		// NOT the image's default interactive CLI — the CLI reads stdin, hits EOF
		// with no TTY, prints "Goodbye!" and exits, so s6 tears the container down
		// seconds after boot. `gateway run` is the headless daemon.
		Cmd: []string{"gateway", "run"},
		Env: env,
		Labels: map[string]string{
			LabelManaged:      "true",
			LabelAgent:        key.Role,
			LabelTenant:       key.TenantID,
			LabelSubscription: key.SubsAccID,
			LabelUser:         key.UserAccID,
			LabelMode:         string(agent.Mode),
		},
		Binds:   []string{hostDir + ":" + hermesMountDest},
		Network: m.cfg.Network,
	}
	if err := m.docker.EnsureImage(ctx, m.cfg.HermesImage); err != nil {
		return fmt.Errorf("ensure image %s: %w", m.cfg.HermesImage, err)
	}
	if _, err := m.docker.Create(ctx, spec); err != nil {
		return err
	}
	m.logf("created hermes container %s (mode=%s)", name, agent.Mode)
	return nil
}
