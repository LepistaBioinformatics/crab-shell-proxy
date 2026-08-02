// Package memgraph implements the knowledge-graph memory that a picoclaw
// instance reaches over MCP and that the user interface reads back.
//
// It is a port of better-memory-mcp (github.com/sockeye44/better-memory-mcp)
// v0.8.0, deliberately faithful down to the on-disk format: one JSON object per
// line, entities first, each line carrying a "type" discriminator. Matching that
// exactly means an existing better-memory-mcp memory.json can be copied into a
// workspace and read without conversion.
//
// The package knows nothing about HTTP, MCP or Docker. Everything the tools do to
// a graph lives here, which is where the tests live too.
package memgraph

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Scope identifies whose graph this is. It mirrors docker.WorkspaceKey field for
// field, but is redeclared here so this package (and internal/mcptoken, which
// carries a Scope through the bearer token) does not depend on internal/docker.
//
// Role is part of the identity: a member's "alpha" graph and "beta" graph are
// different graphs, because they are different agents with different jobs.
type Scope struct {
	TenantID  string
	SubsAccID string
	Role      string
	UserAccID string
}

// Observation is one discrete fact about an entity.
//
// Confidence carries omitempty to match upstream's optional field: an
// observation loaded from a legacy record that never had a confidence is written
// back without one, rather than acquiring a 0 that would read as "certainly
// false".
type Observation struct {
	Content    string  `json:"content"`
	Timestamp  int64   `json:"timestamp"`
	Confidence float64 `json:"confidence,omitempty"`
	// SourceSessionID is the conversation this fact came out of, when the proxy
	// could attribute it unambiguously. Empty is NORMAL, not a defect: cron jobs,
	// the heartbeat and post-turn evolution all write with no chat turn open, two
	// concurrent turns on one workspace make attribution ambiguous, and every
	// observation written before this field existed has none. See
	// httpapi.turnRegistry.
	//
	// A divergence from upstream's format, and the only one: it is `omitempty`, so
	// a file with no provenance is byte-identical to what upstream would write, and
	// upstream's JSON.parse ignores the extra key on the way in.
	SourceSessionID string `json:"sourceSessionId,omitempty"`

	// legacy records that this observation arrived as a bare JSON string, so
	// normalize knows to stamp it. It is not serialized: once normalized and
	// written back, the observation is no longer legacy.
	legacy bool
}

// UnmarshalJSON accepts both observation forms upstream can have written:
// a bare string from the original memory-server format, or the full object.
//
// Upstream decides per ARRAY, by inspecting only the first element; this decides
// per ELEMENT. That is a superset — every file upstream reads correctly, this
// reads identically — and it also handles an array that got mixed by a partial
// migration, which upstream would silently leave half-raw.
func (o *Observation) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		*o = Observation{Content: s, legacy: true}
		return nil
	}
	// Alias the type to borrow the default object decoding without recursing back
	// into this method.
	type plain Observation
	var p plain
	if err := json.Unmarshal(trimmed, &p); err != nil {
		return err
	}
	*o = Observation(p)
	return nil
}

// Entity is one node of the graph.
//
// Field order is upstream's, because encoding/json emits fields in declaration
// order and the on-disk lines are meant to be comparable with upstream's output.
type Entity struct {
	Name         string        `json:"name"`
	EntityType   string        `json:"entityType"`
	Observations []Observation `json:"observations"`
	CreatedAt    int64         `json:"createdAt,omitempty"`
	Archived     bool          `json:"archived,omitempty"`
	Merged       bool          `json:"merged,omitempty"`
	MergedInto   string        `json:"mergedInto,omitempty"`
	MergedAt     int64         `json:"mergedAt,omitempty"`
	// SourceSessionID — see Observation.SourceSessionID. Empty is normal.
	SourceSessionID string `json:"sourceSessionId,omitempty"`
}

// Relation is a directed edge, stored in active voice ("alice authored paper").
type Relation struct {
	From         string `json:"from"`
	To           string `json:"to"`
	RelationType string `json:"relationType"`
	CreatedAt    int64  `json:"createdAt,omitempty"`
	// SourceSessionID — see Observation.SourceSessionID. Empty is normal.
	SourceSessionID string `json:"sourceSessionId,omitempty"`
}

