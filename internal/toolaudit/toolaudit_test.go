package toolaudit

import (
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeGz(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := gzip.NewWriter(f)
	if _, err := zw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

const record = `{"id":"a1","name":"sh","arguments":"{\"command\":\"ls\"}","output":"a\nb\n","status":"ok"}`

func TestReadsAPlainRecord(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a1.json", record)

	rec, err := Read(dir, "a1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if rec.Arguments != `{"command":"ls"}` {
		t.Errorf("arguments = %q", rec.Arguments)
	}
	if rec.Output != "a\nb\n" {
		t.Errorf("output = %q", rec.Output)
	}
}

// The sweep has been past. The member must not be able to tell.
func TestReadsACompressedRecord(t *testing.T) {
	dir := t.TempDir()
	writeGz(t, dir, "a1.json.gz", record)

	rec, err := Read(dir, "a1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if rec.Output != "a\nb\n" {
		t.Errorf("output = %q, want the decompressed original", rec.Output)
	}
}

// `.json` FIRST. The sweep writes the compressed copy and only then unlinks the
// original, so for a moment both exist; whichever is read must be the record.
func TestPrefersThePlainCopyWhenBothExist(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a1.json", record)
	writeGz(t, dir, "a1.json.gz", `{"id":"a1","output":"stale"}`)

	rec, err := Read(dir, "a1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if rec.Output == "stale" {
		t.Error("read the compressed copy while the original was still there")
	}
}

// ORDINARY, NOT AN ERROR. Every call made before the harness minted these has
// no record, and so does every picoclaw call.
func TestAnAbsentRecordIsNotRecordedRatherThanAFailure(t *testing.T) {
	if _, err := Read(t.TempDir(), "nope"); !errors.Is(err, ErrNotRecorded) {
		t.Errorf("err = %v, want ErrNotRecorded", err)
	}
}

// THE ID ARRIVES FROM A URL. The harness sanitises what it mints, which makes
// those safe where they were written and says nothing about a string a caller
// typed.
func TestATraversalIdCannotEscapeTheDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "conv")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A record that exists OUTSIDE the conversation's directory, which the
	// traversal is aiming at.
	write(t, root, "secret.json", `{"id":"secret","output":"not yours"}`)

	if _, err := Read(dir, "../secret"); !errors.Is(err, ErrNotRecorded) {
		t.Errorf("err = %v, want the traversal flattened into a name that does not exist", err)
	}
	if got := Safe("../secret"); got != "___secret" {
		t.Errorf("Safe(%q) = %q", "../secret", got)
	}
}

func TestAnUnreadableRecordIsAnErrorRatherThanAnEmptyOne(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a1.json", "{not json")
	_, err := Read(dir, "a1")
	if err == nil {
		t.Fatal("want an error")
	}
	if errors.Is(err, ErrNotRecorded) {
		t.Error("a corrupt record is not the same fact as an absent one")
	}
}
