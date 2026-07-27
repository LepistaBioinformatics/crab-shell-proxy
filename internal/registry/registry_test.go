package registry

import (
	"path/filepath"
	"testing"
	"time"
)

// fixedNow keeps timestamps deterministic so tests can assert on them.
func testRegistry(t *testing.T) *Registry {
	t.Helper()
	at := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	r, err := Open(filepath.Join(t.TempDir(), "model-registry.db"), func() time.Time { return at })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestOpenCreatesBucketsAndZeroSchemaVersion(t *testing.T) {
	r := testRegistry(t)

	v, err := r.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != 0 {
		t.Errorf("fresh database SchemaVersion = %d, want 0", v)
	}

	if err := r.SetSchemaVersion(1); err != nil {
		t.Fatalf("SetSchemaVersion: %v", err)
	}
	if v, err = r.SchemaVersion(); err != nil || v != 1 {
		t.Errorf("SchemaVersion after set = %d (err %v), want 1", v, err)
	}
}

func TestWorkspaceRefKeyIsSanitizedAndStable(t *testing.T) {
	a := WorkspaceRef{TenantID: "T 1", SubsAccID: "s/1", Agent: "alpha", UserAccID: "u1"}
	b := WorkspaceRef{TenantID: "T 1", SubsAccID: "s/1", Agent: "alpha", UserAccID: "u1"}
	if a.Key() != b.Key() {
		t.Errorf("Key not stable: %q vs %q", a.Key(), b.Key())
	}
	if a.Key() == "" {
		t.Error("Key must not be empty")
	}
	// A separator inside a segment must not let one ref forge another's key.
	c := WorkspaceRef{TenantID: "T 1/s", SubsAccID: "1", Agent: "alpha", UserAccID: "u1"}
	if a.Key() == c.Key() {
		t.Errorf("segment separator collision: both %q", a.Key())
	}
}
