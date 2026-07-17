// Package history reads a conversation's transcript from a per-user picoclaw
// data dir, mirroring picoclaw-openai-proxy/server.js's sessions/history logic.
package history

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Message is one conversational turn returned to the client.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// metaFile mirrors the subset of a *.meta.json we match on. picoclaw derives
// its own on-disk filename hash internally, but every meta carries
// scope.values.chat = "direct:pico:<our-session-key>" — a value we control — so
// we scan for that rather than trying to reproduce picoclaw's hash.
type metaFile struct {
	Scope struct {
		Values struct {
			Chat string `json:"chat"`
		} `json:"values"`
	} `json:"scope"`
}

// jsonlEntry is one line of a picoclaw session transcript.
type jsonlEntry struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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

// SyncDurable folds the current live transcript for sessionKey into the
// append-only durable transcript next to it, adding only lines not already
// present (deduped by created_at + role, falling back to the whole line). Call
// it after every turn — while the live file still holds the full history — so a
// later restart that rewrites the live file can't erase what the durable file
// already captured. Best-effort: an absent live file or a write error leaves the
// durable file unchanged.
func SyncDurable(sessionsDir, sessionKey string) error {
	basename := FindSessionFile(sessionsDir, sessionKey)
	if basename == "" {
		return nil // nothing live to fold in yet
	}
	live, err := readRawLines(filepath.Join(sessionsDir, basename+".jsonl"))
	if err != nil || len(live) == 0 {
		return err
	}
	durableDirPath := filepath.Join(sessionsDir, durableDir)
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

// readRawLines returns the non-empty, trimmed lines of a jsonl file (empty when
// absent).
func readRawLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

// FindSessionFile resolves the picoclaw on-disk file basename (without
// extension) for the conversation whose session key is sessionKey, by scanning
// the *.meta.json files under sessionsDir for the scope.values.chat marker
// picoclaw wrote. It returns "" when the sessions dir is missing or no file
// matches yet (picoclaw hasn't persisted the transcript).
func FindSessionFile(sessionsDir, sessionKey string) string {
	marker := "direct:pico:" + sessionKey
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return "" // no sessions dir yet — no conversations at all
	}
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
		if meta.Scope.Values.Chat == marker {
			return strings.TrimSuffix(name, ".meta.json")
		}
	}
	return ""
}

func readMessages(sessionsDir, basename string) ([]Message, error) {
	f, err := os.Open(filepath.Join(sessionsDir, basename+".jsonl"))
	if err != nil {
		return []Message{}, nil // no transcript yet
	}
	defer f.Close()

	messages := []Message{}
	sc := bufio.NewScanner(f)
	// Transcript lines can be large (full assistant answers); grow the buffer.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e jsonlEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // skip a malformed line rather than failing the whole history
		}
		// Only plain conversational turns — picoclaw also logs "tool" entries
		// inline, which the live stream already discards. History should match
		// what the user actually saw.
		if (e.Role == "user" || e.Role == "assistant") && e.Content != "" {
			messages = append(messages, Message{Role: e.Role, Content: e.Content})
		}
	}
	if err := sc.Err(); err != nil {
		return messages, err
	}
	return messages, nil
}
