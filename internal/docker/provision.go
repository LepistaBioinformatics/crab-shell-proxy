package docker

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// templateFiles is the config-only allowlist copied into a fresh per-user data
// dir. CRITICAL: never copy workspace/ (holds conversation history) — the
// shared alpha/beta dirs' sessions would otherwise leak into every new user's
// container. logs/ and .picoclaw.pid are likewise excluded; picoclaw recreates
// workspace/ empty on first gateway run.
var templateFiles = []string{"config.json", ".security.yml"}

// provision ensures the per-user data dir (userDir, a path INSIDE this proxy)
// is seeded from templateDir and returns the pico token to connect with. If the
// dir already has config.json it is treated as a returning user and left as-is.
//
// home is the in-container HOME the spawned picoclaw will use; the config's
// workspace path is aligned to <home>/.picoclaw/workspace so it matches the
// mount point. user ("uid:gid", may be empty) is the non-root owner the data
// dir is chowned to so a non-root container can write it. secretsDir is the
// EFFECTIVE secret view (the merged user + shared cascade), which is where the
// native overlay to merge into .security.yml is read from.
// provision seeds a workspace from templateDir. The MODEL is not a parameter:
// the caller materializes it from the inventory after seeding, because that is
// the only place a model may come from.
func provision(userDir, templateDir, secretsDir, home, user string, key WorkspaceKey, ownerEmail string) (picoToken string, err error) {
	configPath := filepath.Join(userDir, "config.json")
	secPath := filepath.Join(userDir, ".security.yml")
	if _, statErr := os.Stat(configPath); statErr != nil {
		// Self-heal: if the agent's template is missing (e.g. data/ was wiped or
		// never seeded), materialize the bundled fallback template first so
		// provisioning succeeds instead of failing on a missing seed.
		if _, tErr := os.Stat(filepath.Join(templateDir, "config.json")); tErr != nil {
			// "picoclaw" is the only harness today; thread the agent's harness
			// here once multi-harness support lands.
			if err := materializeDefaultTemplate(templateDir, "picoclaw", user); err != nil {
				return "", fmt.Errorf("materialize default template: %w", err)
			}
		}
		if err := seedFromTemplate(userDir, templateDir); err != nil {
			return "", err
		}
		if err := alignWorkspace(configPath, home); err != nil {
			return "", fmt.Errorf("align workspace path: %w", err)
		}
		// The pico channel token is generated here; the model is materialized
		// by the caller from the inventory. Splitting these is what lets the
		// model come from exactly one place.
		if err := seedPicoToken(secPath); err != nil {
			return "", fmt.Errorf("seed pico token: %w", err)
		}
		// Seed the allowlisted agent template workspace files (persona/skills/
		// memory) so the agent starts customized. Only on first provision, so a
		// returning user's evolved files are never clobbered (AC-01).
		if err := seedWorkspace(userDir, templateDir); err != nil {
			return "", fmt.Errorf("seed workspace files: %w", err)
		}
		// Traceability: the dir is named by accId (not email), so drop a small
		// marker recording the full workspace tuple + owner email, for finding
		// the user later.
		if err := writeOwnerFile(userDir, key, ownerEmail); err != nil {
			return "", fmt.Errorf("write owner marker: %w", err)
		}
		if err := chownTree(userDir, user); err != nil {
			return "", fmt.Errorf("chown data dir to %q: %w", user, err)
		}
	}
	// Merge the EFFECTIVE native overlay into this workspace's .security.yml on
	// EVERY ensure (not just first provision), so a brand-new workspace of an
	// existing pair — e.g. a second subscription — picks up the already-stored
	// secrets (AC-04/AC-05, CTX-AC-03), and so an admin's scope-level native
	// secret reaches a user who has never chatted (native-secrets-admin-only
	// NFR-3). No-op when none are set.
	if err := applyNativeSecrets(secPath, secretsDir, user); err != nil {
		return "", fmt.Errorf("apply native secrets: %w", err)
	}
	tok, err := readPicoToken(secPath)
	if err != nil {
		return "", fmt.Errorf("read pico token: %w", err)
	}
	if tok == "" {
		return "", fmt.Errorf("no pico channel token found in %s/.security.yml", userDir)
	}
	return tok, nil
}

// writeOwnerFile drops a small JSON marker in the user's data dir recording the
// full workspace tuple and the owner email, so an operator can find which human
// a container belongs to. Lives next to config.json (a dotfile picoclaw
// ignores).
func writeOwnerFile(userDir string, key WorkspaceKey, email string) error {
	info := map[string]string{
		"tenantId":  key.TenantID,
		"subsAccId": key.SubsAccID,
		"role":      key.Role,
		"userAccId": key.UserAccID,
		"email":     email,
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(userDir, ".crab-owner.json"), b, 0o600)
}

