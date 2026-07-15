package docker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sgelias/crab-shell-proxy/internal/config"
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

func TestApplyModel(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	secPath := filepath.Join(dir, ".security.yml")
	os.WriteFile(cfgPath, []byte(`{
	  "channel_list":{"pico":{"enabled":false,"settings":{}}},
	  "agents":{"defaults":{"provider":"","model_name":""}}
	}`), 0o600)
	os.WriteFile(secPath, []byte("channel_list:\n  pico:\n    settings: {}\n"), 0o600)

	model := &config.ModelConfig{Provider: "deepseek", Name: "deepseek-chat", APIKey: "sk-xyz"}
	if err := applyModel(cfgPath, secPath, model); err != nil {
		t.Fatalf("applyModel: %v", err)
	}

	// config.json: pico enabled + provider/model set.
	var d map[string]any
	raw, _ := os.ReadFile(cfgPath)
	json.Unmarshal(raw, &d)
	pico := d["channel_list"].(map[string]any)["pico"].(map[string]any)
	if pico["enabled"] != true {
		t.Error("pico not enabled")
	}
	defs := d["agents"].(map[string]any)["defaults"].(map[string]any)
	if defs["provider"] != "deepseek" || defs["model_name"] != "deepseek-chat" {
		t.Errorf("provider/model = %v/%v", defs["provider"], defs["model_name"])
	}

	// .security.yml: fresh pico token + the api key from env.
	tok, err := readPicoToken(secPath)
	if err != nil || tok == "" {
		t.Fatalf("pico token missing after applyModel: %q err=%v", tok, err)
	}
	sec, _ := os.ReadFile(secPath)
	if !contains(string(sec), "sk-xyz") {
		t.Errorf(".security.yml missing api key:\n%s", sec)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
