package docker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// instanceConfigFixture builds a Manager over a temp data root with one
// workspace holding body as its config.json (body == "" leaves the workspace
// unprovisioned), and returns the registry, the key and the file path.
//
// testManagerWithRegistry leaves PicoclawUser empty, which is what makes these
// tests runnable here: chownTree no-ops without it, and Lchown is not permitted
// in the sandbox (the TestEnsureRunning* family fails on exactly that). The chown
// path is exercised by those integration tests, not by these.
func instanceConfigFixture(t *testing.T, body string) (*Manager, *registry.Registry, WorkspaceKey, string) {
	t.Helper()
	m, reg, root := testManagerWithRegistry(t)
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	userDir := config.UserWorkspace(root, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(userDir, "config.json")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return m, reg, key, path
}

const validConfigBody = `{
  "version": 3,
  "agents": {
    "defaults": {
      "provider": "openai",
      "model_name": "main",
      "max_tokens": 32768
    }
  },
  "channel_list": {
    "pico": {
      "enabled": true
    }
  },
  "model_list": [],
  "tools": {
    "exec": {
      "enabled": true,
      "timeout_seconds": 60
    }
  }
}`

func TestReadInstanceConfigReturnsBytesVerbatim(t *testing.T) {
	m, _, key, _ := instanceConfigFixture(t, validConfigBody)

	got, err := m.ReadInstanceConfig(key)
	if err != nil {
		t.Fatalf("ReadInstanceConfig: %v", err)
	}
	if got.Raw != validConfigBody {
		t.Errorf("Raw was reformatted; the admin must see the file, not a re-marshal:\n%s", got.Raw)
	}
	if !got.Valid || got.ParseError != "" {
		t.Errorf("Valid=%v ParseError=%q, want a clean parse", got.Valid, got.ParseError)
	}
	if got.Revision != revisionOf([]byte(validConfigBody)) {
		t.Errorf("Revision = %q, want the hash of the on-disk bytes", got.Revision)
	}
	if got.Size != int64(len(validConfigBody)) {
		t.Errorf("Size = %d, want %d", got.Size, len(validConfigBody))
	}
	if len(got.ManagedPaths) != len(ManagedConfigPaths) {
		t.Errorf("ManagedPaths = %v, want the exported constant", got.ManagedPaths)
	}
}

func TestReadInstanceConfigOnBrokenJSON(t *testing.T) {
	broken := `{"version": 3, "agents": {`
	m, _, key, _ := instanceConfigFixture(t, broken)

	got, err := m.ReadInstanceConfig(key)
	if err != nil {
		t.Fatalf("a broken config is data, not an error: %v", err)
	}
	if got.Valid {
		t.Error("Valid = true for a truncated document")
	}
	if got.ParseError == "" {
		t.Error("ParseError is empty; the UI has nothing to show the admin")
	}
	if got.Offset <= 0 {
		t.Errorf("Offset = %d, want the syntax error's position", got.Offset)
	}
	if got.Raw != broken {
		t.Errorf("Raw = %q, want the bytes as they are on disk", got.Raw)
	}
}

// A top-level `null` decodes into a map without error and would otherwise read
// as a valid config. It is not one, and picoclaw cannot boot from it.
func TestReadInstanceConfigRejectsNonObject(t *testing.T) {
	for _, body := range []string{"null", "[]", "42"} {
		m, _, key, _ := instanceConfigFixture(t, body)
		got, err := m.ReadInstanceConfig(key)
		if err != nil {
			t.Fatalf("body %q: %v", body, err)
		}
		if got.Valid {
			t.Errorf("body %q reported Valid=true", body)
		}
	}
}

func TestReadInstanceConfigNotProvisioned(t *testing.T) {
	m, _, key, _ := instanceConfigFixture(t, "")

	if _, err := m.ReadInstanceConfig(key); !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("err = %v, want ErrNotProvisioned", err)
	}
}

func TestReadInstanceConfigRedactsLegacyModelKeys(t *testing.T) {
	legacy := `{"model_list":[{"model_name":"main","api_keys":["sk-live-secret"]}]}`
	m, _, key, _ := instanceConfigFixture(t, legacy)

	got, err := m.ReadInstanceConfig(key)
	if err != nil {
		t.Fatalf("ReadInstanceConfig: %v", err)
	}
	if strings.Contains(got.Raw, "sk-live-secret") {
		t.Fatalf("credential reached the response:\n%s", got.Raw)
	}
	if !strings.Contains(got.Raw, `"***"`) {
		t.Errorf("no mask in the response:\n%s", got.Raw)
	}
	if len(got.RedactedPaths) != 1 || got.RedactedPaths[0] != "model_list[0].api_keys" {
		t.Errorf("RedactedPaths = %v, want the masked path so the UI can explain it", got.RedactedPaths)
	}
	// The revision is the DISK revision, not the redacted body's: the admin's
	// first save compares it against the file and would 409 otherwise.
	if got.Revision != revisionOf([]byte(legacy)) {
		t.Error("Revision was computed over the redacted body; the first save would 409")
	}
}

