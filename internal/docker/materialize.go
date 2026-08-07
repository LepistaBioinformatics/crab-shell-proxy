package docker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/projects"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// projectList is what materializeModels needs to project agents.list and
// agents.dispatch. Passed in rather than read inside, so the pure config
// rewriting stays testable without a store on disk.
type projectList struct {
	Home     string
	Projects []projects.Project
}

// materializeModels writes a resolved model set into one workspace. It replaces
// applyModel's model handling and is the ONLY writer of a workspace's model
// configuration.
//
// config.json gets full model_list entries WITHOUT api_key: picoclaw removed
// api_key (singular) from config.json in schema V2+ and ignores it, and the
// shipped template is "version": 3. Keys go to .security.yml, which is the only
// sink that works — the containers receive no key environment variable
// (manager.go: Env is PICOCLAW_GATEWAY_HOST and HOME only).
//
// The write ORDER is load-bearing, and neither of the two obvious orders is
// fail-closed: writing config.json first names a model whose key is not in
// .security.yml yet, and pruning .security.yml first strips the OLD model's key
// while config.json still names it. Both leave an unbootable workspace if the
// process dies between the two writes. So the sequence is three steps —
// .security.yml with old ∪ new keys, then config.json, then .security.yml pruned
// to the new set — and every intermediate state names a model whose key is
// present.
func materializeModels(configPath, secPath string, res registry.Resolution, projs projectList) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config.json: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse config.json: %w", err)
	}

	// The pico channel is how the proxy reaches picoclaw at all; a workspace with
	// it disabled is unreachable regardless of its model.
	if cl, ok := cfg["channel_list"].(map[string]any); ok {
		if pico, ok := cl["pico"].(map[string]any); ok {
			pico["enabled"] = true
		}
	}

	models := append([]registry.Model{res.Primary}, res.Chain...)
	list := make([]any, 0, len(models))
	for _, m := range models {
		list = append(list, modelListEntry(m))
	}
	cfg["model_list"] = list

	// Create the structure rather than skipping the write: an ok-guard with no else
	// branch produces a workspace with a correct model_list and no active model —
	// picoclaw then boots with no model at all, silently, which is the failure mode
	// this whole feature removes. An operator who edited agents.defaults out of a
	// template gets it back, not a mystery.
	defaults := childMap(childMap(cfg, "agents"), "defaults")
	defaults["provider"] = res.Primary.Provider
	defaults["model_name"] = res.Primary.ModelName
	if names := res.ChainNames(); len(names) > 0 {
		fb := make([]any, 0, len(names))
		for _, n := range names {
			fb = append(fb, n)
		}
		defaults["model_fallbacks"] = fb
	} else {
		// Clear a stale chain rather than leaving one behind: the primary
		// may have had fallbacks and no longer does.
		delete(defaults, "model_fallbacks")
	}

	// Projects ride along in THIS read-modify-write, not in a write of their own.
	// This function rewrites config.json wholesale, so a separately-written
	// agents.list would survive exactly until the user's next chat and then
	// vanish — the project would stop routing with no error anywhere.
	projectAgents(cfg, projs.Home, projs.Projects)

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	sec, err := readSecurityConfig(secPath)
	if err != nil {
		return fmt.Errorf("read .security.yml: %w", err)
	}
	keep := make([]string, 0, len(models))
	for _, m := range models {
		if m.APIKey == "" {
			// No key to write. An empty api_keys array would read as a
			// configured-but-blank credential, which fails less clearly than an
			// absent entry (oauth models legitimately have no key).
			continue
		}
		setModelListEntry(sec, m.ModelName, m.APIKey)
		keep = append(keep, m.ModelName)
	}
	// Step 1: old ∪ new keys. sec is read-modify-write, so it already holds the
	// outgoing model's key alongside the incoming one.
	if err := writeSecurityConfig(secPath, sec, ""); err != nil {
		return fmt.Errorf("write .security.yml: %w", err)
	}
	// Step 2: config.json, whose named model's key is now present either way.
	if err := os.WriteFile(configPath, out, 0o600); err != nil {
		return fmt.Errorf("write config.json: %w", err)
	}
	// Step 3: drop the keys no model in the new set needs (FR-17b).
	pruneSecurityModelList(sec, keep)
	if err := writeSecurityConfig(secPath, sec, ""); err != nil {
		return fmt.Errorf("prune .security.yml: %w", err)
	}
	return nil
}

