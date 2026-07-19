package history

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestReadFindsAndFiltersTranscript(t *testing.T) {
	dir := t.TempDir()
	key := "abc123def456"

	// Matching meta + transcript.
	writeFile(t, dir, "sk_v1_match.meta.json",
		`{"scope":{"values":{"chat":"direct:pico:`+key+`"}}}`)
	writeFile(t, dir, "sk_v1_match.jsonl",
		`{"role":"user","content":"hi","created_at":"2026-07-16T19:39:06.983127587Z"}`+"\n"+
			`{"role":"tool","content":"raw tool output"}`+"\n"+
			`{"role":"assistant","content":"hello there","created_at":"2026-07-16T19:39:08.697371182Z"}`+"\n"+
			`not valid json`+"\n"+
			`{"role":"assistant","content":""}`+"\n")

	// A different conversation that must NOT be returned.
	writeFile(t, dir, "sk_v1_other.meta.json",
		`{"scope":{"values":{"chat":"direct:pico:zzzzzzzzzzzz"}}}`)
	writeFile(t, dir, "sk_v1_other.jsonl",
		`{"role":"user","content":"someone else"}`+"\n")

	msgs, err := Read(dir, key)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(msgs), msgs)
	}
	if msgs[0] != (Message{Role: "user", Content: "hi", CreatedAt: "2026-07-16T19:39:06.983127587Z"}) {
		t.Errorf("msg[0] = %+v", msgs[0])
	}
	if msgs[1] != (Message{Role: "assistant", Content: "hello there", CreatedAt: "2026-07-16T19:39:08.697371182Z"}) {
		t.Errorf("msg[1] = %+v", msgs[1])
	}
}

func TestFindSessionFile(t *testing.T) {
	dir := t.TempDir()
	key := "abc123def456"

	writeFile(t, dir, "sk_v1_match.meta.json",
		`{"scope":{"values":{"chat":"direct:pico:`+key+`"}}}`)
	writeFile(t, dir, "sk_v1_other.meta.json",
		`{"scope":{"values":{"chat":"direct:pico:zzzzzzzzzzzz"}}}`)

	if got := FindSessionFile(dir, key); got != "sk_v1_match" {
		t.Errorf("FindSessionFile = %q, want %q", got, "sk_v1_match")
	}
	if got := FindSessionFile(dir, "nomatch"); got != "" {
		t.Errorf("FindSessionFile (no match) = %q, want empty", got)
	}
	if got := FindSessionFile(filepath.Join(dir, "does-not-exist"), key); got != "" {
		t.Errorf("FindSessionFile (missing dir) = %q, want empty", got)
	}
}

// SyncDurable must preserve the transcript across a picoclaw restart that
// overwrites the live file with only the post-restart turns.
func TestSyncDurablePreservesAcrossOverwrite(t *testing.T) {
	dir := t.TempDir()
	key := "durkey000000"
	writeFile(t, dir, "live.meta.json", `{"scope":{"values":{"chat":"direct:pico:`+key+`"}}}`)

	// Turns 1-2 live; fold into durable.
	writeFile(t, dir, "live.jsonl",
		`{"role":"user","content":"one","created_at":"t1"}`+"\n"+
			`{"role":"assistant","content":"reply one","created_at":"t2"}`+"\n")
	if err := SyncDurable(dir, key); err != nil {
		t.Fatalf("sync1: %v", err)
	}

	// picoclaw restarts and OVERWRITES the live file with only a fresh turn.
	writeFile(t, dir, "live.jsonl",
		`{"role":"user","content":"two","created_at":"t3"}`+"\n"+
			`{"role":"assistant","content":"reply two","created_at":"t4"}`+"\n")
	if err := SyncDurable(dir, key); err != nil {
		t.Fatalf("sync2: %v", err)
	}

	want := []Message{
		{Role: "user", Content: "one", CreatedAt: "t1"}, {Role: "assistant", Content: "reply one", CreatedAt: "t2"},
		{Role: "user", Content: "two", CreatedAt: "t3"}, {Role: "assistant", Content: "reply two", CreatedAt: "t4"},
	}
	msgs, err := Read(dir, key)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(msgs) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(msgs), len(want), msgs)
	}
	for i := range want {
		if msgs[i] != want[i] {
			t.Errorf("msg[%d] = %+v, want %+v", i, msgs[i], want[i])
		}
	}

	// A repeated sync must not duplicate already-captured turns.
	if err := SyncDurable(dir, key); err != nil {
		t.Fatalf("sync3: %v", err)
	}
	if msgs2, _ := Read(dir, key); len(msgs2) != len(want) {
		t.Errorf("re-sync duplicated: got %d, want %d", len(msgs2), len(want))
	}
}

func TestReadMissingDir(t *testing.T) {
	msgs, err := Read(filepath.Join(t.TempDir(), "does-not-exist"), "whatever")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("want empty, got %v", msgs)
	}
}

func TestReadNoMatchingMeta(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "sk_v1_x.meta.json", `{"scope":{"values":{"chat":"direct:pico:nomatch"}}}`)
	msgs, err := Read(dir, "target")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("want empty, got %v", msgs)
	}
}
