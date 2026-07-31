package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedConfigWorkspace seeds a provisioned workspace and replaces its config.json
// with body, so a test can state exactly the document the inspection will read.
// It reuses seedProvisionedWorkspace rather than restating the layout, because
// the on-disk shape (.../agents/<role>/users/<u>) is what workspacesInScope
// globs for and a private copy would drift from it.
func seedConfigWorkspace(t *testing.T, root string, key WorkspaceKey, body string) string {
	t.Helper()
	userDir := seedProvisionedWorkspace(t, root, key)
	if err := os.WriteFile(filepath.Join(userDir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return userDir
}

// seedUnprovisionedWorkspace leaves the user dir in place — workspacesInScope
// still finds it — but with no config.json, which is the "member never started
// this agent" case.
func seedUnprovisionedWorkspace(t *testing.T, root string, key WorkspaceKey) {
	t.Helper()
	userDir := seedProvisionedWorkspace(t, root, key)
	if err := os.Remove(filepath.Join(userDir, "config.json")); err != nil {
		t.Fatal(err)
	}
}

// subsScope is the scope the inspection is designed for: one subscription, one
// agent.
func subsScope(agent string) Scope {
	return Scope{Kind: ScopeSubscription, TenantID: "t1", SubsAccID: "s1", AgentKey: agent}
}

func bucketFor(t *testing.T, got ScopeConfigInspection, state BucketState) ConfigKeyBucket {
	t.Helper()
	for _, b := range got.Buckets {
		if b.State == state {
			return b
		}
	}
	t.Fatalf("no %q bucket in %+v", state, got.Buckets)
	return ConfigKeyBucket{}
}

// valueBucket finds the value bucket whose encoded value is enc. Encoding is the
// bucket identity, so the lookup is the assertion that grouping produced it.
func valueBucket(t *testing.T, got ScopeConfigInspection, enc string) ConfigKeyBucket {
	t.Helper()
	for _, b := range got.Buckets {
		if b.State == BucketValue && string(b.Value) == enc {
			return b
		}
	}
	t.Fatalf("no value bucket %s; buckets = %s", enc, mustJSON(t, got.Buckets))
	return ConfigKeyBucket{}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func userIDs(b ConfigKeyBucket) []string {
	out := make([]string, 0, len(b.Instances))
	for _, i := range b.Instances {
		out = append(out, i.UserAccID)
	}
	return out
}

// TestInspectScopeConfigKeyRefusesABadKeyBeforeTouchingDisk pins the ORDER of the
// two guards, not just their existence: the data root does not exist, so any
// implementation that enumerated first would return a filesystem error (or an
// empty success) instead of the key error the admin needs to see.
func TestInspectScopeConfigKeyRefusesABadKeyBeforeTouchingDisk(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	m.cfg.ContainerDataRoot = filepath.Join(root, "does-not-exist")

	for _, tc := range []struct {
		name string
		key  string
		want error
	}{
		{"empty", "", ErrInvalidConfigKey},
		{"empty segment", "tools..web", ErrInvalidConfigKey},
		{"traversal", "../etc/passwd", ErrInvalidConfigKey},
		{"managed leaf", "channel_list.pico.enabled", ErrManagedConfigPath},
		{"managed subtree", "model_list.deepseek-chat.api_keys", ErrManagedConfigPath},
		{"prefix of a managed path", "agents", ErrManagedConfigPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := m.InspectScopeConfigKey(subsScope("alpha"), tc.key)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got.Total != 0 || len(got.Buckets) != 0 || got.Key != "" {
				t.Errorf("refused inspection returned %+v, want the zero value", got)
			}
		})
	}
}

// TestInspectScopeConfigKeyGroupsEquivalentValues is the FR-2.3 contract that
// justifies having no custom canonicaliser: the encoder sorts object keys and
// the decoder normalises every number to float64, so equivalent JSON collides on
// its own. A hand-rolled canonicaliser would be a second source of truth.
func TestInspectScopeConfigKeyGroupsEquivalentValues(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	seedConfigWorkspace(t, root, wk("u1"), `{"n":1,"o":{"a":1,"b":2}}`)
	seedConfigWorkspace(t, root, wk("u2"), `{"n":1.0,"o":{"b":2,"a":1}}`)
	seedConfigWorkspace(t, root, wk("u3"), `{"n":2,"o":{"a":9}}`)

	byNumber, err := m.InspectScopeConfigKey(subsScope("alpha"), "n")
	if err != nil {
		t.Fatalf("InspectScopeConfigKey(n): %v", err)
	}
	if byNumber.Total != 3 {
		t.Errorf("Total = %d, want 3", byNumber.Total)
	}
	if b := valueBucket(t, byNumber, "1"); b.Count != 2 || len(b.Instances) != 2 {
		t.Errorf("1 and 1.0 did not share a bucket: %s", mustJSON(t, b))
	}
	if b := valueBucket(t, byNumber, "2"); b.Count != 1 {
		t.Errorf("bucket 2 count = %d, want 1", b.Count)
	}
	if len(byNumber.Buckets) != 2 {
		t.Errorf("got %d buckets, want 2: %s", len(byNumber.Buckets), mustJSON(t, byNumber.Buckets))
	}

	byObject, err := m.InspectScopeConfigKey(subsScope("alpha"), "o")
	if err != nil {
		t.Fatalf("InspectScopeConfigKey(o): %v", err)
	}
	if b := valueBucket(t, byObject, `{"a":1,"b":2}`); b.Count != 2 {
		t.Errorf("objects differing only in key order did not share a bucket: %s",
			mustJSON(t, byObject.Buckets))
	}
	if len(byObject.Buckets) != 2 {
		t.Errorf("got %d buckets, want 2: %s", len(byObject.Buckets), mustJSON(t, byObject.Buckets))
	}
}

// TestInspectScopeConfigKeySeparatesNullFromAbsent: a member who explicitly set
// the key to null and a member who never had it need DIFFERENT buckets, because
// the bulk edit that follows means something different for each. This is the
// reason lookupPath exists next to valueAtPath, which conflates them.
func TestInspectScopeConfigKeySeparatesNullFromAbsent(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	seedConfigWorkspace(t, root, wk("u1"), `{"k":null}`)
	seedConfigWorkspace(t, root, wk("u2"), `{"other":1}`)

	got, err := m.InspectScopeConfigKey(subsScope("alpha"), "k")
	if err != nil {
		t.Fatalf("InspectScopeConfigKey: %v", err)
	}
	if len(got.Buckets) != 2 {
		t.Fatalf("got %d buckets, want a value bucket and an absent bucket: %s",
			len(got.Buckets), mustJSON(t, got.Buckets))
	}
	nulls := valueBucket(t, got, "null")
	if ids := userIDs(nulls); len(ids) != 1 || ids[0] != "u1" {
		t.Errorf("null bucket holds %v, want [u1]", ids)
	}
	if ids := userIDs(bucketFor(t, got, BucketAbsent)); len(ids) != 1 || ids[0] != "u2" {
		t.Errorf("absent bucket holds %v, want [u2]", ids)
	}
}

func TestInspectScopeConfigKeyReportsAnUnprovisionedWorkspace(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	seedUnprovisionedWorkspace(t, root, wk("u1"))

	got, err := m.InspectScopeConfigKey(subsScope("alpha"), "k")
	if err != nil {
		t.Fatalf("InspectScopeConfigKey: %v", err)
	}
	if got.Total != 1 {
		t.Errorf("Total = %d, want 1 — a workspace with no config.json is still an instance", got.Total)
	}
	b := bucketFor(t, got, BucketUnreadable)
	if len(b.Instances) != 1 {
		t.Fatalf("unreadable bucket = %s", mustJSON(t, b))
	}
	if b.Instances[0].Detail != "not_provisioned" {
		t.Errorf("Detail = %q, want not_provisioned", b.Instances[0].Detail)
	}
	// No file, no revision: a later bulk apply must have nothing to match, so it
	// can never write into a workspace the admin never saw a document for.
	if b.Instances[0].Revision != "" {
		t.Errorf("Revision = %q, want empty", b.Instances[0].Revision)
	}
}

// TestInspectScopeConfigKeyReportsAnUnparseableConfig: the broken document must
// not be counted as holding a value. Silently treating it as absent would make a
// bulk apply look like it fixed an instance it never touched.
func TestInspectScopeConfigKeyReportsAnUnparseableConfig(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	seedConfigWorkspace(t, root, wk("u1"), `{"k":`)
	seedConfigWorkspace(t, root, wk("u2"), `{"k":"ok"}`)

	got, err := m.InspectScopeConfigKey(subsScope("alpha"), "k")
	if err != nil {
		t.Fatalf("InspectScopeConfigKey: %v", err)
	}
	if got.Total != 2 {
		t.Errorf("Total = %d, want 2", got.Total)
	}
	b := bucketFor(t, got, BucketUnreadable)
	if ids := userIDs(b); len(ids) != 1 || ids[0] != "u1" {
		t.Fatalf("unreadable bucket holds %v, want [u1]", ids)
	}
	if b.Instances[0].Detail == "" || b.Instances[0].Detail == "not_provisioned" {
		t.Errorf("Detail = %q, want the parse error", b.Instances[0].Detail)
	}
	if v := valueBucket(t, got, `"ok"`); v.Count != 1 {
		t.Errorf("the broken instance leaked into the value bucket: %s", mustJSON(t, v))
	}
	if len(got.Buckets) != 2 {
		t.Errorf("got %d buckets, want value + unreadable only: %s",
			len(got.Buckets), mustJSON(t, got.Buckets))
	}
}

// TestInspectScopeConfigKeyGivesAPathConflictItsOwnBucket: a scalar in the way is
// neither a value nor an unreadable file. Setting the key there would replace
// that scalar, so the admin has to be told about it separately.
func TestInspectScopeConfigKeyGivesAPathConflictItsOwnBucket(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	seedConfigWorkspace(t, root, wk("u1"), `{"tools":"off"}`)
	seedConfigWorkspace(t, root, wk("u2"), `{"tools":{"web":true}}`)

	got, err := m.InspectScopeConfigKey(subsScope("alpha"), "tools.web")
	if err != nil {
		t.Fatalf("InspectScopeConfigKey: %v", err)
	}
	b := bucketFor(t, got, BucketConflict)
	if ids := userIDs(b); len(ids) != 1 || ids[0] != "u1" {
		t.Fatalf("path_conflict bucket holds %v, want [u1]", ids)
	}
	if b.Instances[0].Detail == "" {
		t.Error("path_conflict instance carries no Detail naming the problem")
	}
	if v := valueBucket(t, got, "true"); v.Count != 1 {
		t.Errorf("value bucket = %s, want only u2", mustJSON(t, v))
	}
}

// TestInspectScopeConfigKeyCarriesRevisionAndEmail: the revision is what a later
// bulk apply gates on, so it must be the ON-DISK one; the email is what makes
// the admin screen readable, and it comes from a different source than the
// workspace enumeration.
func TestInspectScopeConfigKeyCarriesRevisionAndEmail(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	body := `{"k":"v"}`
	userDir := seedConfigWorkspace(t, root, wk("u1"), body)
	if err := os.WriteFile(filepath.Join(userDir, ".crab-owner.json"),
		[]byte(`{"email":"member@example.org"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A member with no owner marker must be listed without an email, not dropped.
	seedConfigWorkspace(t, root, wk("u2"), body)

	got, err := m.InspectScopeConfigKey(subsScope("alpha"), "k")
	if err != nil {
		t.Fatalf("InspectScopeConfigKey: %v", err)
	}
	b := valueBucket(t, got, `"v"`)
	if len(b.Instances) != 2 {
		t.Fatalf("bucket = %s, want both members", mustJSON(t, b))
	}
	for _, inst := range b.Instances {
		if inst.Revision != revisionOf([]byte(body)) {
			t.Errorf("%s Revision = %q, want the on-disk %q", inst.UserAccID, inst.Revision, revisionOf([]byte(body)))
		}
	}
	if b.Instances[0].Email != "member@example.org" {
		t.Errorf("u1 Email = %q, want member@example.org", b.Instances[0].Email)
	}
	if b.Instances[1].Email != "" {
		t.Errorf("u2 Email = %q, want empty — a missing marker is not a failure", b.Instances[1].Email)
	}
}

// TestInspectScopeConfigKeyFiltersByAgentKey: config.json is per WORKSPACE, and a
// member holding two agents has two of them. The inspection is scoped to one
// agent, so that member must appear exactly once — otherwise the counts an admin
// reasons about would double for every multi-agent member.
func TestInspectScopeConfigKeyFiltersByAgentKey(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	seedConfigWorkspace(t, root, WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}, `{"k":"from-alpha"}`)
	seedConfigWorkspace(t, root, WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "beta", UserAccID: "u1"}, `{"k":"from-beta"}`)

	got, err := m.InspectScopeConfigKey(subsScope("alpha"), "k")
	if err != nil {
		t.Fatalf("InspectScopeConfigKey: %v", err)
	}
	if got.Agent != "alpha" {
		t.Errorf("Agent = %q, want alpha", got.Agent)
	}
	if got.Total != 1 {
		t.Fatalf("Total = %d, want 1: %s", got.Total, mustJSON(t, got.Buckets))
	}
	if b := valueBucket(t, got, `"from-alpha"`); b.Count != 1 {
		t.Errorf("bucket = %s, want only the alpha workspace", mustJSON(t, b))
	}
}

// TestInspectScopeConfigKeyOrdersBucketsDeterministically: the two value buckets
// with EQUAL counts are the load-bearing part. Descending count alone leaves
// their relative order to Go's randomised map iteration, so an admin refreshing
// the screen would see the rows swap; the encoded-value tiebreak is what stops
// that.
func TestInspectScopeConfigKeyOrdersBucketsDeterministically(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	for _, u := range []string{"u10", "u02", "u06"} {
		seedConfigWorkspace(t, root, wk(u), `{"k":"mid"}`)
	}
	seedConfigWorkspace(t, root, wk("u20"), `{"k":"zeta"}`)
	seedConfigWorkspace(t, root, wk("u21"), `{"k":"zeta"}`)
	seedConfigWorkspace(t, root, wk("u30"), `{"k":"alpha"}`)
	seedConfigWorkspace(t, root, wk("u31"), `{"k":"alpha"}`)
	seedConfigWorkspace(t, root, wk("u40"), `{"other":1}`)
	seedConfigWorkspace(t, root, wk("u50"), `{"k":{"nested":1}}`)
	seedConfigWorkspace(t, root, wk("u60"), `{"k":"scalar"}`)
	seedUnprovisionedWorkspace(t, root, wk("u70"))

	got, err := m.InspectScopeConfigKey(subsScope("alpha"), "k.nested")
	if err != nil {
		t.Fatalf("InspectScopeConfigKey: %v", err)
	}
	if got.Total != 11 {
		t.Errorf("Total = %d, want 11", got.Total)
	}

	// k.nested resolves only for u50; the "mid"/"zeta"/"alpha"/"scalar" members
	// hold a string at k, so they conflict, and u40 has no k at all. Inspecting
	// the leaf k instead gives the value spread — do both, so ordering is checked
	// with several value buckets AND with the three non-value states present.
	byLeaf, err := m.InspectScopeConfigKey(subsScope("alpha"), "k")
	if err != nil {
		t.Fatalf("InspectScopeConfigKey(k): %v", err)
	}
	wantOrder := []string{`value:"mid"`, `value:"alpha"`, `value:"zeta"`,
		`value:"scalar"`, `value:{"nested":1}`, "absent:", "unreadable:"}
	if gotOrder := bucketOrder(byLeaf); !equalStrings(gotOrder, wantOrder) {
		t.Errorf("bucket order = %v, want %v", gotOrder, wantOrder)
	}
	if ids := userIDs(valueBucket(t, byLeaf, `"mid"`)); !equalStrings(ids, []string{"u02", "u06", "u10"}) {
		t.Errorf("instances = %v, want sorted by UserAccID", ids)
	}

	// The conflict bucket only exists for the nested key, so its position in the
	// tail (after absent, before unreadable) is asserted there.
	wantNested := []string{`value:1`, "absent:", "path_conflict:", "unreadable:"}
	if gotOrder := bucketOrder(got); !equalStrings(gotOrder, wantNested) {
		t.Errorf("nested bucket order = %v, want %v", gotOrder, wantNested)
	}

	// Two identical calls must be byte-identical: the UI diffs the response.
	again, err := m.InspectScopeConfigKey(subsScope("alpha"), "k")
	if err != nil {
		t.Fatalf("second InspectScopeConfigKey: %v", err)
	}
	if mustJSON(t, byLeaf) != mustJSON(t, again) {
		t.Errorf("two identical calls disagreed:\n%s\n%s", mustJSON(t, byLeaf), mustJSON(t, again))
	}
}

func bucketOrder(got ScopeConfigInspection) []string {
	out := make([]string, 0, len(got.Buckets))
	for _, b := range got.Buckets {
		out = append(out, string(b.State)+":"+string(b.Value))
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- apply -------------------------------------------------------------------

// applyAt is the one timestamp a batch carries, fixed so a written record can be
// compared against it exactly.
var applyAt = time.Date(2026, 7, 31, 13, 45, 2, 0, time.UTC)

// applyChange builds the request an admin's PUT turns into. Revisions is the gate,
// so it is always passed explicitly — a helper that filled it in from disk would
// make every test look like the happy path.
func applyChange(key, value string, revs map[string]string) ScopeConfigChange {
	return ScopeConfigChange{
		Key:       key,
		Value:     json.RawMessage(value),
		Revisions: revs,
		By:        "admin@example.org",
		AppliedAt: applyAt,
	}
}

// currentRevisions reads the revisions the way an inspect just did, which is where
// the admin's map comes from in production. Hand-writing hashes per test would
// drift from what the gate compares against.
func currentRevisions(t *testing.T, m *Manager, users ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, u := range users {
		cfg, err := m.ReadInstanceConfig(wk(u))
		if err != nil {
			t.Fatalf("ReadInstanceConfig(%s): %v", u, err)
		}
		out[u] = cfg.Revision
	}
	return out
}

func outcomeFor(t *testing.T, got ScopeConfigResult, user string) InstanceOutcome {
	t.Helper()
	for _, o := range got.Outcomes {
		if o.UserAccID == user {
			return o
		}
	}
	t.Fatalf("no outcome for %s in %s", user, mustJSON(t, got.Outcomes))
	return InstanceOutcome{}
}

// migrationRecords decodes every record in one workspace's .config-migrations,
// or nil when the directory was never created.
func migrationRecords(t *testing.T, userDir string) []ConfigMigration {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(userDir, ".config-migrations"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	out := make([]ConfigMigration, 0, len(entries))
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(userDir, ".config-migrations", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var rec ConfigMigration
		if err := json.Unmarshal(b, &rec); err != nil {
			t.Fatalf("record %s does not decode: %v", e.Name(), err)
		}
		out = append(out, rec)
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestApplyScopeConfigKeyRefusesABadRequestBeforeTouchingDisk pins the guards as
// UP-FRONT, the way the inspect test does: the data root does not exist, so an
// implementation that enumerated first would fail differently.
//
// The two value cases are about the ABSENCE of a request, not about nil: zero
// bytes mean the caller omitted the field and `{` is not JSON at all. An explicit
// `null` is a value and is accepted — see TestApplyScopeConfigKeyAcceptsAJSONNull.
func TestApplyScopeConfigKeyRefusesABadRequestBeforeTouchingDisk(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	m.cfg.ContainerDataRoot = filepath.Join(root, "does-not-exist")

	for _, tc := range []struct {
		name  string
		key   string
		value string
		want  error
	}{
		{"empty key", "", `true`, ErrInvalidConfigKey},
		{"empty segment", "tools..web", `true`, ErrInvalidConfigKey},
		{"managed leaf", "channel_list.pico.enabled", `true`, ErrManagedConfigPath},
		{"prefix of a managed path", "agents", `{}`, ErrManagedConfigPath},
		{"empty value", "tools.web", ``, ErrInvalidConfigValue},
		{"unparseable value", "tools.web", `{`, ErrInvalidConfigValue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := m.ApplyScopeConfigKey(subsScope("alpha"),
				applyChange(tc.key, tc.value, map[string]string{"u1": "sha256:whatever"}))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got.Key != "" || len(got.Outcomes) != 0 || got.Summary != nil {
				t.Errorf("refused apply returned %+v, want the zero value", got)
			}
		})
	}
}

// TestApplyScopeConfigKeyLeavesAMatchingInstanceUntouched: "apply to every
// instance that DIFFERS" is the whole contract. An instance already holding the
// value must not be rewritten — the mtime is the observable proof, and a record
// there would suggest a change that never happened.
func TestApplyScopeConfigKeyLeavesAMatchingInstanceUntouched(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	// 1 vs 1.0: the same value, differently encoded. Equality is canonical or this
	// instance is rewritten for nothing.
	body := `{"tools":{"web":1.0}}`
	userDir := seedConfigWorkspace(t, root, wk("u1"), body)
	path := filepath.Join(userDir, "config.json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	got, err := m.ApplyScopeConfigKey(subsScope("alpha"),
		applyChange("tools.web", `1`, currentRevisions(t, m, "u1")))
	if err != nil {
		t.Fatalf("ApplyScopeConfigKey: %v", err)
	}
	o := outcomeFor(t, got, "u1")
	if o.Outcome != OutcomeUnchanged {
		t.Errorf("Outcome = %q, want unchanged: %s", o.Outcome, mustJSON(t, o))
	}
	// No write means no reapply, and a non-nil zero ReapplyResult would read as a
	// reapply that ran and failed.
	if o.Reapplied != nil {
		t.Errorf("Reapplied = %s, want nil — nothing was written", mustJSON(t, o.Reapplied))
	}
	if o.Migration != "" {
		t.Errorf("Migration = %q, want none", o.Migration)
	}
	if recs := migrationRecords(t, userDir); len(recs) != 0 {
		t.Errorf("unchanged instance wrote %d records", len(recs))
	}
	if got := readFile(t, path); got != body {
		t.Errorf("config.json = %q, want the original bytes", got)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("mtime moved from %v to %v — the file was rewritten",
			before.ModTime(), after.ModTime())
	}
}

// TestApplyScopeConfigKeyRefusesAnInstanceTheAdminNeverSaw covers both halves of
// the revision gate. A MISSING revision is as stale as a mismatched one: the
// instance was provisioned after the admin's inspect, and writing to a document
// nobody looked at is exactly what the gate exists to prevent.
func TestApplyScopeConfigKeyRefusesAnInstanceTheAdminNeverSaw(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	body := `{"tools":{"web":false}}`
	newcomer := seedConfigWorkspace(t, root, wk("u1"), body)
	edited := seedConfigWorkspace(t, root, wk("u2"), body)

	got, err := m.ApplyScopeConfigKey(subsScope("alpha"), applyChange("tools.web", `true`,
		// u1 absent entirely; u2 present but pointing at bytes that are no longer there.
		map[string]string{"u2": revisionOf([]byte(`{"tools":{"web":"stale"}}`))}))
	if err != nil {
		t.Fatalf("ApplyScopeConfigKey: %v", err)
	}
	for user, dir := range map[string]string{"u1": newcomer, "u2": edited} {
		o := outcomeFor(t, got, user)
		if o.Outcome != OutcomeStale {
			t.Errorf("%s Outcome = %q, want stale: %s", user, o.Outcome, mustJSON(t, o))
		}
		if o.Reapplied != nil {
			t.Errorf("%s Reapplied = %s, want nil", user, mustJSON(t, o.Reapplied))
		}
		if got := readFile(t, filepath.Join(dir, "config.json")); got != body {
			t.Errorf("%s config.json = %q, want byte-identical", user, got)
		}
		if recs := migrationRecords(t, dir); len(recs) != 0 {
			t.Errorf("%s wrote %d records for a refused write", user, len(recs))
		}
	}
	if got.Summary["stale"] != 2 || len(got.Summary) != 1 {
		t.Errorf("Summary = %v, want stale:2 only", got.Summary)
	}
}

// TestApplyScopeConfigKeyKeepsGoingPastAPathConflict: one member with a scalar in
// the way must not block a policy change for the rest of the subscription. There
// is no transaction across instances to have.
func TestApplyScopeConfigKeyKeepsGoingPastAPathConflict(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	blocked := `{"tools":"off"}`
	conflicting := seedConfigWorkspace(t, root, wk("u1"), blocked)
	ok := seedConfigWorkspace(t, root, wk("u2"), `{"tools":{"web":false}}`)

	got, err := m.ApplyScopeConfigKey(subsScope("alpha"),
		applyChange("tools.web", `true`, currentRevisions(t, m, "u1", "u2")))
	if err != nil {
		t.Fatalf("ApplyScopeConfigKey: %v", err)
	}

	bad := outcomeFor(t, got, "u1")
	if bad.Outcome != OutcomePathConflict {
		t.Errorf("u1 Outcome = %q, want path_conflict", bad.Outcome)
	}
	if bad.Detail == "" {
		t.Error("u1 carries no Detail naming the conflict")
	}
	if got := readFile(t, filepath.Join(conflicting, "config.json")); got != blocked {
		t.Errorf("u1 config.json = %q, want byte-identical — a conflict is never forced", got)
	}
	if recs := migrationRecords(t, conflicting); len(recs) != 0 {
		t.Errorf("u1 wrote %d records for a refused write", len(recs))
	}

	good := outcomeFor(t, got, "u2")
	if good.Outcome != OutcomeApplied {
		t.Fatalf("u2 Outcome = %q, want applied: %s", good.Outcome, mustJSON(t, good))
	}
	onDisk := readFile(t, filepath.Join(ok, "config.json"))
	doc, err := parseConfigObject([]byte(onDisk))
	if err != nil {
		t.Fatalf("u2 config.json does not parse: %v\n%s", err, onDisk)
	}
	if v, state := lookupPath(doc, "tools.web"); state != pathFound || v != true {
		t.Errorf("u2 tools.web = %#v/%v, want true\n%s", v, state, onDisk)
	}
	if len(migrationRecords(t, ok)) != 1 {
		t.Errorf("u2 wrote %d records, want 1", len(migrationRecords(t, ok)))
	}
}

// TestApplyScopeConfigKeyPreservesALegacyCredential is the redaction/unmask
// pairing. ReadInstanceConfig masks a legacy model_list[*].api_keys and
// WriteInstanceConfig restores it from disk; editing cfg.Raw is what keeps those
// two halves together. A fresh os.ReadFile here would bypass the mask and pass
// only by luck — and the reapply that would otherwise rebuild model_list cannot
// run, because no model resolves in this fixture.
func TestApplyScopeConfigKeyPreservesALegacyCredential(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	legacy := `{"model_list":[{"model_name":"main","api_keys":["sk-live-secret"]}],` +
		`"tools":{"web":false}}`
	userDir := seedConfigWorkspace(t, root, wk("u1"), legacy)

	got, err := m.ApplyScopeConfigKey(subsScope("alpha"),
		applyChange("tools.web", `true`, currentRevisions(t, m, "u1")))
	if err != nil {
		t.Fatalf("ApplyScopeConfigKey: %v", err)
	}
	o := outcomeFor(t, got, "u1")
	if o.Outcome != OutcomeApplied {
		t.Fatalf("Outcome = %q, want applied: %s", o.Outcome, mustJSON(t, o))
	}
	if o.Reapplied == nil || o.Reapplied.OK {
		t.Fatalf("Reapplied = %s; this test needs the failing reapply path, so that a "+
			"rebuilt model_list cannot be what saved the credential", mustJSON(t, o.Reapplied))
	}

	onDisk := readFile(t, filepath.Join(userDir, "config.json"))
	if strings.Contains(onDisk, maskPlaceholder) {
		t.Errorf("the mask was stored as the credential:\n%s", onDisk)
	}
	if !strings.Contains(onDisk, "sk-live-secret") {
		t.Errorf("the credential was destroyed by the bulk edit:\n%s", onDisk)
	}
	doc, err := parseConfigObject([]byte(onDisk))
	if err != nil {
		t.Fatalf("config.json does not parse: %v\n%s", err, onDisk)
	}
	if v, state := lookupPath(doc, "tools.web"); state != pathFound || v != true {
		t.Errorf("tools.web = %#v/%v, want true\n%s", v, state, onDisk)
	}
}

// TestApplyScopeConfigKeyKeepsAWriteWhoseReapplyFailed: the write landed, and
// undoing it would throw away the admin's change. The failure is REPORTED per
// instance instead.
func TestApplyScopeConfigKeyKeepsAWriteWhoseReapplyFailed(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	userDir := seedConfigWorkspace(t, root, wk("u1"), `{"tools":{"web":false}}`)

	got, err := m.ApplyScopeConfigKey(subsScope("alpha"),
		applyChange("tools.web", `true`, currentRevisions(t, m, "u1")))
	if err != nil {
		t.Fatalf("ApplyScopeConfigKey: %v", err)
	}
	o := outcomeFor(t, got, "u1")
	// No model resolves in this fixture, so resolveAndMaterialize refuses.
	if o.Reapplied == nil || o.Reapplied.OK {
		t.Fatalf("Reapplied = %s, want a reported failure", mustJSON(t, o.Reapplied))
	}
	if o.Reapplied.Detail == "" {
		t.Error("a failing reapply carries no Detail")
	}
	if o.Outcome != OutcomeApplied {
		t.Errorf("Outcome = %q, want applied — the write stands", o.Outcome)
	}
	if !strings.Contains(readFile(t, filepath.Join(userDir, "config.json")), `"web": true`) {
		t.Error("the write was rolled back by the failing reapply")
	}
	if len(migrationRecords(t, userDir)) != 1 {
		t.Error("no record for an applied instance")
	}
}

// TestApplyScopeConfigKeyRecordsWhatItChanged covers the three prior states a
// revert has to tell apart: no key at all (deleting is the revert), a value, and
// an explicit null. "from": null and a missing "from" are DIFFERENT, which is why
// FromAbsent exists next to From.
func TestApplyScopeConfigKeyRecordsWhatItChanged(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	created := seedConfigWorkspace(t, root, wk("u1"), `{"tools":{}}`)
	changed := seedConfigWorkspace(t, root, wk("u2"), `{"tools":{"web":false}}`)
	nulled := seedConfigWorkspace(t, root, wk("u3"), `{"tools":{"web":null}}`)
	same := seedConfigWorkspace(t, root, wk("u4"), `{"tools":{"web":true}}`)
	revs := currentRevisions(t, m, "u1", "u2", "u3", "u4")
	skipped := seedConfigWorkspace(t, root, wk("u5"), `{"tools":{"web":false}}`)

	got, err := m.ApplyScopeConfigKey(subsScope("alpha"), applyChange("tools.web", `true`, revs))
	if err != nil {
		t.Fatalf("ApplyScopeConfigKey: %v", err)
	}

	for _, tc := range []struct {
		user       string
		dir        string
		wantAbsent bool
		wantFrom   string
	}{
		{"u1", created, true, ""},
		{"u2", changed, false, "false"},
		{"u3", nulled, false, "null"},
	} {
		recs := migrationRecords(t, tc.dir)
		if len(recs) != 1 {
			t.Fatalf("%s wrote %d records, want 1", tc.user, len(recs))
		}
		rec := recs[0]
		if rec.FromAbsent != tc.wantAbsent {
			t.Errorf("%s FromAbsent = %v, want %v", tc.user, rec.FromAbsent, tc.wantAbsent)
		}
		if string(rec.From) != tc.wantFrom {
			t.Errorf("%s From = %q, want %q", tc.user, rec.From, tc.wantFrom)
		}
		if string(rec.To) != "true" {
			t.Errorf("%s To = %q, want true", tc.user, rec.To)
		}
		if rec.Key != "tools.web" || rec.By != "admin@example.org" || !rec.AppliedAt.Equal(applyAt) {
			t.Errorf("%s record header = %s", tc.user, mustJSON(t, rec))
		}
		if rec.Scope != (ConfigMigrationScope{TenantID: "t1", SubsAccID: "s1", Agent: "alpha"}) {
			t.Errorf("%s Scope = %+v", tc.user, rec.Scope)
		}
		if rec.RevisionBefore != revs[tc.user] {
			t.Errorf("%s RevisionBefore = %q, want the inspected %q",
				tc.user, rec.RevisionBefore, revs[tc.user])
		}
		after := revisionOf([]byte(readFile(t, filepath.Join(tc.dir, "config.json"))))
		if rec.RevisionAfter != after {
			t.Errorf("%s RevisionAfter = %q, want the on-disk %q", tc.user, rec.RevisionAfter, after)
		}
		if o := outcomeFor(t, got, tc.user); o.Migration == "" || o.RecordErr != "" {
			t.Errorf("%s outcome = %s, want the record filename and no error", tc.user, mustJSON(t, o))
		}
	}

	// Nothing changed, nothing to revert: a record here would describe a write
	// that never happened.
	if recs := migrationRecords(t, same); len(recs) != 0 {
		t.Errorf("unchanged u4 wrote %d records", len(recs))
	}
	if recs := migrationRecords(t, skipped); len(recs) != 0 {
		t.Errorf("stale u5 wrote %d records", len(recs))
	}
}

// TestApplyScopeConfigKeyKeepsTheChangeWhenTheRecordFails: the record is a
// recovery aid, not the change. Losing it is the lesser harm, so the outcome
// stays applied and the failure is surfaced in RecordErr.
func TestApplyScopeConfigKeyKeepsTheChangeWhenTheRecordFails(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	userDir := seedConfigWorkspace(t, root, wk("u1"), `{"tools":{"web":false}}`)
	// A plain file where the record directory has to go: MkdirAll cannot succeed.
	if err := os.WriteFile(filepath.Join(userDir, ".config-migrations"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := m.ApplyScopeConfigKey(subsScope("alpha"),
		applyChange("tools.web", `true`, currentRevisions(t, m, "u1")))
	if err != nil {
		t.Fatalf("ApplyScopeConfigKey: %v", err)
	}
	o := outcomeFor(t, got, "u1")
	if o.Outcome != OutcomeApplied {
		t.Errorf("Outcome = %q, want applied", o.Outcome)
	}
	if o.RecordErr == "" {
		t.Error("RecordErr is empty; the missing recovery data must be surfaced")
	}
	if o.Migration != "" {
		t.Errorf("Migration = %q, want empty — no record was written", o.Migration)
	}
	if !strings.Contains(readFile(t, filepath.Join(userDir, "config.json")), `"web": true`) {
		t.Error("the change was lost with the record")
	}
}

// TestApplyScopeConfigKeySummarizesEveryOutcome: the Summary is what the admin
// screen reports, so it must be the tally of the outcomes rather than a second
// count kept alongside them. The mixed batch is also the proof that the batch
// never fails wholesale.
func TestApplyScopeConfigKeySummarizesEveryOutcome(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	applied := seedConfigWorkspace(t, root, wk("u1"), `{"tools":{"web":false}}`)
	seedConfigWorkspace(t, root, wk("u2"), `{"tools":{"web":true}}`)
	seedConfigWorkspace(t, root, wk("u4"), `{"tools":"off"}`)
	seedConfigWorkspace(t, root, wk("u6"), `{"tools":`)
	revs := currentRevisions(t, m, "u1", "u2", "u4", "u6")
	// u3 is provisioned after the inspect, so it holds no revision: stale.
	seedConfigWorkspace(t, root, wk("u3"), `{"tools":{"web":false}}`)
	seedUnprovisionedWorkspace(t, root, wk("u5"))

	// The email comes from a different source than the workspace enumeration, the
	// same way the inspect view gets it.
	if err := os.WriteFile(filepath.Join(applied, ".crab-owner.json"),
		[]byte(`{"email":"member@example.org"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := m.ApplyScopeConfigKey(subsScope("alpha"), applyChange("tools.web", `true`, revs))
	if err != nil {
		t.Fatalf("ApplyScopeConfigKey: %v", err)
	}
	if got.Key != "tools.web" {
		t.Errorf("Key = %q", got.Key)
	}

	want := map[string]string{
		"u1": OutcomeApplied, "u2": OutcomeUnchanged, "u3": OutcomeStale,
		"u4": OutcomePathConflict, "u5": OutcomeUnreadable, "u6": OutcomeUnreadable,
	}
	if len(got.Outcomes) != len(want) {
		t.Fatalf("got %d outcomes, want %d: %s", len(got.Outcomes), len(want), mustJSON(t, got.Outcomes))
	}
	order := make([]string, 0, len(got.Outcomes))
	for _, o := range got.Outcomes {
		order = append(order, o.UserAccID)
		if o.Outcome != want[o.UserAccID] {
			t.Errorf("%s Outcome = %q, want %q", o.UserAccID, o.Outcome, want[o.UserAccID])
		}
	}
	if !equalStrings(order, []string{"u1", "u2", "u3", "u4", "u5", "u6"}) {
		t.Errorf("outcome order = %v, want sorted by UserAccID", order)
	}
	if d := outcomeFor(t, got, "u5").Detail; d != "not_provisioned" {
		t.Errorf("u5 Detail = %q, want not_provisioned", d)
	}
	if d := outcomeFor(t, got, "u6").Detail; d == "" || d == "not_provisioned" {
		t.Errorf("u6 Detail = %q, want the parse error", d)
	}
	if e := outcomeFor(t, got, "u1").Email; e != "member@example.org" {
		t.Errorf("u1 Email = %q, want member@example.org", e)
	}

	tally := map[string]int{}
	for _, o := range got.Outcomes {
		tally[o.Outcome]++
	}
	if mustJSON(t, tally) != mustJSON(t, got.Summary) {
		t.Errorf("Summary = %v, want the outcome tally %v", got.Summary, tally)
	}
	total := 0
	for _, n := range got.Summary {
		total += n
	}
	if total != len(got.Outcomes) {
		t.Errorf("Summary totals %d, want %d", total, len(got.Outcomes))
	}
}

// TestApplyScopeConfigKeyWritesOnlyThroughWriteInstanceConfig is the NFR-3 gate.
// The bulk path must not open config.json itself, or the mask restore, the
// revision check, the atomic rename and the re-materialization would all have a
// second, weaker implementation.
//
// It is asserted on observable behaviour only WriteInstanceConfig produces: the
// seeded 0o644 becomes 0o600 (writeConfigAtomic chmods its temp file before the
// rename), and the reapply ran.
func TestApplyScopeConfigKeyWritesOnlyThroughWriteInstanceConfig(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	userDir := seedConfigWorkspace(t, root, wk("u1"), `{"tools":{"web":false}}`)
	path := filepath.Join(userDir, "config.json")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := m.ApplyScopeConfigKey(subsScope("alpha"),
		applyChange("tools.web", `true`, currentRevisions(t, m, "u1")))
	if err != nil {
		t.Fatalf("ApplyScopeConfigKey: %v", err)
	}
	o := outcomeFor(t, got, "u1")
	if o.Outcome != OutcomeApplied {
		t.Fatalf("Outcome = %q, want applied: %s", o.Outcome, mustJSON(t, o))
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 — the bytes did not go through writeConfigAtomic",
			fi.Mode().Perm())
	}
	if o.Reapplied == nil {
		t.Error("no ReapplyResult — the write did not go through WriteInstanceConfig")
	}
	// The proxy's own indent, so the file stays in the shape materializeModels
	// produces elsewhere.
	if !strings.Contains(readFile(t, path), "\n  \"tools\": {") {
		t.Errorf("config.json is not two-space indented:\n%s", readFile(t, path))
	}
}

// TestApplyScopeConfigKeyReportsAWriteFailurePerInstance: a write that cannot
// land is that instance's failure, not the batch's. Not in the required set — the
// unwritable directory is the only cheap way to force it, and root ignores the
// mode bits.
func TestApplyScopeConfigKeyReportsAWriteFailurePerInstance(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not restrain root")
	}
	m, _, root := testManagerWithRegistry(t)
	body := `{"tools":{"web":false}}`
	locked := seedConfigWorkspace(t, root, wk("u1"), body)
	ok := seedConfigWorkspace(t, root, wk("u2"), body)
	revs := currentRevisions(t, m, "u1", "u2")

	// No write bit: writeConfigAtomic's temp file cannot be created. Restored so
	// the TempDir cleanup (registered earlier, so it runs later) can remove it.
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	got, err := m.ApplyScopeConfigKey(subsScope("alpha"), applyChange("tools.web", `true`, revs))
	if err != nil {
		t.Fatalf("ApplyScopeConfigKey: %v", err)
	}
	bad := outcomeFor(t, got, "u1")
	if bad.Outcome != OutcomeError {
		t.Errorf("u1 Outcome = %q, want error: %s", bad.Outcome, mustJSON(t, bad))
	}
	if bad.Detail == "" {
		t.Error("u1 carries no Detail for the failed write")
	}
	if got := readFile(t, filepath.Join(locked, "config.json")); got != body {
		t.Errorf("u1 config.json = %q, want byte-identical", got)
	}
	if recs := migrationRecords(t, locked); len(recs) != 0 {
		t.Errorf("u1 wrote %d records for a write that never landed", len(recs))
	}
	if o := outcomeFor(t, got, "u2"); o.Outcome != OutcomeApplied {
		t.Errorf("u2 Outcome = %q, want applied — one failure must not block the rest", o.Outcome)
	}
	if !strings.Contains(readFile(t, filepath.Join(ok, "config.json")), `"web": true`) {
		t.Error("u2 was not written")
	}
}

// TestApplyScopeConfigKeyRefusesToTakeProvenanceFromTheBody: By and AppliedAt are
// what a migration record attributes the change to, so a caller must not be able
// to set them. Both fields are exported, so json:"-" is the only thing standing
// between a request body and a forged record — Go's field matching is
// case-insensitive, and "by" would otherwise populate By on its own.
func TestApplyScopeConfigKeyRefusesToTakeProvenanceFromTheBody(t *testing.T) {
	var ch ScopeConfigChange
	body := `{"key":"tools.web","value":true,"revisions":{"u1":"sha256:x"},` +
		`"alsoTemplate":true,"templateRevision":"sha256:t",` +
		`"by":"forged@example.org","appliedAt":"2001-01-01T00:00:00Z","at":"2001-01-01T00:00:00Z"}`
	if err := json.Unmarshal([]byte(body), &ch); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ch.By != "" {
		t.Errorf("By = %q, want empty — the body forged the record's provenance", ch.By)
	}
	if !ch.AppliedAt.IsZero() {
		t.Errorf("AppliedAt = %v, want the zero time — the body chose when this happened", ch.AppliedAt)
	}
	// The rest of the request does decode, including the two fields the HTTP layer
	// carries through to the separate template write.
	if ch.Key != "tools.web" || string(ch.Value) != "true" || ch.Revisions["u1"] != "sha256:x" {
		t.Errorf("request did not decode: %+v", ch)
	}
	if !ch.AlsoTemplate || ch.TemplateRevision != "sha256:t" {
		t.Errorf("template opt-in did not decode: %+v", ch)
	}
}

// TestApplyScopeConfigKeyAcceptsAJSONNull: null is a VALUE, which the inspect side
// already buckets as BucketValue. Only zero bytes are a malformed request, so
// "set this key to null" has to land — and the record must say "to": null rather
// than omit it.
func TestApplyScopeConfigKeyAcceptsAJSONNull(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	userDir := seedConfigWorkspace(t, root, wk("u1"), `{"tools":{"web":false}}`)

	got, err := m.ApplyScopeConfigKey(subsScope("alpha"),
		applyChange("tools.web", `null`, currentRevisions(t, m, "u1")))
	if err != nil {
		t.Fatalf("ApplyScopeConfigKey: %v", err)
	}
	if o := outcomeFor(t, got, "u1"); o.Outcome != OutcomeApplied {
		t.Fatalf("Outcome = %q, want applied: %s", o.Outcome, mustJSON(t, o))
	}
	onDisk := readFile(t, filepath.Join(userDir, "config.json"))
	doc, err := parseConfigObject([]byte(onDisk))
	if err != nil {
		t.Fatalf("config.json does not parse: %v\n%s", err, onDisk)
	}
	// pathFound with a nil value, NOT pathAbsent: setting a key to null must not
	// delete it.
	if v, state := lookupPath(doc, "tools.web"); state != pathFound || v != nil {
		t.Errorf("tools.web = %#v/%v, want a found null\n%s", v, state, onDisk)
	}
	recs := migrationRecords(t, userDir)
	if len(recs) != 1 {
		t.Fatalf("wrote %d records, want 1", len(recs))
	}
	if string(recs[0].To) != "null" {
		t.Errorf("To = %q, want null", recs[0].To)
	}
}

// TestApplyScopeConfigKeyNeverLogsTheValue is FR-5.1. The value an admin pushes
// can be a credential — a webhook URL with a token in it, an API key an agent
// needs — and the proxy log is not access-controlled the way the config is. The
// skipped instances are logged by identity and outcome only.
func TestApplyScopeConfigKeyNeverLogsTheValue(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	var logged strings.Builder
	m.logf = func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) }

	seedConfigWorkspace(t, root, wk("u1"), `{"tools":{"web":false}}`) // applied
	seedConfigWorkspace(t, root, wk("u2"), `{"tools":"off"}`)         // path_conflict
	seedConfigWorkspace(t, root, wk("u3"), `{"tools":`)               // unreadable
	revs := currentRevisions(t, m, "u1", "u2")
	seedConfigWorkspace(t, root, wk("u4"), `{"tools":{"web":false}}`) // stale

	const secret = "https://hooks.example.org/sk-live-do-not-log"
	got, err := m.ApplyScopeConfigKey(subsScope("alpha"),
		applyChange("tools.web", `"`+secret+`"`, revs))
	if err != nil {
		t.Fatalf("ApplyScopeConfigKey: %v", err)
	}
	if got.Summary[OutcomeApplied] != 1 {
		t.Fatalf("Summary = %v, want one applied — nothing was exercised otherwise", got.Summary)
	}
	if strings.Contains(logged.String(), secret) {
		t.Errorf("the pushed value reached the log:\n%s", logged.String())
	}
	// The skipped instances ARE logged — one line each, or the admin has no trail
	// for the members a batch passed over. Identity is the container name, the way
	// the rest of the package names a workspace in a log; the userAccId lives in the
	// returned outcome, which is not the log.
	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		if strings.HasPrefix(line, "bulk config ") {
			lines++
		}
	}
	if lines != 3 {
		t.Errorf("logged %d skipped instances, want 3 (path_conflict, unreadable, stale):\n%s",
			lines, logged.String())
	}
	for _, want := range []string{OutcomePathConflict, OutcomeUnreadable, OutcomeStale} {
		if !strings.Contains(logged.String(), want+":") {
			t.Errorf("outcome %q is not in the log:\n%s", want, logged.String())
		}
	}
}

// TestApplyScopeConfigKeyStopsAtWriteInstanceConfigsSizeCap is the second half of
// the NFR-3 gate, and the sharper half: maxInstanceConfigBytes is enforced ONLY by
// WriteInstanceConfig, so a bulk path with a writer of its own would happily
// rewrite this document and report applied.
func TestApplyScopeConfigKeyStopsAtWriteInstanceConfigsSizeCap(t *testing.T) {
	m, _, root := testManagerWithRegistry(t)
	oversized := `{"pad":"` + strings.Repeat("x", maxInstanceConfigBytes) +
		`","tools":{"web":false}}`
	userDir := seedConfigWorkspace(t, root, wk("u1"), oversized)

	got, err := m.ApplyScopeConfigKey(subsScope("alpha"),
		applyChange("tools.web", `true`, currentRevisions(t, m, "u1")))
	if err != nil {
		t.Fatalf("ApplyScopeConfigKey: %v", err)
	}
	o := outcomeFor(t, got, "u1")
	if o.Outcome != OutcomeError {
		t.Fatalf("Outcome = %q, want error — the bulk path wrote past the cap", o.Outcome)
	}
	// The exact sentinel pins WHICH refusal ran, so the test cannot pass on an
	// unrelated failure.
	if o.Detail != ErrConfigTooLarge.Error() {
		t.Errorf("Detail = %q, want %q", o.Detail, ErrConfigTooLarge.Error())
	}
	if readFile(t, filepath.Join(userDir, "config.json")) != oversized {
		t.Error("config.json changed despite the refusal")
	}
	if recs := migrationRecords(t, userDir); len(recs) != 0 {
		t.Errorf("a refused write left %d records", len(recs))
	}
}
