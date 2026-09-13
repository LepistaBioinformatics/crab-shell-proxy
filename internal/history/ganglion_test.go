package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// crab-ganglion-harness names its transcript after the session key and writes
// no *.meta.json, so the marker scan that finds picoclaw's hashed filenames
// never matches it. Before this, reloading a ganglion conversation returned an
// empty history.
func writeGanglion(t *testing.T, dir, key string, msgs []map[string]any) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, key+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, m := range msgs {
		if err := enc.Encode(m); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRead_FindsAGanglionTranscriptByName(t *testing.T) {
	dir := t.TempDir()
	writeGanglion(t, dir, "sk1", []map[string]any{
		{"role": "user", "content": "oi", "created_at": "2026-09-10T10:00:00Z"},
		{"role": "assistant", "content": "ola", "created_at": "2026-09-10T10:00:02Z"},
	})

	got, err := Read(dir, "sk1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 || got[1].Content != "ola" {
		t.Fatalf("got %d messages: %+v", len(got), got)
	}
}

// The recovery a member sees immediately after a crash, without waiting for
// the harness to restart and fold.
func TestRead_FoldsALivePartial(t *testing.T) {
	dir := t.TempDir()
	asked := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
	writeGanglion(t, dir, "sk1", []map[string]any{
		{"role": "user", "content": "explique", "created_at": asked.Format(time.RFC3339Nano)},
	})
	b, _ := json.Marshal(partialFile{
		AnswersAt: asked, Content: "comecei a responder e", UpdatedAt: asked.Add(3 * time.Second),
	})
	os.WriteFile(filepath.Join(dir, "sk1.partial.json"), b, 0o644)

	got, err := Read(dir, "sk1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want the question plus the recovered answer: %+v", len(got), got)
	}
	if got[1].Role != "assistant" || got[1].Content != "comecei a responder e" {
		t.Errorf("recovered message = %+v", got[1])
	}
}

// The crash window between appending the real answer and removing the sidecar.
// Showing both would repeat the answer.
func TestRead_IgnoresASupersededPartial(t *testing.T) {
	dir := t.TempDir()
	asked := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
	writeGanglion(t, dir, "sk1", []map[string]any{
		{"role": "user", "content": "explique", "created_at": asked.Format(time.RFC3339Nano)},
		{"role": "assistant", "content": "resposta completa",
			"created_at": asked.Add(5 * time.Second).Format(time.RFC3339Nano)},
	})
	b, _ := json.Marshal(partialFile{
		AnswersAt: asked, Content: "resposta pela met", UpdatedAt: asked.Add(3 * time.Second),
	})
	os.WriteFile(filepath.Join(dir, "sk1.partial.json"), b, 0o644)

	got, err := Read(dir, "sk1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("the answer was shown twice: %+v", got)
	}
	if got[1].Content != "resposta completa" {
		t.Errorf("last message = %q, want the complete answer", got[1].Content)
	}
}

// An earlier turn's answer must not hide a later turn's partial.
func TestRead_AnOlderAnswerDoesNotSupersedeANewerPartial(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Minute)
	writeGanglion(t, dir, "sk1", []map[string]any{
		{"role": "user", "content": "1", "created_at": t0.Format(time.RFC3339Nano)},
		{"role": "assistant", "content": "r1", "created_at": t0.Add(time.Second).Format(time.RFC3339Nano)},
		{"role": "user", "content": "2", "created_at": t1.Format(time.RFC3339Nano)},
	})
	b, _ := json.Marshal(partialFile{AnswersAt: t1, Content: "r2 parcial", UpdatedAt: t1.Add(2 * time.Second)})
	os.WriteFile(filepath.Join(dir, "sk1.partial.json"), b, 0o644)

	got, err := Read(dir, "sk1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 4 || got[3].Content != "r2 parcial" {
		t.Errorf("the newer partial was hidden by an older answer: %+v", got)
	}
}

// A corrupt or empty sidecar must not break a history read.
func TestRead_ToleratesAnUnusablePartial(t *testing.T) {
	dir := t.TempDir()
	writeGanglion(t, dir, "sk1", []map[string]any{
		{"role": "user", "content": "oi", "created_at": "2026-09-10T10:00:00Z"},
	})
	os.WriteFile(filepath.Join(dir, "sk1.partial.json"), []byte("{ not json"), 0o644)

	got, err := Read(dir, "sk1")
	if err != nil {
		t.Fatalf("Read must not fail on a corrupt sidecar: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %+v", got)
	}
}

