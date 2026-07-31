package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// A deliberately small template that still carries every leaf shape the
// flattener has to decide about: a string, a number, a bool, a JSON null, a free
// array, a MANAGED array, a nested object, and two empty objects. The shipped
// template is ~470 lines; none of the extra lines exercise a rule this one does
// not.
const sampleTemplateConfig = `{
  "agents": {
    "defaults": {
      "model_name": "gpt-x",
      "provider": "openai",
      "workspace": "/data/workspace"
    }
  },
  "allowed_hosts": ["example.org", "api.example.org"],
  "isolation": {},
  "model_list": [
    {"model_name": "gpt-x", "provider": "openai"}
  ],
  "persona": null,
  "tools": {
    "web": {
      "brave": {"enabled": false},
      "max_results": 5
    }
  },
  "turn_profile": {
    "history": {},
    "max_turns": 12
  },
  "version": 3
}`

// templateConfigManager builds the unprivileged Manager these tests need and
// seeds one agent template. PicoclawUser is empty because the suite runs as a
// normal user; logf is set because the record-failure path logs.
func templateConfigManager(t *testing.T, body string) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	m := &Manager{
		cfg:  &config.Config{ContainerDataRoot: root, PicoclawUser: ""},
		logf: func(string, ...any) {},
	}
	dir := config.TemplatesDir(root, "picoclaw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return m, root
}

func templateConfigPathFor(root string) string {
	return filepath.Join(config.TemplatesDir(root, "picoclaw"), "config.json")
}

