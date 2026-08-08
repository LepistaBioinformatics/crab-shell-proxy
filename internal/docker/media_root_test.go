package docker

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The uploads tree is confined by os.Root, so containment is decided by the
// kernel during path resolution rather than by a check this code performs and
// then acts on. These tests state that as behaviour.
//
// The scenario worth having a test for is the one the previous hand-rolled
// resolver could not cover: a component of the path is a SYMLINK OUT OF THE TREE
// that was already in place. The old code called EvalSymlinks, compared with
// filepath.Rel, and then performed the operation on the joined path — correct
// for a link present at check time, but the check and the operation were two
// moments. The agent runs in the container with write access to this very tree
// and can create a link between them.
//
// We cannot reproduce the race deterministically in a unit test. What we can
// assert is the property that makes the race unwinnable: the operation itself
// refuses, so there is no window in which a stale answer could be used.

func newTree(t *testing.T) *treeRoot {
	t.Helper()
	tree, err := openTree(filepath.Join(t.TempDir(), "uploads"))
	if err != nil {
		t.Fatalf("openTree: %v", err)
	}
	t.Cleanup(func() { _ = tree.Close() })
	return tree
}

// A symlink planted inside the tree, pointing outside it, must not be usable as
// a path component by ANY operation — not merely detected before one.
func TestTreeRootRefusesAnEscapingSymlinkOnEveryOperation(t *testing.T) {
	tree := newTree(t)

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("not yours"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// The link is created with a raw os call on purpose: this is the agent, or a
	// prior state of the tree — not something this package would do.
	if err := os.Symlink(outside, filepath.Join(tree.path, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Reading through the link.
	if _, err := tree.root.Open("escape/secret.txt"); err == nil {
		t.Error("Open followed a symlink out of the tree")
	}
	if _, err := tree.stat("escape/secret.txt"); err == nil {
		t.Error("Stat followed a symlink out of the tree")
	}
	// Writing through it.
	if _, err := tree.root.OpenFile("escape/planted.txt", os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		t.Error("OpenFile created a file outside the tree")
	}
	// Creating a directory through it.
	if err := tree.root.MkdirAll("escape/deep/deeper", 0o700); err == nil {
		t.Error("MkdirAll created directories outside the tree")
	}
	// And the destructive ones — the pair that would do real damage.
	if err := tree.root.RemoveAll("escape/secret.txt"); err == nil {
		t.Error("RemoveAll deleted through a symlink out of the tree")
	}
	if _, err := os.Stat(secret); err != nil {
		t.Errorf("the file outside the tree was touched: %v", err)
	}
}

// The link itself is inside the tree, so removing THE LINK (not its target) is a
// legitimate operation a member may perform on their own uploads. Refusing it
// would make an escaping link undeletable, which is the opposite of safe.
func TestTreeRootStillRemovesTheLinkItself(t *testing.T) {
	tree := newTree(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(tree.path, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := tree.root.Remove("escape"); err != nil {
		t.Errorf("Remove on the link itself: %v", err)
	}
	if _, err := os.Lstat(outside); err != nil {
		t.Errorf("removing the link removed its target: %v", err)
	}
}

// Traversal by name, with no symlink involved.
func TestTreeRootRefusesTraversalByName(t *testing.T) {
	tree := newTree(t)
	for _, rel := range []string{
		"../outside", "../../etc/passwd", "a/../../b",
		strings.Repeat("../", 32) + "etc/passwd",
	} {
		if _, err := tree.root.Stat(rel); err == nil {
			t.Errorf("Stat(%q) resolved outside the tree", rel)
		}
		if err := tree.root.MkdirAll(rel, 0o700); err == nil {
			t.Errorf("MkdirAll(%q) resolved outside the tree", rel)
		}
	}
}

// countFiles walks the Root's own fs.FS, so a link inside the counted subtree
// cannot make the walk wander onto the host — which would both inflate the count
// the member is shown and read directory names they do not own.
func TestCountFilesDoesNotWalkOutOfTheTree(t *testing.T) {
	tree := newTree(t)
	if err := tree.root.MkdirAll("reports", 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := tree.root.WriteFile("reports/a.txt", []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	outside := t.TempDir()
	for _, n := range []string{"1.txt", "2.txt", "3.txt"} {
		if err := os.WriteFile(filepath.Join(outside, n), []byte("y"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(tree.path, "reports", "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// TWO, not one and not four: the real file, plus the link ITSELF as an entry
	// of the folder — it is a thing in there, and RemoveAll will remove it, so the
	// number the member is shown should include it. What must not appear is the
	// three files it points at: counting those would both inflate the warning and
	// mean the walk had read a directory outside the member's tree.
	if got := tree.countFiles("reports"); got != 2 {
		t.Errorf("countFiles = %d, want 2 (a.txt + the link entry); "+
			"4 or more means the walk followed the link out of the tree", got)
	}
}

// escaped() decides whether a failure is the member's fault. Its first version
// asked "is this an error that is not NotExist or Exist", which is also true of
// a full disk, a permission fault and an I/O error — so an operator problem was
// reported to the member as an invalid filename, on exactly the write and delete
// paths this change was meant to harden.
func TestEscapedRecognisesOnlyTheRefusal(t *testing.T) {
	tree := newTree(t)

	// The real thing, straight from the kernel.
	_, err := tree.root.Stat("../outside")
	if err == nil {
		t.Fatal("Stat on an escaping path succeeded")
	}
	if !escaped(err) {
		t.Errorf("escaped() missed the refusal: %v — os may have reworded %q",
			err, pathEscapesMsg)
	}

	// Everything else must NOT be classified as the member's naming mistake.
	for _, other := range []error{
		&fs.PathError{Op: "openat", Path: "x", Err: syscall.ENOSPC},
		&fs.PathError{Op: "openat", Path: "x", Err: syscall.EACCES},
		&fs.PathError{Op: "unlinkat", Path: "x", Err: syscall.EIO},
		&fs.PathError{Op: "openat", Path: "x", Err: syscall.EROFS},
		errors.New("some wrapped failure"),
	} {
		if escaped(other) {
			t.Errorf("escaped() claimed %v is a path escape", other)
		}
	}
}

// A read must not conjure the uploads directory. The proxy runs as root and only
// the write paths chown, so a tree created by a download is one the non-root
// agent cannot traverse until a later upload happens to fix it.
func TestReadsDoNotCreateTheUploadsTree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "uploads")
	if _, err := openTreeIfExists(dir); !errors.Is(err, ErrMediaNotFound) {
		t.Errorf("openTreeIfExists on an absent tree = %v, want ErrMediaNotFound", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("openTreeIfExists created the uploads directory")
	}
}
