// Package history reads a conversation's transcript from a per-user picoclaw
// data dir, mirroring picoclaw-openai-proxy/server.js's sessions/history logic.
package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Message is one conversational turn returned to the client.
type Message struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

// metaFile mirrors the subset of a *.meta.json we match on. picoclaw derives
// its own on-disk filename hash internally, but every meta carries
// scope.values.chat = "direct:pico:<our-session-key>" — a value we control — so
// we scan for that rather than trying to reproduce picoclaw's hash.
//
// Key says what KIND of session wrote the transcript, which matters because the
// marker alone does not distinguish them: see cronSessionPrefix.
type metaFile struct {
	Key   string `json:"key"`
	Scope struct {
		Values struct {
			Chat string `json:"chat"`
		} `json:"values"`
	} `json:"scope"`
}

// cronSessionPrefix marks a meta written by a scheduled task rather than by the
// user talking to the agent. Every cron RUN gets its own session file and stamps
// it with the originating chat's scope.values.chat, so a conversation that owns a
// couple of daily tasks accumulates one marker-sharing file per run — which is how
// a chat with cron tasks ended up resolving to a cron transcript instead of its
// own. Those turns ran unattended, so they stay out: history has to match what the
// user actually saw, the same rule readMessages applies to "tool" entries.
// picoclaw's own aliases agree with the split — a cron session carries
// "…:direct:cron" where the user's carries "…:direct:pico-user".
//
// Excluding the cron kind rather than requiring the pico-user kind is deliberate:
// if picoclaw renames these, an unrecognised session kind should surface as noise
// in a transcript, never silently stop the conversation from being captured.
const cronSessionPrefix = "agent:cron-"

// jsonlEntry is one line of a picoclaw session transcript.
type jsonlEntry struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

// durableDir is the subdirectory (under a session's sessions/ dir) holding the
// proxy-maintained, append-only transcripts: durable/<sessionKey>.jsonl. It sits
// in its own dir so picoclaw's session scan (which pairs *.jsonl with *.meta.json
// at the top level) never mistakes it for one of its sessions. picoclaw rewrites
// its live file from its in-memory session and loses earlier turns on a restart;
// this file only ever grows, so the conversation history survives restarts.
const durableDir = "durable"

// Read returns the plain user/assistant turns for the conversation whose
// session key is sessionKey (the sha256(email::session_id)[:32] value), located
// under sessionsDir (<per-user-data>/workspace/sessions). It prefers the durable
// transcript (which survives picoclaw's live-file rewrites) and falls back to
// the live file. Missing dir or file yields an empty slice, never an error.
func Read(sessionsDir, sessionKey string) ([]Message, error) {
	durablePath := filepath.Join(sessionsDir, durableDir)
	if _, err := os.Stat(filepath.Join(durablePath, sessionKey+".jsonl")); err == nil {
		return readMessages(durablePath, sessionKey)
	}
	basename := FindSessionFile(sessionsDir, sessionKey)
	if basename == "" {
		return []Message{}, nil
	}
	return readMessages(sessionsDir, basename)
}

// ErrLiveTranscriptMissing means no live transcript carries this conversation's
// marker even though a durable one already exists — so the live file the durable
// transcript was being fed from has gone out from under us. Reported rather than
// swallowed: every turn from here on is invisible to the client, and the previous
// silent `return nil` made that look like a successful sync.
var ErrLiveTranscriptMissing = errors.New("no live transcript matches the session key")

// SyncDurable folds the live transcript(s) for sessionKey into the append-only
// durable transcript next to it, adding only lines not already present (deduped
// by created_at + role, falling back to the whole line). Call it after every turn
// — while the live file still holds the full history — so a later restart that
// rewrites the live file can't erase what the durable file already captured.
//
// A write error leaves the durable file unchanged. So does having nothing live to
// fold yet, which is normal before picoclaw's first persist; but once a durable
// transcript exists, a live one that no longer resolves is a failure and returns
// ErrLiveTranscriptMissing rather than reporting success.
func SyncDurable(sessionsDir, sessionKey string) error {
	basenames := findSessionFiles(sessionsDir, sessionKey)
	durableDirPath := filepath.Join(sessionsDir, durableDir)
	if len(basenames) == 0 {
		// No live transcript is normal for a conversation picoclaw hasn't
		// persisted yet. Once a durable one exists it is not: see the error.
		if _, err := os.Stat(filepath.Join(durableDirPath, sessionKey+".jsonl")); err == nil {
			return ErrLiveTranscriptMissing
		}
		return nil
	}
	// Every matching file is folded, not just one. picoclaw can leave a previous
	// session file (and its meta) behind and continue the same chat in a new one;
	// folding only the file that happened to sort first meant the live turns were
	// never captured at all. Dedup below makes covering all of them safe.
	var live []string
	for _, basename := range basenames {
		lines, err := readRawLines(filepath.Join(sessionsDir, basename+".jsonl"))
		if err != nil {
			return err
		}
		live = append(live, lines...)
	}
	if len(live) == 0 {
		return nil
	}
	if err := os.MkdirAll(durableDirPath, 0o755); err != nil {
		return err
	}
	durablePath := filepath.Join(durableDirPath, sessionKey+".jsonl")
	existing, err := readRawLines(durablePath)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(existing))
	for _, l := range existing {
		seen[dedupKey(l)] = true
	}
	var add []string
	for _, l := range live {
		k := dedupKey(l)
		if seen[k] {
			continue
		}
		seen[k] = true
		add = append(add, l)
	}
	if len(add) == 0 {
		return nil
	}
	// 0o644 so the non-root agent can read it back (per memory/CONTEXT_RECOVERY.md).
	f, err := os.OpenFile(durablePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, l := range add {
		if _, err := f.WriteString(l + "\n"); err != nil {
			return err
		}
	}
	return nil
}

