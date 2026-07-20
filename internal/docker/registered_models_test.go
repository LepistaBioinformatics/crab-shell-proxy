package docker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

func TestRegisteredModelsRoundTrip(t *testing.T) {
	root := t.TempDir()
	m := &Manager{cfg: &config.Config{ContainerDataRoot: root, PicoclawUser: ""}}

	// Add a model to alpha's registry.
	rm := RegisteredModel{Provider: "openai", Name: "gpt-5.4", Model: "gpt-5.4", APIBase: "https://api.openai.com/v1", APIKey: "sk-secret"}
	if err := m.AddRegisteredModel("alpha", rm); err != nil {
		t.Fatalf("add: %v", err)
	}

	// List never exposes the key, reports has_key.
	models, err := m.ListRegisteredModels("alpha")
	if err != nil || len(models) != 1 {
		t.Fatalf("list: %v models=%+v", err, models)
	}
	if models[0].APIKey != "" || !models[0].HasKey {
		t.Errorf("list must hide key + report has_key: %+v", models[0])
	}

	// Registry is per-agent: beta is empty.
	if bm, _ := m.ListRegisteredModels("beta"); len(bm) != 0 {
		t.Errorf("beta registry should be empty, got %+v", bm)
	}

	// Applying to an unprovisioned user fails clearly.
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	if err := m.ApplyRegisteredModelToUser("alpha", key, "openai", "gpt-5.4"); !errors.Is(err, ErrWorkspaceNotProvisioned) {
		t.Fatalf("apply unprovisioned: want ErrWorkspaceNotProvisioned, got %v", err)
	}

	// Provision a minimal workspace, then apply.
	userDir := config.UserWorkspace(root, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"agents":     map[string]any{"defaults": map[string]any{"provider": "", "model_name": ""}},
		"model_list": []any{},
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(filepath.Join(userDir, "config.json"), raw, 0o600)
	os.WriteFile(filepath.Join(userDir, ".security.yml"), []byte("channel_list:\n  pico:\n    settings:\n      token: t\n"), 0o600)

	if err := m.ApplyRegisteredModelToUser("alpha", key, "openai", "gpt-5.4"); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// config.json now has the definition + active; .security.yml has the key.
	outRaw, _ := os.ReadFile(filepath.Join(userDir, "config.json"))
	var out map[string]any
	json.Unmarshal(outRaw, &out)
	defs := out["agents"].(map[string]any)["defaults"].(map[string]any)
	if defs["model_name"] != "gpt-5.4" || defs["provider"] != "openai" {
		t.Errorf("active model not set: %+v", defs)
	}
	list := out["model_list"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["api_base"] != "https://api.openai.com/v1" {
		t.Errorf("model definition not written: %+v", list)
	}
	sec, _ := readSecurityConfig(filepath.Join(userDir, ".security.yml"))
	ml := sec["model_list"].(map[string]any)["gpt-5.4"].(map[string]any)
	if ak, _ := ml["api_keys"].([]any); len(ak) != 1 || ak[0] != "sk-secret" {
		t.Errorf("key not written to .security.yml: %+v", ml)
	}

	// Delete removes it from the registry.
	if err := m.DeleteRegisteredModel("alpha", "openai", "gpt-5.4"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if models, _ := m.ListRegisteredModels("alpha"); len(models) != 0 {
		t.Errorf("registry should be empty after delete: %+v", models)
	}

	// Applying a non-registered model errors.
	if err := m.ApplyRegisteredModelToUser("alpha", key, "openai", "gone"); !errors.Is(err, ErrRegisteredModelNotFound) {
		t.Fatalf("apply missing: want ErrRegisteredModelNotFound, got %v", err)
	}
}
