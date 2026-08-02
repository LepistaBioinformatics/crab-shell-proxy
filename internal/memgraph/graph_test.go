package memgraph

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testNow is the injected clock every test uses, so a stamped timestamp is
// recognisable rather than "whatever the machine said".
const testNow int64 = 1_800_000_000_000

func TestDecodeJSONLReadsUpstreamFormat(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "upstream-modern.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	g, err := decodeJSONL(bytes.NewReader(raw), testNow)
	if err != nil {
		t.Fatalf("decodeJSONL: %v", err)
	}
	if len(g.Entities) != 3 {
		t.Fatalf("entities = %d, want 3", len(g.Entities))
	}
	if len(g.Relations) != 1 {
		t.Fatalf("relations = %d, want 1", len(g.Relations))
	}
	if g.Entities[0].Name != "alice" || g.Entities[0].EntityType != "person" {
		t.Errorf("first entity = %+v, want alice/person", g.Entities[0])
	}
	if got := g.Entities[0].Observations[0].Confidence; got != 0.9 {
		t.Errorf("confidence = %v, want 0.9 (a stored value must not be overwritten)", got)
	}
	if !g.Entities[2].Archived {
		t.Errorf("retired-note.Archived = false, want true")
	}
	if g.Relations[0].RelationType != "works with" {
		t.Errorf("relationType = %q, want %q", g.Relations[0].RelationType, "works with")
	}
}

// A file whose records already carry every field must survive a decode/encode
// cycle byte for byte. Anything less means our on-disk format has quietly
// diverged from upstream's, which is the whole of FR-7.2.
func TestEncodeJSONLRoundTripsUpstreamFileByteForByte(t *testing.T) {
	t.Parallel()
	path := filepath.Join("testdata", "upstream-modern.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	g, err := decodeJSONL(bytes.NewReader(raw), testNow)
	if err != nil {
		t.Fatalf("decodeJSONL: %v", err)
	}
	out, err := encodeJSONL(g)
	if err != nil {
		t.Fatalf("encodeJSONL: %v", err)
	}
	if !bytes.Equal(raw, out) {
		t.Errorf("round trip differs:\n on disk: %s\n encoded: %s", raw, out)
	}
}

func TestEncodeJSONLPutsEntitiesBeforeRelationsWithTypeFirst(t *testing.T) {
	t.Parallel()
	g := &Graph{
		Entities:  []Entity{{Name: "a", EntityType: "t", Observations: []Observation{}}},
		Relations: []Relation{{From: "a", To: "a", RelationType: "self"}},
	}
	out, err := encodeJSONL(g)
	if err != nil {
		t.Fatalf("encodeJSONL: %v", err)
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}
	if !strings.HasPrefix(lines[0], `{"type":"entity",`) {
		t.Errorf("entity line = %s, want it to lead with the type discriminator", lines[0])
	}
	if !strings.HasPrefix(lines[1], `{"type":"relation",`) {
		t.Errorf("relation line = %s, want it to lead with the type discriminator", lines[1])
	}
	if strings.HasSuffix(string(out), "\n") {
		t.Errorf("output ends with a newline; upstream joins with \\n and emits none")
	}
}

func TestDecodeJSONLNormalizesLegacyRecords(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "upstream-legacy.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	g, err := decodeJSONL(bytes.NewReader(raw), testNow)
	if err != nil {
		t.Fatalf("decodeJSONL: %v", err)
	}
	if len(g.Entities) != 2 {
		t.Fatalf("entities = %d, want 2 (the unknown record type must be skipped, not counted)", len(g.Entities))
	}
	carol := g.Entities[0]
	if carol.CreatedAt != testNow {
		t.Errorf("carol.CreatedAt = %d, want the load time %d", carol.CreatedAt, testNow)
	}
	if len(carol.Observations) != 2 {
		t.Fatalf("carol observations = %d, want 2", len(carol.Observations))
	}
	for i, o := range carol.Observations {
		if o.Timestamp != testNow {
			t.Errorf("observation %d timestamp = %d, want %d", i, o.Timestamp, testNow)
		}
		if o.Confidence != 1.0 {
			t.Errorf("observation %d confidence = %v, want 1.0", i, o.Confidence)
		}
	}
	if got := carol.Observations[0].Content; got != "joined in 2019" {
		t.Errorf("observation 0 content = %q, want %q", got, "joined in 2019")
	}

	// An observation that arrived as an object keeps its own timestamp and, having
	// carried no confidence, must not acquire one.
	ledger := g.Entities[1]
	if ledger.Observations[0].Timestamp != 1_600_000_000_000 {
		t.Errorf("ledger observation timestamp = %d, want its stored 1600000000000",
			ledger.Observations[0].Timestamp)
	}
	if ledger.Observations[0].Confidence != 0 {
		t.Errorf("ledger observation confidence = %v, want 0 (absent stays absent)",
			ledger.Observations[0].Confidence)
	}
	if ledger.CreatedAt != testNow {
		t.Errorf("ledger.CreatedAt = %d, want the load time", ledger.CreatedAt)
	}
	if g.Relations[0].CreatedAt != testNow {
		t.Errorf("relation CreatedAt = %d, want the load time", g.Relations[0].CreatedAt)
	}
}

// A normalized legacy observation must serialize as a full object, so the next
// load needs no normalization at all.
func TestNormalizedLegacyObservationSerializesAsObject(t *testing.T) {
	t.Parallel()
	g, err := decodeJSONL(strings.NewReader(
		`{"type":"entity","name":"c","entityType":"person","observations":["bare"]}`), testNow)
	if err != nil {
		t.Fatalf("decodeJSONL: %v", err)
	}
	out, err := encodeJSONL(g)
	if err != nil {
		t.Fatalf("encodeJSONL: %v", err)
	}
	want := `"observations":[{"content":"bare","timestamp":1800000000000,"confidence":1}]`
	if !strings.Contains(string(out), want) {
		t.Errorf("encoded = %s\nwant it to contain %s", out, want)
	}
	// And the second pass is a no-op.
	g2, err := decodeJSONL(bytes.NewReader(out), testNow+1)
	if err != nil {
		t.Fatalf("second decodeJSONL: %v", err)
	}
	if g2.Entities[0].Observations[0].Timestamp != testNow {
		t.Errorf("re-load restamped an already-normalized observation")
	}
}

func TestObservationUnmarshalAcceptsBothForms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		in         string
		wantText   string
		wantLegacy bool
	}{
		{"bare string", `"hello"`, "hello", true},
		{"padded bare string", `  "hello"  `, "hello", true},
		{"object", `{"content":"hello","timestamp":7,"confidence":0.5}`, "hello", false},
		{"object without confidence", `{"content":"hello","timestamp":7}`, "hello", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var o Observation
			if err := json.Unmarshal([]byte(c.in), &o); err != nil {
				t.Fatalf("Unmarshal(%s): %v", c.in, err)
			}
			if o.Content != c.wantText {
				t.Errorf("Content = %q, want %q", o.Content, c.wantText)
			}
			if o.legacy != c.wantLegacy {
				t.Errorf("legacy = %v, want %v", o.legacy, c.wantLegacy)
			}
		})
	}
}

