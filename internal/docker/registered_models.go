package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
)

// ErrWorkspaceNotProvisioned is returned when applying a model to a user who
// has no provisioned workspace yet (config.json only exists after first chat).
var ErrWorkspaceNotProvisioned = errors.New("workspace not provisioned yet — the user must start a chat first")

// ErrRegisteredModelNotFound is returned when applying/deleting a model that is
// not in the agent's registry.
var ErrRegisteredModelNotFound = errors.New("registered model not found")

// RegisteredModel is an admin-registered model for an agent: the full picoclaw
// model_list definition plus the api key. Stored on disk (per agent) because
// admins register it via the UI rather than via an environment variable.
type RegisteredModel struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Model    string `json:"model"`
	APIBase  string `json:"api_base"`
	APIKey   string `json:"api_key,omitempty"`
	HasKey   bool   `json:"has_key"`
}

func (m *Manager) registeredModelsPath(agentKey string) string {
	return filepath.Join(m.cfg.ContainerDataRoot, "registered-models", identity.SanitizeID(agentKey)+".json")
}

func (m *Manager) readRegisteredModels(agentKey string) ([]RegisteredModel, error) {
	raw, err := os.ReadFile(m.registeredModelsPath(agentKey))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []RegisteredModel
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse registered models: %w", err)
	}
	return out, nil
}

func (m *Manager) writeRegisteredModelsFile(agentKey string, models []RegisteredModel) error {
	path := m.registeredModelsPath(agentKey)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	out, err := json.MarshalIndent(models, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return err
	}
	return chownTree(filepath.Dir(path), m.cfg.PicoclawUser)
}

// ListRegisteredModels returns the agent's registered models, NEVER the keys
// (only HasKey). Safe to surface to the client.
func (m *Manager) ListRegisteredModels(agentKey string) ([]RegisteredModel, error) {
	models, err := m.readRegisteredModels(agentKey)
	if err != nil {
		return nil, err
	}
	for i := range models {
		models[i].HasKey = models[i].APIKey != ""
		models[i].APIKey = ""
	}
	return models, nil
}

// AddRegisteredModel upserts a model (definition + key) into the agent registry,
// keyed by (provider, name).
func (m *Manager) AddRegisteredModel(agentKey string, rm RegisteredModel) error {
	models, err := m.readRegisteredModels(agentKey)
	if err != nil {
		return err
	}
	replaced := false
	for i := range models {
		if models[i].Provider == rm.Provider && models[i].Name == rm.Name {
			models[i] = rm
			replaced = true
			break
		}
	}
	if !replaced {
		models = append(models, rm)
	}
	return m.writeRegisteredModelsFile(agentKey, models)
}

// DeleteRegisteredModel removes a model from the agent registry (idempotent).
func (m *Manager) DeleteRegisteredModel(agentKey, provider, name string) error {
	models, err := m.readRegisteredModels(agentKey)
	if err != nil {
		return err
	}
	out := make([]RegisteredModel, 0, len(models))
	for _, rm := range models {
		if rm.Provider == provider && rm.Name == name {
			continue
		}
		out = append(out, rm)
	}
	return m.writeRegisteredModelsFile(agentKey, out)
}

// ApplyRegisteredModelToUser writes a registered model's definition into the
// target user's config.json model_list, its key into .security.yml, and sets it
// as the active model (agents.defaults). Requires a provisioned workspace. The
// caller restarts the container afterward so picoclaw reloads it.
func (m *Manager) ApplyRegisteredModelToUser(agentKey string, key WorkspaceKey, provider, name string) error {
	models, err := m.readRegisteredModels(agentKey)
	if err != nil {
		return err
	}
	var rm *RegisteredModel
	for i := range models {
		if models[i].Provider == provider && models[i].Name == name {
			rm = &models[i]
			break
		}
	}
	if rm == nil {
		return ErrRegisteredModelNotFound
	}

	userDir := config.UserWorkspace(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	configPath := filepath.Join(userDir, "config.json")
	secPath := filepath.Join(userDir, ".security.yml")
	if _, statErr := os.Stat(configPath); statErr != nil {
		return ErrWorkspaceNotProvisioned
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config.json: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse config.json: %w", err)
	}
	list, _ := cfg["model_list"].([]any)
	// picoclaw/litellm reads the model's key from the config.json model_list
	// entry (the template ships a "sk-dummy-placeholder" there), NOT from
	// .security.yml — so the real key MUST go here or the provider 401s.
	entry := map[string]any{
		"model_name": rm.Name, "provider": rm.Provider, "model": rm.Model,
		"api_base": rm.APIBase, "api_key": rm.APIKey, "enabled": true,
	}
	replaced := false
	for i, item := range list {
		if mm, ok := item.(map[string]any); ok {
			if n, _ := mm["model_name"].(string); n == rm.Name {
				list[i] = entry
				replaced = true
				break
			}
		}
	}
	if !replaced {
		list = append(list, entry)
	}
	cfg["model_list"] = list
	if agents, ok := cfg["agents"].(map[string]any); ok {
		if defaults, ok := agents["defaults"].(map[string]any); ok {
			defaults["provider"] = rm.Provider
			defaults["model_name"] = rm.Name
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
	setModelListEntry(sec, rm.Name, rm.APIKey)
	if err := writeSecurityConfig(secPath, sec, m.cfg.PicoclawUser); err != nil {
		return fmt.Errorf("write .security.yml: %w", err)
	}
	return chownTree(userDir, m.cfg.PicoclawUser)
}
