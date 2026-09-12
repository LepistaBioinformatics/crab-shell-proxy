package docker

// The per-instance configuration overlay for a ganglion workspace.
//
// WHY THIS HAS TO EXIST, and why the picoclaw editors needed nothing like it: a
// picoclaw workspace's config.json is seeded once and then edited in place, so an
// admin's change simply stays. The ganglion's is RENDERED WHOLE on every ensure
// from the inventory, the agent's configuration and the admin's secrets
// (ganglionConfigDoc) -- by design, because there is nothing to merge into and a
// merge is how two sources of truth start disagreeing.
//
// Rendered whole means an edit written into the file is gone on the next turn,
// which for a scale-to-zero agent is immediately. So the edit is stored BESIDE the
// file and re-applied every time the file is rendered. That is not a new idea
// here: config_overlay.go's own comment contrasts its seed-once behaviour with
// "native secrets and persona [which] take the other route and re-apply on every
// ensure". This is that route, for configuration.
//
// ABOVE THE WORKSPACE BIND, like every other proxy-owned file. It carries the
// sub-agent fan-out budget and the evolution ladder; an agent that could edit its
// own budget could raise it.

import (
	"encoding/json"
	"path/filepath"
	"sort"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// ganglionOverlayFile is the per-instance overlay, beside .ganglion-config.json
// and outside every bind.
const ganglionOverlayFile = ".ganglion-overlay.json"

// ganglionOverlayPath is the overlay for one workspace.
func ganglionOverlayPath(userDir string) string {
	return filepath.Join(userDir, ganglionOverlayFile)
}

// applyOverlayToDoc merges an overlay onto a rendered configuration document and
// returns the result plus how many keys landed.
//
// Byte-in, byte-out, because the ganglion's document is rendered in memory and
// compared against the file to decide whether anything changed -- writing it out
// only to merge and rewrite would make every ensure look like a change.
//
// The rules are the overlay's own and are NOT restated: an invalid key or a
// managed one is skipped, exactly as applyConfigOverlay skips them, because an
// overlay must not become a way around ManagedConfigPaths. Sorted, so two entries
// touching one branch resolve the same way on every render.
func applyOverlayToDoc(doc []byte, overlayPath string) ([]byte, int, error) {
	overlay, err := readConfigOverlay(overlayPath)
	if err != nil {
		return doc, 0, err
	}
	if len(overlay) == 0 {
		return doc, 0, nil
	}
	parsed, err := parseConfigObject(doc)
	if err != nil {
		return doc, 0, err
	}

	keys := make([]string, 0, len(overlay))
	for k := range overlay {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	applied := 0
	for _, key := range keys {
		if err := ValidateConfigKey(key); err != nil || IsManagedConfigPath(key) {
			continue
		}
		var value any
		if err := json.Unmarshal(overlay[key], &value); err != nil {
			continue
		}
		if err := setPath(parsed, key, value); err != nil {
			continue
		}
		applied++
	}
	if applied == 0 {
		return doc, 0, nil
	}
	out, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return doc, 0, err
	}
	return out, applied, nil
}

// diffConfigPaths returns the dotted leaf paths where `next` differs from `prev`.
//
// It is how a WHOLE-DOCUMENT edit becomes overlay entries. The instance editor
// submits a document, not a key, and for a ganglion workspace a document written
// to the file is discarded by the next render -- so what the admin changed has to
// be recovered from the document itself and stored where it survives.
//
// LEAVES ONLY, and an array counts as one. A path like `model_list` is a single
// value here rather than five, which is what an overlay entry has to be anyway:
// `setPath` writes whole values. It also means a reordered array is one change,
// which is the honest reading of "the admin replaced the list".
//
// A key REMOVED in `next` is not reported. An overlay says what a value should be;
// it has no way to say "and this one should not exist", and inventing one would
// mean an overlay that can delete what the renderer produces -- including, one
// refactor later, a managed key.
func diffConfigPaths(prev, next map[string]any) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	walkConfigDiff("", prev, next, out)
	return out
}

func walkConfigDiff(prefix string, prev, next map[string]any, out map[string]json.RawMessage) {
	for k, nv := range next {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		pv, had := prev[k]

		nObj, nIsObj := nv.(map[string]any)
		pObj, pIsObj := pv.(map[string]any)
		if nIsObj && pIsObj {
			walkConfigDiff(path, pObj, nObj, out)
			continue
		}
		if nIsObj && !had {
			// A whole new branch: recurse against nothing, so each leaf is recorded
			// on its own. Storing the branch as one value would overwrite siblings
			// the renderer adds later.
			walkConfigDiff(path, map[string]any{}, nObj, out)
			continue
		}
		nEnc, err := json.Marshal(nv)
		if err != nil {
			continue
		}
		if had {
			if pEnc, perr := json.Marshal(pv); perr == nil && string(pEnc) == string(nEnc) {
				continue
			}
		}
		out[path] = nEnc
	}
}

// recordGanglionOverlay stores what an edit changed, so the next render keeps it.
//
// Managed and invalid keys are skipped rather than refused: they are skipped on
// the way OUT too (applyOverlayToDoc), so storing one would only be a promise the
// renderer breaks. The write to the file still happens -- a managed key an admin
// somehow set is reverted by the next materialization, which is the behaviour
// ManagedConfigPaths describes, not a failure to report here.
func (m *Manager) recordGanglionOverlay(key WorkspaceKey, currentDoc map[string]any, next []byte) error {
	nextDoc, err := parseConfigObject(next)
	if err != nil {
		return err
	}
	if currentDoc == nil {
		currentDoc = map[string]any{}
	}
	changed := diffConfigPaths(currentDoc, nextDoc)
	if len(changed) == 0 {
		return nil
	}
	path := ganglionOverlayPath(config.UserWorkspace(m.cfg.ContainerDataRoot,
		key.TenantID, key.SubsAccID, key.Role, key.UserAccID))
	for k, v := range changed {
		if err := ValidateConfigKey(k); err != nil || IsManagedConfigPath(k) {
			continue
		}
		if err := upsertConfigOverlay(path, k, v); err != nil {
			return err
		}
	}
	return nil
}
