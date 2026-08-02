package docker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// These operate on a directory the AGENT also writes to, and every one of them takes
// a member-supplied path. The containment cases below are the reason this file is
// longer than the code it tests.

func exists(t *testing.T, parts ...string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(parts...))
	return err == nil
}

// --- CreateFolder ---

func TestCreateFolderMakesNestedPaths(t *testing.T) {
	t.Parallel()
	m, key, uploads := mediaFixture(t)
	if err := m.CreateFolder(key, "archive/2026/q3"); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if !exists(t, uploads, "archive", "2026", "q3") {
		t.Error("nested folder was not created")
	}
	fi, err := os.Stat(filepath.Join(uploads, "archive"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("folder mode = %o, want 0700", perm)
	}
}

// Two tabs, or a double click, must not produce an error about something that is
// already true.
func TestCreateFolderIsIdempotent(t *testing.T) {
	t.Parallel()
	m, key, _ := mediaFixture(t)
	if err := m.CreateFolder(key, "notes"); err != nil {
		t.Fatalf("first CreateFolder: %v", err)
	}
	if err := m.CreateFolder(key, "notes"); err != nil {
		t.Errorf("second CreateFolder: %v — an existing folder is success, not a conflict", err)
	}
}

func TestCreateFolderRefusesAPathThatIsAFile(t *testing.T) {
	t.Parallel()
	m, key, _ := mediaFixture(t)
	if err := m.CreateFolder(key, "top.txt"); !errors.Is(err, ErrMediaExists) {
		t.Errorf("err = %v, want ErrMediaExists", err)
	}
}

// The whole point of safeStoredPath sitting in front of these.
func TestFolderOperationsRefuseTraversal(t *testing.T) {
	t.Parallel()
	bad := []string{
		"../escape", "../../etc", "/etc/passwd", `\windows`, "", "   ",
		"a/../../b", "./..", "..", ".",
	}
	for _, rel := range bad {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			m, key, _ := mediaFixture(t)
			if err := m.CreateFolder(key, rel); err == nil {
				t.Errorf("CreateFolder(%q) was accepted", rel)
			}
			if err := m.MoveMedia(key, "top.txt", rel); err == nil {
				t.Errorf("MoveMedia(to %q) was accepted", rel)
			}
			if _, err := m.DeleteFolder(key, rel); err == nil {
				t.Errorf("DeleteFolder(%q) was accepted", rel)
			}
		})
	}
}

// A NUL byte truncates a path in a C API. safeStoredPath rejects it; asserted here
// because it is the kind of case a rewrite drops.
func TestFolderOperationsRefuseNulByte(t *testing.T) {
	t.Parallel()
	m, key, _ := mediaFixture(t)
	if err := m.CreateFolder(key, "ok\x00/evil"); err == nil {
		t.Error("a path containing NUL was accepted")
	}
}

