package docker

import (
	"encoding/json"
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
	mustWrite(t, filepath.Join(ws, "AGENT.md"), "persona")
	mustWrite(t, filepath.Join(ws, "SOUL.md"), "soul")
	mustWrite(t, filepath.Join(ws, "skills", "brave", "SKILL.md"), "skill")
	mustWrite(t, filepath.Join(ws, "memory", "MEMORY.md"), "mem")
	// These MUST NEVER be copied (isolation + runtime state).
	mustWrite(t, filepath.Join(ws, "sessions", "leak.jsonl"), "SECRET SESSION")
	mustWrite(t, filepath.Join(ws, "logs", "run.log"), "log")
	mustWrite(t, filepath.Join(ws, ".picoclaw.pid"), "123")
	// USER.md is intentionally absent — a partial template must be seeded fine.

	userDir := filepath.Join(t.TempDir(), "u")
	if err := seedWorkspace(userDir, tmpl); err != nil {
		t.Fatalf("seedWorkspace: %v", err)
	}
	dws := filepath.Join(userDir, "workspace")
	for _, p := range []string{"AGENT.md", "SOUL.md", "skills/brave/SKILL.md", "memory/MEMORY.md"} {
		if _, err := os.Stat(filepath.Join(dws, filepath.FromSlash(p))); err != nil {
			t.Errorf("expected %s seeded: %v", p, err)
		}
	}
	for _, p := range []string{"sessions", "sessions/leak.jsonl", "logs", ".picoclaw.pid"} {
		if _, err := os.Stat(filepath.Join(dws, filepath.FromSlash(p))); err == nil {
			t.Errorf("%s must NEVER be seeded (isolation invariant)", p)
		}
	}
	if _, err := os.Stat(filepath.Join(dws, "USER.md")); err == nil {
		t.Error("USER.md absent in template must not appear (partial template)")
	}
}

func TestProvisionFirstSeedsWorkspace(t *testing.T) {
	tmpl := t.TempDir()
	mustWrite(t, filepath.Join(tmpl, "config.json"), `{"agents":{"defaults":{}}}`)
	mustWrite(t, filepath.Join(tmpl, ".security.yml"),
		"channel_list:\n  pico:\n    settings:\n      token: tok\n")
	mustWrite(t, filepath.Join(tmpl, "workspace", "AGENT.md"), "persona")
	mustWrite(t, filepath.Join(tmpl, "workspace", "sessions", "leak.jsonl"), "LEAK")

	userDir := filepath.Join(t.TempDir(), "u")
	storeDir := t.TempDir()
	// user "" so chownTree is a no-op (this test does not run as root).
	tok, err := provision(userDir, tmpl, storeDir, "/data", "", WorkspaceKey{}, "e@x")
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	// seedPicoToken always mints a fresh token on first provision, regardless of
	// whatever placeholder the template shipped.
	if !strings.HasPrefix(tok, "pico-") {
		t.Errorf("token = %q, want a pico- prefixed random token", tok)
	}
	if _, err := os.Stat(filepath.Join(userDir, "workspace", "AGENT.md")); err != nil {
		t.Errorf("AGENT.md not seeded on first provision: %v", err)
	}
	if _, err := os.Stat(filepath.Join(userDir, "workspace", "sessions")); err == nil {
		t.Error("sessions/ leaked into the user workspace")
	}
}

func TestProvisionReturningUserNotReseeded(t *testing.T) {
	userDir := t.TempDir()
	// A returning user: config.json present + an evolved AGENT.md.
	mustWrite(t, filepath.Join(userDir, "config.json"), "{}")
	mustWrite(t, filepath.Join(userDir, ".security.yml"),
		"channel_list:\n  pico:\n    settings:\n      token: tok\n")
	mustWrite(t, filepath.Join(userDir, "workspace", "AGENT.md"), "EVOLVED")

	tmpl := t.TempDir()
	mustWrite(t, filepath.Join(tmpl, "config.json"), "{}")
	mustWrite(t, filepath.Join(tmpl, ".security.yml"),
		"channel_list:\n  pico:\n    settings:\n      token: tmpltok\n")
	mustWrite(t, filepath.Join(tmpl, "workspace", "AGENT.md"), "TEMPLATE-DEFAULT")

	storeDir := t.TempDir()
	if _, err := provision(userDir, tmpl, storeDir, "/data", "", WorkspaceKey{}, ""); err != nil {
		t.Fatalf("provision: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(userDir, "workspace", "AGENT.md"))
	if string(raw) != "EVOLVED" {
		t.Errorf("returning user's AGENT.md was clobbered: %q", raw)
	}
}
