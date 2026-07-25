package docker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// modelRegistrySchemaVersion marks the inventory as migrated. The marker is
// written LAST, so a failure anywhere leaves the whole pass re-runnable.
const modelRegistrySchemaVersion = 1

// legacyRegisteredModel is the on-disk shape of the deleted registered-models
// store, kept only so the migration can read what it left behind.
type legacyRegisteredModel struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Model    string `json:"model"`
	APIBase  string `json:"api_base"`
	APIKey   string `json:"api_key"`
}

// legacyModelSel is the on-disk shape of an admin-model-override selection file.
type legacyModelSel struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

// migrateModelRegistry seeds the inventory from every pre-existing source and
// records what each existing workspace is currently running.
//
// It only READS workspaces — no workspace's active model changes as a result of
// migrating. Later sources win on model_name collision, because a
// registered-models entry or a live workspace holds a key an admin actually
// entered, whereas the config.yaml seed may name an environment variable that is
// no longer set.
func (m *Manager) migrateModelRegistry() error {
	have, err := m.reg.SchemaVersion()
	if err != nil {
		return err
	}
	if have >= modelRegistrySchemaVersion {
		return nil
	}
	root := m.cfg.ContainerDataRoot

	// 1. config.yaml: declared models, and each agent's default.
	//
	// config.ModelConfig.BaseURL is a hermes-only field and is EMPTY for every
	// picoclaw model — config.yaml never carried an api_base for them, because the
	// template's model_list did. So these records import without one, and picoclaw
	// falls back to its provider default. CreateModelRaw is used precisely because
	// the public API would reject a record with no api_base and no auth_method: the
	// shape is not the proxy's choice, it is what the old config expressed. An admin
	// editing such a record in the UI supplies the api_base then.
	for _, agent := range m.cfg.Agents {
		for _, mc := range agent.SelectableModels() {
			m.importLegacyModel(registry.Model{
				ModelName: mc.Name, Provider: mc.Provider, Model: mc.Name,
				APIBase: mc.BaseURL, APIKey: mc.APIKey, Status: registry.StatusActive,
			})
		}
		if agent.Model != nil && agent.Model.Name != "" {
			key, kerr := registry.ScopeSel{Level: registry.LevelAgent, Agent: agent.Key}.Key()
			if kerr == nil {
				if err := m.reg.SetScopeDefaultRaw(key, agent.Model.Name); err != nil {
					m.logf("migrate models: agent %q default: %v", agent.Key, err)
				}
			}
		}
	}

	// 2. registered-models/<agent>.json — real keys an admin typed.
	entries, _ := filepath.Glob(filepath.Join(root, "registered-models", "*.json"))
	for _, path := range entries {
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			m.logf("migrate models: read %s: %v", path, rerr)
			continue
		}
		var legacy []legacyRegisteredModel
		if jerr := json.Unmarshal(raw, &legacy); jerr != nil {
			m.logf("migrate models: parse %s: %v", path, jerr)
			continue
		}
		for _, l := range legacy {
			m.importLegacyModel(registry.Model{
				ModelName: l.Name, Provider: l.Provider, Model: l.Model,
				APIBase: l.APIBase, APIKey: l.APIKey, Status: registry.StatusActive,
			})
		}
	}

	// 3. Scope override files -> scope defaults.
	m.importOverrideFiles(root)

	// 4. Every existing workspace: capture what it is actually running.
	//
	// Enumerated directly from disk (allExistingWorkspaces), not via
	// m.existingWorkspaces(agent.Key) looped over m.cfg.Agents: an agent removed
	// from config.yaml, or a hermes agent config.Load dropped for missing secrets
	// (DisabledAgents), would otherwise make every workspace under its role
	// invisible to this pass. This is the anti-orphaning step — it must see every
	// workspace that exists, not only the ones the current config still declares.
	for _, key := range m.allExistingWorkspaces() {
		if err := m.captureWorkspaceModel(key); err != nil {
			m.logf("migrate models: capture %+v: %v", key, err)
		}
	}

	m.logf("migrate models: superseded files are no longer read " +
		"(registered-models/*.json, tenants/*/shared/model.json, " +
		"tenants/*/subscriptions/*/shared/model.json, .crab-model.json); " +
		"they are left on disk for rollback")
	return m.reg.SetSchemaVersion(modelRegistrySchemaVersion)
}