func TestReadInstanceConfigRedactsLegacyObjectLayout(t *testing.T) {
	legacy := `{"model_list":{"main":{"api_keys":["sk-live-secret"]}}}`
	m, _, key, _ := instanceConfigFixture(t, legacy)

	got, err := m.ReadInstanceConfig(key)
	if err != nil {
		t.Fatalf("ReadInstanceConfig: %v", err)
	}
	if strings.Contains(got.Raw, "sk-live-secret") {
		t.Fatalf("credential reached the response:\n%s", got.Raw)
	}
	if len(got.RedactedPaths) != 1 || got.RedactedPaths[0] != "model_list.main.api_keys" {
		t.Errorf("RedactedPaths = %v", got.RedactedPaths)
	}
}

// Redaction must not mutate the document it was handed, or a caller reusing the
// parse would see masked values as real config.
func TestRedactModelKeysDoesNotMutateInput(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(`{"model_list":[{"api_keys":["sk-a"]}]}`), &doc); err != nil {
		t.Fatal(err)
	}
	redactModelKeys(doc)
	entry := doc["model_list"].([]any)[0].(map[string]any)
	if got := entry["api_keys"].([]any)[0]; got != "sk-a" {
		t.Errorf("input was mutated: api_keys[0] = %v", got)
	}
}

func TestWriteInstanceConfigRejectsInvalidJSON(t *testing.T) {
	m, _, key, path := instanceConfigFixture(t, validConfigBody)

	if _, _, err := m.WriteInstanceConfig(key, `{"broken":`, ""); err == nil {
		t.Fatal("a truncated document was accepted")
	}
	if _, _, err := m.WriteInstanceConfig(key, `["not","an","object"]`, ""); !errors.Is(err, ErrConfigNotObject) {
		t.Fatalf("array top level: err = %v, want ErrConfigNotObject", err)
	}
	if _, _, err := m.WriteInstanceConfig(key, `null`, ""); !errors.Is(err, ErrConfigNotObject) {
		t.Fatalf("null: err = %v, want ErrConfigNotObject", err)
	}
	onDisk, _ := os.ReadFile(path)
	if string(onDisk) != validConfigBody {
		t.Error("a rejected write still touched the file")
	}
}

func TestWriteInstanceConfigRejectsStaleRevision(t *testing.T) {
	m, _, key, path := instanceConfigFixture(t, validConfigBody)

	_, _, err := m.WriteInstanceConfig(key, `{"version":4}`, "sha256:deadbeef")
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("err = %v, want ErrStaleRevision", err)
	}
	onDisk, _ := os.ReadFile(path)
	if string(onDisk) != validConfigBody {
		t.Error("a stale write reached the file")
	}
}

func TestWriteInstanceConfigRejectsOversize(t *testing.T) {
	m, _, key, path := instanceConfigFixture(t, validConfigBody)

	huge := `{"pad":"` + strings.Repeat("x", maxInstanceConfigBytes) + `"}`
	if _, _, err := m.WriteInstanceConfig(key, huge, ""); !errors.Is(err, ErrConfigTooLarge) {
		t.Fatalf("err = %v, want ErrConfigTooLarge", err)
	}
	onDisk, _ := os.ReadFile(path)
	if string(onDisk) != validConfigBody {
		t.Error("an oversize write reached the file")
	}
}

