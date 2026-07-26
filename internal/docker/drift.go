package docker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// normalizeDiskTemplates empties every per-instance disk template's model_list
// and clears its pinned default.
//
// Leaving models in a template would keep a place the truth could appear to live:
// an operator editing it would get no effect and no explanation, because
// materialization overwrites model_list wholesale from the inventory. Nothing in
// use is lost — the migration already recovered every running model from the
// workspaces themselves, which is where a working model provably exists.
func (m *Manager) normalizeDiskTemplates() error {
	// Enumerate templates from DISK, not from m.cfg.Agents: config.Load drops
	// disabled or removed agents from that map, so a per-agent loop would leave
	// exactly those agents' templates un-normalized — still carrying models,
	// still looking like a place the truth lives. The same reason the migration
	// and the drift check enumerate workspaces from disk.
	matches, _ := filepath.Glob(filepath.Join(m.cfg.ContainerDataRoot, "templates", "*", "config.json"))
	for _, path := range matches {
		if err := normalizeTemplateFile(path); err != nil {
			m.logf("normalize template %s: %v", path, err)
		}
	}
	return nil
}

func normalizeTemplateFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing seeded yet; the embedded template is already empty
		}
		return err
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse: %w", err)
	}

	// Back up before the only destructive write the migration performs, and never
	// overwrite an existing backup: a re-run must not replace the real original
	// with an already-normalized copy.
	backup := path + ".pre-registry"
	switch _, statErr := os.Stat(backup); {
	case os.IsNotExist(statErr):
		if err := os.WriteFile(backup, raw, 0o600); err != nil {
			return fmt.Errorf("write backup: %w", err)
		}
	case statErr != nil:
		// Cannot tell whether a backup exists. Refuse to normalize rather than
		// destroy the original with no recoverable copy.
		return fmt.Errorf("stat backup %s: %w", backup, statErr)
	}

	cfg["model_list"] = []any{}
	// Create the structure when a hand-edited template lacks it, rather than
	// silently leaving the file half-normalized: the normalized shape IS an empty
	// agents.defaults model, so writing it is both the fix and the goal.
	defaults := childMap(childMap(cfg, "agents"), "defaults")
	defaults["provider"] = ""
	defaults["model_name"] = ""
	delete(defaults, "model_fallbacks")
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

// checkModelDrift compares each workspace's config.json — its active model AND
// its fallback chain — against the recorded assignment, and logs mismatches.
//
// Read-only on purpose: a correction is an explicit admin reapply, never a
// boot-time surprise that changes which model someone's agent uses.
// It enumerates workspaces from DISK rather than from m.cfg.Agents, for the same
// reason the migration does: config.Load drops disabled or removed agents from that
// map, so a per-agent loop would silently stop reporting drift for exactly the
// workspaces most likely to have it.
func (m *Manager) checkModelDrift() {
	for _, key := range m.allExistingWorkspaces() {
		onDisk, chain, ok := readWorkspaceActiveModel(
			config.UserWorkspace(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID))
		if !ok {
			continue
		}
		a, err := m.reg.GetAssignment(m.workspaceRef(key))
		if err != nil {
			m.logf("model drift: workspace %+v runs %q but has no recorded assignment", key, onDisk)
			continue
		}
		if a.ModelName != onDisk {
			m.logf("model drift: workspace %+v runs %q, recorded %q", key, onDisk, a.ModelName)
			continue
		}
		if strings.Join(a.Chain, ",") != strings.Join(chain, ",") {
			m.logf("model drift: workspace %+v fallback chain on disk %v, recorded %v", key, chain, a.Chain)
		}
	}
}

// readWorkspaceActiveModel reports the primary and chain a workspace's config.json
// currently names.
func readWorkspaceActiveModel(userDir string) (primary string, chain []string, ok bool) {
	raw, err := os.ReadFile(filepath.Join(userDir, "config.json"))
	if err != nil {
		return "", nil, false
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", nil, false
	}
	agents, _ := cfg["agents"].(map[string]any)
	defaults, _ := agents["defaults"].(map[string]any)
	primary, _ = defaults["model_name"].(string)
	if primary == "" {
		return "", nil, false
	}
	if fb, ok := defaults["model_fallbacks"].([]any); ok {
		for _, v := range fb {
			if s, ok := v.(string); ok && s != "" {
				chain = append(chain, s)
			}
		}
	}
	return primary, chain, true
}
