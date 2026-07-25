package docker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// materializeModels writes a resolved model set into one workspace. It replaces
// applyModel's model handling and is the ONLY writer of a workspace's model
// configuration.
//
// config.json gets full model_list entries WITHOUT api_key: picoclaw removed
// api_key (singular) from config.json in schema V2+ and ignores it, and the
// shipped template is "version": 3. Keys go to .security.yml, which is the only
// sink that works — the containers receive no key environment variable
// (manager.go: Env is PICOCLAW_GATEWAY_HOST and HOME only).
func materializeModels(configPath, secPath string, res registry.Resolution) error {
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

	if agents, ok := cfg["agents"].(map[string]any); ok {
		if defaults, ok := agents["defaults"].(map[string]any); ok {
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
	pruneSecurityModelList(sec, keep)
	if err := writeSecurityConfig(secPath, sec, ""); err != nil {
		return fmt.Errorf("write .security.yml: %w", err)
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
func (m *Manager) resolveAndMaterialize(key WorkspaceKey, userDir string) error {
	ref := m.workspaceRef(key)
	res, err := m.reg.Resolve(ref)
	if err != nil {
		return err
	}
	for _, name := range res.Skipped {
		m.logf("materialize %s: fallback %q is not active, skipped", ref.Key(), name)
	}

	configPath := filepath.Join(userDir, "config.json")
	secPath := filepath.Join(userDir, ".security.yml")
	if err := materializeModels(configPath, secPath, res); err != nil {
		return err
	}

	// An existing EXPLICIT pin keeps its source: re-materializing must not demote
	// a deliberate per-user choice into an inherited one, which would let the next
	// scope-default change silently override it.
	source := registry.SourceInherited
	if prev, err := m.reg.GetAssignment(ref); err == nil && prev.Source == registry.SourceExplicit {
		source = registry.SourceExplicit
	}
	if err := m.reg.PutAssignment(ref, registry.Assignment{
		ModelName: res.Primary.ModelName,
		Chain:     res.ChainNames(),
		Source:    source,
	}); err != nil {
		return fmt.Errorf("record assignment: %w", err)
	}
	return chownTree(userDir, m.cfg.PicoclawUser)
}
