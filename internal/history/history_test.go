package history

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// seedDurable establishes a conversation whose first turn is already captured
// durably — the state every freeze below starts from.
func seedDurable(t *testing.T, dir, key, basename string) {
	t.Helper()
	writeFile(t, dir, basename+".meta.json", `{"scope":{"values":{"chat":"direct:pico:`+key+`"}}}`)
	writeFile(t, dir, basename+".jsonl",
		`{"role":"user","content":"one","created_at":"t1"}`+"\n"+
			`{"role":"assistant","content":"reply one","created_at":"t2"}`+"\n")
	if err := SyncDurable(dir, key); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
}

// An oversized line (picoclaw inlines whole tool results) must not stop the fold:
// the small turns after it still have to reach the durable transcript. Before
// this, bufio.Scanner's ErrTooLong aborted SyncDurable before it folded
// ANYTHING, and the conversation silently stopped growing for good.
func TestSyncDurableSkipsOversizedLineAndKeepsFolding(t *testing.T) {
	dir, key := t.TempDir(), "oversize0000"
	seedDurable(t, dir, key, "live")

	huge := strings.Repeat("x", maxLineBytes+1)
	writeFile(t, dir, "live.jsonl",
		`{"role":"user","content":"one","created_at":"t1"}`+"\n"+
			`{"role":"assistant","content":"reply one","created_at":"t2"}`+"\n"+
			`{"role":"tool","content":"`+huge+`","created_at":"t3"}`+"\n"+
			`{"role":"user","content":"two","created_at":"t4"}`+"\n"+
			`{"role":"assistant","content":"reply two","created_at":"t5"}`+"\n")

	if err := SyncDurable(dir, key); err != nil {
		t.Fatalf("sync: %v", err)
	}
	msgs, err := Read(dir, key)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4 (turn after the oversized line was dropped): %+v", len(msgs), msgs)
	}
	if msgs[3].Content != "reply two" {
		t.Errorf("msgs[3] = %+v, want the post-oversize reply", msgs[3])
	}
}

// A long-but-valid answer spans several of the reader's 64KB fills and must come
// back byte-for-byte. Guards the chunk accumulation that replaced bufio.Scanner:
// getting this wrong would truncate or drop ordinary long replies.
func TestReadPreservesLineSpanningManyBufferFills(t *testing.T) {
	dir, key := t.TempDir(), "longline0000"
	body := strings.Repeat("orçamento anual ", 40000) // ~640KB, 10 buffer fills
	writeFile(t, dir, "live.meta.json", `{"scope":{"values":{"chat":"direct:pico:`+key+`"}}}`)
	writeFile(t, dir, "live.jsonl",
		`{"role":"user","content":"resume","created_at":"t1"}`+"\n"+
			`{"role":"assistant","content":"`+body+`","created_at":"t2"}`+"\n")

	msgs, err := Read(dir, key)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(msgs), msgs)
	}
	if msgs[1].Content != body {
		t.Errorf("long content corrupted: got %d bytes, want %d", len(msgs[1].Content), len(body))
	}
}

// picoclaw can continue the same chat in a NEW session file and leave the old
// meta.json behind, so two metas carry the same marker. Folding only the one that
// sorted first in directory order meant the live turns were never captured —
// permanently, since the old file never changes again.
func TestSyncDurableFoldsEveryFileSharingTheMarker(t *testing.T) {
	dir, key := t.TempDir(), "twometas0000"
	seedDurable(t, dir, key, "aaa_old")

	// The current session is a second file whose name sorts AFTER the dead one.
	writeFile(t, dir, "zzz_new.meta.json", `{"scope":{"values":{"chat":"direct:pico:`+key+`"}}}`)
	writeFile(t, dir, "zzz_new.jsonl",
		`{"role":"user","content":"two","created_at":"t3"}`+"\n"+
			`{"role":"assistant","content":"reply two","created_at":"t4"}`+"\n")
	// mtime decides which file is current, so make the ordering explicit rather
	// than dependent on how fast the test wrote the two files.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "aaa_old.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}

	if got := FindSessionFile(dir, key); got != "zzz_new" {
		t.Errorf("FindSessionFile = %q, want the most recent file %q", got, "zzz_new")
	}
	if err := SyncDurable(dir, key); err != nil {
		t.Fatalf("sync: %v", err)
	}
	msgs, err := Read(dir, key)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4 (new session file was not folded): %+v", len(msgs), msgs)
	}
	if msgs[3].Content != "reply two" {
		t.Errorf("msgs[3] = %+v, want the turn from the new session file", msgs[3])
	}
}