func readTemplateConfigBytes(t *testing.T, root string) []byte {
	t.Helper()
	b, err := os.ReadFile(templateConfigPathFor(root))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func templateConfigKeyNames(cat TemplateCatalog) []string {
	out := make([]string, 0, len(cat.Keys))
	for _, k := range cat.Keys {
		out = append(out, k.Key)
	}
	return out
}

func templateConfigValueOf(t *testing.T, cat TemplateCatalog, key string) string {
	t.Helper()
	for _, k := range cat.Keys {
		if k.Key == key {
			return string(k.Value)
		}
	}
	t.Fatalf("key %q missing from catalog %v", key, templateConfigKeyNames(cat))
	return ""
}

var wantTemplateConfigLeaves = []string{
	"agents.defaults.model_name",
	"agents.defaults.provider",
	"agents.defaults.workspace",
	"allowed_hosts",
	"isolation",
	"model_list",
	"persona",
	"tools.web.brave.enabled",
	"tools.web.max_results",
	"turn_profile.history",
	"turn_profile.max_turns",
	"version",
}

// An array is a leaf and an EMPTY object is a leaf; a non-empty object is
// descended and never emitted itself. Indexing into the array would offer the
// admin a key (`allowed_hosts.0`) that setPath cannot write, and dropping the
// empty objects would silently hide `isolation` from the picker.
func TestTemplateConfigKeysFlattensLeafShapes(t *testing.T) {
	m, _ := templateConfigManager(t, sampleTemplateConfig)

	cat, err := m.TemplateConfigKeys("picoclaw")
	if err != nil {
		t.Fatalf("TemplateConfigKeys: %v", err)
	}
	if cat.Template != "picoclaw" {
		t.Errorf("Template = %q, want picoclaw", cat.Template)
	}
	if got := templateConfigKeyNames(cat); !reflect.DeepEqual(got, wantTemplateConfigLeaves) {
		t.Fatalf("keys =\n%v\nwant\n%v", got, wantTemplateConfigLeaves)
	}

	// The free array appears exactly once, whole, not descended into.
	if got := templateConfigValueOf(t, cat, "allowed_hosts"); got != `["example.org","api.example.org"]` {
		t.Errorf("allowed_hosts value = %s, want the whole array", got)
	}
	// Both empty objects survive as themselves.
	if got := templateConfigValueOf(t, cat, "isolation"); got != "{}" {
		t.Errorf("isolation value = %s, want {}", got)
	}
	if got := templateConfigValueOf(t, cat, "turn_profile.history"); got != "{}" {
		t.Errorf("turn_profile.history value = %s, want {}", got)
	}
	// A null is a value, not an absence.
	if got := templateConfigValueOf(t, cat, "persona"); got != "null" {
		t.Errorf("persona value = %s, want null", got)
	}
	if got := templateConfigValueOf(t, cat, "tools.web.max_results"); got != "5" {
		t.Errorf("tools.web.max_results value = %s, want 5", got)
	}
}

// Managed keys stay IN the catalog: the picker renders them disabled and says
// why. Filtering them out would leave an admin hunting for a key that is simply
// not editable.
func TestTemplateConfigKeysMarksManagedWithoutHidingThem(t *testing.T) {
	m, _ := templateConfigManager(t, sampleTemplateConfig)

	cat, err := m.TemplateConfigKeys("picoclaw")
	if err != nil {
		t.Fatalf("TemplateConfigKeys: %v", err)
	}

	managed := map[string]bool{}
	for _, k := range cat.Keys {
		managed[k.Key] = k.Managed
	}
	for _, key := range []string{"model_list", "agents.defaults.provider"} {
		got, present := managed[key]
		if !present {
			t.Errorf("%q missing from the catalog — managed keys must be included, not filtered", key)
			continue
		}
		if !got {
			t.Errorf("%q managed = false, want true", key)
		}
	}
	if managed["tools.web.max_results"] {
		t.Error("tools.web.max_results managed = true, want false — it is a free key")
	}
}

func TestTemplateConfigKeysAreSortedAndRevisionTracksTheFile(t *testing.T) {
	m, root := templateConfigManager(t, sampleTemplateConfig)

	cat, err := m.TemplateConfigKeys("picoclaw")
	if err != nil {
		t.Fatalf("TemplateConfigKeys: %v", err)
	}
	for i := 1; i < len(cat.Keys); i++ {
		if cat.Keys[i-1].Key >= cat.Keys[i].Key {
			t.Fatalf("keys not sorted ascending at %d: %q then %q",
				i, cat.Keys[i-1].Key, cat.Keys[i].Key)
		}
	}
	if cat.TemplateRevision == "" {
		t.Fatal("TemplateRevision is empty — the write path has no token to gate on")
	}
	if cat.TemplateRevision != revisionOf(readTemplateConfigBytes(t, root)) {
		t.Error("TemplateRevision does not match the bytes on disk")
	}

	res, err := m.ApplyTemplateConfigKey("picoclaw", "tools.web.max_results", 9,
		cat.TemplateRevision, "admin@example.org", time.Now().UTC())
	if err != nil || !res.OK {
		t.Fatalf("ApplyTemplateConfigKey: %v (result %+v)", err, res)
	}

	after, err := m.TemplateConfigKeys("picoclaw")
	if err != nil {
		t.Fatalf("TemplateConfigKeys after write: %v", err)
	}
	if after.TemplateRevision == cat.TemplateRevision {
		t.Error("TemplateRevision unchanged after a write — a stale token would be accepted forever")
	}
}

// A key no dotted path can address is OMITTED, not an error: the editor
// addresses every key by dotted path, so a row it could never resolve is worse
// than a row that is not there. A literal dot is the case a ValidateConfigKey
// check on the JOINED path misses — "my.key" splits into two legal segments and
// passes, while the nested path it describes does not exist in the document.
func TestTemplateConfigKeysOmitsKeysNoDottedPathCanAddress(t *testing.T) {
	m, root := templateConfigManager(t, `{
  "my.key": 1,
  "bad key": 2,
  "nested": {"worse key": {"good": 3}},
  "tools": {"web": {"max results": 4, "max_results": 5}},
  "version": 3
}`)

	cat, err := m.TemplateConfigKeys("picoclaw")
	if err != nil {
		t.Fatalf("TemplateConfigKeys: %v", err)
	}
	// nested disappears entirely: its only child is unaddressable, so descending
	// it yields no leaf and the object itself is not one.
	want := []string{"tools.web.max_results", "version"}
	if got := templateConfigKeyNames(cat); !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}

	// The invariant the omission exists for: every key the catalog offers can be
	// validated and then resolved in the very document it came from.
	doc, err := parseConfigObject(readTemplateConfigBytes(t, root))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range cat.Keys {
		if err := ValidateConfigKey(k.Key); err != nil {
			t.Errorf("catalog offers %q, which ValidateConfigKey refuses: %v", k.Key, err)
		}
		if _, state := lookupPath(doc, k.Key); state != pathFound {
			t.Errorf("catalog offers %q, which lookupPath cannot resolve (state %v)", k.Key, state)
		}
	}
}