// dedupKey identifies a transcript line for durable de-duplication: created_at
// (unique per message) + role, falling back to the whole line when a line has no
// timestamp.
func dedupKey(line string) string {
	var e struct {
		Role      string `json:"role"`
		CreatedAt string `json:"created_at"`
	}
	if json.Unmarshal([]byte(line), &e) == nil && e.CreatedAt != "" {
		return e.CreatedAt + "\x00" + e.Role
	}
	return line
}

// maxLineBytes caps how long a single transcript line may be before it is
// skipped. picoclaw inlines whole tool results, so one line can be enormous.
const maxLineBytes = 8 * 1024 * 1024

// eachLine calls fn for every non-empty, trimmed line of path. A line longer
// than maxLineBytes is SKIPPED, and the rest of the file is still read — which is
// the whole point of not using bufio.Scanner here. Scanner reports ErrTooLong and
// then stops, so a single oversized line used to abort the read and take every
// following line with it: in SyncDurable that meant nothing was folded at all and
// the conversation's durable transcript stopped growing for good. Memory stays
// bounded because an oversized line is discarded as it is consumed, never
// assembled. A missing file yields no lines and no error.
func eachLine(path string, fn func(line string)) error {
	return eachLineUntil(path, func(line string) bool {
		fn(line)
		return true
	})
}

// eachLineUntil is eachLine with an early exit: fn returns false to stop reading.
//
// It exists so a caller that wants only the FIRST line does not need its own reader.
// The obvious bufio.Scanner version is exactly what the comment above forbids, and it
// fails the same way — Scan() gives up on an oversized line, which for a first-line
// read means silently reporting no content at all.
func eachLineUntil(path string, fn func(line string) bool) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 64*1024)
	var buf []byte
	oversized := false
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > maxLineBytes {
			oversized, buf = true, buf[:0]
		} else if !oversized {
			buf = append(buf, chunk...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // mid-line: keep consuming until the newline
		}
		if !oversized {
			if line := strings.TrimSpace(string(buf)); line != "" {
				if !fn(line) {
					return nil
				}
			}
		}
		buf, oversized = buf[:0], false
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// readRawLines returns the non-empty, trimmed lines of a jsonl file (empty when
// absent).
func readRawLines(path string) ([]string, error) {
	var out []string
	err := eachLine(path, func(line string) { out = append(out, line) })
	return out, err
}

// FindSessionFile resolves the picoclaw on-disk file basename (without
// extension) for the conversation whose session key is sessionKey, by scanning
// the *.meta.json files under sessionsDir for the scope.values.chat marker
// picoclaw wrote. It returns "" when the sessions dir is missing or no file
// matches yet (picoclaw hasn't persisted the transcript).
func FindSessionFile(sessionsDir, sessionKey string) string {
	files := findSessionFiles(sessionsDir, sessionKey)
	if len(files) == 0 {
		return ""
	}
	return files[len(files)-1] // the most recently written one
}

// findSessionFiles returns every basename whose meta carries sessionKey's marker
// and was written by the user's own session, least-recently-written first.
//
// Two things make this more than a lookup. Scheduled tasks stamp their own session
// files with the chat's marker (cronSessionPrefix), and picoclaw may continue a
// chat in a fresh session file while leaving the old meta.json in place — so a
// marker can legitimately match several files. This used to return the first match
// in os.ReadDir order, which is lexicographic: "agent_cron-…" sorts before
// "sk_v1_…", so any conversation owning a cron task resolved to a cron transcript
// and the user's real one was never read at all. Filtering by session kind and
// ordering by mtime is what makes "the conversation's current file" mean that.
func findSessionFiles(sessionsDir, sessionKey string) []string {
	marker := "direct:pico:" + sessionKey
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil // no sessions dir yet — no conversations at all
	}
	type match struct {
		basename string
		modTime  time.Time
	}
	var matches []match
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".meta.json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(sessionsDir, name))
		if err != nil {
			continue
		}
		var meta metaFile
		if err := json.Unmarshal(raw, &meta); err != nil {
			continue
		}
		if meta.Scope.Values.Chat != marker {
			continue
		}
		if strings.HasPrefix(meta.Key, cronSessionPrefix) {
			continue // a scheduled run, not the conversation the user is having
		}
		basename := strings.TrimSuffix(name, ".meta.json")
		var mod time.Time
		if fi, err := os.Stat(filepath.Join(sessionsDir, basename+".jsonl")); err == nil {
			mod = fi.ModTime()
		}
		matches = append(matches, match{basename, mod})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].modTime.Before(matches[j].modTime)
	})
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = m.basename
	}
	return out
}

func readMessages(sessionsDir, basename string) ([]Message, error) {
	messages := []Message{}
	err := eachLine(filepath.Join(sessionsDir, basename+".jsonl"), func(line string) {
		var e jsonlEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return // skip a malformed line rather than failing the whole history
		}
		// Only plain conversational turns — picoclaw also logs "tool" entries
		// inline, which the live stream already discards. History should match
		// what the user actually saw.
		if (e.Role == "user" || e.Role == "assistant") && e.Content != "" {
			messages = append(messages, Message{Role: e.Role, Content: e.Content, CreatedAt: e.CreatedAt})
		}
	})
	if err != nil {
		return messages, err
	}
	return messages, nil
}
