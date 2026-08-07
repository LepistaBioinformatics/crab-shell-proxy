package docker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// seedWorkspaceFiles writes a minimal provisioned workspace: a config.json with
// an empty model_list (the new template shape) and a .security.yml holding the
// pico channel token plus a stale model key from a previous model.
func seedWorkspaceFiles(t *testing.T) (dir, configPath, secPath string) {
	t.Helper()
	dir = t.TempDir()
	configPath = filepath.Join(dir, "config.json")
	secPath = filepath.Join(dir, ".security.yml")

	cfg := map[string]any{
		"version":      3,
		"channel_list": map[string]any{"pico": map[string]any{"enabled": false}},
		"agents":       map[string]any{"defaults": map[string]any{"provider": "", "model_name": ""}},
		"model_list":   []any{},
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sec := "channel_list:\n  pico:\n    settings:\n      token: pico-seed\n" +
		"model_list:\n  retired-model:\n    api_keys:\n    - sk-old\n" +
		"web:\n  brave:\n    api_keys:\n    - brave-key\n"
	if err := os.WriteFile(secPath, []byte(sec), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, configPath, secPath
}

func testResolution() registry.Resolution {
	return registry.Resolution{
		Primary: registry.Model{
			ModelName: "main", Provider: "openai", Model: "gpt-5.4",
			APIBase: "https://api.openai.com/v1", APIKey: "sk-main", Status: registry.StatusActive,
			Fallbacks: []string{"backup"},
		},
		Chain: []registry.Model{{
			ModelName: "backup", Provider: "anthropic", Model: "claude-sonnet-4-6",
			APIBase: "https://api.anthropic.com/v1", APIKey: "sk-backup", Status: registry.StatusActive,
		}},
	}
}

func readConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestMaterializeWritesModelListWithoutAnyAPIKey(t *testing.T) {
	_, configPath, secPath := seedWorkspaceFiles(t)

	if err := materializeModels(configPath, secPath, testResolution(), projectList{}); err != nil {
		t.Fatalf("materializeModels: %v", err)
	}

	cfg := readConfig(t, configPath)
	list, ok := cfg["model_list"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("model_list = %#v, want 2 entries", cfg["model_list"])
	}
	for i, item := range list {
		entry := item.(map[string]any)
		// picoclaw removed api_key (singular) from config.json in schema V2+ and
		// ignores it; the template is version 3. A key here is dead weight that
		// also leaks the secret into a file with looser handling than .security.yml.
		if _, present := entry["api_key"]; present {
			t.Errorf("model_list[%d] carries api_key: %#v", i, entry)
		}
		if entry["enabled"] != true {
			t.Errorf("model_list[%d].enabled = %#v, want true", i, entry["enabled"])
		}
	}
	first := list[0].(map[string]any)
	if first["model_name"] != "main" || first["provider"] != "openai" || first["api_base"] != "https://api.openai.com/v1" {
		t.Errorf("primary entry = %#v", first)
	}
}

func TestMaterializeSetsDefaultsAndFallbackOrder(t *testing.T) {
	_, configPath, secPath := seedWorkspaceFiles(t)

	if err := materializeModels(configPath, secPath, testResolution(), projectList{}); err != nil {
		t.Fatalf("materializeModels: %v", err)
	}

	cfg := readConfig(t, configPath)
	defaults := cfg["agents"].(map[string]any)["defaults"].(map[string]any)
	if defaults["provider"] != "openai" || defaults["model_name"] != "main" {
		t.Errorf("defaults = %#v", defaults)
	}
	fb, ok := defaults["model_fallbacks"].([]any)
	if !ok || len(fb) != 1 || fb[0] != "backup" {
		t.Errorf("model_fallbacks = %#v, want [backup]", defaults["model_fallbacks"])
	}
	// The pico channel must be enabled or the proxy cannot reach picoclaw at all.
	pico := cfg["channel_list"].(map[string]any)["pico"].(map[string]any)
	if pico["enabled"] != true {
		t.Errorf("channel_list.pico.enabled = %#v, want true", pico["enabled"])
	}
}

func TestMaterializeWritesKeysToSecurityAndPrunesStaleOnes(t *testing.T) {
	_, configPath, secPath := seedWorkspaceFiles(t)

	if err := materializeModels(configPath, secPath, testResolution(), projectList{}); err != nil {
		t.Fatalf("materializeModels: %v", err)
	}

	sec, err := readSecurityConfig(secPath)
	if err != nil {
		t.Fatalf("readSecurityConfig: %v", err)
	}
	ml, ok := sec["model_list"].(map[string]any)
	if !ok {
		t.Fatalf("model_list = %#v", sec["model_list"])
	}
	for name, wantKey := range map[string]string{"main": "sk-main", "backup": "sk-backup"} {
		entry, ok := ml[name].(map[string]any)
		if !ok {
			t.Fatalf("model_list.%s = %#v", name, ml[name])
		}
		keys, ok := entry["api_keys"].([]any)
		if !ok || len(keys) != 1 || keys[0] != wantKey {
			t.Errorf("model_list.%s.api_keys = %#v, want [%s]", name, entry["api_keys"], wantKey)
		}
	}
	// config.json's model_list is replaced wholesale while this file is
	// read-modify-write, so without pruning every model a workspace ever used
	// keeps its key here forever and the two files drift permanently.
	if _, present := ml["retired-model"]; present {
		t.Errorf("stale model key was not pruned: %#v", ml)
	}
	// Pruning must not reach past model_list.
	tok := sec["channel_list"].(map[string]any)["pico"].(map[string]any)["settings"].(map[string]any)["token"]
	if tok != "pico-seed" {
		t.Errorf("pico token = %#v, want pico-seed preserved", tok)
	}
	if _, present := sec["web"]; !present {
		t.Error("web.* family was removed; pruning must be scoped to model_list")
	}
}

func TestMaterializeIsIdempotent(t *testing.T) {
	_, configPath, secPath := seedWorkspaceFiles(t)
	res := testResolution()

	if err := materializeModels(configPath, secPath, res, projectList{}); err != nil {
		t.Fatalf("first: %v", err)
	}
	firstCfg, _ := os.ReadFile(configPath)
	firstSec, _ := os.ReadFile(secPath)

	if err := materializeModels(configPath, secPath, res, projectList{}); err != nil {
		t.Fatalf("second: %v", err)
	}
	secondCfg, _ := os.ReadFile(configPath)
	secondSec, _ := os.ReadFile(secPath)

	if string(firstCfg) != string(secondCfg) {
		t.Error("config.json changed on a repeat materialization")
	}
	if string(firstSec) != string(secondSec) {
		t.Error(".security.yml changed on a repeat materialization")
	}
}

func TestMaterializeCarriesOptionalFieldsOnlyWhenSet(t *testing.T) {
	_, configPath, secPath := seedWorkspaceFiles(t)
	res := registry.Resolution{Primary: registry.Model{
		ModelName: "oauth-model", Provider: "antigravity", Model: "gemini-3-flash",
		AuthMethod: "oauth", APIKey: "", Status: registry.StatusActive,
		ExtraBody: json.RawMessage(`{"reasoning_split":true}`),
	}}

	if err := materializeModels(configPath, secPath, res, projectList{}); err != nil {
		t.Fatalf("materializeModels: %v", err)
	}

	entry := readConfig(t, configPath)["model_list"].([]any)[0].(map[string]any)
	if entry["auth_method"] != "oauth" {
		t.Errorf("auth_method = %#v, want oauth", entry["auth_method"])
	}
	if _, present := entry["api_base"]; present {
		t.Errorf("api_base must be omitted when empty: %#v", entry)
	}
	eb, ok := entry["extra_body"].(map[string]any)
	if !ok || eb["reasoning_split"] != true {
		t.Errorf("extra_body = %#v", entry["extra_body"])
	}
	// A model with no key must not write an empty api_keys array, which picoclaw
	// would read as a configured-but-blank credential.
	sec, _ := readSecurityConfig(secPath)
	if ml, ok := sec["model_list"].(map[string]any); ok {
		if _, present := ml["oauth-model"]; present {
			t.Errorf("keyless model got a .security.yml entry: %#v", ml)
		}
	}
}

// TestMaterializeLeavesEveryIntermediateStateBootable pins the write ORDER down.
// Neither obvious order is fail-closed: config.json first names a model whose key
// is not in .security.yml yet, and pruning .security.yml first strips the OLD
// model's key while config.json still names it. So the sequence writes old ∪ new
// keys, then config.json, then prunes — and a failure at the config.json write
// must leave the workspace running its OLD model with its key still present.
//
// Skipped as root (the Docker gate), where file mode does not gate the owner's
// own write — the same environment caveat
// TestMigrateLogsAnUnreadableWorkspaceConfigInsteadOfSkippingSilently carries.
func TestMaterializeLeavesEveryIntermediateStateBootable(t *testing.T) {
	_, configPath, secPath := seedWorkspaceFiles(t)
	if err := os.Chmod(configPath, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(configPath, 0o600) })
	if f, err := os.OpenFile(configPath, os.O_WRONLY, 0); err == nil {
		_ = f.Close()
		t.Skip("chmod 444 does not block this uid's write (running as root); the order is unobservable here")
	}

	err := materializeModels(configPath, secPath, testResolution(), projectList{})
	if err == nil {
		t.Fatal("materializeModels succeeded despite an unwritable config.json")
	}

	sec, rerr := readSecurityConfig(secPath)
	if rerr != nil {
		t.Fatalf("readSecurityConfig: %v", rerr)
	}
	ml, ok := sec["model_list"].(map[string]any)
	if !ok {
		t.Fatalf("model_list = %#v", sec["model_list"])
	}
	// The old model config.json still names must keep its key: pruning it here is
	// exactly as unbootable as naming a model with no key.
	if _, present := ml["retired-model"]; !present {
		t.Errorf("the outgoing model's key was pruned before config.json stopped naming it: %#v", ml)
	}
	// And the incoming one's key is already there, so the retried write completes a
	// state that was never inconsistent.
	if _, present := ml["main"]; !present {
		t.Errorf("the incoming model's key was not written before config.json: %#v", ml)
	}
}

// TestMaterializeCreatesAMissingAgentsDefaults covers the same ok-guard on the
// workspace side, where the consequence is worse: a correct model_list and NO
// active model, silently — picoclaw then boots with no model, which is the failure
// mode this feature exists to remove.
func TestMaterializeCreatesAMissingAgentsDefaults(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	secPath := filepath.Join(dir, ".security.yml")
	if err := os.WriteFile(configPath, []byte(`{"version":3,"model_list":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secPath, []byte("channel_list:\n  pico:\n    settings:\n      token: t\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := materializeModels(configPath, secPath, testResolution(), projectList{}); err != nil {
		t.Fatalf("materializeModels: %v", err)
	}

	defaults, ok := readConfig(t, configPath)["agents"].(map[string]any)["defaults"].(map[string]any)
	if !ok {
		t.Fatal("agents.defaults was not created")
	}
	if defaults["model_name"] != "main" || defaults["provider"] != "openai" {
		t.Errorf("defaults = %#v, want the resolved primary", defaults)
	}
}
