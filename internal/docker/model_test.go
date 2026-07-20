package docker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// modelManager builds a Manager over a temp root with no chown (PicoclawUser
// empty) so the filesystem-only model-override methods run unprivileged,
// mirroring sharedManager in shared_test.go.
func modelManager(t *testing.T) *Manager {
	t.Helper()
	cfg := &config.Config{
		HostDataRoot:      "/host/data",
		ContainerDataRoot: t.TempDir(),
		ContainerPrefix:   "picoclaw",
	}
	return NewManager(cfg, nil, func(context.Context, string, int) error { return nil }, nil)
}

func TestModelOverrideRoundTrip(t *testing.T) {
	m := modelManager(t)
	path := filepath.Join(m.cfg.ContainerDataRoot, "tenants", "t1", "shared", "model.json")

	sel, err := m.getModelOverride(path)
	if err != nil || sel != nil {
		t.Fatalf("absent override should be (nil, nil), got (%v, %v)", sel, err)
	}

	want := ModelSel{Provider: "openai", Name: "gpt-4o"}
	if err := m.setModelOverride(path, want); err != nil {
		t.Fatalf("setModelOverride: %v", err)
	}
	got, err := m.getModelOverride(path)
	if err != nil {
		t.Fatalf("getModelOverride after set: %v", err)
	}
	if got == nil || *got != want {
		t.Fatalf("getModelOverride = %v, want %v", got, want)
	}

	if err := m.clearModelOverride(path); err != nil {
		t.Fatalf("clearModelOverride: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("override file should be gone after clear, stat err = %v", err)
	}
	// Idempotent.
	if err := m.clearModelOverride(path); err != nil {
		t.Fatalf("clearModelOverride on already-absent file should not error: %v", err)
	}
}

func TestGetModelOverrideMalformedErrors(t *testing.T) {
	m := modelManager(t)
	path := filepath.Join(m.cfg.ContainerDataRoot, "malformed.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.getModelOverride(path); err == nil {
		t.Fatal("malformed override file should error")
	}
}

func testAgent() config.Agent {
	return config.Agent{
		Key:   "alpha",
		Model: &config.ModelConfig{Provider: "deepseek", Name: "deepseek-chat", APIKey: "sk-default"},
		Models: []*config.ModelConfig{
			{Provider: "openai", Name: "gpt-4o", APIKey: "sk-openai"},
			{Provider: "anthropic", Name: "claude-sonnet", APIKey: "sk-anthropic"},
		},
	}
}

func TestResolveModelPrecedence(t *testing.T) {
	m := modelManager(t)
	agent := testAgent()
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	root := m.cfg.ContainerDataRoot

	tenantPath := config.TenantModelOverrideFile(root, key.TenantID)
	subPath := config.SubscriptionModelOverrideFile(root, key.TenantID, key.SubsAccID)
	userPath := config.UserModelOverrideFile(root, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)

	// No overrides at all -> agent default.
	if got := m.resolveModel(agent, key); got != agent.Model {
		t.Fatalf("no override should resolve to agent.Model, got %v", got)
	}

	// tenant -> X (openai/gpt-4o)
	if err := m.setModelOverride(tenantPath, ModelSel{Provider: "openai", Name: "gpt-4o"}); err != nil {
		t.Fatal(err)
	}
	if got := m.resolveModel(agent, key); got == nil || got.Name != "gpt-4o" {
		t.Fatalf("tenant override should resolve to gpt-4o, got %v", got)
	}

	// sub -> Y (anthropic/claude-sonnet) overrides tenant
	if err := m.setModelOverride(subPath, ModelSel{Provider: "anthropic", Name: "claude-sonnet"}); err != nil {
		t.Fatal(err)
	}
	if got := m.resolveModel(agent, key); got == nil || got.Name != "claude-sonnet" {
		t.Fatalf("sub override should win over tenant, got %v", got)
	}

	// user -> Z (deepseek/deepseek-chat, same as default here for a distinct Z
	// use a third selectable) overrides sub
	if err := m.setModelOverride(userPath, ModelSel{Provider: "deepseek", Name: "deepseek-chat"}); err != nil {
		t.Fatal(err)
	}
	if got := m.resolveModel(agent, key); got == nil || got.Name != "deepseek-chat" {
		t.Fatalf("user override should win over sub, got %v", got)
	}

	// remove user -> falls back to sub (claude-sonnet)
	if err := m.clearModelOverride(userPath); err != nil {
		t.Fatal(err)
	}
	if got := m.resolveModel(agent, key); got == nil || got.Name != "claude-sonnet" {
		t.Fatalf("after clearing user, should fall back to sub, got %v", got)
	}

	// remove sub -> falls back to tenant (gpt-4o)
	if err := m.clearModelOverride(subPath); err != nil {
		t.Fatal(err)
	}
	if got := m.resolveModel(agent, key); got == nil || got.Name != "gpt-4o" {
		t.Fatalf("after clearing sub, should fall back to tenant, got %v", got)
	}

	// remove tenant -> falls back to agent.Model default
	if err := m.clearModelOverride(tenantPath); err != nil {
		t.Fatal(err)
	}
	if got := m.resolveModel(agent, key); got != agent.Model {
		t.Fatalf("after clearing tenant, should fall back to agent.Model default, got %v", got)
	}
}

func TestResolveModelStaleOverrideFallsThrough(t *testing.T) {
	m := modelManager(t)
	agent := testAgent()
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	root := m.cfg.ContainerDataRoot

	tenantPath := config.TenantModelOverrideFile(root, key.TenantID)
	userPath := config.UserModelOverrideFile(root, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)

	// Tenant sets a legitimate override.
	if err := m.setModelOverride(tenantPath, ModelSel{Provider: "openai", Name: "gpt-4o"}); err != nil {
		t.Fatal(err)
	}
	// User has a stale override pointing at a model no longer in the allowlist.
	if err := m.setModelOverride(userPath, ModelSel{Provider: "mistral", Name: "no-longer-selectable"}); err != nil {
		t.Fatal(err)
	}

	got := m.resolveModel(agent, key)
	if got == nil || got.Name != "gpt-4o" {
		t.Fatalf("stale user override should fall through to tenant override, got %v", got)
	}
}

// writeTestConfigJSON writes a minimal config.json with agents.defaults, as
// left behind by a first provision (see applyModel in provision.go).
func writeTestConfigJSON(t *testing.T, dir, provider, modelName string) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	body := map[string]any{
		"channel_list": map[string]any{
			"pico": map[string]any{"enabled": true, "settings": map[string]any{}},
		},
		"agents": map[string]any{
			"defaults": map[string]any{
				"provider":   provider,
				"model_name": modelName,
				"workspace":  "/some/other/field/must/survive",
			},
		},
	}
	b, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReapplyModelPreservesTokenAndSecrets(t *testing.T) {
	dir := t.TempDir()
	writeTestConfigJSON(t, dir, "deepseek", "deepseek-chat")

	secPath := filepath.Join(dir, ".security.yml")
	secBody := "channel_list:\n  pico:\n    settings:\n      token: pico-existing-token\n" +
		"model_list:\n  legacy-model:\n    api_keys:\n      - legacy-key-xyz\n"
	if err := os.WriteFile(secPath, []byte(secBody), 0o600); err != nil {
		t.Fatal(err)
	}

	newModel := &config.ModelConfig{Provider: "openai", Name: "gpt-4o", APIKey: "sk-new-openai"}
	if err := reapplyModel(dir, newModel); err != nil {
		t.Fatalf("reapplyModel: %v", err)
	}

	// config.json: provider/model_name updated, other fields survive.
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	defaults := cfg["agents"].(map[string]any)["defaults"].(map[string]any)
	if defaults["provider"] != "openai" || defaults["model_name"] != "gpt-4o" {
		t.Errorf("config.json provider/model_name = %v/%v", defaults["provider"], defaults["model_name"])
	}
	if defaults["workspace"] != "/some/other/field/must/survive" {
		t.Errorf("config.json unrelated field did not survive: %v", defaults["workspace"])
	}

	// .security.yml: new model_list entry present, AND the pico token AND the
	// pre-existing legacy model_list entry are STILL present (the critical
	// assertion — reapplyModel must never regenerate the token or drop
	// pre-existing merged secrets).
	secRaw, err := os.ReadFile(secPath)
	if err != nil {
		t.Fatal(err)
	}
	secStr := string(secRaw)
	if !strings.Contains(secStr, "sk-new-openai") {
		t.Errorf(".security.yml missing new api key:\n%s", secStr)
	}
	if !strings.Contains(secStr, "pico-existing-token") {
		t.Errorf(".security.yml lost the pico token:\n%s", secStr)
	}
	if !strings.Contains(secStr, "legacy-key-xyz") {
		t.Errorf(".security.yml lost the pre-existing legacy model_list secret:\n%s", secStr)
	}

	tok, err := readPicoToken(secPath)
	if err != nil || tok != "pico-existing-token" {
		t.Errorf("pico token changed: got %q err=%v, want unchanged pico-existing-token", tok, err)
	}
}

func TestReapplyModelNilNoOp(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTestConfigJSON(t, dir, "deepseek", "deepseek-chat")
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := reapplyModel(dir, nil); err != nil {
		t.Fatalf("reapplyModel(nil) should no-op without error: %v", err)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("reapplyModel(nil) should not touch config.json")
	}
}
