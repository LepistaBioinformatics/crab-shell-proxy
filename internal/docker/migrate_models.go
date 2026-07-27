package docker

import (
	"encoding/json"
	"errors"
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

// MigrateModels runs the one-time inventory import. It is exported because it must
// complete BEFORE the HTTP server accepts requests: a chat arriving while the
// inventory is still empty resolves nothing and fails, and the rest of Reconcile
// (container adoption, continuous start) is what actually takes a long time.
func (m *Manager) MigrateModels() error { return m.migrateModelRegistry() }

// migrateModelRegistry seeds the inventory from every pre-existing source and
// records what each existing workspace is currently running.
//
// It only READS workspaces — no workspace's active model changes as a result of
// migrating. Later sources win on model_name collision for the DEFINITION fields
// (model, api_base, auth_method), because a registered-models entry or a live
// workspace holds the shape that is actually in service, whereas the config.yaml
// seed carries only a model_name. A non-empty api_key is never overwritten: it is
// the one field an earlier source may hold and a later one may not.
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
	// config.ModelConfig carries only a name for a picoclaw model: BaseURL is a
	// hermes-only field and is EMPTY for every picoclaw one, and there is no field
	// at all for the provider's own model id. Both lived in the TEMPLATE's
	// model_list, where model_name and model differ for most entries
	// (claude-sonnet-4.6 -> claude-sonnet-4-6, nearai-glm -> zai-org/GLM-5.1-FP8).
	// Importing {Model: mc.Name, APIBase: ""} alone would write a model id the
	// provider does not have into every workspace that lands on it, so the
	// definition is taken from the template that was in service — the same file
	// step 5 normalizes, read here BEFORE that happens (and from its
	// .pre-registry backup on a re-run).
	//
	// This is not a template IMPORT (FR-20): a template-only model no source
	// declares is still dropped. It corrects the definition of a model config.yaml
	// already declares, from the only place that definition existed.
	//
	// CreateModelRaw is used because the public API would reject a record with no
	// api_base and no auth_method, which is a shape the old config could express.
	for _, agent := range m.cfg.Agents {
		defs := m.templateModelDefs(agent.Template)
		for _, mc := range agent.SelectableModels() {
			mod := registry.Model{
				ModelName: mc.Name, Provider: mc.Provider, Model: mc.Name,
				APIBase: mc.BaseURL, APIKey: mc.APIKey, Status: registry.StatusActive,
			}
			if def, ok := defs[mc.Name]; ok {
				if def.Model != "" {
					mod.Model = def.Model
				}
				if mod.APIBase == "" {
					mod.APIBase = def.APIBase
				}
				if mod.AuthMethod == "" {
					mod.AuthMethod = def.AuthMethod
				}
			}
			m.importLegacyModel(mod)
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

	// 4. Every existing workspace: capture what it is actually running. A capture
	// failure is counted, not fatal: the other workspaces still get their
	// assignment, and the schema marker is withheld below so the pass re-runs.
	//
	// Enumerated directly from disk (allExistingWorkspaces), not via
	// m.existingWorkspaces(agent.Key) looped over m.cfg.Agents: an agent removed
	// from config.yaml, or a hermes agent config.Load dropped for missing secrets
	// (DisabledAgents), would otherwise make every workspace under its role
	// invisible to this pass. This is the anti-orphaning step — it must see every
	// workspace that exists, not only the ones the current config still declares.
	captureFailures := 0
	for _, key := range m.allExistingWorkspaces() {
		if err := m.captureWorkspaceModel(key); err != nil {
			captureFailures++
			m.logf("migrate models: capture %+v: %v", key, err)
		}
	}

	// 5. Normalize the disk templates LAST among the mutating steps. Not a data
	// dependency — it touches templates, step 4 touches workspaces — but ordering
	// it here means a failure anywhere earlier leaves the templates untouched and
	// the whole pass re-runnable.
	if err := m.normalizeDiskTemplates(); err != nil {
		return err
	}

	m.logf("migrate models: superseded files are no longer read " +
		"(registered-models/*.json, tenants/*/shared/model.json, " +
		"tenants/*/subscriptions/*/shared/model.json, .crab-model.json); " +
		"they are left on disk for rollback")

	// The marker is what makes the pass a no-op forever after, so it must not be
	// written while any workspace is still uncaptured: such a workspace has no
	// assignment, and the first scope-default change re-resolves it — the exact
	// failure step 4 exists to prevent. Withholding the marker keeps it visible for
	// a retry, which is what the capture-failure path promises.
	if captureFailures > 0 {
		m.logf("migrate models: %d workspace(s) could not be captured; schema marker NOT set, "+
			"the whole pass will re-run on the next boot", captureFailures)
		return nil
	}
	return m.reg.SetSchemaVersion(modelRegistrySchemaVersion)
}

// templateModelDefs reads one per-instance disk template's model_list into
// definitions keyed by model_name, for correcting a config.yaml seed that carries
// only a name. The .pre-registry backup is read first so a re-run (after step 5
// already emptied the live file) still finds the definitions; the live file wins
// where it still has entries.
//
// No keys are read: a template never held one.
func (m *Manager) templateModelDefs(template string) map[string]registry.Model {
	path := filepath.Join(config.TemplatesDir(m.cfg.ContainerDataRoot, template), "config.json")
	out := map[string]registry.Model{}
	for _, p := range []string{path + ".pre-registry", path} {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg map[string]any
		if err := json.Unmarshal(raw, &cfg); err != nil {
			m.logf("migrate models: parse template %s: %v", p, err)
			continue
		}
		for name, def := range modelDefsFromConfig(cfg) {
			out[name] = def
		}
	}
	return out
}

// importLegacyModel creates a record, or lets this source correct one an earlier
// source already created (refineLegacyModel).
func (m *Manager) importLegacyModel(mod registry.Model) {
	if mod.ModelName == "" || mod.Provider == "" {
		return
	}
	if mod.Model == "" {
		mod.Model = mod.ModelName
	}
	if _, err := m.reg.GetModel(mod.ModelName); err == nil {
		m.refineLegacyModel(mod)
		return
	}
	if _, err := m.reg.CreateModelRaw(mod); err != nil {
		m.logf("migrate models: import %q: %v", mod.ModelName, err)
	}
}

// errNoRefinement aborts refineLegacyModel's transaction when nothing would
// change, so a no-op refine does not bump Version and UpdatedAt on every pass.
var errNoRefinement = errors.New("no refinement needed")

// refineLegacyModel lets a LATER source correct a record an EARLIER one created.
// This is what makes the documented "later sources win" ordering true for the
// definition fields, not just for a missing key: the config.yaml seed can only
// supply a model_name, so without this a registered-models entry's or a live
// workspace's real model id and api_base never reach the record, and the next
// ensure writes a model the provider does not have into a workspace that worked.
//
// Every field is guarded, and each guard is a different question:
//
//   - api_base and auth_method are backfilled only when empty — an admin may have
//     edited them, and a later source is not automatically better at those.
//   - a NON-EMPTY api_key is never touched: it is the one field an earlier source
//     may hold and a later one may not.
//   - model is corrected only while it still carries the config.yaml-seed
//     PLACEHOLDER, which is exactly the state `cur.Model == cur.ModelName`: step 1
//     sets Model = mc.Name because config.ModelConfig has no real model id for
//     picoclaw. Once a real id is in place it is unoverwritable.
//
// That last guard is not cosmetic. captureWorkspaceModel calls this for every name
// already in the inventory, and step 4 walks workspaces in glob order, so an
// unguarded correction is last-writer-wins across workspaces: where two workspaces
// carry the same model_name with DIFFERENT model ids — reachable on a legacy
// instance, since provision never re-seeds a returning user and the old registry UI
// wrote model_list entries from a free-text field — the last workspace iterated
// would impose its id on every other workspace using that name at its next start.
// A workspace's active model changing as a result of the migration alone is exactly
// what AC-11 forbids.
func (m *Manager) refineLegacyModel(better registry.Model) {
	// A model id this pass declined to overwrite. Recorded here and logged after the
	// transaction: two workspaces naming one model_name with different ids cannot
	// both be served by one record (model_name is unique, FR-3), so the divergence
	// is the admin's to resolve and must not be silent.
	conflict := ""
	_, err := m.reg.UpdateModelRaw(better.ModelName, func(cur *registry.Model) error {
		changed := false
		if cur.APIKey == "" && better.APIKey != "" {
			cur.APIKey = better.APIKey
			changed = true
		}
		if cur.APIBase == "" && better.APIBase != "" {
			cur.APIBase = better.APIBase
			changed = true
		}
		if cur.AuthMethod == "" && better.AuthMethod != "" {
			cur.AuthMethod = better.AuthMethod
			changed = true
		}
		if better.Model != "" && better.Model != cur.Model {
			if cur.Model == cur.ModelName {
				cur.Model = better.Model
				changed = true
			} else {
				conflict = cur.Model
			}
		}
		if !changed {
			return errNoRefinement
		}
		return nil
	})
	if conflict != "" {
		m.logf("migrate models: model %q keeps model id %q; another source declares %q — "+
			"one inventory record cannot serve both, review", better.ModelName, conflict, better.Model)
	}
	if err != nil && !errors.Is(err, errNoRefinement) {
		m.logf("migrate models: refine %q: %v", better.ModelName, err)
	}
}

// importOverrideFiles reads admin-model-override's selection files into scope
// defaults. A per-user file is handled by captureWorkspaceModel, which knows the
// workspace it belongs to.
func (m *Manager) importOverrideFiles(root string) {
	tenantFiles, _ := filepath.Glob(filepath.Join(root, "tenants", "*", "shared", "model.json"))
	for _, path := range tenantFiles {
		sel, ok := readLegacySel(path, m.logf)
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
		sel, ok := readLegacySel(path, m.logf)
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

// readLegacySel reads one admin-model-override selection file. A missing file
// is the normal case (no override was ever set at that scope) and is silent; any
// other read or parse failure is logged rather than swallowed, since a missed
// override only drops a scope default (step 4 still captures each workspace's
// own assignment independently) but should still be visible, not invisible.
func readLegacySel(path string, logf func(string, ...any)) (legacyModelSel, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logf("migrate models: read %s: %v", path, err)
		}
		return legacyModelSel{}, false
	}
	var sel legacyModelSel
	if err := json.Unmarshal(raw, &sel); err != nil {
		logf("migrate models: parse %s: %v", path, err)
		return legacyModelSel{}, false
	}
	if sel.Name == "" {
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
	if os.IsNotExist(err) {
		return nil // never provisioned — nothing to capture
	}
	if err != nil {
		// A workspace we cannot read may well have a live model. Surfacing the
		// error gets it logged and keeps it visible for a retry, rather than
		// silently recording it as unassigned and letting the next scope change
		// move it — the exact failure this task exists to prevent.
		return fmt.Errorf("read config.json: %w", err)
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
	// own model_list definition and .security.yml key — and, for one it DOES have,
	// let this workspace correct the definition. A running workspace is the most
	// authoritative source there is: its model_list entry is the shape a provider
	// is currently accepting, which an earlier source (config.yaml, which carries
	// only a name) cannot express.
	secPath := filepath.Join(userDir, ".security.yml")
	for _, name := range append([]string{primary}, chain...) {
		mod, ok := recoverModelFromWorkspace(cfg, secPath, name)
		if _, err := m.reg.GetModel(name); err == nil {
			if ok {
				m.refineLegacyModel(mod)
			}
			continue
		}
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

	// The source decides whether the next scope-default change may move this
	// workspace, so it is decided by REPRODUCIBILITY, not by which file happened to
	// carry the choice.
	//
	// An imported per-user override file is a pin, unconditionally. But the deleted
	// registry UI (ApplyRegisteredModelToUser) wrote the model straight into the
	// workspace and left no override file anywhere — so "no override file" does NOT
	// mean "inherited". Recording those as inherited would hand the next
	// EnsureRunning a workspace whose model the cascade does not produce: it would
	// silently replace it where a scope default exists, and refuse to boot where
	// none does. That is the unrecoverable overwrite this whole feature removes.
	//
	// So: if the scope cascade would resolve exactly what this workspace runs, the
	// assignment is genuinely inherited and the workspace keeps tracking its scope.
	// If it would not, the current model is not reproducible from the cascade and is
	// recorded as an explicit pin, which is the only shape that survives. The
	// comparison uses the registry's own cascade function, so it cannot drift from
	// what resolution will actually do afterwards.
	ref := m.workspaceRef(key)
	source := registry.SourceInherited
	if _, err := os.Stat(config.UserModelOverrideFile(m.cfg.ContainerDataRoot,
		key.TenantID, key.SubsAccID, key.Role, key.UserAccID)); err == nil {
		source = registry.SourceExplicit
	} else if cascade, level, cerr := m.reg.ScopeCandidate(ref); cerr != nil || cascade != primary {
		source = registry.SourceExplicit
		m.logf("migrate models: workspace %+v runs %q, which the scope cascade does not reproduce "+
			"(cascade: %q at %q, err %v) — recorded as an explicit pin so the next resolve keeps it",
			key, primary, cascade, level, cerr)
	}
	return m.reg.PutAssignment(ref, registry.Assignment{
		ModelName: primary, Chain: chain, Source: source,
	})
}

// modelDefsFromConfig reads a picoclaw config.json's model_list into definitions
// keyed by model_name. One parser serves both callers — a workspace's own file and
// a disk template — so the two cannot read the same shape differently.
func modelDefsFromConfig(cfg map[string]any) map[string]registry.Model {
	out := map[string]registry.Model{}
	list, _ := cfg["model_list"].([]any)
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := entry["model_name"].(string)
		if name == "" {
			continue
		}
		mod := registry.Model{ModelName: name, Status: registry.StatusActive}
		mod.Provider, _ = entry["provider"].(string)
		mod.Model, _ = entry["model"].(string)
		mod.APIBase, _ = entry["api_base"].(string)
		mod.AuthMethod, _ = entry["auth_method"].(string)
		out[name] = mod
	}
	return out
}

// recoverModelFromWorkspace rebuilds a model record from one workspace's own
// files — the only place a model that no other source declares still exists, and
// the most authoritative definition of one that another source declares badly.
func recoverModelFromWorkspace(cfg map[string]any, secPath, name string) (registry.Model, bool) {
	mod, ok := modelDefsFromConfig(cfg)[name]
	if !ok || mod.Provider == "" {
		return registry.Model{}, false
	}
	if mod.Model == "" {
		mod.Model = name
	}
	mod.APIKey = readWorkspaceModelKey(secPath, name)
	return mod, true
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