// importLegacyModel creates a record unless one already exists with that name.
// Skipping an existing name is what makes the pass safe to re-run and keeps a
// later, better source (a real key) from being overwritten by an earlier one.
func (m *Manager) importLegacyModel(mod registry.Model) {
	if mod.ModelName == "" || mod.Provider == "" {
		return
	}
	if mod.Model == "" {
		mod.Model = mod.ModelName
	}
	if _, err := m.reg.GetModel(mod.ModelName); err == nil {
		// Already present. Fill in a key only if the record has none — a later
		// source holding a real credential should win over an empty one.
		if mod.APIKey != "" {
			if _, uerr := m.reg.UpdateModelRaw(mod.ModelName, func(cur *registry.Model) error {
				if cur.APIKey == "" {
					cur.APIKey = mod.APIKey
				}
				return nil
			}); uerr != nil {
				m.logf("migrate models: backfill key for %q: %v", mod.ModelName, uerr)
			}
		}
		return
	}
	if _, err := m.reg.CreateModelRaw(mod); err != nil {
		m.logf("migrate models: import %q: %v", mod.ModelName, err)
	}
}

// importOverrideFiles reads admin-model-override's selection files into scope
// defaults. A per-user file is handled by captureWorkspaceModel, which knows the
// workspace it belongs to.
func (m *Manager) importOverrideFiles(root string) {
	tenantFiles, _ := filepath.Glob(filepath.Join(root, "tenants", "*", "shared", "model.json"))
	for _, path := range tenantFiles {
		sel, ok := readLegacySel(path)
		if !ok {
			continue
		}
		// tenants/<t>/shared/model.json
		tenant := filepath.Base(filepath.Dir(filepath.Dir(path)))
		key, err := registry.ScopeSel{Level: registry.LevelTenant, TenantID: tenant}.Key()
		if err == nil {
			if serr := m.reg.SetScopeDefaultRaw(key, sel.Name); serr != nil {
				m.logf("migrate models: tenant default %s: %v", tenant, serr)
			}
		}
	}

	subsFiles, _ := filepath.Glob(filepath.Join(root, "tenants", "*", "subscriptions", "*", "shared", "model.json"))
	for _, path := range subsFiles {
		sel, ok := readLegacySel(path)
		if !ok {
			continue
		}
		// tenants/<t>/subscriptions/<s>/shared/model.json
		subs := filepath.Base(filepath.Dir(filepath.Dir(path)))
		tenant := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(path)))))
		key, err := registry.ScopeSel{Level: registry.LevelSubscription, TenantID: tenant, SubsAccID: subs}.Key()
		if err == nil {
			if serr := m.reg.SetScopeDefaultRaw(key, sel.Name); serr != nil {
				m.logf("migrate models: subscription default %s/%s: %v", tenant, subs, serr)
			}
		}
	}
}

func readLegacySel(path string) (legacyModelSel, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return legacyModelSel{}, false
	}
	var sel legacyModelSel
	if err := json.Unmarshal(raw, &sel); err != nil || sel.Name == "" {
		return legacyModelSel{}, false
	}
	return sel, true
}

