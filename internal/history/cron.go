package history

// Reading scheduled-task runs out of the same sessions dir the conversation
// history is read from.
//
// These are deliberately SEPARATE from findSessionFiles/readMessages rather than a
// parameterization of them, for two reasons that both came from production bugs:
//
//   - The kind filter at findSessionFiles must keep excluding cronSessionPrefix.
//     Cron runs stamp the ORIGINATING chat's scope.values.chat, and "agent_cron-…"
//     sorts before "sk_v1_…" in os.ReadDir order, so a conversation owning a daily
//     task used to resolve to a cron transcript and the user's own turns were never
//     read. Nothing here may relax that.
//   - readMessages keeps only user/assistant turns, because a chat transcript has
//     to match what the user saw. A scheduled run is the opposite case: its tool
//     activity IS the result the user came to inspect.
//
// Do not unify the two paths. TestCronReadersDoNotLeakIntoConversationHistory
// exists to fail if someone tries.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CronRun is one execution of a scheduled task, as discovered from its session
// file pair. JobID is the first segment of the session key after the cron prefix
// (picoclaw writes "agent:cron-<jobId>-<runId>"), which is the only link back to
// the job — and often the only trace left of it, since a one-shot job deletes
// itself from the store while its transcripts stay on disk.
type CronRun struct {
	JobID    string `json:"jobId"`
	RunID    string `json:"runId"`
	Basename string `json:"basename"`
	// StartedAt and UpdatedAt are the meta's own timestamps: when picoclaw opened
	// the run's session and when it last wrote to it. There is no recorded
	// per-run outcome, so callers must not present one.
	StartedAt         string `json:"startedAt"`
	UpdatedAt         string `json:"updatedAt"`
	Count             int    `json:"count"`
	Prompt            string `json:"prompt"`
	TranscriptMissing bool   `json:"transcriptMissing"`
}

// CronToolCall is one tool invocation on an assistant entry. Arguments stays the
// raw JSON string picoclaw wrote, so no assumption is made about a given tool's
// argument schema.
type CronToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// CronEntry is one entry of a run transcript, tool activity included.
type CronEntry struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	CreatedAt  string         `json:"created_at"`
	ModelName  string         `json:"model_name,omitempty"`
	ToolCalls  []CronToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// cronMeta is a run's *.meta.json. It embeds metaFile so the key and scope parse
// exactly as the conversation reader parses them, and adds the run bookkeeping
// only this reader needs.
type cronMeta struct {
	metaFile
	Count     int    `json:"count"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// CronRuns returns every scheduled-task run in sessionsDir, newest first.
//
// Only the metas and each transcript's FIRST line are read: a run transcript
// reaches six figures of bytes, and the full text is wanted only when the user
// opens one. A missing sessions dir means the agent has no sessions at all, which
// is not an error.
func CronRuns(sessionsDir string) ([]CronRun, error) {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var runs []CronRun
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".meta.json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(sessionsDir, name))
		if err != nil {
			continue // unreadable meta: skip the row, don't blank the list
		}
		var meta cronMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			continue
		}
		if !strings.HasPrefix(meta.Key, cronSessionPrefix) {
			continue // the user's own conversation, not a scheduled run
		}

		jobID, runID := splitCronKey(strings.TrimPrefix(meta.Key, cronSessionPrefix))
		basename := strings.TrimSuffix(name, ".meta.json")
		prompt, found := firstEntryContent(filepath.Join(sessionsDir, basename+".jsonl"))

		runs = append(runs, CronRun{
			JobID:             jobID,
			RunID:             runID,
			Basename:          basename,
			StartedAt:         meta.CreatedAt,
			UpdatedAt:         meta.UpdatedAt,
			Count:             meta.Count,
			Prompt:            prompt,
			TranscriptMissing: !found,
		})
	}

	// Newest first, tie-broken by run id so the order is total: two runs of the
	// same job can share a start instant at second resolution.
	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].StartedAt != runs[j].StartedAt {
			return runs[i].StartedAt > runs[j].StartedAt
		}
		return runs[i].RunID > runs[j].RunID
	})
	return runs, nil
}

// splitCronKey separates the job segment from the run segment of a key body.
// A key with no separator is treated as all job and no run rather than discarded,
// so an unfamiliar naming scheme still lists.
func splitCronKey(body string) (jobID, runID string) {
	if i := strings.Index(body, "-"); i >= 0 {
		return body[:i], body[i+1:]
	}
	return body, ""
}

// firstEntryContent returns the content of a transcript's first entry, which for a
// scheduled run is the prompt the job fired with. The second return distinguishes
// "the transcript is not there" from "its first entry has no content" — the former
// is worth telling the user about.
//
// Built on eachLineUntil rather than its own scanner: this package's reader exists
// because bufio.Scanner abandons a file on an oversized line, and picoclaw inlines
// whole tool results, so that is a line shape this data really has.
func firstEntryContent(path string) (string, bool) {
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	content := ""
	_ = eachLineUntil(path, func(line string) bool {
		var e jsonlEntry
		if err := json.Unmarshal([]byte(line), &e); err == nil {
			content = e.Content
		}
		return false // the first line is the whole job
	})
	return content, true
}

// ReadCronRun returns a run's whole transcript, tool entries and tool calls
// included. basename must already have been resolved against CronRuns by the
// caller; nothing here interprets it beyond joining it to sessionsDir.
func ReadCronRun(sessionsDir, basename string) ([]CronEntry, error) {
	entries := []CronEntry{}
	err := eachLine(filepath.Join(sessionsDir, basename+".jsonl"), func(line string) {
		var e CronEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return // skip a malformed line rather than failing the whole run
		}
		if e.Role == "" {
			return
		}
		entries = append(entries, e)
	})
	if err != nil {
		return entries, err
	}
	return entries, nil
}