func TestTemplateConfigKeysMissingTemplateIsAnError(t *testing.T) {
	m, root := templateConfigManager(t, sampleTemplateConfig)
	if err := os.Remove(templateConfigPathFor(root)); err != nil {
		t.Fatal(err)
	}

	if _, err := m.TemplateConfigKeys("picoclaw"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want it to wrap os.ErrNotExist", err)
	}
}

// A broken TEMPLATE is a real failure, unlike a broken instance document: there
// is no catalog to offer and no repair to make from it here. An empty catalog
// would read as "this agent has no keys", which is the wrong answer to act on.
func TestTemplateConfigKeysUnusableTemplateIsAnError(t *testing.T) {
	t.Run("syntax error", func(t *testing.T) {
		m, _ := templateConfigManager(t, `{"version": 3,`)
		if _, err := m.TemplateConfigKeys("picoclaw"); err == nil {
			t.Fatal("err = nil, want a parse failure")
		}
	})
	t.Run("json array", func(t *testing.T) {
		m, _ := templateConfigManager(t, `[{"version": 3}]`)
		if _, err := m.TemplateConfigKeys("picoclaw"); !errors.Is(err, ErrConfigNotObject) {
			t.Fatalf("err = %v, want ErrConfigNotObject", err)
		}
	})
}

// No screen shows the template document, so without the revision token two
// admins editing the same agent would clobber each other with no sign of it.
func TestApplyTemplateConfigKeyStaleRevisionWritesNothing(t *testing.T) {
	m, root := templateConfigManager(t, sampleTemplateConfig)
	before := readTemplateConfigBytes(t, root)

	res, err := m.ApplyTemplateConfigKey("picoclaw", "tools.web.max_results", 9,
		"sha256:not-the-current-one", "admin@example.org", time.Now().UTC())
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("err = %v, want ErrStaleRevision", err)
	}
	if res.OK {
		t.Error("OK = true on a stale revision")
	}
	if got := readTemplateConfigBytes(t, root); string(got) != string(before) {
		t.Error("template config.json changed on a stale revision")
	}
}

// WriteInstanceConfig treats an empty revision as "write anyway" for its
// non-UI callers. This path has none, so the same token is REQUIRED here:
// otherwise the one document that reaches every subscription would be the one
// with the weakest guard.
func TestApplyTemplateConfigKeyEmptyRevisionIsNotABypass(t *testing.T) {
	m, root := templateConfigManager(t, sampleTemplateConfig)
	before := readTemplateConfigBytes(t, root)

	res, err := m.ApplyTemplateConfigKey("picoclaw", "tools.web.max_results", 9,
		"", "admin@example.org", time.Now().UTC())
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("err = %v, want ErrStaleRevision", err)
	}
	if res.OK {
		t.Error("OK = true for an empty revision")
	}
	if got := readTemplateConfigBytes(t, root); string(got) != string(before) {
		t.Error("template config.json changed for an empty revision")
	}
}

// The same cap the instance editor enforces, for the same reason: a template is
// seeded into every future member's workspace, so a document bloated here is
// bloat multiplied.
func TestApplyTemplateConfigKeyRefusesADocumentOverTheCap(t *testing.T) {
	m, root := templateConfigManager(t, sampleTemplateConfig)
	before := readTemplateConfigBytes(t, root)

	huge := strings.Repeat("x", maxInstanceConfigBytes)
	res, err := m.ApplyTemplateConfigKey("picoclaw", "tools.web.note", huge,
		revisionOf(before), "admin@example.org", time.Now().UTC())
	if !errors.Is(err, ErrConfigTooLarge) {
		t.Fatalf("err = %v, want ErrConfigTooLarge", err)
	}
	if res.OK {
		t.Error("OK = true for an oversized document")
	}
	if got := readTemplateConfigBytes(t, root); string(got) != string(before) {
		t.Error("template config.json changed for an oversized document")
	}
}

