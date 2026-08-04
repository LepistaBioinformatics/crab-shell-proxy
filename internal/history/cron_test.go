package history

import (
	"strconv"
	"strings"
	"testing"
)

// cronMetaJSON builds a meta the way picoclaw writes one for a scheduled run:
// the key names the job and the run, and scope.values.chat is the ORIGINATING
// chat's marker — the same value the user's own session carries.
func cronMetaJSON(runKey, chatKey string, count int, created, updated string) string {
	return `{"key":"agent:cron-` + runKey + `","summary":"","count":` + strconv.Itoa(count) +
		`,"created_at":"` + created + `","updated_at":"` + updated +
		`","scope":{"values":{"chat":"direct:pico:` + chatKey + `"}}}`
}

// Two runs of one job plus one of another — the real shape from the deployment,
// where the first hex segment of the key is the job and the rest is the run.
func TestCronRunsSplitsJobAndRun(t *testing.T) {
	dir, chat := t.TempDir(), "6f5c2836013f8162279a7b480d43628c"

	writeFile(t, dir, "agent_cron-e520b224e7714d16-5e055123-b25a.meta.json",
		cronMetaJSON("e520b224e7714d16-5e055123-b25a", chat, 34,
			"2026-08-02T20:58:00Z", "2026-08-02T21:00:33Z"))
	writeFile(t, dir, "agent_cron-e520b224e7714d16-5e055123-b25a.jsonl",
		`{"role":"user","content":"RELATORIO DIARIO: buscar noticias","created_at":"2026-08-02T20:58:00Z"}`+"\n"+
			`{"role":"assistant","content":"pronto","created_at":"2026-08-02T21:00:33Z"}`+"\n")

	writeFile(t, dir, "agent_cron-e520b224e7714d16-3a5a895e-1573.meta.json",
		cronMetaJSON("e520b224e7714d16-3a5a895e-1573", chat, 28,
			"2026-08-01T20:58:00Z", "2026-08-01T20:59:48Z"))
	writeFile(t, dir, "agent_cron-e520b224e7714d16-3a5a895e-1573.jsonl",
		`{"role":"user","content":"RELATORIO DIARIO: buscar noticias","created_at":"2026-08-01T20:58:00Z"}`+"\n")

	writeFile(t, dir, "agent_cron-0a2d1312e29318af-d3eeb404-3338.meta.json",
		cronMetaJSON("0a2d1312e29318af-d3eeb404-3338", chat, 12,
			"2026-08-03T06:00:00Z", "2026-08-03T06:01:10Z"))
	writeFile(t, dir, "agent_cron-0a2d1312e29318af-d3eeb404-3338.jsonl",
		`{"role":"user","content":"outra tarefa","created_at":"2026-08-03T06:00:00Z"}`+"\n")

	// The user's own session must be ignored entirely by this reader.
	writeFile(t, dir, "sk_v1_7f7c41b4.meta.json",
		`{"key":"sk_v1_7f7c41b4","scope":{"values":{"chat":"direct:pico:`+chat+`"}}}`)
	writeFile(t, dir, "sk_v1_7f7c41b4.jsonl",
		`{"role":"user","content":"a tarefa rodou hoje?","created_at":"u1"}`+"\n")

	runs, err := CronRuns(dir)
	if err != nil {
		t.Fatalf("CronRuns: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("len(runs) = %d, want 3 (the user's session must not appear): %+v", len(runs), runs)
	}

	// Newest first, so the panel's first row is the most recent execution.
	if runs[0].StartedAt != "2026-08-03T06:00:00Z" {
		t.Errorf("runs[0].StartedAt = %q, want the newest run first", runs[0].StartedAt)
	}
	if runs[2].StartedAt != "2026-08-01T20:58:00Z" {
		t.Errorf("runs[2].StartedAt = %q, want the oldest run last", runs[2].StartedAt)
	}

	byJob := map[string]int{}
	for _, r := range runs {
		byJob[r.JobID]++
	}
	if byJob["e520b224e7714d16"] != 2 {
		t.Errorf("job e520b224e7714d16 has %d runs, want 2", byJob["e520b224e7714d16"])
	}
	if byJob["0a2d1312e29318af"] != 1 {
		t.Errorf("job 0a2d1312e29318af has %d runs, want 1", byJob["0a2d1312e29318af"])
	}

	newest := runs[0]
	if newest.RunID != "d3eeb404-3338" {
		t.Errorf("RunID = %q, want everything after the job segment", newest.RunID)
	}
	if newest.Basename != "agent_cron-0a2d1312e29318af-d3eeb404-3338" {
		t.Errorf("Basename = %q, want the on-disk basename", newest.Basename)
	}
	if newest.Count != 12 {
		t.Errorf("Count = %d, want the meta's count", newest.Count)
	}
	if newest.Prompt != "outra tarefa" {
		t.Errorf("Prompt = %q, want the transcript's first entry", newest.Prompt)
	}
	if newest.TranscriptMissing {
		t.Error("TranscriptMissing = true, but the .jsonl exists")
	}
}

// A one-shot job removes itself from the store but leaves its transcript, and a
// transcript can also be pruned while its meta survives. Neither may drop the row
// silently — the user would just see an execution that never happened.
func TestCronRunsReportsMissingTranscript(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "agent_cron-4daaedfb795f4be8-9a26b122.meta.json",
		cronMetaJSON("4daaedfb795f4be8-9a26b122", "chatkey", 5,
			"2026-07-31T21:00:00Z", "2026-07-31T21:02:00Z"))

	runs, err := CronRuns(dir)
	if err != nil {
		t.Fatalf("CronRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("len(runs) = %d, want the row to survive a missing transcript", len(runs))
	}
	if !runs[0].TranscriptMissing {
		t.Error("TranscriptMissing = false, want it flagged")
	}
	if runs[0].Prompt != "" {
		t.Errorf("Prompt = %q, want empty when there is no transcript", runs[0].Prompt)
	}
}

func TestCronRunsOnMissingDir(t *testing.T) {
	runs, err := CronRuns(t.TempDir() + "/nope")
	if err != nil {
		t.Fatalf("a workspace with no sessions dir must not error: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("runs = %+v, want none", runs)
	}
}

// Unlike readMessages, which drops "tool" entries because a chat transcript must
// match what the user saw, a scheduled run's tool activity IS the content the user
// is inspecting. This is why the two readers stay separate.
func TestReadCronRunKeepsToolActivity(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "agent_cron-abc-run1.jsonl",
		`{"role":"user","content":"buscar noticias","created_at":"t0"}`+"\n"+
			`{"role":"assistant","content":"vou buscar","model_name":"DeepSeek-V4-Pro-Azure","created_at":"t1",`+
			`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"iran\"}"}}]}`+"\n"+
			`{"role":"tool","content":"Results for: iran","created_at":"t2","tool_call_id":"call_1"}`+"\n"+
			`{"role":"assistant","content":"relatorio pronto","model_name":"DeepSeek-V4-Pro-Azure","created_at":"t3"}`+"\n")

	entries, err := ReadCronRun(dir, "agent_cron-abc-run1")
	if err != nil {
		t.Fatalf("ReadCronRun: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("len(entries) = %d, want all 4 including the tool entry: %+v", len(entries), entries)
	}
	if entries[2].Role != "tool" || entries[2].Content != "Results for: iran" {
		t.Errorf("entries[2] = %+v, want the tool entry kept", entries[2])
	}
	if entries[1].ModelName != "DeepSeek-V4-Pro-Azure" {
		t.Errorf("ModelName = %q, want it carried", entries[1].ModelName)
	}
	if len(entries[1].ToolCalls) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1", len(entries[1].ToolCalls))
	}
	if entries[1].ToolCalls[0].Function.Name != "web_search" {
		t.Errorf("tool call name = %q, want web_search", entries[1].ToolCalls[0].Function.Name)
	}
	if !strings.Contains(entries[1].ToolCalls[0].Function.Arguments, "iran") {
		t.Errorf("tool call arguments = %q, want the raw argument JSON", entries[1].ToolCalls[0].Function.Arguments)
	}
}

func TestReadCronRunOnMissingFile(t *testing.T) {
	entries, err := ReadCronRun(t.TempDir(), "agent_cron-nope-run")
	if err != nil {
		t.Fatalf("a missing transcript must not error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("entries = %+v, want none", entries)
	}
}

// The regression this feature must not reintroduce. Reading cron runs and
// resolving the user's conversation are two different questions over the same
// directory: "agent_cron-…" sorts before "sk_v1_…" in os.ReadDir order, so before
// the kind filter existed, a chat that owned cron tasks resolved to a cron
// transcript and the user's own turns vanished. Adding a cron reader must not
// weaken that filter — this test fails loudly if anyone unifies the two paths.
func TestCronReadersDoNotLeakIntoConversationHistory(t *testing.T) {
	dir, key := t.TempDir(), "6f5c2836013f8162279a7b480d43628c"

	writeFile(t, dir, "agent_cron-e520b224e7714d16-5e055123.meta.json",
		cronMetaJSON("e520b224e7714d16-5e055123", key, 2, "2026-08-02T20:58:00Z", "2026-08-02T21:00:00Z"))
	writeFile(t, dir, "agent_cron-e520b224e7714d16-5e055123.jsonl",
		`{"role":"user","content":"[cron] gerar relatorio","created_at":"c1"}`+"\n"+
			`{"role":"assistant","content":"relatorio gerado","created_at":"c2"}`+"\n")
	writeFile(t, dir, "sk_v1_7f7c41b4.meta.json",
		`{"key":"sk_v1_7f7c41b4","scope":{"values":{"chat":"direct:pico:`+key+`"}}}`)
	writeFile(t, dir, "sk_v1_7f7c41b4.jsonl",
		`{"role":"user","content":"a tarefa rodou hoje?","created_at":"u1"}`+"\n"+
			`{"role":"assistant","content":"esta rodando diariamente","created_at":"u2"}`+"\n")

	// The cron reader sees the run...
	runs, err := CronRuns(dir)
	if err != nil {
		t.Fatalf("CronRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].JobID != "e520b224e7714d16" {
		t.Fatalf("CronRuns = %+v, want the one cron run", runs)
	}

	// ...and the conversation still resolves to the user's own session.
	if got := FindSessionFile(dir, key); got != "sk_v1_7f7c41b4" {
		t.Fatalf("FindSessionFile = %q, want the user's session — the cron filter was weakened", got)
	}
	if err := SyncDurable(dir, key); err != nil {
		t.Fatalf("sync: %v", err)
	}
	msgs, err := Read(dir, key)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, "[cron]") {
			t.Fatalf("a cron turn leaked into the conversation: %+v", m)
		}
	}
	if len(msgs) != 2 {
		t.Errorf("got %d messages, want just the user's 2: %+v", len(msgs), msgs)
	}
}
