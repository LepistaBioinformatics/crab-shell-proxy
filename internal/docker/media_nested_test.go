package docker

import (
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// mediaFixture builds a workspace whose uploads dir contains files at the root
// AND inside folders -- the shape that used to be invisible, because ListMedia
// did a flat ReadDir and skipped every directory.
func mediaFixture(t *testing.T) (*Manager, WorkspaceKey, string) {
	t.Helper()
	root := t.TempDir()
	key := WorkspaceKey{TenantID: "t", SubsAccID: "s", Role: "alpha", UserAccID: "u"}
	m := &Manager{cfg: &config.Config{ContainerDataRoot: root, HostDataRoot: root}}
	uploads := config.PublicDir(root, key.TenantID, key.SubsAccID, key.Role, key.UserAccID, config.MainWorkspace)

	for _, rel := range []string{
		"top.txt",
		"reports/q1.pdf",
		"reports/2026/q2.pdf",
		"images/logo.png",
	} {
		full := filepath.Join(uploads, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("body-"+rel), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return m, key, uploads
}

func listPaths(t *testing.T, m *Manager, key WorkspaceKey) []string {
	t.Helper()
	got, err := m.ListMedia(key, "")
	if err != nil {
		t.Fatalf("ListMedia: %v", err)
	}
	var paths []string
	for _, f := range got {
		paths = append(paths, f.Path)
	}
	sort.Strings(paths)
	return paths
}

// The listing carries folders as well as files, each folder marked IsDir.
//
// It used to be files only, and the interface derived the tree from the folder
// PREFIXES of file paths. That works until a folder is EMPTY: a member who created one
// saw no row at all — and therefore no drop target to put a file into, which made
// creating a folder pointless. The folders are now listed in their own right.
func TestListMediaListsFoldersAndFiles(t *testing.T) {
	m, key, _ := mediaFixture(t)
	got := listPaths(t, m, key)
	want := []string{
		"public/images",
		"public/images/logo.png",
		"public/reports",
		"public/reports/2026",
		"public/reports/2026/q2.pdf",
		"public/reports/q1.pdf",
		"public/top.txt",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("listing:\n got %v\nwant %v", got, want)
	}
}

// The case the change exists for: a folder with nothing in it.
func TestListMediaListsAnEmptyFolder(t *testing.T) {
	m, key, _ := mediaFixture(t)
	if err := m.CreateFolder(key, "", "brand-new"); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	entries, err := m.ListMedia(key, "")
	if err != nil {
		t.Fatalf("ListMedia: %v", err)
	}
	for _, e := range entries {
		if e.Path == "public/brand-new" {
			if !e.IsDir {
				t.Error("the empty folder is listed but not marked IsDir")
			}
			if e.Name != "brand-new" {
				t.Errorf("Name = %q, want the workspace-relative path", e.Name)
			}
			return
		}
	}
	t.Errorf("an empty folder is missing from the listing: %v", listPaths(t, m, key))
}

// Only folders carry the flag, or the interface would render files as branches.
func TestListMediaMarksOnlyFoldersAsDirs(t *testing.T) {
	m, key, _ := mediaFixture(t)
	entries, err := m.ListMedia(key, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		wantDir := !strings.Contains(path.Base(e.Path), ".")
		if e.IsDir != wantDir {
			t.Errorf("%s: IsDir = %v, want %v", e.Path, e.IsDir, wantDir)
		}
	}
}

// The uid prefix is something StoreMedia adds to FILE base names. Running the strip
// over a folder would rename it on screen.
func TestListMediaDoesNotStripAUidLikeFolderName(t *testing.T) {
	m, key, _ := mediaFixture(t)
	if err := m.CreateFolder(key, "", "ab12cd34-notes"); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	entries, err := m.ListMedia(key, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Path == "public/ab12cd34-notes" {
			if e.Name != "ab12cd34-notes" {
				t.Errorf("folder Name = %q, want it unstripped", e.Name)
			}
			return
		}
	}
	t.Error("the folder is missing from the listing")
}

func TestListMediaKeepsTheFolderInTheDisplayName(t *testing.T) {
	// Two files can share a base name in different folders; without the prefix
	// the sidebar would show two identical rows.
	m, key, _ := mediaFixture(t)
	got, err := m.ListMedia(key, "")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range got {
		names[f.Name] = true
	}
	if !names["reports/q1.pdf"] || !names["reports/2026/q2.pdf"] {
		t.Errorf("names lost their folder: %v", names)
	}
}

func TestOpenMediaReadsANestedFile(t *testing.T) {
	m, key, _ := mediaFixture(t)
	rc, display, err := m.OpenMedia(key, "", "reports/2026/q2.pdf")
	if err != nil {
		t.Fatalf("OpenMedia: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if string(body) != "body-reports/2026/q2.pdf" {
		t.Errorf("body: %q", body)
	}
	// A browser can't save into a folder, so the download name is the base.
	if display != "q2.pdf" {
		t.Errorf("display name: %q, want q2.pdf", display)
	}
}

func TestDeleteMediaRemovesANestedFile(t *testing.T) {
	m, key, uploads := mediaFixture(t)
	if err := m.DeleteMedia(key, "", "reports/q1.pdf"); err != nil {
		t.Fatalf("DeleteMedia: %v", err)
	}
	if _, err := os.Stat(filepath.Join(uploads, "reports", "q1.pdf")); !os.IsNotExist(err) {
		t.Error("nested file was not deleted")
	}
	// Siblings untouched.
	if _, err := os.Stat(filepath.Join(uploads, "reports", "2026", "q2.pdf")); err != nil {
		t.Errorf("sibling was collateral damage: %v", err)
	}
}

func TestDeleteMediaIsIdempotent(t *testing.T) {
	m, key, _ := mediaFixture(t)
	if err := m.DeleteMedia(key, "", "reports/nope.pdf"); err != nil {
		t.Errorf("missing file must be a no-op, got %v", err)
	}
}

// The security half. Allowing folders widens the accepted path shape, so every
// escape has to stay closed.
func TestNestedPathsCannotEscapeTheWorkspace(t *testing.T) {
	m, key, _ := mediaFixture(t)
	for _, bad := range []string{
		"../../../../etc/passwd",
		"reports/../../../../etc/passwd",
		"/etc/passwd",
		"reports/../../secrets",
		"..",
		"./..",
		"",
		"   ",
	} {
		t.Run(bad, func(t *testing.T) {
			if _, _, err := m.OpenMedia(key, "", bad); err == nil {
				t.Errorf("OpenMedia(%q) must fail", bad)
			}
			if err := m.DeleteMedia(key, "", bad); err == nil {
				// A rejected path must be an error, never a silent success that
				// could have removed something outside the workspace.
				t.Errorf("DeleteMedia(%q) must fail", bad)
			}
		})
	}
}

func TestSafeStoredPathRejectsTraversalShapes(t *testing.T) {
	for _, bad := range []string{"..", "../x", "a/../../b", "/abs", "", "a\x00b"} {
		if _, err := safeStoredPath(bad); !errors.Is(err, ErrMediaName) {
			t.Errorf("safeStoredPath(%q) = %v, want ErrMediaName", bad, err)
		}
	}
	for _, ok := range []string{"a.txt", "dir/a.txt", "a/b/c.txt", "dir/./a.txt"} {
		if _, err := safeStoredPath(ok); err != nil {
			t.Errorf("safeStoredPath(%q) unexpectedly rejected: %v", ok, err)
		}
	}
}

// A symlink is the escape that path validation alone cannot catch: the string
// is perfectly well-formed and still resolves outside the workspace.
func TestSymlinkOutOfTheWorkspaceIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privilege on windows")
	}
	m, key, uploads := mediaFixture(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("classified"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(uploads, "escape.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}

	if _, _, err := m.OpenMedia(key, "", "escape.txt"); err == nil {
		t.Error("a symlink pointing outside uploads must not be readable")
	}
	// It must not even be advertised.
	for _, p := range listPaths(t, m, key) {
		if strings.HasSuffix(p, "escape.txt") {
			t.Error("symlink must not appear in the listing")
		}
	}
}

// A file the AGENT delivered has to land somewhere the existing plumbing already
// reaches: under uploads/, nested one level. That is the whole trick behind
// "no frontend work" — ListMedia walks folders and OpenMedia reads nested paths,
// so the uploads sidebar shows it with click-to-download like any user upload.
func TestStoreAgentAttachmentLandsWhereTheSidebarLooks(t *testing.T) {
	m, key, _ := mediaFixture(t)

	stored, err := m.StoreAgentAttachment(key, "", "report.pdf", strings.NewReader("%PDF-1.4 fake"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if stored.Path != "public/attachments/report.pdf" {
		t.Errorf("path = %q, want uploads/attachments/report.pdf", stored.Path)
	}
	if stored.Size != int64(len("%PDF-1.4 fake")) {
		t.Errorf("size = %d, want the written length", stored.Size)
	}

	// The two operations the sidebar performs, on the value the store returned.
	listed, err := m.ListMedia(key, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, f := range listed {
		if f.Path == "public/attachments/report.pdf" {
			found = true
		}
	}
	if !found {
		t.Errorf("attachment not listed: %+v", listed)
	}
	rc, _, err := m.OpenMedia(key, "", stored.Name)
	if err != nil {
		t.Fatalf("open %q: %v", stored.Name, err)
	}
	defer rc.Close()
	raw, _ := io.ReadAll(rc)
	if string(raw) != "%PDF-1.4 fake" {
		t.Errorf("downloaded %q, want the stored bytes", raw)
	}
}

// A delivered name is still a caller-supplied string: it must not be able to
// escape the attachments dir, and re-delivering the same name must overwrite
// rather than pile up.
func TestStoreAgentAttachmentSanitizesAndOverwrites(t *testing.T) {
	m, key, _ := mediaFixture(t)

	stored, err := m.StoreAgentAttachment(key, "", "../../etc/passwd", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if stored.Path != "public/attachments/passwd" {
		t.Errorf("path = %q, want the traversal stripped to a leaf name", stored.Path)
	}

	if _, err := m.StoreAgentAttachment(key, "", "report.pdf", strings.NewReader("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.StoreAgentAttachment(key, "", "report.pdf", strings.NewReader("v2-longer")); err != nil {
		t.Fatal(err)
	}
	listed, _ := m.ListMedia(key, "")
	var n int
	for _, f := range listed {
		if f.Path == "public/attachments/report.pdf" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("report.pdf listed %d times, want 1 (a re-delivery overwrites)", n)
	}
	rc, _, err := m.OpenMedia(key, "", "attachments/report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	raw, _ := io.ReadAll(rc)
	if string(raw) != "v2-longer" {
		t.Errorf("content = %q, want the newest delivery", raw)
	}
}