func TestApplyTemplateConfigKeySetsOnlyTheTargetedLeaf(t *testing.T) {
	m, root := templateConfigManager(t, sampleTemplateConfig)
	before, err := parseConfigObject(readTemplateConfigBytes(t, root))
	if err != nil {
		t.Fatal(err)
	}

	rev := revisionOf(readTemplateConfigBytes(t, root))
	res, err := m.ApplyTemplateConfigKey("picoclaw", "tools.web.brave.enabled", true,
		rev, "admin@example.org", time.Now().UTC())
	if err != nil {
		t.Fatalf("ApplyTemplateConfigKey: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false: %+v", res)
	}

	// Compared on PARSED values: the write goes out through MarshalIndent, so
	// every byte of a compact seed moves and a byte diff would say nothing.
	after, err := parseConfigObject(readTemplateConfigBytes(t, root))
	if err != nil {
		t.Fatalf("template no longer parses: %v", err)
	}
	if got, _ := lookupPath(after, "tools.web.brave.enabled"); got != true {
		t.Errorf("tools.web.brave.enabled = %v, want true", got)
	}
	// Every sibling leaf, including the one next to the target inside tools.web,
	// has to survive: setPath must not replace a parent object wholesale.
	for _, key := range wantTemplateConfigLeaves {
		if key == "tools.web.brave.enabled" {
			continue
		}
		wantV, _ := json.Marshal(mustLookupTemplateConfig(t, before, key))
		gotV, _ := json.Marshal(mustLookupTemplateConfig(t, after, key))
		if string(gotV) != string(wantV) {
			t.Errorf("%s = %s after the write, want %s", key, gotV, wantV)
		}
	}
	// The loop above cannot see an ADDED key, and setPath creates intermediate
	// objects on its way to a leaf — so the key set has to be pinned too.
	cat, err := m.TemplateConfigKeys("picoclaw")
	if err != nil {
		t.Fatalf("TemplateConfigKeys after write: %v", err)
	}
	if got := templateConfigKeyNames(cat); !reflect.DeepEqual(got, wantTemplateConfigLeaves) {
		t.Errorf("keys after the write =\n%v\nwant the same set\n%v", got, wantTemplateConfigLeaves)
	}
}

func mustLookupTemplateConfig(t *testing.T, doc map[string]any, key string) any {
	t.Helper()
	v, state := lookupPath(doc, key)
	if state != pathFound {
		t.Fatalf("%q not found in the document (state %v)", key, state)
	}
	return v
}

// A managed key is rewritten on every materialization, so accepting the edit
// would promise the admin something the next provision undoes.
func TestApplyTemplateConfigKeyRefusesAManagedKey(t *testing.T) {
	m, root := templateConfigManager(t, sampleTemplateConfig)
	before := readTemplateConfigBytes(t, root)
	rev := revisionOf(before)

	res, err := m.ApplyTemplateConfigKey("picoclaw", "agents.defaults.provider", "anthropic",
		rev, "admin@example.org", time.Now().UTC())
	if !errors.Is(err, ErrManagedConfigPath) {
		t.Fatalf("err = %v, want ErrManagedConfigPath", err)
	}
	if res.OK {
		t.Error("OK = true for a managed key")
	}
	if got := readTemplateConfigBytes(t, root); string(got) != string(before) {
		t.Error("template config.json changed for a refused managed key")
	}
}

func TestApplyTemplateConfigKeyRejectsAnInvalidKey(t *testing.T) {
	m, root := templateConfigManager(t, sampleTemplateConfig)
	before := readTemplateConfigBytes(t, root)

	res, err := m.ApplyTemplateConfigKey("picoclaw", "tools..web", 1,
		revisionOf(before), "admin@example.org", time.Now().UTC())
	if !errors.Is(err, ErrInvalidConfigKey) {
		t.Fatalf("err = %v, want ErrInvalidConfigKey", err)
	}
	if res.OK {
		t.Error("OK = true for an invalid key")
	}
	if got := readTemplateConfigBytes(t, root); string(got) != string(before) {
		t.Error("template config.json changed for a refused key")
	}
}

