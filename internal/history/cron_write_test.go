package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The contract closure: what WriteCronMeta writes is what CronRuns reads.
//
// These two are written apart -- the reader was built against picoclaw's files,
// the writer exists because the ganglion produces none -- and every field they
// share is a chance to disagree by one character. A mismatch does not fail: the
// run simply never appears, and a member is left believing their task never ran.
func TestWriteCronMetaIsFoundByCronRuns(t *testing.T) {
	dir := t.TempDir()
	sessionID := CronSessionID("9abd3e01bd0a082a", "7f3c1d")

	// The harness writes the transcript under its own sanitised name.
	writeGanglion(t, dir, CronBasename(sessionID), []map[string]any{
		{"role": "user", "content": "resumo diario", "created_at": "2026-09-10T09:00:00Z"},
		{"role": "assistant", "content": "pronto", "created_at": "2026-09-10T09:00:40Z"},
	})

	started := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	if err := WriteCronMeta(dir, sessionID, "", started, started.Add(40*time.Second)); err != nil {
		t.Fatalf("WriteCronMeta: %v", err)
	}

	runs, err := CronRuns(dir)
	if err != nil {
		t.Fatalf("CronRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	got := runs[0]
	if got.JobID != "9abd3e01bd0a082a" {
		t.Errorf("jobId = %q", got.JobID)
	}
	if got.RunID != "7f3c1d" {
		t.Errorf("runId = %q", got.RunID)
	}
	if got.TranscriptMissing {
		t.Error("the transcript beside the meta was not found")
	}
	if got.Prompt != "resumo diario" {
		t.Errorf("prompt = %q", got.Prompt)
	}
	if got.Count != 2 {
		t.Errorf("count = %d, want the 2 entries actually in the transcript", got.Count)
	}
	// No originating conversation: a fired job answers nobody, and inventing a
	// link would put unattended work on some chat's timeline.
	if got.SessionKey != "" {
		t.Errorf("sessionKey = %q, want empty", got.SessionKey)
	}
}

// A run whose turn died before the harness wrote anything still has to be
// listed. "It failed" and "it never fired" are different things to a member, and
// only the meta can tell them apart.
func TestWriteCronMetaListsARunWithNoTranscript(t *testing.T) {
	dir := t.TempDir()
	sessionID := CronSessionID("abcd", "ef01")
	now := time.Now()
	if err := WriteCronMeta(dir, sessionID, "", now, now); err != nil {
		t.Fatalf("WriteCronMeta: %v", err)
	}
	runs, err := CronRuns(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || !runs[0].TranscriptMissing || runs[0].Count != 0 {
		t.Fatalf("want one run with no transcript, got %+v", runs)
	}
}

// A cron meta must not be mistaken for a conversation. The reader excludes them
// by key prefix; this asserts the writer produces a key that prefix matches.
func TestWriteCronMetaDoesNotLeakIntoConversationHistory(t *testing.T) {
	dir := t.TempDir()
	const chat = "0123456789abcdef0123456789abcdef"
	writeGanglion(t, dir, chat, []map[string]any{
		{"role": "user", "content": "oi", "created_at": "2026-09-10T10:00:00Z"},
	})
	sessionID := CronSessionID("abcd", "ef01")
	writeGanglion(t, dir, CronBasename(sessionID), []map[string]any{
		{"role": "user", "content": "resumo", "created_at": "2026-09-10T09:00:00Z"},
	})
	now := time.Now()
	// Stamped with the chat marker, which is the shape that once made a
	// conversation resolve to a cron transcript.
	if err := WriteCronMeta(dir, sessionID, chat, now, now); err != nil {
		t.Fatal(err)
	}

	msgs, err := Read(dir, chat)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Content != "oi" {
		t.Fatalf("the conversation read back the scheduled run: %+v", msgs)
	}
}

// The meta lands beside the transcript, under the harness's own name for it.
func TestCronBasenameMatchesTheHarnessSanitiser(t *testing.T) {
	sessionID := CronSessionID("abcd", "ef01")
	if got, want := CronBasename(sessionID), "agent_cron-abcd-ef01"; got != want {
		t.Fatalf("CronBasename = %q, want %q", got, want)
	}
	dir := t.TempDir()
	now := time.Now()
	if err := WriteCronMeta(dir, sessionID, "", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent_cron-abcd-ef01.meta.json")); err != nil {
		t.Fatalf("meta not written where the reader looks: %v", err)
	}
}