// The boundary path validation alone cannot cover: the agent can create symlinks
// inside its own uploads dir, and a folder created "through" one would land outside.
func TestFolderOperationsRefuseASymlinkedEscape(t *testing.T) {
	t.Parallel()
	m, key, uploads := mediaFixture(t)
	outside := t.TempDir()
	link := filepath.Join(uploads, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := m.CreateFolder(key, "escape/planted"); err == nil {
		if exists(t, outside, "planted") {
			t.Fatal("a folder was created OUTSIDE the uploads root through a symlink")
		}
		t.Error("CreateFolder through a symlink was accepted")
	}
	if err := m.MoveMedia(key, "top.txt", "escape/top.txt"); err == nil {
		t.Error("a file was moved out of the uploads root through a symlink")
	}
}

// --- MoveMedia ---

func TestMoveFileBetweenFolders(t *testing.T) {
	t.Parallel()
	m, key, uploads := mediaFixture(t)
	if err := m.MoveMedia(key, "top.txt", "reports/top.txt"); err != nil {
		t.Fatalf("MoveMedia: %v", err)
	}
	if exists(t, uploads, "top.txt") {
		t.Error("the source still exists after a move")
	}
	if !exists(t, uploads, "reports", "top.txt") {
		t.Error("the file is not at the destination")
	}
}

// A move within the same parent IS the rename. There is no separate operation, and
// this is what proves the UI needs none.
func TestMoveWithinTheSameParentRenames(t *testing.T) {
	t.Parallel()
	m, key, uploads := mediaFixture(t)
	if err := m.MoveMedia(key, "reports/q1.pdf", "reports/first-quarter.pdf"); err != nil {
		t.Fatalf("MoveMedia: %v", err)
	}
	if exists(t, uploads, "reports", "q1.pdf") {
		t.Error("the old name survived")
	}
	if !exists(t, uploads, "reports", "first-quarter.pdf") {
		t.Error("the new name is missing")
	}
}

func TestMoveFolderTakesItsContents(t *testing.T) {
	t.Parallel()
	m, key, uploads := mediaFixture(t)
	if err := m.MoveMedia(key, "reports", "images/reports"); err != nil {
		t.Fatalf("MoveMedia: %v", err)
	}
	if !exists(t, uploads, "images", "reports", "2026", "q2.pdf") {
		t.Error("a nested file did not travel with its folder")
	}
	if exists(t, uploads, "reports") {
		t.Error("the source folder still exists")
	}
}

// os.Rename would happily replace a file. A drag that lands on a same-named file
// would then destroy it with no undo, so the move is refused instead.
func TestMoveNeverOverwrites(t *testing.T) {
	t.Parallel()
	m, key, uploads := mediaFixture(t)
	if err := m.CreateFolder(key, "dest"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploads, "dest", "top.txt"), []byte("PRECIOUS"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.MoveMedia(key, "top.txt", "dest/top.txt"); !errors.Is(err, ErrMediaExists) {
		t.Fatalf("err = %v, want ErrMediaExists", err)
	}
	body, err := os.ReadFile(filepath.Join(uploads, "dest", "top.txt"))
	if err != nil || string(body) != "PRECIOUS" {
		t.Errorf("the destination file was clobbered: %q, %v", body, err)
	}
	if !exists(t, uploads, "top.txt") {
		t.Error("the source was removed despite the refusal")
	}
}

// Dropping a folder into itself or a descendant detaches the subtree from the tree.
func TestMoveFolderIntoItselfIsRefused(t *testing.T) {
	t.Parallel()
	for _, dest := range []string{"reports/inner", "reports/2026/deep"} {
		t.Run(dest, func(t *testing.T) {
			t.Parallel()
			m, key, uploads := mediaFixture(t)
			if err := m.MoveMedia(key, "reports", dest); !errors.Is(err, ErrMediaIntoSelf) {
				t.Errorf("MoveMedia(reports -> %s) err = %v, want ErrMediaIntoSelf", dest, err)
			}
			if !exists(t, uploads, "reports", "q1.pdf") {
				t.Error("the folder was damaged by the refused move")
			}
		})
	}
}

// The descendant check compares resolved paths with a separator appended, so a
// SIBLING whose name merely starts with the source's is still a legal destination.
func TestMoveToASiblingWithASharedPrefixIsAllowed(t *testing.T) {
	t.Parallel()
	m, key, uploads := mediaFixture(t)
	if err := m.CreateFolder(key, "reports-archive"); err != nil {
		t.Fatal(err)
	}
	if err := m.MoveMedia(key, "reports", "reports-archive/reports"); err != nil {
		t.Fatalf("MoveMedia: %v — 'reports-archive' is not a descendant of 'reports'", err)
	}
	if !exists(t, uploads, "reports-archive", "reports", "q1.pdf") {
		t.Error("the move did not land")
	}
}

func TestMoveOntoItselfIsANoOp(t *testing.T) {
	t.Parallel()
	m, key, uploads := mediaFixture(t)
	if err := m.MoveMedia(key, "top.txt", "top.txt"); err != nil {
		t.Errorf("MoveMedia onto itself: %v", err)
	}
	if !exists(t, uploads, "top.txt") {
		t.Error("moving a file onto itself deleted it")
	}
}

func TestMoveAMissingSourceIsNotFound(t *testing.T) {
	t.Parallel()
	m, key, _ := mediaFixture(t)
	if err := m.MoveMedia(key, "ghost.txt", "reports/ghost.txt"); !errors.Is(err, ErrMediaNotFound) {
		t.Errorf("err = %v, want ErrMediaNotFound", err)
	}
}

// --- DeleteFolder ---

func TestDeleteFolderRemovesTheSubtreeAndCountsFiles(t *testing.T) {
	t.Parallel()
	m, key, uploads := mediaFixture(t)
	n, err := m.DeleteFolder(key, "reports")
	if err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}
	// reports/q1.pdf + reports/2026/q2.pdf — folders are not counted.
	if n != 2 {
		t.Errorf("removed = %d, want 2 (the count the confirmation names)", n)
	}
	if exists(t, uploads, "reports") {
		t.Error("the folder survived")
	}
	if !exists(t, uploads, "images", "logo.png") {
		t.Error("an unrelated folder was removed")
	}
}

// The member does not own the uploads root; deleting it takes the agent's whole
// working tree.
func TestDeleteFolderRefusesTheUploadsRoot(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{".", "/", ""} {
		m, key, uploads := mediaFixture(t)
		if _, err := m.DeleteFolder(key, rel); err == nil {
			t.Errorf("DeleteFolder(%q) was accepted", rel)
		}
		if !exists(t, uploads, "top.txt") {
			t.Fatalf("DeleteFolder(%q) destroyed the uploads root", rel)
		}
	}
}

func TestDeleteFolderRefusesAFile(t *testing.T) {
	t.Parallel()
	m, key, uploads := mediaFixture(t)
	if _, err := m.DeleteFolder(key, "top.txt"); !errors.Is(err, ErrMediaNotFolder) {
		t.Errorf("err = %v, want ErrMediaNotFolder", err)
	}
	if !exists(t, uploads, "top.txt") {
		t.Error("the file was deleted by a folder operation")
	}
}

func TestDeleteFolderOnAMissingPathIsNotFound(t *testing.T) {
	t.Parallel()
	m, key, _ := mediaFixture(t)
	if _, err := m.DeleteFolder(key, "ghost"); !errors.Is(err, ErrMediaNotFound) {
		t.Errorf("err = %v, want ErrMediaNotFound", err)
	}
}

func TestDeleteEmptyFolderCountsZero(t *testing.T) {
	t.Parallel()
	m, key, _ := mediaFixture(t)
	if err := m.CreateFolder(key, "empty"); err != nil {
		t.Fatal(err)
	}
	n, err := m.DeleteFolder(key, "empty")
	if err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}
	if n != 0 {
		t.Errorf("removed = %d, want 0", n)
	}
}