func TestDecodeJSONLRejectsAMalformedLineWithItsNumber(t *testing.T) {
	t.Parallel()
	_, err := decodeJSONL(strings.NewReader(
		`{"type":"entity","name":"a","entityType":"t","observations":[]}`+"\n"+`{not json`), testNow)
	if err == nil {
		t.Fatal("decodeJSONL accepted a malformed line, want an error")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error = %v, want it to name line 2", err)
	}
}

func TestGraphZeroValueIsAnEmptyGraph(t *testing.T) {
	t.Parallel()
	g, err := decodeJSONL(strings.NewReader(""), testNow)
	if err != nil {
		t.Fatalf("decodeJSONL(empty): %v", err)
	}
	if len(g.Entities) != 0 || len(g.Relations) != 0 {
		t.Errorf("empty input decoded to %+v, want an empty graph", g)
	}
	out, err := encodeJSONL(&Graph{})
	if err != nil {
		t.Fatalf("encodeJSONL(zero): %v", err)
	}
	if len(out) != 0 {
		t.Errorf("encoded zero graph = %q, want empty", out)
	}
}

func TestEntityHiddenGovernsBrowsingNotNaming(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                           string
		e                              Entity
		includeArchived, includeMerged bool
		want                           bool
	}{
		{"plain", Entity{}, false, false, false},
		{"archived hidden by default", Entity{Archived: true}, false, false, true},
		{"archived included on request", Entity{Archived: true}, true, false, false},
		{"merged hidden by default", Entity{Merged: true}, false, false, true},
		{"merged included on request", Entity{Merged: true}, false, true, false},
		{"archived not unhidden by includeMerged", Entity{Archived: true}, false, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := c.e.hidden(c.includeArchived, c.includeMerged); got != c.want {
				t.Errorf("hidden(%v,%v) = %v, want %v", c.includeArchived, c.includeMerged, got, c.want)
			}
		})
	}
}

func TestObservationContentsProjectsText(t *testing.T) {
	t.Parallel()
	e := Entity{Observations: []Observation{{Content: "a"}, {Content: "b"}}}
	got := e.ObservationContents()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("ObservationContents() = %v, want [a b]", got)
	}
}
