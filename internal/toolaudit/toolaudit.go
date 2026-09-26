// Package toolaudit reads the per-tool-call records the ganglion writes.
//
// The harness keeps one record per call under `<workspace>/.tool-audit/<conv>/`,
// holding the FULL command and the output. Neither is recoverable from anything
// else it writes: the transcript records what the member saw, and they never saw
// a tool result, while the arguments that do reach the transcript are a display
// string capped at 200 runes.
//
// This package only reads. The harness owns the format, the lifetime and the
// compression; the proxy's job is to hand one record to the member who owns it.
package toolaudit

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Record is the on-disk shape, and is the harness's
// `internal/adapter/store/toolaudit.Record` field for field.
//
// EVERY TAG HERE IS PART OF A CONTRACT NO COMPILER CHECKS. A rename on either
// side does not fail a build or a test by itself -- it decodes to a zero value
// and serves an empty sheet. `tool_audit_contract_test.go` reads the harness's
// source and compares, which is what turns drift back into a failure.
type Record struct {
	ID        string `json:"id"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// Output is absent until the call returned, and absent forever if the turn
	// died inside it. That absence is information, and the client renders it as
	// such rather than as an empty string.
	Output    string `json:"output,omitempty"`
	Status    string `json:"status,omitempty"`
	Detail    string `json:"detail,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
}

// ErrNotRecorded is an absent record, which is ORDINARY rather than an error
// condition: every call made before the harness minted these has none, every
// picoclaw call has none, and a turn whose disk was full has none. The handler
// turns it into a clean empty answer, not a 500.
var ErrNotRecorded = errors.New("tool call not recorded")

// maxRecord caps what is read off disk, compressed or not.
//
// The harness caps a shell result at 64 KiB before the loop ever sees it, so a
// record is bounded in practice -- but "in practice" is the harness's promise
// about its own tools, not a fact about a file on a disk the agent can write to.
// A decompression bomb is a file the agent could create; this is what stops one
// becoming the proxy's memory.
const maxRecord = 4 << 20

// Read returns one record, decompressing when the sweep has been past.
//
// `.json` FIRST, THEN `.json.gz`, and the order is what makes a sweep racing a
// read harmless: the sweep writes the compressed copy and only then unlinks the
// original, so a reader that missed the original finds the copy. The reverse
// order would have a window in which neither stat succeeds.
func Read(dir, id string) (Record, error) {
	base := filepath.Join(dir, Safe(id))

	if raw, err := readCapped(base+".json", false); err == nil {
		return decode(raw)
	} else if !os.IsNotExist(err) {
		return Record{}, err
	}

	raw, err := readCapped(base+".json.gz", true)
	if err != nil {
		if os.IsNotExist(err) {
			return Record{}, ErrNotRecorded
		}
		return Record{}, err
	}
	return decode(raw)
}

func decode(raw []byte) (Record, error) {
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Record{}, err
	}
	return rec, nil
}

func readCapped(path string, compressed bool) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var r io.Reader = f
	if compressed {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = zr
	}
	return io.ReadAll(io.LimitReader(r, maxRecord))
}

// Safe is the harness's own rule for a path segment, applied again here.
//
// APPLIED AGAIN BECAUSE THE ID NOW ARRIVES FROM A URL. The harness mints these
// and sanitises them on the way in, which makes them safe where they were
// written; none of that is true of a string a caller typed. A reader that
// trusted it is one join away from serving any file under the data root.
func Safe(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "_"
	}
	return string(out)
}
