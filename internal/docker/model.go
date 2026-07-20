package docker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// ModelSel is the on-disk shape of a model override selection: which of the
// agent's SelectableModels a scope/user picked. It never carries an API key
// (CTX-AMO-06) — resolveModel maps it back to the full config.ModelConfig
// (and its key) via the agent's declared allowlist.
type ModelSel struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

// getModelOverride reads a model override file. A missing file is (nil, nil)
// — no override set, not an error. Malformed JSON is an error.
func (m *Manager) getModelOverride(path string) (*ModelSel, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var sel ModelSel
	if err := json.Unmarshal(raw, &sel); err != nil {
		return nil, fmt.Errorf("parse model override %s: %w", path, err)
	}
	return &sel, nil
}

// setModelOverride writes a model override selection, creating the parent dir
// and chowning it to the picoclaw user so a mounted container's view of it
// (where applicable) stays consistent with the rest of the shared tree. The
// caller validates sel against the agent's SelectableModels before calling.
func (m *Manager) setModelOverride(path string, sel ModelSel) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create model override dir: %w", err)
	}
	out, err := json.MarshalIndent(sel, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return err
	}
	return chownTree(dir, m.cfg.PicoclawUser)
}

// clearModelOverride removes a model override file. Missing file is a success
// (idempotent).
func (m *Manager) clearModelOverride(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// resolveModel returns the effective ModelConfig for a workspace: the most
// specific override in the user > subscription > tenant precedence chain that
// still maps to one of the agent's SelectableModels, else agent.Model (which
// may be nil). An override whose {provider,name} is no longer selectable
// (e.g. the operator removed it from config.yaml) is logged and skipped in
// favor of the next level down.
func (m *Manager) resolveModel(agent config.Agent, key WorkspaceKey) *config.ModelConfig {
	root := m.cfg.ContainerDataRoot
	paths := []string{
		config.UserModelOverrideFile(root, key.TenantID, key.SubsAccID, key.Role, key.UserAccID),
		config.SubscriptionModelOverrideFile(root, key.TenantID, key.SubsAccID),
		config.TenantModelOverrideFile(root, key.TenantID),
	}
	for _, path := range paths {
		sel, err := m.getModelOverride(path)
		if err != nil {
			m.logf("resolve model: reading override %s: %v (skipping)", path, err)
			continue
		}
		if sel == nil {
			continue
		}
		if mc := agent.FindModel(sel.Provider, sel.Name); mc != nil {
			return mc
		}
		m.logf("resolve model: override {provider:%q, name:%q} at %s is no longer selectable for agent %q, falling back",
			sel.Provider, sel.Name, path, agent.Key)
	}
	return agent.Model
}

// reapplyModel re-applies a resolved model to an ALREADY-PROVISIONED workspace
// without the destructive overwrite applyModel does on first provision: it
// rewrites only config.json's agents.defaults.provider/model_name, and
// read-modify-writes .security.yml's model_list[model.Name] entry, preserving
// the existing pico channel token and every other existing key (siblings in
// model_list, channel_list, etc.). It does NOT regenerate the pico token. A
// nil model is a no-op.
func reapplyModel(userDir string, model *config.ModelConfig) error {
	if model == nil {
		return nil
	}
	configPath := filepath.Join(userDir, "config.json")
	secPath := filepath.Join(userDir, ".security.yml")

	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config.json: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse config.json: %w", err)
	}
	if agents, ok := cfg["agents"].(map[string]any); ok {
		if defaults, ok := agents["defaults"].(map[string]any); ok {
			defaults["provider"] = model.Provider
			defaults["model_name"] = model.Name
		}
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(configPath, out, 0o600); err != nil {
		return fmt.Errorf("write config.json: %w", err)
	}

	sec, err := readSecurityConfig(secPath)
	if err != nil {
		return fmt.Errorf("read .security.yml: %w", err)
	}
	setModelListEntry(sec, model.Name, model.APIKey)
	// user="" — this bare function has no chown context; the caller (later
	// HTTP-task wiring) re-chowns via its own Manager as needed.
	if err := writeSecurityConfig(secPath, sec, ""); err != nil {
		return fmt.Errorf("write .security.yml: %w", err)
	}
	return nil
}

// setModelListEntry upserts model_list[name] = {api_keys: [apiKey]} into the
// parsed .security.yml, creating model_list only if absent, and leaving every
// other model_list entry and every sibling top-level key untouched.
func setModelListEntry(sec map[string]any, name, apiKey string) {
	ml, ok := sec["model_list"].(map[string]any)
	if !ok {
		ml = map[string]any{}
		sec["model_list"] = ml
	}
	ml[name] = map[string]any{"api_keys": []string{apiKey}}
}
