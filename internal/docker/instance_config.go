package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// One workspace's config.json, read and replaced as an administrative repair
// (admin-instance-config-editor). A workspace's config.json is seeded once from
// the agent template and, for a returning user, NEVER re-seeded — provisionUser
// treats an existing file as "returning user, leave as-is". So a workspace whose
// config.json is wrong stays wrong forever, and the only remedies were host
// access or destroying the member's data. This is the third one.

// maxInstanceConfigBytes caps a submitted document. The seeded template is ~12
// KiB; the cap exists so the endpoint cannot be used to plant a large file
// inside a member's workspace.
const maxInstanceConfigBytes = 1 << 20

var (
	// ErrNotProvisioned means the workspace has no config.json: that member has
	// never started that agent, so there is nothing to repair.
	ErrNotProvisioned = errors.New("workspace has no config.json")
	// ErrStaleRevision means the file changed between the read and the write.
	ErrStaleRevision = errors.New("config.json changed since it was read")
	// ErrConfigNotObject means the submitted document parses but is not a JSON
	// object. picoclaw reads an object; an array or scalar would brick the
	// workspace being repaired.
	ErrConfigNotObject = errors.New("config.json must be a JSON object")
	// ErrConfigTooLarge means the submitted document exceeds maxInstanceConfigBytes.
	ErrConfigTooLarge = errors.New("config.json is too large")
)

// ManagedConfigPaths are the config.json paths the PROXY owns. Dotted, and a
// listed path covers its whole subtree (model_list is listed, so every entry in
// it is managed too).
//
// They are rewritten on every materialization (materializeModels) or provision
// (alignWorkspace), so an admin edit to one of them cannot survive. The admin UI
// renders them read-only from this list — it never keeps its own copy.
//
// Adding a writer to materializeModels or alignWorkspace WITHOUT adding its path
// here silently makes that key look admin-editable. TestManagedConfigPathsMatchWriters
// is the gate that catches it.
var ManagedConfigPaths = []string{
	"model_list",
	"agents.defaults.provider",
	"agents.defaults.model_name",
	"agents.defaults.model_fallbacks",
	"agents.defaults.workspace",
	"channel_list.pico.enabled",
}

// InstanceConfig is one workspace's config.json as an admin sees it.
//
// The bytes travel as a string and are never re-marshalled from a parsed value:
// a config.json that does NOT parse is the primary thing this repairs, so the
// transport has to survive one. Valid/ParseError describe the bytes; they are
// not an error condition.
type InstanceConfig struct {
	Raw        string `json:"raw"`
	Valid      bool   `json:"valid"`
	ParseError string `json:"parseError,omitempty"`
	// Offset is the byte offset a *json.SyntaxError reported, or -1 when the
	// parse failed for a reason that carries no position.
	Offset        int64     `json:"offset,omitempty"`
	Size          int64     `json:"size"`
	ModifiedAt    time.Time `json:"modifiedAt"`
	Revision      string    `json:"revision"`
	ManagedPaths  []string  `json:"managedPaths"`
	RedactedPaths []string  `json:"redactedPaths,omitempty"`
}

// ReapplyResult reports whether the post-write re-materialization succeeded. It
// is REPORTED rather than returned as an error: the admin's write already landed
// and undoing it would throw away the repair.
type ReapplyResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// ReadInstanceConfig returns one workspace's config.json.
//
// A file that does not parse comes back with Valid=false and a nil error: it is
// data an admin needs to see, not a failure. ErrNotProvisioned when absent.
func (m *Manager) ReadInstanceConfig(key WorkspaceKey) (InstanceConfig, error) {
	path := m.instanceConfigPath(key)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return InstanceConfig{}, ErrNotProvisioned
		}
		return InstanceConfig{}, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return InstanceConfig{}, err
	}

	out := InstanceConfig{
		// The revision is computed over the ON-DISK bytes, before any redaction
		// (see redactModelKeys): it is the token WriteInstanceConfig compares
		// against the file, so a redacted response must still carry the real one
		// or the admin's first save would 409.
		Revision:     revisionOf(raw),
		Size:         fi.Size(),
		ModifiedAt:   fi.ModTime().UTC(),
		ManagedPaths: ManagedConfigPaths,
	}

	doc, err := parseConfigObject(raw)
	if err != nil {
		out.Raw = string(raw)
		out.ParseError = err.Error()
		out.Offset = syntaxOffset(err)
		return out, nil
	}
	out.Valid = true

	redacted, paths := redactModelKeys(doc)
	if len(paths) == 0 {
		out.Raw = string(raw)
		return out, nil
	}
	masked, err := json.MarshalIndent(redacted, "", "  ")
	if err != nil {
		return InstanceConfig{}, err
	}
	out.Raw = string(masked)
	out.RedactedPaths = paths
	return out, nil
}