// modelListEntry renders one picoclaw model_list entry. Optional fields are
// omitted when unset so the file stays close to what an operator would hand-write.
func modelListEntry(m registry.Model) map[string]any {
	entry := map[string]any{
		"model_name": m.ModelName,
		"provider":   m.Provider,
		"model":      m.Model,
		"enabled":    true,
	}
	if m.APIBase != "" {
		entry["api_base"] = m.APIBase
	}
	if m.AuthMethod != "" {
		entry["auth_method"] = m.AuthMethod
	}
	if len(m.ExtraBody) > 0 {
		var eb any
		if err := json.Unmarshal(m.ExtraBody, &eb); err == nil {
			entry["extra_body"] = eb
		}
	}
	return entry
}

// pruneSecurityModelList drops model_list entries outside keep, and removes the
// section entirely when nothing is left. Scoped strictly to model_list: the pico
// channel token, the web.* families and native-secret overlay slots are the
// user's or the admin's data and are not this function's business.
func pruneSecurityModelList(sec map[string]any, keep []string) {
	ml, ok := sec["model_list"].(map[string]any)
	if !ok {
		return
	}
	wanted := make(map[string]bool, len(keep))
	for _, name := range keep {
		wanted[name] = true
	}
	for name := range ml {
		if !wanted[name] {
			delete(ml, name)
		}
	}
	if len(ml) == 0 {
		delete(sec, "model_list")
	}
}

// ErrNoModel is the package-level alias the HTTP layer matches on, so handlers
// do not need to import registry just to classify one error.
var ErrNoModel = registry.ErrNoModelResolvable

// workspaceRef converts a WorkspaceKey into the registry's own ref. The registry
// cannot import this package (it would be a cycle), so the conversion lives here.
func (m *Manager) workspaceRef(key WorkspaceKey) registry.WorkspaceRef {
	return registry.WorkspaceRef{
		TenantID:  key.TenantID,
		SubsAccID: key.SubsAccID,
		Agent:     key.Role,
		UserAccID: key.UserAccID,
	}
}

// resolveAndMaterialize resolves the workspace's model and writes it. It is the
// single entry point every provision and every re-apply goes through.
//
// A workspace with no resolvable model is REFUSED, not defaulted: picoclaw fails
// at startup when agents.defaults.model_name names a model absent from
// model_list, so a silent default would produce a permanently unbootable
// container. Nothing is written on refusal.
// The native-secret overlay is applied HERE, after materialization, on every path
// that materializes — not once at first provision. materializeModels rewrites
// every .security.yml model_list entry and prunes the rest, so an overlay applied
// before it is overwritten on the very next ensure: a scope admin's own key would
// take effect once and then silently revert to the inventory's key, which is a
// credential substitution with billing and isolation consequences (FR-32,
// CTX-MR-12). Applying it last is what makes the documented precedence true.
func (m *Manager) resolveAndMaterialize(key WorkspaceKey, userDir string) error {
	ref := m.workspaceRef(key)
	res, err := m.reg.Resolve(ref)
	if err != nil {
		return err
	}
	for _, name := range res.Skipped {
		m.logf("materialize %s: fallback %q is not active, skipped", ref.Key(), name)
	}
	for _, level := range res.SkippedLevels {
		m.logf("materialize %s: cascade level %s names a model the inventory does not have, skipped", ref.Key(), level)
	}

	configPath := filepath.Join(userDir, "config.json")
	secPath := filepath.Join(userDir, ".security.yml")

	// Re-derived on EVERY ensure, from the store rather than from whatever
	// config.json currently says. That is what makes a project's routing
	// self-healing: an operator repairing the file by hand, a restored backup, or
	// this very function rewriting the config cannot leave the rules behind.
	list, err := m.projectStore(key).List()
	if err != nil {
		return fmt.Errorf("read projects: %w", err)
	}

	if err := materializeModels(configPath, secPath, res,
		projectList{Home: m.cfg.PicoclawHome, Projects: list}); err != nil {
		return err
	}

	// RecordMaterialization preserves an existing EXPLICIT pin's source in the
	// same transaction that reads it: re-materializing must not demote a
	// deliberate per-user choice into an inherited one, which would let the next
	// scope-default change silently override it.
	if err := m.reg.RecordMaterialization(ref, res.Primary.ModelName, res.ChainNames()); err != nil {
		return fmt.Errorf("record assignment: %w", err)
	}

	effDir := config.EffectiveSecretsDir(m.cfg.ContainerDataRoot, key.UserAccID, key.Role)
	if err := applyNativeSecrets(secPath, effDir, m.cfg.PicoclawUser, m.logf); err != nil {
		return fmt.Errorf("apply native secrets: %w", err)
	}
	return chownTree(userDir, m.cfg.PicoclawUser)
}
