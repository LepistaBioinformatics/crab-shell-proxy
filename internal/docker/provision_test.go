package docker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadPicoToken(t *testing.T) {
	dir := t.TempDir()
	sec := filepath.Join(dir, ".security.yml")
	// nested pico: settings: token: form (the only one picoclaw honors)
	os.WriteFile(sec, []byte("channel_list:\n  pico:\n    settings:\n      token: \"abc123\"\n"), 0o600)
	tok, err := readPicoToken(sec)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "abc123" {
		t.Errorf("token = %q, want abc123", tok)
	}
}

func TestAlignWorkspace(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(`{"agents":{"defaults":{"workspace":"/root/.picoclaw/workspace"}}}`), 0o600)
	if err := alignWorkspace(cfgPath, "/data"); err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	raw, _ := os.ReadFile(cfgPath)
	json.Unmarshal(raw, &d)
	got := d["agents"].(map[string]any)["defaults"].(map[string]any)["workspace"]
	if got != "/data/.picoclaw/workspace" {
		t.Errorf("workspace = %v, want /data/.picoclaw/workspace", got)
	}
}

func TestSeedPicoTokenPreservesTemplateContentAndGeneratesAToken(t *testing.T) {
	dir := t.TempDir()
	secPath := filepath.Join(dir, ".security.yml")
	if err := os.WriteFile(secPath, []byte("web:\n  brave:\n    api_keys:\n    - seeded\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := seedPicoToken(secPath); err != nil {
		t.Fatalf("seedPicoToken: %v", err)
	}

	sec, err := readSecurityConfig(secPath)
	if err != nil {
		t.Fatalf("readSecurityConfig: %v", err)
	}
	tok, _ := sec["channel_list"].(map[string]any)["pico"].(map[string]any)["settings"].(map[string]any)["token"].(string)
	if !strings.HasPrefix(tok, "pico-") {
		t.Errorf("token = %q, want a pico- prefixed random token", tok)
	}
	// Only the nested pico.settings.token form is honored by picoclaw (the flat
	// form silently leaves the channel disabled), and the template's own keys
	// must survive.
	if _, ok := sec["web"]; !ok {
		t.Error("template content was clobbered")
	}
}

// A venv the agent creates inside its workspace is made of ABSOLUTE symlinks
// pointing at the interpreter of the picoclaw container (Alpine:
// /usr/bin/python3). That path does not exist in THIS process's rootfs
// (debian-slim, no python) — a symlink carries no notion of which rootfs it
// speaks of, and both containers bind-mount the same host dir. chown(2) resolves
// symlinks, so it fails ENOENT on such a link; because ScaffoldSubscription
// chowns the whole subscription root on every chat, one venv would 502 every
// user of that subscription. chownTree must therefore chown the LINK, never its
// target — the target is the agent's business, not ours.
func TestChownTreeToleratesDanglingSymlinks(t *testing.T) {
	dir := t.TempDir()
	venvBin := filepath.Join(dir, "workspace", ".venv", "bin")
	if err := os.MkdirAll(venvBin, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(venvBin, "python")
	if err := os.Symlink("/nonexistent-rootfs/usr/bin/python3", link); err != nil {
		t.Fatal(err)
	}

	// Chowning to our OWN uid:gid is permitted unprivileged, so this asserts the
	// dangling link is tolerated without the test needing root.
	user := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if err := chownTree(dir, user); err != nil {
		t.Fatalf("chownTree over a dangling symlink: %v", err)
	}

	// The link itself must survive untouched: chownTree may not follow, replace,
	// or prune it.
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("symlink gone after chownTree: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("%s is no longer a symlink (mode %v)", link, fi.Mode())
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSeedWorkspaceAllowlist(t *testing.T) {
	tmpl := t.TempDir()
	ws := filepath.Join(tmpl, "workspace")
	mustWrite(t, filepath.Join(ws, "USER.md"), "user")
	mustWrite(t, filepath.Join(ws, "skills", "brave", "SKILL.md"), "skill")
	mustWrite(t, filepath.Join(ws, "memory", "MEMORY.md"), "mem")
	// The identity files are delivered by READ-ONLY MOUNT, not copied — a writable
	// duplicate is exactly what the mount exists to prevent.
	mustWrite(t, filepath.Join(ws, "AGENT.md"), "persona")
	mustWrite(t, filepath.Join(ws, "SOUL.md"), "soul")
	mustWrite(t, filepath.Join(ws, "HEARTBEAT.md"), "beat")
	// These MUST NEVER be copied (isolation + runtime state).
	mustWrite(t, filepath.Join(ws, "sessions", "leak.jsonl"), "SECRET SESSION")
	mustWrite(t, filepath.Join(ws, "logs", "run.log"), "log")
	mustWrite(t, filepath.Join(ws, ".picoclaw.pid"), "123")

	userDir := filepath.Join(t.TempDir(), "u")
	if err := seedWorkspace(userDir, tmpl, ""); err != nil {
		t.Fatalf("seedWorkspace: %v", err)
	}
	dws := filepath.Join(userDir, "workspace")
	for _, p := range []string{"USER.md", "skills/brave/SKILL.md", "memory/MEMORY.md"} {
		if _, err := os.Stat(filepath.Join(dws, filepath.FromSlash(p))); err != nil {
			t.Errorf("expected %s seeded: %v", p, err)
		}
	}
	for _, p := range []string{"sessions", "sessions/leak.jsonl", "logs", ".picoclaw.pid"} {
		if _, err := os.Stat(filepath.Join(dws, filepath.FromSlash(p))); err == nil {
			t.Errorf("%s must NEVER be seeded (isolation invariant)", p)
		}
	}
	for _, p := range []string{"AGENT.md", "SOUL.md", "HEARTBEAT.md"} {
		if _, err := os.Stat(filepath.Join(dws, p)); err == nil {
			t.Errorf("%s must not be COPIED — it is bind-mounted read-only", p)
		}
	}
}

// A partial template stays valid: an absent allowlist entry is skipped, not an
// error.
func TestSeedWorkspacePartialTemplate(t *testing.T) {
	tmpl := t.TempDir()
	mustWrite(t, filepath.Join(tmpl, "workspace", "memory", "MEMORY.md"), "mem")

	userDir := filepath.Join(t.TempDir(), "u")
	if err := seedWorkspace(userDir, tmpl, ""); err != nil {
		t.Fatalf("seedWorkspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(userDir, "workspace", "USER.md")); err == nil {
		t.Error("USER.md absent in template must not appear")
	}
}

// An operator's injected USER.md is what a first provision starts from: the
// resolved persona set wins over the template for any entry it holds.
func TestSeedWorkspacePrefersPersonaDir(t *testing.T) {
	tmpl := t.TempDir()
	mustWrite(t, filepath.Join(tmpl, "workspace", "USER.md"), "TEMPLATE")
	persona := t.TempDir()
	mustWrite(t, filepath.Join(persona, "USER.md"), "INJECTED")

	userDir := filepath.Join(t.TempDir(), "u")
	if err := seedWorkspace(userDir, tmpl, persona); err != nil {
		t.Fatalf("seedWorkspace: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(userDir, "workspace", "USER.md"))
	if string(raw) != "INJECTED" {
		t.Errorf("USER.md = %q, want the injected content", raw)
	}
}

func TestProvisionFirstSeedsWorkspace(t *testing.T) {
	tmpl := t.TempDir()
	mustWrite(t, filepath.Join(tmpl, "config.json"), `{"agents":{"defaults":{}}}`)
	mustWrite(t, filepath.Join(tmpl, ".security.yml"),
		"channel_list:\n  pico:\n    settings:\n      token: tok\n")
	mustWrite(t, filepath.Join(tmpl, "workspace", "USER.md"), "user")
	mustWrite(t, filepath.Join(tmpl, "workspace", "sessions", "leak.jsonl"), "LEAK")

	userDir := filepath.Join(t.TempDir(), "u")
	// user "" so chownTree is a no-op (this test does not run as root).
	tok, err := provision(userDir, tmpl, "", "", "/data", "", WorkspaceKey{}, "e@x")
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	// seedPicoToken always mints a fresh token on first provision, regardless of
	// whatever placeholder the template shipped.
	if !strings.HasPrefix(tok, "pico-") {
		t.Errorf("token = %q, want a pico- prefixed random token", tok)
	}
	if _, err := os.Stat(filepath.Join(userDir, "workspace", "USER.md")); err != nil {
		t.Errorf("USER.md not seeded on first provision: %v", err)
	}
	if _, err := os.Stat(filepath.Join(userDir, "workspace", "sessions")); err == nil {
		t.Error("sessions/ leaked into the user workspace")
	}
}

// The no-clobber property now belongs to USER.md, and only to it.
//
// It used to be asserted on AGENT.md, on the reasoning that a returning user's
// evolved identity must survive. That reasoning is gone: AGENT.md is bind-mounted
// read-only, so a user cannot have an evolved one at all. USER.md is where the
// agent accumulates what it learns about the user, which is exactly the content
// that must never be reset — an operator's injection sets the STARTING point
// (TestSeedWorkspacePrefersPersonaDir), not the running value.
func TestProvisionReturningUserKeepsUserMD(t *testing.T) {
	userDir := t.TempDir()
	mustWrite(t, filepath.Join(userDir, "config.json"), "{}")
	mustWrite(t, filepath.Join(userDir, ".security.yml"),
		"channel_list:\n  pico:\n    settings:\n      token: tok\n")
	mustWrite(t, filepath.Join(userDir, "workspace", "USER.md"), "LEARNED")

	tmpl := t.TempDir()
	mustWrite(t, filepath.Join(tmpl, "config.json"), "{}")
	mustWrite(t, filepath.Join(tmpl, ".security.yml"),
		"channel_list:\n  pico:\n    settings:\n      token: tmpltok\n")
	mustWrite(t, filepath.Join(tmpl, "workspace", "USER.md"), "TEMPLATE-DEFAULT")

	persona := t.TempDir()
	mustWrite(t, filepath.Join(persona, "USER.md"), "INJECTED")

	if _, err := provision(userDir, tmpl, persona, "", "/data", "", WorkspaceKey{}, ""); err != nil {
		t.Fatalf("provision: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(userDir, "workspace", "USER.md"))
	if string(raw) != "LEARNED" {
		t.Errorf("returning user's USER.md was clobbered: %q", raw)
	}
}