// WriteInstanceConfig replaces one workspace's config.json with raw.
//
// revision, when non-empty, must match the current bytes or ErrStaleRevision is
// returned and nothing is written: this file has a second writer
// (materializeModels), so a blind write could silently discard a materialization
// that landed between the admin's read and their save.
//
// After the write it runs the ordinary already-provisioned re-materialization.
// That — not a diff, not a whitelist — is what keeps ManagedConfigPaths
// authoritative: the keys become correct by construction, and there is no second
// copy of the merge rules to drift away from materializeModels. A submitted
// document that edited one of them is overwritten, which is why the returned
// InstanceConfig is read back AFTER the reapply.
func (m *Manager) WriteInstanceConfig(key WorkspaceKey, raw, revision string) (InstanceConfig, ReapplyResult, error) {
	if len(raw) > maxInstanceConfigBytes {
		return InstanceConfig{}, ReapplyResult{}, ErrConfigTooLarge
	}
	submitted, err := parseConfigObject([]byte(raw))
	if err != nil {
		return InstanceConfig{}, ReapplyResult{}, err
	}

	path := m.instanceConfigPath(key)
	current, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return InstanceConfig{}, ReapplyResult{}, ErrNotProvisioned
		}
		return InstanceConfig{}, ReapplyResult{}, err
	}
	if revision != "" && revision != revisionOf(current) {
		return InstanceConfig{}, ReapplyResult{}, ErrStaleRevision
	}

	// A current file that does not parse holds no credential to restore, which is
	// the whole point of tolerating the error here: that is the repair case.
	currentDoc, _ := parseConfigObject(current)
	bytesToWrite, err := unmaskAgainst([]byte(raw), submitted, currentDoc)
	if err != nil {
		return InstanceConfig{}, ReapplyResult{}, err
	}
	if err := writeConfigAtomic(path, bytesToWrite); err != nil {
		return InstanceConfig{}, ReapplyResult{}, err
	}
	// The rename produced a new inode owned by the proxy, and the workspace is
	// bind-mounted into a container running as PicoclawUser. Walking the single
	// file reuses chownTree's Lchown semantics rather than restating them.
	if err := chownTree(path, m.cfg.PicoclawUser); err != nil {
		return InstanceConfig{}, ReapplyResult{}, fmt.Errorf("chown config.json: %w", err)
	}

	reapplied := ReapplyResult{OK: true}
	if err := m.reapplyWorkspace(key); err != nil {
		m.logf("instance config: reapply %+v failed: %v", key, err)
		reapplied = ReapplyResult{OK: false, Detail: err.Error()}
	}

	out, err := m.ReadInstanceConfig(key)
	return out, reapplied, err
}

func (m *Manager) instanceConfigPath(key WorkspaceKey) string {
	return filepath.Join(config.UserWorkspace(m.cfg.ContainerDataRoot,
		key.TenantID, key.SubsAccID, key.Role, key.UserAccID), "config.json")
}