// A project conversation's session key carries the project prefix
// identity.ProjectSessionID stamps — "p.<project>.<32-hex>" — and the harness
// sanitises that into its file name, so the transcript on disk is
// "p_<project>_<32-hex>.jsonl". Reading the unsanitised name found nothing and
// answered with an empty history, which the member met as the conversation
// blanking the moment its turn finished.
func TestRead_FindsAProjectTranscriptUnderTheHarnessSanitisedName(t *testing.T) {
	dir := t.TempDir()
	const key = "p.seedtrial.0123456789abcdef0123456789abcdef"
	writeGanglion(t, dir, "p_seedtrial_0123456789abcdef0123456789abcdef", []map[string]any{
		{"role": "user", "content": "oi", "created_at": "2026-09-10T10:00:00Z"},
		{"role": "assistant", "content": "ola", "created_at": "2026-09-10T10:00:02Z"},
	})

	got, err := Read(dir, key)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 messages for a project conversation, got %d", len(got))
	}
	if got[1].Content != "ola" {
		t.Fatalf("unexpected answer: %q", got[1].Content)
	}
}

// The partial sidecar is named by the same sanitiser, so an interrupted answer in
// a project has to be found the same way. Without this the fix above would serve
// the transcript and silently drop the in-flight answer it exists to rescue.
func TestRead_FindsAProjectPartialUnderTheHarnessSanitisedName(t *testing.T) {
	dir := t.TempDir()
	const key = "p.seedtrial.0123456789abcdef0123456789abcdef"
	const base = "p_seedtrial_0123456789abcdef0123456789abcdef"
	writeGanglion(t, dir, base, []map[string]any{
		{"role": "user", "content": "oi", "created_at": "2026-09-10T10:00:00Z"},
	})
	partial := map[string]any{
		"answers_at": "2026-09-10T10:00:01Z",
		"content":    "meia resposta",
		"updated_at": "2026-09-10T10:00:03Z",
	}
	raw, err := json.Marshal(partial)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, base+".partial.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Read(dir, key)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 || got[1].Content != "meia resposta" {
		t.Fatalf("the in-flight answer was not served: %+v", got)
	}
}

// A conversation MIGRATED from picoclaw and then continued under the ganglion.
//
// Its history is in two files: the durable one, frozen at the moment of the
// move, and the harness's own, holding everything since. Read used to return on
// the durable file, so the frozen half won and every turn taken after the
// migration was invisible — the member saw their conversation stop on the day
// they moved, with their own later messages missing and nothing saying so.
//
// Found on a live volume: 38 durable lines ending 2026-08-18, 3 harness lines
// beginning 2026-09-12. Disjoint, so the conversation is their concatenation.
func TestRead_AMigratedConversationKeepsBothHalves(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, durableDir), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, filepath.Join(dir, durableDir, "sk1.jsonl"), []map[string]any{
		{"role": "user", "content": "before the move", "created_at": "2026-08-18T13:04:58Z"},
		{"role": "assistant", "content": "answered then", "created_at": "2026-08-18T13:05:00Z"},
	})
	writeGanglion(t, dir, "sk1", []map[string]any{
		{"role": "user", "content": "after the move", "created_at": "2026-09-12T22:51:34Z"},
		{"role": "assistant", "content": "answered now", "created_at": "2026-09-12T22:51:36Z"},
	})

	got, err := Read(dir, "sk1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("expected both halves, got %d: %+v", len(got), got)
	}
	want := []string{"before the move", "answered then", "after the move", "answered now"}
	for i, w := range want {
		if got[i].Content != w {
			t.Fatalf("at %d: got %q, want %q (full: %+v)", i, got[i].Content, w, got)
		}
	}
}

// The durable file alone must still be served. Concatenation is the rule when
// both exist; it must not become "only the harness file counts".
func TestRead_ADurableTranscriptAloneIsStillServed(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, durableDir), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, filepath.Join(dir, durableDir, "sk1.jsonl"), []map[string]any{
		{"role": "user", "content": "only half", "created_at": "2026-08-18T13:04:58Z"},
	})

	got, err := Read(dir, "sk1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 || got[0].Content != "only half" {
		t.Fatalf("got %d messages: %+v", len(got), got)
	}
}

func writeJSONL(t *testing.T, path string, msgs []map[string]any) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, m := range msgs {
		if err := enc.Encode(m); err != nil {
			t.Fatal(err)
		}
	}
}