func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "pico-" + hex.EncodeToString(b), nil
}

// seedPicoToken writes a fresh proxy<->picoclaw channel token into a new
// workspace's .security.yml, preserving anything the template already put there.
func seedPicoToken(secPath string) error {
	token, err := randomToken()
	if err != nil {
		return err
	}
	sec, err := readSecurityConfig(secPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		sec = map[string]any{}
	}
	cl := childMap(sec, "channel_list")
	pico := childMap(cl, "pico")
	settings := childMap(pico, "settings")
	settings["token"] = token
	return writeSecurityConfig(secPath, sec, "")
}

// alignWorkspace rewrites agents.defaults.workspace in config.json to
// <home>/.picoclaw/workspace, so the path picoclaw writes matches where the
// per-user dir is actually mounted (regardless of the HOME the template was
// generated under). No-op if the structure is absent.
func alignWorkspace(configPath, home string) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	agents, ok := cfg["agents"].(map[string]any)
	if !ok {
		return nil
	}
	defaults, ok := agents["defaults"].(map[string]any)
	if !ok {
		return nil
	}
	defaults["workspace"] = home + "/.picoclaw/workspace"
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, out, 0o600)
}

// chownTree recursively chowns dir to the given "uid:gid". No-op when user is
// empty (containers run as root). Requires numeric uid:gid.
func chownTree(dir, user string) error {
	if user == "" {
		return nil
	}
	parts := strings.SplitN(user, ":", 2)
	uid, err := strconv.Atoi(parts[0])
	if err != nil {
		return fmt.Errorf("picoclawUser uid must be numeric, got %q", user)
	}
	gid := uid
	if len(parts) == 2 {
		if gid, err = strconv.Atoi(parts[1]); err != nil {
			return fmt.Errorf("picoclawUser gid must be numeric, got %q", user)
		}
	}
	return filepath.Walk(dir, func(path string, _ os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return os.Chown(path, uid, gid)
	})
}

func seedFromTemplate(userDir, templateDir string) error {
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		return fmt.Errorf("create user data dir: %w", err)
	}
	for _, name := range templateFiles {
		src := filepath.Join(templateDir, name)
		dst := filepath.Join(userDir, name)
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("seed %s: %w", name, err)
		}
	}
	return nil
}

// seedWorkspace copies the config.WorkspaceSeed allowlist from the template's
// workspace/ into the user's workspace/ (files verbatim, directories
// recursively). Absent entries are skipped without error (partial templates are
// valid, AC-01.3). sessions/, logs/ and .picoclaw.pid are never in the allowlist
// so they can never leak.
func seedWorkspace(userDir, templateDir string) error {
	srcRoot := filepath.Join(templateDir, "workspace")
	dstRoot := filepath.Join(userDir, "workspace")
	for _, entry := range config.WorkspaceSeed {
		src := filepath.Join(srcRoot, entry)
		info, err := os.Stat(src)
		if err != nil {
			continue // absent entry: skip (partial templates OK)
		}
		dst := filepath.Join(dstRoot, entry)
		if info.IsDir() {
			if err := copyTree(src, dst); err != nil {
				return fmt.Errorf("seed workspace %s: %w", entry, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("seed workspace %s: %w", entry, err)
		}
	}
	return nil
}

// copyTree recursively copies the directory src to dst, creating directories as
// needed.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

var (
	picoBlockRe = regexp.MustCompile(`^(\s*)pico:\s*$`)
	tokenLineRe = regexp.MustCompile(`^\s*token:\s*(.+?)\s*$`)
)

// readPicoToken extracts channel_list.pico(.settings).token from a picoclaw
// .security.yml without a YAML dependency — the same nested-block scan
// picoclaw-openai-proxy/server.js uses (find a `pico:` line, then the first
// `token:` line more-indented than it).
func readPicoToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(raw), "\n")
	for i := 0; i < len(lines); i++ {
		m := picoBlockRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		baseIndent := len(m[1])
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "" {
				continue
			}
			indent := len(lines[j]) - len(strings.TrimLeft(lines[j], " \t"))
			if indent <= baseIndent {
				break // left the pico: block
			}
			if tm := tokenLineRe.FindStringSubmatch(lines[j]); tm != nil {
				return strings.Trim(tm[1], `'"`), nil
			}
		}
	}
	return "", nil
}