// Graph is a whole knowledge graph. The zero value is a usable empty graph.
type Graph struct {
	Entities  []Entity
	Relations []Relation
}

// ObservationContents projects an entity's observations down to their text, which
// is what every search path and every summary needs.
func (e Entity) ObservationContents() []string {
	out := make([]string, len(e.Observations))
	for i, o := range e.Observations {
		out[i] = o.Content
	}
	return out
}

// hidden reports whether an entity is excluded from the queries that browse the
// graph (read_graph, search_nodes, semantic_search) rather than name entities
// explicitly. Retrieval BY NAME deliberately does not use this: asking for an
// entity you archived is how you inspect it.
func (e Entity) hidden(includeArchived, includeMerged bool) bool {
	if e.Archived && !includeArchived {
		return true
	}
	return e.Merged && !includeMerged
}

// normalizeObservations stamps legacy bare-string observations with a timestamp
// and full confidence. Observations that arrived as objects keep their own
// values, including a deliberate zero confidence.
func normalizeObservations(obs []Observation, now int64) []Observation {
	for i := range obs {
		if obs[i].legacy {
			obs[i].Timestamp = now
			obs[i].Confidence = 1.0
			obs[i].legacy = false
		}
	}
	return obs
}

// lineType is the discriminator every stored line carries. Decoding reads it
// first to decide which of the two shapes the rest of the line is.
type lineType struct {
	Type string `json:"type"`
}

// entityLine and relationLine put "type" ahead of the record's own fields, which
// is the order upstream's `JSON.stringify({ type: "entity", ...e })` produces.
// The embedded struct is inlined by encoding/json, so the field order is
// type, then the record's declaration order.
type entityLine struct {
	Type string `json:"type"`
	Entity
}

type relationLine struct {
	Type string `json:"type"`
	Relation
}

// maxLineBytes caps one JSONL line. A single entity with many observations is
// still small; this exists so a corrupted file cannot make the scanner allocate
// without bound.
const maxLineBytes = 4 << 20

// decodeJSONL parses upstream's storage format.
//
// now stamps records that predate the fields carrying their own timestamps: an
// entity with no createdAt, and observations stored as bare strings. Lines whose
// type is neither "entity" nor "relation" are skipped rather than rejected —
// upstream's reduce ignores them, and a future record type should not make an
// existing memory unreadable.
func decodeJSONL(r io.Reader, now int64) (*Graph, error) {
	g := &Graph{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for line := 1; sc.Scan(); line++ {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var lt lineType
		if err := json.Unmarshal(raw, &lt); err != nil {
			return nil, fmt.Errorf("memory graph line %d: %w", line, err)
		}
		switch lt.Type {
		case "entity":
			var el entityLine
			if err := json.Unmarshal(raw, &el); err != nil {
				return nil, fmt.Errorf("memory graph line %d: %w", line, err)
			}
			e := el.Entity
			if e.CreatedAt == 0 {
				e.CreatedAt = now
			}
			e.Observations = normalizeObservations(e.Observations, now)
			g.Entities = append(g.Entities, e)
		case "relation":
			var rl relationLine
			if err := json.Unmarshal(raw, &rl); err != nil {
				return nil, fmt.Errorf("memory graph line %d: %w", line, err)
			}
			rel := rl.Relation
			if rel.CreatedAt == 0 {
				rel.CreatedAt = now
			}
			g.Relations = append(g.Relations, rel)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read memory graph: %w", err)
	}
	return g, nil
}

// encodeJSONL writes the graph back in upstream's format: every entity, then
// every relation, one compact JSON object per line separated by "\n".
//
// Upstream joins with "\n" and so emits no trailing newline. We match that,
// because AC-5 compares a round-tripped fixture byte for byte and a stray
// newline would be a silent format divergence.
func encodeJSONL(g *Graph) ([]byte, error) {
	var lines []string
	for _, e := range g.Entities {
		b, err := json.Marshal(entityLine{Type: "entity", Entity: e})
		if err != nil {
			return nil, err
		}
		lines = append(lines, string(b))
	}
	for _, r := range g.Relations {
		b, err := json.Marshal(relationLine{Type: "relation", Relation: r})
		if err != nil {
			return nil, err
		}
		lines = append(lines, string(b))
	}
	return []byte(strings.Join(lines, "\n")), nil
}
