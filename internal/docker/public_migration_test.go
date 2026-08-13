package docker

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// publicMigrationFixture builds a workspace with a pre-rename `uploads/` dir holding
// a file at the root and one in a folder, and nothing else.
func publicMigrationFixture(t *testing.T) (*Manager, WorkspaceKey, string, string) {
	t.Helper()
	root := t.TempDir()
	key := WorkspaceKey{TenantID: "t", SubsAccID: "s", Role: "alpha", UserAccID: "u"}
	m := &Manager{cfg: &config.Config{ContainerDataRoot: root, HostDataRoot: root}}

	legacy := config.LegacyPublicDir(root, key.TenantID, key.SubsAccID, key.Role, key.UserAccID, config.MainWorkspace)
	for _, rel := range []string{"top.txt", "reports/q1.pdf"} {
		full := filepath.Join(legacy, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := config.PublicDir(root, key.TenantID, key.SubsAccID, key.Role, key.UserAccID, config.MainWorkspace)
	return m, key, legacy, want
}

// The migration exists because workspaces are never re-provisioned: an account
// created before the rename would otherwise keep writing into a directory the
// member's interface no longer lists, so their files would appear to vanish.
func TestPublicRootMigratesALegacyUploadsDir(t *testing.T) {
	m, key, legacy, want := publicMigrationFixture(t)

	got, err := m.publicRoot(key, "")
	if err != nil {
		t.Fatalf("publicRoot: %v", err)
	}
	if got != want {
		t.Errorf("dir = %q, want %q", got, want)
	}
	// The CONTENT moved, not just the name -- a rename that created an empty
	// `public` beside the real data is the failure this guards.
	for _, rel := range []string{"top.txt", "reports/q1.pdf"} {
		b, err := os.ReadFile(filepath.Join(want, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s did not survive the migration: %v", rel, err)
			continue
		}
		if string(b) != rel {
			t.Errorf("%s = %q, want %q", rel, b, rel)
		}
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Error("the legacy dir is still there; a later access would find both")
	}
}

// Called on every access, so it has to be a no-op the second time and every time
// after.
func TestPublicRootIsIdempotent(t *testing.T) {
	m, key, _, want := publicMigrationFixture(t)
	for i := 0; i < 3; i++ {
		got, err := m.publicRoot(key, "")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got != want {
			t.Fatalf("call %d: dir = %q, want %q", i, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(want, "top.txt")); err != nil {
		t.Errorf("content lost across repeated calls: %v", err)
	}
}

// Both present means an earlier partial state or a folder someone made by hand, so
// the two are MERGED. On a same-path collision the newer file wins: the member's most
// recent version of a document is the one they meant to keep, and mtime is the only
// evidence available -- nothing records which directory a file was "supposed" to be in.
func TestPublicRootMergesWhenBothExist(t *testing.T) {
	m, key, legacy, want := publicMigrationFixture(t)
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatal(err)
	}

	older := time.Now().Add(-2 * time.Hour)
	newer := time.Now()

	// top.txt exists on BOTH sides; public's copy is the newer one and must survive.
	write := func(dir, rel, body string, mtime time.Time) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	write(legacy, "top.txt", "legacy top", older)
	write(want, "top.txt", "public top", newer)
	// contested.txt is the other direction: the LEGACY copy is newer and must win.
	write(want, "contested.txt", "public contested", older)
	write(legacy, "contested.txt", "legacy contested", newer)
	// A legacy-only file, nested, must arrive.
	write(legacy, "reports/q1.pdf", "legacy q1", older)

	if _, err := m.publicRoot(key, ""); err != nil {
		t.Fatalf("publicRoot: %v", err)
	}

	for _, c := range []struct{ rel, want string }{
		{"top.txt", "public top"},
		{"contested.txt", "legacy contested"},
		{"reports/q1.pdf", "legacy q1"},
	} {
		b, err := os.ReadFile(filepath.Join(want, filepath.FromSlash(c.rel)))
		if err != nil {
			t.Errorf("%s missing after the merge: %v", c.rel, err)
			continue
		}
		if string(b) != c.want {
			t.Errorf("%s = %q, want %q (the newer copy)", c.rel, b, c.want)
		}
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Error("the legacy dir survived a completed merge; a later access would redo it")
	}
}

// A workspace that never had the old name must not gain one, and must not error.
func TestPublicRootIsAnoopWithNoLegacyDir(t *testing.T) {
	root := t.TempDir()
	key := WorkspaceKey{TenantID: "t", SubsAccID: "s", Role: "alpha", UserAccID: "u"}
	m := &Manager{cfg: &config.Config{ContainerDataRoot: root, HostDataRoot: root}}

	got, err := m.publicRoot(key, "")
	if err != nil {
		t.Fatalf("publicRoot on a fresh workspace: %v", err)
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Error("the dir was created eagerly; callers create it on demand")
	}
}
