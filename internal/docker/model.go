package docker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
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

// ModelTarget identifies where a model-override read/write/clear applies
// (admin-model-override HTTP API): tenant scope, subscription scope, or — when
// UserAccID is set — one specific user's override within that subscription.
// Role is the caller's own agent key; it is required only for a user target
// (the per-user override file lives inside that agent's workspace).
type ModelTarget struct {
	Kind      ScopeKind
	TenantID  string
	SubsAccID string
	Role      string
	UserAccID string
}

// modelOverridePath resolves the on-disk override file a target reads/writes.
func (m *Manager) modelOverridePath(t ModelTarget) string {
	root := m.cfg.ContainerDataRoot
	if t.UserAccID != "" {
		return config.UserModelOverrideFile(root, t.TenantID, t.SubsAccID, t.Role, t.UserAccID)
	}
	if t.Kind == ScopeSubscription {
		return config.SubscriptionModelOverrideFile(root, t.TenantID, t.SubsAccID)
	}
	return config.TenantModelOverrideFile(root, t.TenantID)
}

// SetModelOverride writes a model selection at a target. The caller (the HTTP
// handler) must have already validated sel against the agent's
// SelectableModels — this layer only persists it (CTX-AMO-06: sel never
// carries a key).
func (m *Manager) SetModelOverride(t ModelTarget, sel ModelSel) error {
	return m.setModelOverride(m.modelOverridePath(t), sel)
}

// ClearModelOverride removes a model override at a target (idempotent).
func (m *Manager) ClearModelOverride(t ModelTarget) error {
	return m.clearModelOverride(m.modelOverridePath(t))
}

// EffectiveModel resolves the model in effect at a target — the same user >
// subscription > tenant > default precedence resolveModel applies to a
// workspace, but starting only from the target's own level downward (e.g. a
// tenant-scope query never reads a subscription or user file) — and reports
// which level actually set it: "tenant" | "subscription" | "user" | "default".
// NEVER exposes an api key (CTX-AMO-06): callers must read only
// Provider/Name off the returned *config.ModelConfig.
func (m *Manager) EffectiveModel(agent config.Agent, t ModelTarget) (*config.ModelConfig, string) {
	root := m.cfg.ContainerDataRoot
	type step struct{ level, path string }
	var steps []step
	if t.UserAccID != "" {
		steps = append(steps, step{"user", config.UserModelOverrideFile(root, t.TenantID, t.SubsAccID, t.Role, t.UserAccID)})
	}
	if t.UserAccID != "" || t.Kind == ScopeSubscription {
		steps = append(steps, step{"subscription", config.SubscriptionModelOverrideFile(root, t.TenantID, t.SubsAccID)})
	}
	steps = append(steps, step{"tenant", config.TenantModelOverrideFile(root, t.TenantID)})
	for _, st := range steps {
		sel, err := m.getModelOverride(st.path)
		if err != nil {
			m.logf("effective model: reading override %s: %v (skipping)", st.path, err)
			continue
		}
		if sel == nil {
			continue
		}
		if mc := agent.FindModel(sel.Provider, sel.Name); mc != nil {
			return mc, st.level
		}
		m.logf("effective model: override {provider:%q, name:%q} at %s no longer selectable, falling back",
			sel.Provider, sel.Name, st.path)
	}
	return agent.Model, "default"
}

// ReapplyModelScope re-applies the resolved model to every ESTABLISHED
// (already-provisioned) workspace under scope, then restarts the scope's
// running containers so picoclaw reloads it. A workspace that has never been
// provisioned (no config.json yet) is skipped — resolveModel already applies
// automatically at its first provision. Per-workspace failures are logged and
// skipped, not returned, so one bad workspace doesn't block the restart pass
// for the others (mirrors RestartScope's own best-effort contract).
func (m *Manager) ReapplyModelScope(scope Scope) error {
	keys, err := m.scopeWorkspaceKeys(scope)
	if err != nil {
		return err
	}
	for _, key := range keys {
		agent, ok := m.cfg.Agents[key.Role]
		if !ok {
			m.logf("reapply model scope: unknown agent role %q for workspace %+v, skipping", key.Role, key)
			continue
		}
		if err := m.reapplyModelToWorkspace(key, agent); err != nil {
			m.logf("reapply model scope: workspace %+v: %v", key, err)
		}
	}
	return m.RestartScope(scope)
}

// ReapplyModelUser re-applies the resolved model to one user's established
// workspace, then restarts it if running (RestartWorkspace is itself a no-op
// when the container isn't running — the next cold start picks up the change
// already written to disk).
func (m *Manager) ReapplyModelUser(key WorkspaceKey, agent config.Agent) error {
	if err := m.reapplyModelToWorkspace(key, agent); err != nil {
		return err
	}
	return m.RestartWorkspace(key)
}

// reapplyModelToWorkspace re-applies agent's resolved model to key's workspace
// on disk, if (and only if) it has already been provisioned, chowning it back
// to the picoclaw user afterward (reapplyModel itself does not chown, so a
// non-root workspace must not end up root-owned).
func (m *Manager) reapplyModelToWorkspace(key WorkspaceKey, agent config.Agent) error {
	userDir := config.UserWorkspace(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if _, err := os.Stat(filepath.Join(userDir, "config.json")); err != nil {
		return nil // not provisioned yet — nothing to reapply
	}
	model := m.resolveModel(agent, key)
	if err := reapplyModel(userDir, model); err != nil {
		return err
	}
	return chownTree(userDir, m.cfg.PicoclawUser)
}

// scopeWorkspaceKeys enumerates every discovered WorkspaceKey under scope:
// ListSubscriptionUsers for a single subscription, or the
// tenants/<t>/subscriptions/*/agents/*/users/* glob (mirroring reconcile.go's
// existingWorkspaces pattern) for every subscription/agent/user under a whole
// tenant.
func (m *Manager) scopeWorkspaceKeys(scope Scope) ([]WorkspaceKey, error) {
	if scope.Kind == ScopeSubscription {
		users, err := m.ListSubscriptionUsers(scope.TenantID, scope.SubsAccID)
		if err != nil {
			return nil, err
		}
		keys := make([]WorkspaceKey, 0, len(users))
		for _, u := range users {
			keys = append(keys, WorkspaceKey{TenantID: scope.TenantID, SubsAccID: scope.SubsAccID, Role: u.Role, UserAccID: u.AccID})
		}
		return keys, nil
	}
	pattern := filepath.Join(m.cfg.ContainerDataRoot, "tenants", identity.SanitizeID(scope.TenantID),
		"subscriptions", "*", "agents", "*", "users", "*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	var keys []WorkspaceKey
	for _, uw := range matches {
		fi, statErr := os.Stat(uw)
		if statErr != nil || !fi.IsDir() {
			continue
		}
		rel, err := filepath.Rel(m.cfg.ContainerDataRoot, uw)
		if err != nil {
			continue
		}
		// rel = tenants/<t>/subscriptions/<s>/agents/<role>/users/<u>
		parts := strings.Split(rel, string(os.PathSeparator))
		if len(parts) != 8 {
			continue
		}
		keys = append(keys, WorkspaceKey{TenantID: parts[1], SubsAccID: parts[3], Role: parts[5], UserAccID: parts[7]})
	}
	return keys, nil
}