// writeConfigAtomic writes b through a temp file in the SAME directory and
// renames it over path. A torn write here would brick the instance being
// repaired, and a temp file elsewhere could land on another filesystem, where
// rename(2) is not atomic.
func writeConfigAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "config.json.tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeded
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// parseConfigObject parses b and insists on a JSON object. A syntax error and a
// well-formed non-object are different repairs, so they stay different errors:
// the first points at a character, the second at the shape. `null` and a
// top-level array both land in the second case — decoding `null` into a map
// succeeds and leaves it nil, which would otherwise read as a valid config.
func parseConfigObject(b []byte) (map[string]any, error) {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	doc, ok := v.(map[string]any)
	if !ok {
		return nil, ErrConfigNotObject
	}
	return doc, nil
}

func revisionOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// syntaxOffset is the byte position a parse failure points at, or -1 when the
// error carries none. An admin cannot act on "invalid character at offset
// 4213" alone, but the UI turns the offset into a line and column.
func syntaxOffset(err error) int64 {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return se.Offset
	}
	return -1
}

// redactModelKeys masks credentials a legacy layout may still hold in
// config.json. The current code writes model keys to .security.yml
// (migrate_models.go) and materializeModels rebuilds model_list from the
// registry with no key field at all — but a workspace not materialized since
// that migration can still carry model_list[*].api_keys here.
//
// It returns a COPY plus the dotted paths it masked, and runs on the read path
// only. Masking cannot round-trip a "***" onto disk as a credential because
// model_list is a managed path: the reapply after every write replaces the whole
// array from the registry.
//
// Both layouts are handled: the current array form and the older object form
// keyed by model name.
func redactModelKeys(doc map[string]any) (map[string]any, []string) {
	var paths []string
	out := doc
	switch list := doc["model_list"].(type) {
	case []any:
		for i, item := range list {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if _, has := entry["api_keys"]; !has {
				continue
			}
			if len(paths) == 0 {
				out = shallowCopyForRedaction(doc, list)
			}
			redacted := out["model_list"].([]any)
			redacted[i] = maskAPIKeys(entry)
			paths = append(paths, fmt.Sprintf("model_list[%d].api_keys", i))
		}
	case map[string]any:
		for name, item := range list {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if _, has := entry["api_keys"]; !has {
				continue
			}
			if len(paths) == 0 {
				out = shallowCopyForRedactionMap(doc, list)
			}
			out["model_list"].(map[string]any)[name] = maskAPIKeys(entry)
			paths = append(paths, "model_list."+name+".api_keys")
		}
	}
	return out, paths
}

// shallowCopyForRedaction / shallowCopyForRedactionMap copy only what redaction
// replaces — the top level and the model_list container. Redaction must not
// mutate the caller's parsed document.
func shallowCopyForRedaction(doc map[string]any, list []any) map[string]any {
	out := make(map[string]any, len(doc))
	for k, v := range doc {
		out[k] = v
	}
	copied := make([]any, len(list))
	copy(copied, list)
	out["model_list"] = copied
	return out
}

func shallowCopyForRedactionMap(doc map[string]any, list map[string]any) map[string]any {
	out := make(map[string]any, len(doc))
	for k, v := range doc {
		out[k] = v
	}
	copied := make(map[string]any, len(list))
	for k, v := range list {
		copied[k] = v
	}
	out["model_list"] = copied
	return out
}

// unmaskAgainst returns the bytes to write, restoring any credential the read
// path masked.
//
// The reason it exists: an admin round-trips the document they were SHOWN, which
// carries "***" wherever redactModelKeys hid a legacy credential. Writing that
// verbatim replaces the key with the mask. The original design leaned on the
// post-write materialization to rebuild `model_list` from the registry and make
// the mask unreachable — but that pass is best-effort by design, and a registry
// that resolves nothing is exactly the broken-instance case this feature exists
// for. A save would then have quietly destroyed the workspace's only copy of the
// key.
//
// So the restore happens BEFORE the write and does not depend on the reapply. A
// document with no masks is written byte-for-byte, which keeps the common case
// (a post-migration workspace) exactly as the admin typed it.
func unmaskAgainst(raw []byte, submitted, current map[string]any) ([]byte, error) {
	if current == nil {
		return raw, nil
	}
	restored, changed := restoreMaskedModelKeys(submitted, current)
	if !changed {
		return raw, nil
	}
	// Only the legacy path re-marshals. Those documents were already reformatted
	// on the way out (redaction re-marshals), so the admin is not seeing a
	// formatting change they did not cause.
	return json.MarshalIndent(restored, "", "  ")
}

