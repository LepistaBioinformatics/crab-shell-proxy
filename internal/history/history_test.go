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
		`{"role":"user","content":"hi"}`+"\n"+
			`{"role":"tool","content":"raw tool output"}`+"\n"+
			`{"role":"assistant","content":"hello there"}`+"\n"+
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
	if msgs[0] != (Message{Role: "user", Content: "hi"}) {
		t.Errorf("msg[0] = %+v", msgs[0])
	}
	if msgs[1] != (Message{Role: "assistant", Content: "hello there"}) {
		t.Errorf("msg[1] = %+v", msgs[1])
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