// allExistingWorkspaces enumerates every per-user workspace directory on disk
// for every role, unlike existingWorkspaces (reconcile.go) which only sees roles
// still present in m.cfg.Agents. The migration must see every workspace that
// exists, including one whose agent was since removed from config.yaml or
// dropped by config.Load for missing secrets — otherwise its model is never
// captured and it is orphaned the moment a scope default changes.
func (m *Manager) allExistingWorkspaces() []WorkspaceKey {
	pattern := filepath.Join(m.cfg.ContainerDataRoot, "tenants", "*",
		"subscriptions", "*", "agents", "*", "users", "*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	var keys []WorkspaceKey
	for _, uw := range matches {
		if fi, err := os.Stat(uw); err != nil || !fi.IsDir() {
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
		keys = append(keys, WorkspaceKey{
			TenantID: parts[1], SubsAccID: parts[3], Role: parts[5], UserAccID: parts[7],
		})
	}
	return keys
}

// captureWorkspaceModel records what one workspace is running, recovering any
// model no other source declared from the workspace's own files.
//
// This is the step that prevents orphaning: without it every existing user reads
// as unassigned, and the first scope-default change re-resolves them all.
func (m *Manager) captureWorkspaceModel(key WorkspaceKey) error {
	userDir := config.UserWorkspace(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	configPath := filepath.Join(userDir, "config.json")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil // never provisioned
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse config.json: %w", err)
	}
	agents, _ := cfg["agents"].(map[string]any)
	defaults, _ := agents["defaults"].(map[string]any)
	primary, _ := defaults["model_name"].(string)
	if primary == "" {
		return nil // no model was ever pinned here; it resolves normally next start
	}

	var chain []string
	if fb, ok := defaults["model_fallbacks"].([]any); ok {
		for _, v := range fb {
			if s, ok := v.(string); ok && s != "" {
				chain = append(chain, s)
			}
		}
	}

	// Recover any named model the inventory does not have, from this workspace's
	// own model_list definition and .security.yml key.
	secPath := filepath.Join(userDir, ".security.yml")
	for _, name := range append([]string{primary}, chain...) {
		if _, err := m.reg.GetModel(name); err == nil {
			continue
		}
		mod, ok := recoverModelFromWorkspace(cfg, secPath, name)
		if !ok {
			m.logf("migrate models: workspace %+v names model %q that no source declares "+
				"and its own config.json does not define — left unregistered for admin review", key, name)
			continue
		}
		mod.ImportedOrphan = true
		if _, err := m.reg.CreateModelRaw(mod); err != nil {
			m.logf("migrate models: recover %q from workspace %+v: %v", name, key, err)
		}
	}
	// Reconstruct the primary's declared chain from what the workspace was running,
	// so the next re-materialization reproduces the same set.
	if len(chain) > 0 {
		if _, err := m.reg.UpdateModelRaw(primary, func(cur *registry.Model) error {
			if len(cur.Fallbacks) == 0 {
				cur.Fallbacks = chain
			}
			return nil
		}); err != nil {
			m.logf("migrate models: reconstruct chain for %q: %v", primary, err)
		}
	}

	// An imported per-user override file was a deliberate pin; anything else is
	// inherited. Recording a pin as inherited would let the next scope change
	// silently override it.
	source := registry.SourceInherited
	if _, err := os.Stat(config.UserModelOverrideFile(m.cfg.ContainerDataRoot,
		key.TenantID, key.SubsAccID, key.Role, key.UserAccID)); err == nil {
		source = registry.SourceExplicit
	}
	return m.reg.PutAssignment(m.workspaceRef(key), registry.Assignment{
		ModelName: primary, Chain: chain, Source: source,
	})
}

// recoverModelFromWorkspace rebuilds a model record from one workspace's own
// files — the only place a model that no other source declares still exists.
func recoverModelFromWorkspace(cfg map[string]any, secPath, name string) (registry.Model, bool) {
	list, _ := cfg["model_list"].([]any)
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if n, _ := entry["model_name"].(string); n != name {
			continue
		}
		mod := registry.Model{
			ModelName: name,
			Status:    registry.StatusActive,
		}
		mod.Provider, _ = entry["provider"].(string)
		mod.Model, _ = entry["model"].(string)
		mod.APIBase, _ = entry["api_base"].(string)
		mod.AuthMethod, _ = entry["auth_method"].(string)
		if mod.Model == "" {
			mod.Model = name
		}
		if mod.Provider == "" {
			return registry.Model{}, false
		}
		mod.APIKey = readWorkspaceModelKey(secPath, name)
		return mod, true
	}
	return registry.Model{}, false
}

// readWorkspaceModelKey pulls model_list.<name>.api_keys[0] out of a workspace's
// .security.yml — the sink the old code wrote and the only one picoclaw reads.
func readWorkspaceModelKey(secPath, name string) string {
	sec, err := readSecurityConfig(secPath)
	if err != nil {
		return ""
	}
	ml, ok := sec["model_list"].(map[string]any)
	if !ok {
		return ""
	}
	entry, ok := ml[name].(map[string]any)
	if !ok {
		return ""
	}
	keys, ok := entry["api_keys"].([]any)
	if !ok || len(keys) == 0 {
		return ""
	}
	s, _ := keys[0].(string)
	return s
}
