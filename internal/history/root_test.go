package history

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The sessions dir is inside the agent's own workspace — picoclaw writes its
// transcripts there, and the workspace is bind-mounted read-write into its
// container and chowned to its uid. So every component of a transcript path is
// the agent's to reshape, while THIS package reads and appends to those paths as
// root.

func sessionsFixture(t *testing.T) (dir string, outside string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return dir, t.TempDir()
}

// SyncDurable opens the durable transcript O_CREATE|O_APPEND, mode 0644, as
// root. With `durable` swapped for a symlink that was an append primitive at a
// path the agent picked — message text into any file the proxy could write.
func TestSyncDurableCannotAppendOutsideTheSessionsDir(t *testing.T) {
	dir, outside := sessionsFixture(t)
	victim := filepath.Join(outside, "authorized_keys")
	if err := os.WriteFile(victim, []byte("original\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, durableDir)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	r, err := openSessions(dir)
	if err != nil {
		t.Fatalf("openSessions: %v", err)
	}
	defer r.Close()

	_, err = r.OpenFile(durableDir+"/authorized_keys", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		t.Error("the durable append opened a file outside the sessions dir")
	}
	if !escapes(err) {
		t.Errorf("expected the escape refusal, got %v", err)
	}

	got, _ := os.ReadFile(victim)
	if string(got) != "original\n" {
		t.Errorf("the file outside was appended to: %q", got)
	}
}

// The read side: serving whatever the link points at back as the member's own
// conversation is an arbitrary-file disclosure rendered in the chat transcript.
func TestReadCannotServeATranscriptFromOutside(t *testing.T) {
	dir, outside := sessionsFixture(t)
	if err := os.WriteFile(filepath.Join(outside, "k.jsonl"),
		[]byte(`{"role":"user","content":"someone else's"}`+"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, durableDir)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	msgs, err := Read(dir, "k")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, "someone else's") {
			t.Fatalf("Read served a transcript from outside the sessions dir: %q", m.Content)
		}
	}
	if len(msgs) != 0 {
		t.Errorf("expected no messages, got %d", len(msgs))
	}
}

// The listing must not be redirected either: it names the files every other
// operation then opens.
func TestFindSessionFileDoesNotFollowASymlinkedSessionsDir(t *testing.T) {
	dir, outside := sessionsFixture(t)
	if err := os.WriteFile(filepath.Join(outside, "x.meta.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "nested")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// The nested link is an entry, but nothing under it may be reached by name.
	r, err := openSessions(dir)
	if err != nil {
		t.Fatalf("openSessions: %v", err)
	}
	defer r.Close()
	if _, err := r.ReadFile("nested/x.meta.json"); err == nil {
		t.Error("read a meta file through a symlink out of the sessions dir")
	}
}

// A missing sessions dir is the normal state for an agent that has never run,
// and must stay "no conversations" rather than becoming an error.
func TestAbsentSessionsDirIsEmptyNotAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := openSessions(missing); !errors.Is(err, errNoSessionsDir) {
		t.Errorf("openSessions = %v, want errNoSessionsDir", err)
	}
	msgs, err := Read(missing, "k")
	if err != nil || len(msgs) != 0 {
		t.Errorf("Read on an absent dir = (%v, %v), want (empty, nil)", msgs, err)
	}
	runs, err := CronRuns(missing)
	if err != nil || len(runs) != 0 {
		t.Errorf("CronRuns on an absent dir = (%v, %v), want (empty, nil)", runs, err)
	}
}

// escapes() must classify only the refusal, not every failure — otherwise a full
// disk or a permission fault would be reported as "the path left the tree".
func TestEscapesRecognisesOnlyTheRefusal(t *testing.T) {
	dir, _ := sessionsFixture(t)
	r, err := openSessions(dir)
	if err != nil {
		t.Fatalf("openSessions: %v", err)
	}
	defer r.Close()

	if _, err := r.Stat("../outside"); err == nil || !escapes(err) {
		t.Errorf("escapes() missed the refusal: %v — os may have reworded %q", err, pathEscapesMsg)
	}
	for _, other := range []error{
		&fs.PathError{Op: "openat", Path: "x", Err: syscall.ENOSPC},
		&fs.PathError{Op: "openat", Path: "x", Err: syscall.EACCES},
		errors.New("plain failure"),
	} {
		if escapes(other) {
			t.Errorf("escapes() claimed %v is a path escape", other)
		}
	}
}