func TestWriteInstanceConfigIsAtomicAnd0600(t *testing.T) {
	m, _, key, path := instanceConfigFixture(t, validConfigBody)

	body := `{"version":4}`
	if _, _, err := m.WriteInstanceConfig(key, body, ""); err != nil {
		t.Fatalf("WriteInstanceConfig: %v", err)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != body {
		t.Errorf("on disk = %q, want the submitted bytes", onDisk)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "config.json.tmp-*"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

// The write lands even when the workspace was never model-materialized (no
// registry on this Manager): reapply is best-effort and its failure is reported,
// not returned, because the admin's repair already succeeded.
func TestWriteInstanceConfigSurvivesReapplyFailure(t *testing.T) {
	m, _, key, path := instanceConfigFixture(t, validConfigBody)

	body := `{"version":5}`
	got, reapplied, err := m.WriteInstanceConfig(key, body, "")
	if err != nil {
		t.Fatalf("WriteInstanceConfig: %v", err)
	}
	if reapplied.OK {
		t.Fatal("reapply reported OK with no registry configured")
	}
	if reapplied.Detail == "" {
		t.Error("reapply failure carries no detail for the admin")
	}
	onDisk, _ := os.ReadFile(path)
	if string(onDisk) != body {
		t.Errorf("the write was rolled back: %q", onDisk)
	}
	if got.Raw != body {
		t.Errorf("returned Raw = %q, want the post-write file", got.Raw)
	}
}

// The repair case: a config.json that does not parse blocks materialization
// (materializeModels fails at "parse config.json"). Writing a valid document
// unblocks it, in the same call.
func TestWriteInstanceConfigRepairsUnparseableFile(t *testing.T) {
	m, _, key, _ := instanceConfigFixture(t, `{"version": 3, "agents": {`)

	before, err := m.ReadInstanceConfig(key)
	if err != nil {
		t.Fatal(err)
	}
	if before.Valid {
		t.Fatal("fixture is not broken")
	}

	after, _, err := m.WriteInstanceConfig(key, validConfigBody, before.Revision)
	if err != nil {
		t.Fatalf("WriteInstanceConfig: %v", err)
	}
	if !after.Valid {
		t.Errorf("still invalid after the repair: %s", after.ParseError)
	}
	if after.Revision == before.Revision {
		t.Error("revision did not advance after a successful write")
	}
}

// The reapply after a write is what keeps ManagedConfigPaths authoritative: an
// admin who edits agents.defaults.model_name gets the registry's value back.
func TestWriteInstanceConfigReapplyRestoresManagedPaths(t *testing.T) {
	m, reg, key, path := instanceConfigFixture(t, validConfigBody)
	userDir := filepath.Dir(path)
	sec := "channel_list:\n  pico:\n    settings:\n      token: pico-seed\n"
	if err := os.WriteFile(filepath.Join(userDir, ".security.yml"), []byte(sec), 0o600); err != nil {
		t.Fatal(err)
	}

	// A registry resolving to "main"/openai, so materialization has something
	// authoritative to re-impose.
	if _, err := reg.CreateModel(registry.Model{
		ModelName: "main", Provider: "openai", Model: "gpt-5.4",
		APIBase: "https://api.openai.com/v1", APIKey: "sk-main", Status: registry.StatusActive,
	}); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if err := reg.SetScopeDefault(registry.ScopeSel{Level: registry.LevelGlobal}, "main"); err != nil {
		t.Fatalf("SetScopeDefault: %v", err)
	}

	// The admin points the workspace at a model of their own invention.
	tampered := strings.Replace(validConfigBody, `"model_name": "main"`, `"model_name": "hand-edited"`, 1)
	got, reapplied, err := m.WriteInstanceConfig(key, tampered, "")
	if err != nil {
		t.Fatalf("WriteInstanceConfig: %v", err)
	}
	if !reapplied.OK {
		t.Fatalf("reapply failed: %s", reapplied.Detail)
	}
	if strings.Contains(got.Raw, "hand-edited") {
		t.Errorf("a managed path survived the write:\n%s", got.Raw)
	}
	if !strings.Contains(got.Raw, `"model_name": "main"`) {
		t.Errorf("the registry's model was not re-imposed:\n%s", got.Raw)
	}
}

// TestManagedConfigPathsMatchWriters is the anti-drift gate. It works
// behaviourally rather than by reading source: seed a config whose every managed
// path holds a sentinel, run BOTH writers, and assert that exactly the listed
// paths changed. A new writer touching an unlisted path fails this test; a listed
// path no longer written fails it too.
func TestManagedConfigPathsMatchWriters(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	secPath := filepath.Join(dir, ".security.yml")

	seed := map[string]any{
		"version":      3,
		"model_list":   []any{map[string]any{"model_name": "sentinel"}},
		"agents":       map[string]any{"defaults": map[string]any{"provider": "sentinel", "model_name": "sentinel", "model_fallbacks": []any{"sentinel"}, "workspace": "/sentinel", "max_tokens": 1234}},
		"channel_list": map[string]any{"pico": map[string]any{"enabled": false}},
		"tools":        map[string]any{"exec": map[string]any{"enabled": true}},
		"gateway":      map[string]any{"host": "localhost", "port": 18790},
	}
	raw, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	before := readConfig(t, configPath)
	if err := materializeModels(configPath, secPath, testResolution()); err != nil {
		t.Fatalf("materializeModels: %v", err)
	}
	if err := alignWorkspace(configPath, "/home/picoclaw"); err != nil {
		t.Fatalf("alignWorkspace: %v", err)
	}
	after := readConfig(t, configPath)

	changed := changedConfigPaths(before, after)
	if len(changed) != len(ManagedConfigPaths) {
		t.Errorf("changed = %v, want every managed path to have been rewritten (%v)",
			changed, ManagedConfigPaths)
	}

	// And nothing outside the list moved. Compare every top-level subtree with
	// its managed paths blanked out on both sides.
	beforeRest, afterRest := blankManaged(before), blankManaged(after)
	b, _ := json.Marshal(beforeRest)
	a, _ := json.Marshal(afterRest)
	if string(b) != string(a) {
		t.Errorf("a writer touched a path outside ManagedConfigPaths.\nbefore: %s\nafter:  %s", b, a)
	}
}

// blankManaged deep-copies doc with every managed path removed, so what is left
// is the surface an admin owns.
func blankManaged(doc map[string]any) map[string]any {
	raw, _ := json.Marshal(doc)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	for _, p := range ManagedConfigPaths {
		segs := strings.Split(p, ".")
		cur := out
		ok := true
		for _, seg := range segs[:len(segs)-1] {
			next, isMap := cur[seg].(map[string]any)
			if !isMap {
				ok = false
				break
			}
			cur = next
		}
		if ok {
			delete(cur, segs[len(segs)-1])
		}
	}
	return out
}

// The mask must never become the stored credential. A legacy workspace's key is
// redacted on read, so the admin round-trips a document containing "***" — and
// the reapply that was supposed to replace model_list wholesale can FAIL (an
// unresolvable registry is exactly the broken-instance case this feature
// targets), leaving the mask on disk as the key.
func TestWriteInstanceConfigNeverStoresTheMask(t *testing.T) {
	legacy := `{"model_list":[{"model_name":"main","api_keys":["sk-live-secret"]}]}`
	m, _, key, path := instanceConfigFixture(t, legacy)

	read, err := m.ReadInstanceConfig(key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read.Raw, "***") {
		t.Fatalf("fixture was not redacted: %s", read.Raw)
	}

	// Submit exactly what the editor showed. No registry is configured, so the
	// reapply fails and cannot rebuild model_list.
	_, reapplied, err := m.WriteInstanceConfig(key, read.Raw, read.Revision)
	if err != nil {
		t.Fatalf("WriteInstanceConfig: %v", err)
	}
	if reapplied.OK {
		t.Fatal("reapply unexpectedly succeeded; this test needs the failing path")
	}

	onDisk, _ := os.ReadFile(path)
	if strings.Contains(string(onDisk), "***") {
		t.Errorf("the mask was stored as the credential:\n%s", onDisk)
	}
	if !strings.Contains(string(onDisk), "sk-live-secret") {
		t.Errorf("the credential was destroyed by a round-trip:\n%s", onDisk)
	}
}

func TestWriteInstanceConfigRestoresMaskInObjectLayout(t *testing.T) {
	legacy := `{"model_list":{"main":{"api_keys":["sk-live-secret"]}}}`
	m, _, key, path := instanceConfigFixture(t, legacy)

	read, err := m.ReadInstanceConfig(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.WriteInstanceConfig(key, read.Raw, read.Revision); err != nil {
		t.Fatalf("WriteInstanceConfig: %v", err)
	}
	onDisk, _ := os.ReadFile(path)
	if !strings.Contains(string(onDisk), "sk-live-secret") {
		t.Errorf("the credential was destroyed by a round-trip:\n%s", onDisk)
	}
}

// A mask with no counterpart on disk is a literal the admin typed, not a hidden
// credential, so it is written as given. Restoring "from nowhere" would silently
// resurrect a key the admin removed.
func TestWriteInstanceConfigKeepsAMaskWithNoStoredKey(t *testing.T) {
	m, _, key, path := instanceConfigFixture(t, `{"model_list":[{"model_name":"main"}]}`)

	body := `{"model_list":[{"model_name":"main","api_keys":["***"]}]}`
	if _, _, err := m.WriteInstanceConfig(key, body, ""); err != nil {
		t.Fatalf("WriteInstanceConfig: %v", err)
	}
	onDisk, _ := os.ReadFile(path)
	if string(onDisk) != body {
		t.Errorf("on disk = %q, want the submitted bytes verbatim", onDisk)
	}
}

// A document with no mask is written BYTE-FOR-BYTE. Only the legacy path
// re-marshals, so the common case keeps the admin's own formatting.
func TestWriteInstanceConfigDoesNotReformatWhenNothingIsMasked(t *testing.T) {
	m, _, key, path := instanceConfigFixture(t, validConfigBody)

	body := "{\"version\":9,   \"tools\":{}}"
	if _, _, err := m.WriteInstanceConfig(key, body, ""); err != nil {
		t.Fatalf("WriteInstanceConfig: %v", err)
	}
	onDisk, _ := os.ReadFile(path)
	if string(onDisk) != body {
		t.Errorf("on disk = %q, want the submitted bytes with their spacing intact", onDisk)
	}
}