// --- the system-managed folder ---
//
// `attachments` is where StoreAgentAttachment puts files the AGENT produced. Renaming
// it detaches every future delivery; creating a member folder by that name collides
// with it. Enforced HERE, not only in the interface, because the path arrives from the
// network and hiding a button is not a permission.

func TestAttachmentsCannotBeCreatedRenamedOrDeleted(t *testing.T) {
	t.Parallel()
	m, key, uploads := mediaFixture(t)
	if err := os.MkdirAll(filepath.Join(uploads, "attachments"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploads, "attachments", "from-agent.txt"),
		[]byte("delivered"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := m.CreateFolder(key, "attachments"); !errors.Is(err, ErrMediaReserved) {
		t.Errorf("CreateFolder err = %v, want ErrMediaReserved", err)
	}
	if err := m.MoveMedia(key, "attachments", "anexos"); !errors.Is(err, ErrMediaReserved) {
		t.Errorf("rename err = %v, want ErrMediaReserved", err)
	}
	if err := m.MoveMedia(key, "attachments", "reports/attachments"); !errors.Is(err, ErrMediaReserved) {
		t.Errorf("move err = %v, want ErrMediaReserved", err)
	}
	if _, err := m.DeleteFolder(key, "attachments"); !errors.Is(err, ErrMediaReserved) {
		t.Errorf("delete err = %v, want ErrMediaReserved", err)
	}

	if !exists(t, uploads, "attachments", "from-agent.txt") {
		t.Fatal("a refused operation damaged the system folder")
	}
}

// Moving INTO it is refused for the mirror reason: the agent treats everything there
// as its own output, so a member file dropped in would be read back as a delivery.
func TestNothingCanBeMovedIntoAttachments(t *testing.T) {
	t.Parallel()
	m, key, uploads := mediaFixture(t)
	if err := os.MkdirAll(filepath.Join(uploads, "attachments"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.MoveMedia(key, "top.txt", "attachments/top.txt"); !errors.Is(err, ErrMediaReserved) {
		t.Errorf("err = %v, want ErrMediaReserved", err)
	}
	if err := m.CreateFolder(key, "attachments/mine"); !errors.Is(err, ErrMediaReserved) {
		t.Errorf("nested create err = %v, want ErrMediaReserved", err)
	}
	if !exists(t, uploads, "top.txt") {
		t.Error("the source was moved despite the refusal")
	}
}

// Only the TOP LEVEL is the system folder. Forbidding the word everywhere would be a
// rule about vocabulary rather than about ownership, and would block a folder a member
// may legitimately want.
func TestAttachmentsIsOnlyReservedAtTheTopLevel(t *testing.T) {
	t.Parallel()
	m, key, uploads := mediaFixture(t)
	if err := m.CreateFolder(key, "reports/attachments"); err != nil {
		t.Errorf("CreateFolder(reports/attachments): %v — a nested folder by that name is the member's", err)
	}
	if !exists(t, uploads, "reports", "attachments") {
		t.Error("the nested folder was not created")
	}
	if _, err := m.DeleteFolder(key, "reports/attachments"); err != nil {
		t.Errorf("the member cannot delete their own nested folder: %v", err)
	}
}
