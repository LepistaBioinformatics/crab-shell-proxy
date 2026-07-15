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

// Read returns the plain user/assistant turns for the conversation whose
// session key is sessionKey (the sha256(email::session_id)[:32] value), located
// under sessionsDir (<per-user-data>/workspace/sessions). Missing dir or file
// yields an empty slice, never an error (parity with server.js).
func Read(sessionsDir, sessionKey string) ([]Message, error) {
	basename := findMeta(sessionsDir, sessionKey)
	if basename == "" {
		return []Message{}, nil
	}
	return readMessages(sessionsDir, basename)
}

func findMeta(sessionsDir, sessionKey string) string {
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
