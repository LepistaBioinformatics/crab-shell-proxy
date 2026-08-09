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
// dir is chowned to so a non-root container can write it.
// provision seeds a workspace from templateDir. Neither the MODEL nor the native
// secret overlay is a parameter: the caller materializes the model from the
// inventory after seeding and applies the overlay after THAT, because
// materialization rewrites .security.yml's whole model_list and would otherwise
// overwrite the overlay it is supposed to sit under (see resolveAndMaterialize).
// ensurePicoclawTemplate self-heals a missing agent template (e.g. data/ was
// wiped or never seeded) by materializing the bundled fallback, so provisioning
// succeeds instead of failing on a missing seed. Idempotent — a template that has
// a config.json is left alone.
//
// Separate from provision because the persona cascade's LAST layer is this
// template, and it is resolved before provisioning runs. Inlined in provision, a
// first-ever provision resolved the cascade against a template that did not exist
// yet and left the workspace with no identity files at all.
func ensurePicoclawTemplate(templateDir, user string) error {
	if _, err := os.Stat(filepath.Join(templateDir, "config.json")); err == nil {
		return nil
	}
	// "picoclaw" is the only harness there is; the layout is per-harness anyway.
	if err := materializeDefaultTemplate(templateDir, "picoclaw", user); err != nil {
		return fmt.Errorf("materialize default template: %w", err)
	}
	return nil
}

func provision(userDir, templateDir, personaDir, overlayPath, home, user string, key WorkspaceKey, ownerEmail string) (picoToken string, err error) {
	configPath := filepath.Join(userDir, "config.json")
	secPath := filepath.Join(userDir, ".security.yml")
	if _, statErr := os.Stat(configPath); statErr != nil {
		if err := ensurePicoclawTemplate(templateDir, user); err != nil {
			return "", err
		}
		if err := seedFromTemplate(userDir, templateDir); err != nil {
			return "", err
		}
		if err := alignWorkspace(configPath, home); err != nil {
			return "", fmt.Errorf("align workspace path: %w", err)
		}
		// The subscription's scoped seed defaults, applied ONLY here — on a fresh
		// seed, never to a returning user. This is what lets an admin say "this is
		// the default for MY subscription's future members" instead of having to
		// write the agent template, which every subscription on that agent shares.
		// After alignWorkspace so the aligned path cannot be disturbed, and
		// best-effort per key: see applyConfigOverlay.
		if _, err := applyConfigOverlay(configPath, overlayPath); err != nil {
			return "", fmt.Errorf("apply scoped config overlay: %w", err)
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
		if err := seedWorkspace(userDir, templateDir, personaDir); err != nil {
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
//
// Lchown, not Chown: the trees we walk contain agent-created symlinks (a Python
// venv's bin/python, node_modules/.bin/*, …) whose absolute targets name paths
// in the AGENT container's rootfs, not ours. chown(2) resolves symlinks and
// would fail ENOENT on those, aborting the whole walk — and since
// ScaffoldSubscription chowns the subscription root on every chat, a single venv
// in one user's workspace would 502 every user of that subscription. Owning the
// link itself is also the correct semantics: the target is not ours to touch.
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
	return filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// The knowledge-graph directory is the one thing under a user tree the AGENT
		// must not own. The proxy is its only reader and writer, and leaving it
		// root-owned 0700 is what stops the non-root container process reaching
		// memory.jsonl through `tools.exec` — the file tools already cannot, because
		// it sits above the agent's workspace.
		//
		// This skip is load-bearing rather than tidy: resolveAndMaterialize calls
		// chownTree(userDir) on EVERY ensure, so without it the graph would be handed
		// to picoclawUser on the second chat and the isolation would quietly be gone.
		// See .specs/features/memory-graph-mcp/context.md D-2.
		if info != nil && info.IsDir() && filepath.Base(path) == GraphDirName && path != dir {
			return filepath.SkipDir
		}
		return os.Lchown(path, uid, gid)
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
//
// personaDir is the RESOLVED persona set (admin injection falling back to the
// template). An entry present there is seeded from there instead of from the
// template, which is how an operator controls the starting content of USER.md —
// the one identity file that stays writable, because the agent accumulates what
// it learns about the user in it.
//
// Deliberately expressed as "prefer personaDir for any entry it holds" rather
// than a check for the literal name: the allowlist and the persona set each say
// what they cover, and a name spelled here too would be a third place to keep in
// agreement.
func seedWorkspace(userDir, templateDir, personaDir string) error {
	srcRoot := filepath.Join(templateDir, "workspace")
	dstRoot := filepath.Join(userDir, "workspace")
	for _, entry := range config.WorkspaceSeed {
		src := filepath.Join(srcRoot, entry)
		// The resolved persona set wins where it has the entry: it already IS the
		// template's copy when no admin injected one, so this only ever changes the
		// source when an injection exists.
		if personaDir != "" {
			if candidate := filepath.Join(personaDir, entry); fileExists(candidate) {
				src = candidate
			}
		}
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

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
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