// A non-object on the way to the leaf means setting the key would REPLACE that
// value rather than add a leaf, which is a different edit than the one asked for.
func TestApplyTemplateConfigKeyPathConflictWritesNothing(t *testing.T) {
	m, root := templateConfigManager(t, sampleTemplateConfig)
	before := readTemplateConfigBytes(t, root)

	res, err := m.ApplyTemplateConfigKey("picoclaw", "version.major", 4,
		revisionOf(before), "admin@example.org", time.Now().UTC())
	if !errors.Is(err, ErrPathConflict) {
		t.Fatalf("err = %v, want ErrPathConflict", err)
	}
	if res.OK {
		t.Error("OK = true on a path conflict")
	}
	if got := readTemplateConfigBytes(t, root); string(got) != string(before) {
		t.Error("template config.json changed on a path conflict")
	}
}

func TestApplyTemplateConfigKeyRecordsTheMigrationBesideTheTemplate(t *testing.T) {
	m, root := templateConfigManager(t, sampleTemplateConfig)
	before := readTemplateConfigBytes(t, root)
	at := time.Date(2026, 7, 31, 13, 45, 2, 0, time.UTC)

	res, err := m.ApplyTemplateConfigKey("picoclaw", "tools.web.max_results", 9,
		revisionOf(before), "admin@example.org", at)
	if err != nil {
		t.Fatalf("ApplyTemplateConfigKey: %v", err)
	}
	if res.Migration == "" {
		t.Fatal("Migration is empty — the record name is how a revert is found")
	}

	dir := filepath.Join(config.TemplatesDir(root, "picoclaw"), ".config-migrations")
	body, err := os.ReadFile(filepath.Join(dir, res.Migration))
	if err != nil {
		t.Fatalf("record not beside the template: %v", err)
	}
	var rec ConfigMigration
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatalf("record does not parse: %v", err)
	}
	if rec.Key != "tools.web.max_results" {
		t.Errorf("key = %q", rec.Key)
	}
	if string(rec.From) != "5" {
		t.Errorf("from = %s, want 5", rec.From)
	}
	if rec.FromAbsent {
		t.Error("fromAbsent = true for a key that existed")
	}
	if string(rec.To) != "9" {
		t.Errorf("to = %s, want 9", rec.To)
	}
	if !rec.AppliedAt.Equal(at) {
		t.Errorf("appliedAt = %v, want %v", rec.AppliedAt, at)
	}
	if rec.By != "admin@example.org" {
		t.Errorf("by = %q", rec.By)
	}
	// The template belongs to the AGENT, not to a tenant or subscription — which
	// is exactly why writing it reaches every subscription.
	want := ConfigMigrationScope{Agent: "picoclaw"}
	if rec.Scope != want {
		t.Errorf("scope = %+v, want %+v", rec.Scope, want)
	}
	if rec.RevisionBefore != revisionOf(before) {
		t.Errorf("revisionBefore = %q, want the pre-write revision", rec.RevisionBefore)
	}
	if rec.RevisionAfter != revisionOf(readTemplateConfigBytes(t, root)) {
		t.Errorf("revisionAfter = %q, want the post-write revision", rec.RevisionAfter)
	}
}

// Reverting an absent key means DELETING it, so "from" must be omitted and
// fromAbsent set rather than recording a null that reads as a value.
func TestApplyTemplateConfigKeyRecordsFromAbsentForANewKey(t *testing.T) {
	m, root := templateConfigManager(t, sampleTemplateConfig)
	before := readTemplateConfigBytes(t, root)

	res, err := m.ApplyTemplateConfigKey("picoclaw", "tools.web.brave.api_key_env", "BRAVE",
		revisionOf(before), "admin@example.org", time.Now().UTC())
	if err != nil {
		t.Fatalf("ApplyTemplateConfigKey: %v", err)
	}

	dir := filepath.Join(config.TemplatesDir(root, "picoclaw"), ".config-migrations")
	body, err := os.ReadFile(filepath.Join(dir, res.Migration))
	if err != nil {
		t.Fatal(err)
	}
	var rec ConfigMigration
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatal(err)
	}
	if !rec.FromAbsent {
		t.Error("fromAbsent = false for a key that did not exist")
	}
	if len(rec.From) != 0 {
		t.Errorf("from = %s, want it omitted", rec.From)
	}
	if string(rec.To) != `"BRAVE"` {
		t.Errorf("to = %s", rec.To)
	}
}