// The shape that actually broke in production: a conversation owning two daily
// cron tasks. Each RUN writes its own session file stamped with the chat's marker,
// and "agent_cron-…" sorts before "sk_v1_…" in os.ReadDir order — so the chat
// resolved to a cron transcript and the user's own turns were never folded. The
// cron runs happened unattended and must stay out of the transcript.
func TestSyncDurableIgnoresCronSessionsSharingTheMarker(t *testing.T) {
	dir, key := t.TempDir(), "6f5c2836013f8162279a7b480d43628c"
	marker := `"chat":"direct:pico:` + key + `"`

	// Two runs of one task plus one of another, all sorting before the real chat.
	for _, run := range []string{
		"agent_cron-e520b224e7714d16-3a5a895e",
		"agent_cron-e520b224e7714d16-5e055123",
		"agent_cron-0a2d1312e29318af-d3eeb404",
	} {
		writeFile(t, dir, run+".meta.json",
			`{"key":"agent:cron-`+strings.TrimPrefix(run, "agent_cron-")+`","scope":{"values":{`+marker+`}}}`)
		writeFile(t, dir, run+".jsonl",
			`{"role":"user","content":"[cron] gerar relatorio","created_at":"c1-`+run+`"}`+"\n"+
				`{"role":"assistant","content":"relatorio gerado","created_at":"c2-`+run+`"}`+"\n")
	}
	// The user's own session, sorting last.
	writeFile(t, dir, "sk_v1_7f7c41b4.meta.json",
		`{"key":"sk_v1_7f7c41b4","scope":{"values":{`+marker+`}}}`)
	writeFile(t, dir, "sk_v1_7f7c41b4.jsonl",
		`{"role":"user","content":"a tarefa rodou hoje?","created_at":"u1"}`+"\n"+
			`{"role":"assistant","content":"o relatorio esta rodando diariamente","created_at":"u2"}`+"\n")

	if got := FindSessionFile(dir, key); got != "sk_v1_7f7c41b4" {
		t.Errorf("FindSessionFile = %q, want the user's session (a cron run was picked)", got)
	}
	if err := SyncDurable(dir, key); err != nil {
		t.Fatalf("sync: %v", err)
	}
	msgs, err := Read(dir, key)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want just the user's 2: %+v", len(msgs), msgs)
	}
	if msgs[1].Content != "o relatorio esta rodando diariamente" {
		t.Errorf("msgs[1] = %+v, want the reply that used to vanish", msgs[1])
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, "[cron]") {
			t.Errorf("cron turn leaked into the transcript: %+v", m)
		}
	}
}

// A live transcript that vanishes while a durable one exists is a real failure:
// every later turn is invisible to the client. It must not report success.
func TestSyncDurableReportsMissingLiveTranscript(t *testing.T) {
	dir, key := t.TempDir(), "vanished0000"
	seedDurable(t, dir, key, "live")

	// The marker no longer resolves (meta rewritten under a different scope).
	writeFile(t, dir, "live.meta.json", `{"scope":{"values":{"chat":"direct:pico:something-else"}}}`)

	if err := SyncDurable(dir, key); !errors.Is(err, ErrLiveTranscriptMissing) {
		t.Errorf("SyncDurable = %v, want ErrLiveTranscriptMissing", err)
	}
}

// ...but a conversation picoclaw simply hasn't persisted yet is NOT a failure,
// or every brand-new chat would log one.
func TestSyncDurableSilentBeforeFirstPersist(t *testing.T) {
	if err := SyncDurable(t.TempDir(), "nothingyet00"); err != nil {
		t.Errorf("SyncDurable = %v, want nil for a not-yet-persisted conversation", err)
	}
}
