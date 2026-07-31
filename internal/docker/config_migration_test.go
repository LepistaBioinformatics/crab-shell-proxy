package docker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// No test here needs a privileged step: the records are proxy-owned by design
// (picoclaw never reads them), so the writer does no chown and the suite runs
// unprivileged without special-casing.

func sampleMigration() ConfigMigration {
	return ConfigMigration{
		Key:       "tools.web.brave.enabled",
		From:      json.RawMessage(`false`),
		To:        json.RawMessage(`true`),
		AppliedAt: time.Date(2026, 7, 31, 13, 45, 2, 0, time.UTC),
		By:        "admin@example.org",
		Scope: ConfigMigrationScope{
			TenantID:  "tenant-1",
			SubsAccID: "subs-9",
			Agent:     "picoclaw",
		},
		RevisionBefore: "rev-a",
		RevisionAfter:  "rev-b",
	}
}

func TestWriteConfigMigrationNamesAndModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".config-migrations")

	name, err := writeConfigMigration(dir, sampleMigration())
	if err != nil {
		t.Fatalf("writeConfigMigration: %v", err)
	}

	// Hardcoded, not recomputed from the same layout the implementation uses: a
	// tautological expectation could not catch a wrong layout directive.
	const want = "20260731T134502Z-tools.web.brave.enabled.json"
	if name != want {
		t.Errorf("filename = %q, want %q", name, want)
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("dir mode = %o, want 700", got)
	}

	fi, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("returned name is not on disk: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 600", got)
	}
}

// Second resolution is not unique, so two edits to the same key inside one second
// must both survive — the first record is the only evidence of the value before
// it, and overwriting it loses exactly what the record exists to preserve.
func TestWriteConfigMigrationSameSecondBothSurvive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".config-migrations")
	rec := sampleMigration()

	first, err := writeConfigMigration(dir, rec)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	second, err := writeConfigMigration(dir, rec)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}

	if first != "20260731T134502Z-tools.web.brave.enabled.json" {
		t.Errorf("first = %q", first)
	}
	if second != "20260731T134502Z-tools.web.brave.enabled-2.json" {
		t.Errorf("second = %q, want the -2 suffix before .json", second)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("dir holds %d entries, want 2", len(entries))
	}
	for _, name := range []string{first, second} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s missing: %v", name, err)
		}
	}
}

// readRecordKeys unmarshals into raw keys rather than into ConfigMigration: the
// absent-vs-null distinction lives in whether the "from" KEY exists, and
// unmarshalling into the struct erases it.
func readRecordKeys(t *testing.T, dir, name string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal record: %v", err)
	}
	return m
}

func TestWriteConfigMigrationAbsentBeforeOmitsFrom(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".config-migrations")
	rec := sampleMigration()
	rec.From = nil
	rec.FromAbsent = true

	name, err := writeConfigMigration(dir, rec)
	if err != nil {
		t.Fatalf("writeConfigMigration: %v", err)
	}

	m := readRecordKeys(t, dir, name)
	if _, ok := m["from"]; ok {
		t.Errorf(`record carries a "from" key (%s); an absent prior value must omit it entirely`, m["from"])
	}
	fa, ok := m["fromAbsent"]
	if !ok {
		t.Fatal(`record has no "fromAbsent" key`)
	}
	if string(fa) != "true" {
		t.Errorf("fromAbsent = %s, want true", fa)
	}
}

func TestWriteConfigMigrationNullBeforeKeepsFromNull(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".config-migrations")
	rec := sampleMigration()
	rec.From = json.RawMessage(`null`)
	rec.FromAbsent = false

	name, err := writeConfigMigration(dir, rec)
	if err != nil {
		t.Fatalf("writeConfigMigration: %v", err)
	}

	m := readRecordKeys(t, dir, name)
	from, ok := m["from"]
	if !ok {
		t.Fatal(`record has no "from" key; a stored JSON null is a value, not an absence`)
	}
	if string(from) != "null" {
		t.Errorf("from = %s, want null", from)
	}
	if _, ok := m["fromAbsent"]; ok {
		t.Error(`record carries "fromAbsent" for a key that existed and held null`)
	}
}

func TestWriteConfigMigrationRoundTripsProvenance(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".config-migrations")
	rec := sampleMigration()

	name, err := writeConfigMigration(dir, rec)
	if err != nil {
		t.Fatalf("writeConfigMigration: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var got ConfigMigration
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal record: %v", err)
	}

	if got.Key != rec.Key {
		t.Errorf("key = %q, want %q", got.Key, rec.Key)
	}
	if string(got.From) != string(rec.From) {
		t.Errorf("from = %s, want %s", got.From, rec.From)
	}
	if string(got.To) != string(rec.To) {
		t.Errorf("to = %s, want %s", got.To, rec.To)
	}
	if !got.AppliedAt.Equal(rec.AppliedAt) {
		t.Errorf("appliedAt = %v, want %v", got.AppliedAt, rec.AppliedAt)
	}
	if got.By != rec.By {
		t.Errorf("by = %q, want %q", got.By, rec.By)
	}
	if got.Scope != rec.Scope {
		t.Errorf("scope = %+v, want %+v", got.Scope, rec.Scope)
	}
	if got.RevisionBefore != rec.RevisionBefore {
		t.Errorf("revisionBefore = %q, want %q", got.RevisionBefore, rec.RevisionBefore)
	}
	if got.RevisionAfter != rec.RevisionAfter {
		t.Errorf("revisionAfter = %q, want %q", got.RevisionAfter, rec.RevisionAfter)
	}
}

// AppliedAt is normalized to UTC for the name, so a caller holding a local-zone
// timestamp still files the record under the instant every other record uses.
func TestWriteConfigMigrationFilenameUsesUTC(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".config-migrations")
	rec := sampleMigration()
	rec.AppliedAt = time.Date(2026, 7, 31, 13, 45, 2, 0, time.FixedZone("east", 3*60*60))

	name, err := writeConfigMigration(dir, rec)
	if err != nil {
		t.Fatalf("writeConfigMigration: %v", err)
	}
	if name != "20260731T104502Z-tools.web.brave.enabled.json" {
		t.Errorf("filename = %q, want the UTC instant", name)
	}
}