// The write already landed, so a failed record must not be reported as a failed
// apply: the admin would retry an edit that is already on disk.
func TestApplyTemplateConfigKeyRecordFailureStillReportsOK(t *testing.T) {
	m, root := templateConfigManager(t, sampleTemplateConfig)
	// A FILE where the records directory goes: MkdirAll then fails with ENOTDIR.
	blocker := filepath.Join(config.TemplatesDir(root, "picoclaw"), ".config-migrations")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := m.ApplyTemplateConfigKey("picoclaw", "tools.web.max_results", 9,
		revisionOf(readTemplateConfigBytes(t, root)), "admin@example.org", time.Now().UTC())
	if err != nil {
		t.Fatalf("err = %v, want nil — the write landed", err)
	}
	if !res.OK {
		t.Error("OK = false although the write landed")
	}
	if res.Detail == "" {
		t.Error("Detail is empty — the record failure has to be reported somewhere")
	}
	doc, err := parseConfigObject(readTemplateConfigBytes(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := lookupPath(doc, "tools.web.max_results"); got != float64(9) {
		t.Errorf("tools.web.max_results = %v, want the write to have landed", got)
	}
}

// TestTemplateConfigWriteIsNotChownedOrReapplied pins the two DELIBERATE
// omissions in ApplyTemplateConfigKey:
//
//   - no chown. The templates tree is not bind-mounted into any container, so
//     there is no container user to grant access to. PicoclawUser is set to a
//     bogus, non-existent id here on purpose: a chownTree call would fail EPERM
//     for an unprivileged process, so a successful write is evidence that none
//     was attempted. (Were the suite ever to run as root the Lchown would
//     succeed and this half would go quiet — the mode and ownership assertions
//     below are what still hold in that case.)
//   - no re-materialization. A template is not a workspace; materialization is
//     what happens to a workspace provisioned FROM this file. "No reapply" is
//     asserted through its only observable consequence: a provisioned workspace
//     under the same agent is untouched, byte for byte, and no migration record
//     appears anywhere under tenants/.
func TestTemplateConfigWriteIsNotChownedOrReapplied(t *testing.T) {
	m, root := templateConfigManager(t, sampleTemplateConfig)
	m.cfg.PicoclawUser = "4242:4242"

	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "picoclaw", UserAccID: "u1"}
	userDir := config.UserWorkspace(root, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const workspaceConfig = `{"version":3,"tools":{"web":{"max_results":1}}}`
	wsPath := filepath.Join(userDir, "config.json")
	if err := os.WriteFile(wsPath, []byte(workspaceConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := m.ApplyTemplateConfigKey("picoclaw", "tools.web.max_results", 9,
		revisionOf(readTemplateConfigBytes(t, root)), "admin@example.org", time.Now().UTC())
	if err != nil {
		t.Fatalf("ApplyTemplateConfigKey: %v — a chown attempt would surface here", err)
	}
	if !res.OK {
		t.Fatalf("OK = false: %+v", res)
	}

	fi, err := os.Stat(templateConfigPathFor(root))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("template config.json mode = %o, want 600", got)
	}
	if err := ownedByCurrentUser(templateConfigPathFor(root)); err != nil {
		t.Error(err)
	}

	if got, err := os.ReadFile(wsPath); err != nil || string(got) != workspaceConfig {
		t.Errorf("workspace config.json = %q (err %v), want it untouched — writing a "+
			"template must not re-materialize anybody's workspace", got, err)
	}
	tenantRecords, _ := filepath.Glob(filepath.Join(root, "tenants", "*", "subscriptions",
		"*", "agents", "*", "users", "*", ".config-migrations"))
	if len(tenantRecords) != 0 {
		t.Errorf("migration records under tenants/: %v — a template write touches no instance",
			tenantRecords)
	}
}

// ownedByCurrentUser holds even when the suite runs as root, where a stray
// Lchown to PicoclawUser would otherwise succeed unnoticed.
func ownedByCurrentUser(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("template config.json uid = %d, want %d — it must not be chowned "+
			"to the container user", st.Uid, os.Getuid())
	}
	return nil
}
