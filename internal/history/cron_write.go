package history

// Writing a scheduled run's *.meta.json.
//
// Only for harnesses that have no scheduler of their own. picoclaw fires its own
// jobs and writes its own metas; nothing here ever touches that.
//
// This is the one half of the reader's contract the ganglion cannot satisfy. It
// writes a transcript for the conversation it is handed and nothing else — it has
// no concept of a scheduled turn, so it cannot know a run happened, which job it
// belonged to, or which scope asked for it. The proxy knows all three, because
// the proxy is what fired the job.
//
// It does NOT violate "one writer per file", the invariant livePartial records.
// A meta is a file the ganglion never writes, never reads and does not know
// exists — the same standing the durable transcript beside it already has.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CronSessionID is the conversation id a scheduled run executes under.
//
// picoclaw's own shape, deliberately: CronRuns discovers a run by this prefix and
// splitCronKey takes the job and run ids back out of it, so a ganglion run lists
// in the member's Tasks panel through the reader that already exists, unchanged.
//
// The job id must not contain "-": splitCronKey cuts at the FIRST one. cron.NewID
// is what guarantees that.
func CronSessionID(jobID, runID string) string {
	return cronSessionPrefix + jobID + "-" + runID
}

// CronBasename is the on-disk basename of that run, transcript and meta alike.
// The harness derives the transcript's name from the conversation id with its own
// sanitiser, so the meta has to be named by the same rule or the two never pair up.
func CronBasename(sessionID string) string {
	return harnessBasename(sessionID)
}

// WriteCronMeta writes the meta for one run, so CronRuns can find it.
//
// chatSessionKey is the conversation the run belongs to, or "" for a job that
// belongs to no conversation — which is every job created from the member's Tasks
// panel. Empty is written as an empty marker rather than as a made-up one:
// CronRun.SessionKey is documented as "no conversation" when it cannot be read,
// and inventing a link would put unattended work on some conversation's timeline.
//
// count is read back off the transcript rather than passed in, because the
// transcript is what the member opens and a count that disagrees with it is worse
// than no count. A missing transcript yields 0 and the reader reports
// TranscriptMissing, which is the honest answer when a turn failed before the
// harness wrote anything.
func WriteCronMeta(sessionsDir, sessionID, chatSessionKey string, startedAt, updatedAt time.Time) error {
	basename := CronBasename(sessionID)
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		return err
	}

	var meta cronMeta
	meta.Key = sessionID
	if chatSessionKey != "" {
		meta.Scope.Values.Chat = chatMarkerPrefix + chatSessionKey
	}
	meta.Count = countLines(filepath.Join(sessionsDir, basename+".jsonl"))
	// Second resolution, fixed width, UTC. CronRuns sorts these as STRINGS, so a
	// format that drops trailing zeros would order runs by how their timestamp
	// happened to be spelled.
	meta.CreatedAt = startedAt.UTC().Format(time.RFC3339)
	meta.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)

	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	// 0o644 for the same reason the durable transcript is: the container's
	// non-root user shares this directory.
	return os.WriteFile(filepath.Join(sessionsDir, basename+".meta.json"), append(raw, '\n'), 0o644)
}

// countLines counts the non-empty lines of a transcript, or 0 when it is absent.
//
// Reads the whole file, which is what the entry count costs; a run transcript is
// written once and this runs once per run.
func countLines(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}