// restoreMaskedModelKeys replaces every masked api_keys value in submitted with
// the value the file on disk still holds, returning a copy and whether anything
// was restored. A mask with no counterpart on disk is left alone: it is then a
// literal "***" the admin typed, not a hidden credential.
func restoreMaskedModelKeys(submitted, current map[string]any) (map[string]any, bool) {
	changed := false
	out := submitted

	switch sub := submitted["model_list"].(type) {
	case []any:
		cur, ok := current["model_list"].([]any)
		if !ok {
			return out, false
		}
		for i, item := range sub {
			entry, ok := item.(map[string]any)
			if !ok || !holdsMask(entry["api_keys"]) || i >= len(cur) {
				continue
			}
			curEntry, ok := cur[i].(map[string]any)
			if !ok || curEntry["api_keys"] == nil {
				continue
			}
			if !changed {
				out = shallowCopyForRedaction(submitted, sub)
				changed = true
			}
			restored := copyEntry(entry)
			restored["api_keys"] = curEntry["api_keys"]
			out["model_list"].([]any)[i] = restored
		}
	case map[string]any:
		cur, ok := current["model_list"].(map[string]any)
		if !ok {
			return out, false
		}
		for name, item := range sub {
			entry, ok := item.(map[string]any)
			if !ok || !holdsMask(entry["api_keys"]) {
				continue
			}
			curEntry, ok := cur[name].(map[string]any)
			if !ok || curEntry["api_keys"] == nil {
				continue
			}
			if !changed {
				out = shallowCopyForRedactionMap(submitted, sub)
				changed = true
			}
			restored := copyEntry(entry)
			restored["api_keys"] = curEntry["api_keys"]
			out["model_list"].(map[string]any)[name] = restored
		}
	}
	return out, changed
}

// holdsMask reports whether a value is the redaction placeholder, in either the
// array form maskAPIKeys writes or the bare-string fallback.
func holdsMask(v any) bool {
	switch keys := v.(type) {
	case []any:
		for _, k := range keys {
			if k == maskPlaceholder {
				return true
			}
		}
	case string:
		return keys == maskPlaceholder
	}
	return false
}

func copyEntry(entry map[string]any) map[string]any {
	out := make(map[string]any, len(entry))
	for k, v := range entry {
		out[k] = v
	}
	return out
}

// maskPlaceholder stands in for a credential the admin screen must not display.
const maskPlaceholder = "***"

func maskAPIKeys(entry map[string]any) map[string]any {
	out := copyEntry(entry)
	switch keys := entry["api_keys"].(type) {
	case []any:
		masked := make([]any, len(keys))
		for i := range keys {
			masked[i] = maskPlaceholder
		}
		out["api_keys"] = masked
	default:
		out["api_keys"] = maskPlaceholder
	}
	return out
}

// changedConfigPaths reports which of ManagedConfigPaths differ between two
// parsed configs. Used by the anti-drift test, and by nothing in production —
// the reapply is what enforces ownership, not a diff.
func changedConfigPaths(before, after map[string]any) []string {
	var out []string
	for _, p := range ManagedConfigPaths {
		b, _ := json.Marshal(valueAtPath(before, p))
		a, _ := json.Marshal(valueAtPath(after, p))
		if string(b) != string(a) {
			out = append(out, p)
		}
	}
	return out
}

// valueAtPath walks a dotted path through nested objects, returning nil when any
// segment is missing or is not an object.
func valueAtPath(doc map[string]any, dotted string) any {
	var cur any = doc
	for _, seg := range strings.Split(dotted, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = obj[seg]
		if !ok {
			return nil
		}
	}
	return cur
}
