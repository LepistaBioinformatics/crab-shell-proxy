package memgraph

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// Lexical search: the strict substring match behind search_nodes, and the BM25
// ranker behind semantic_search.
//
// Upstream backs semantic_search with ModernColBERT embeddings via a Python
// sidecar, and degrades to keyword search whenever that sidecar is unavailable.
// Running that sidecar would mean the extra container and the ~500 MB model
// download this whole feature exists to avoid, so the degraded path is the only
// path here — and it is BM25 rather than substring matching, which is a real
// ranking rather than a filter. See context.md D-1; the tool description says so
// too, because a description promising semantic understanding the implementation
// cannot deliver would corrupt the agent's tool choice.

// BM25 parameters. These are the standard values and there is no tuning story
// behind them; a per-member graph of a few hundred entities does not have the
// scale for tuning to mean anything.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// Hit is one ranked entity. Score is normalised to (0, 1] against the best hit in
// the same query, which is what makes `threshold` comparable to upstream's
// "minimum similarity (0-1)" — raw BM25 scores are unbounded and a fixed
// threshold against them would mean nothing.
type Hit struct {
	EntityName string  `json:"entity_name"`
	Score      float64 `json:"score"`
}

// SearchResult is semantic_search's response, shaped like upstream's.
//
// SearchType is "lexical" rather than upstream's "semantic"/"keyword" so a caller
// can see which ranking produced the result instead of inferring it.
type SearchResult struct {
	Entities      []Entity   `json:"entities"`
	Relations     []Relation `json:"relations"`
	SearchResults []Hit      `json:"searchResults"`
	SearchType    string     `json:"searchType"`
}

// tokenize lowercases and splits on everything that is not a letter or digit, so
// "deepseek-chat" matches a query for "deepseek" and punctuation never becomes
// part of a term.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// visibleEntities returns the entities the browsing operations may see. Both
// search paths hide archived and merged entities; retrieval by name does not.
func visibleEntities(g *Graph) []Entity {
	out := make([]Entity, 0, len(g.Entities))
	for _, e := range g.Entities {
		if e.hidden(false, false) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// entityDocument is the text BM25 indexes for one entity: its name, its type and
// every observation. Searching the type as well is what lets "person" find every
// person without the agent having recorded that word as an observation.
func entityDocument(e Entity) []string {
	parts := make([]string, 0, len(e.Observations)+2)
	parts = append(parts, e.Name, e.EntityType)
	parts = append(parts, e.ObservationContents()...)
	return tokenize(strings.Join(parts, " "))
}

// Rank scores the visible entities against query with BM25 and returns at most k
// hits, ordered best first, dropping anything below threshold.
//
// threshold is applied to the NORMALISED score, after ranking, and k is applied
// last — matching upstream's order (filter by similarity, then take k).
//
// Rank does not mutate the graph.
func Rank(g *Graph, query string, k int, threshold float64) []Hit {
	terms := tokenize(query)
	entities := visibleEntities(g)
	if len(terms) == 0 || len(entities) == 0 {
		return []Hit{}
	}

	docs := make([][]string, len(entities))
	totalLen := 0
	for i, e := range entities {
		docs[i] = entityDocument(e)
		totalLen += len(docs[i])
	}
	if totalLen == 0 {
		return []Hit{}
	}
	avgLen := float64(totalLen) / float64(len(docs))

	// Term frequencies per document, and the document frequency per term.
	tf := make([]map[string]int, len(docs))
	df := make(map[string]int, len(terms))
	for i, doc := range docs {
		counts := make(map[string]int, len(doc))
		for _, tok := range doc {
			counts[tok]++
		}
		tf[i] = counts
		// Count each term at most once per document.
		seen := make(map[string]bool, len(terms))
		for _, term := range terms {
			if counts[term] > 0 && !seen[term] {
				df[term]++
				seen[term] = true
			}
		}
	}

	n := float64(len(docs))
	raw := make([]float64, len(docs))
	best := 0.0
	for i := range docs {
		score := 0.0
		docLen := float64(len(docs[i]))
		for _, term := range terms {
			f := float64(tf[i][term])
			if f == 0 {
				continue
			}
			nq := float64(df[term])
			// The standard BM25 IDF, in the 1+… form that stays positive even when a
			// term appears in every document (the classic form goes negative there and
			// would rank a universal term below no match at all).
			idf := math.Log(1 + (n-nq+0.5)/(nq+0.5))
			score += idf * (f * (bm25K1 + 1)) /
				(f + bm25K1*(1-bm25B+bm25B*docLen/avgLen))
		}
		raw[i] = score
		if score > best {
			best = score
		}
	}
	if best == 0 {
		return []Hit{}
	}

	hits := make([]Hit, 0, len(docs))
	for i, e := range entities {
		if raw[i] <= 0 {
			continue
		}
		hits = append(hits, Hit{EntityName: e.Name, Score: raw[i] / best})
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].Score > hits[b].Score })

	filtered := hits[:0:len(hits)]
	for _, h := range hits {
		if h.Score < threshold {
			continue
		}
		filtered = append(filtered, h)
	}
	if k > 0 && len(filtered) > k {
		filtered = filtered[:k]
	}
	return filtered
}

// SearchNodes is search_nodes: a strict, case-insensitive substring match over
// entity name, entity type and observation contents.
//
// It is a filter, not a ranking, and it is deliberately kept separate from Rank —
// an agent that knows the exact string it stored wants everything containing it,
// not the ten best guesses.
func (s *Store) SearchNodes(sc Scope, query string, maxObservations int) (FullGraph, error) {
	g, err := s.Load(sc)
	if err != nil {
		return FullGraph{}, err
	}
	needle := strings.ToLower(query)
	matched := make([]Entity, 0, len(g.Entities))
	for _, e := range visibleEntities(g) {
		if !entityContains(e, needle) {
			continue
		}
		if maxObservations >= 0 && len(e.Observations) > maxObservations {
			// Copy: truncating in place would alias the loaded graph, and the loaded
			// graph is about to be marshalled into the tool result.
			trimmed := make([]Observation, maxObservations)
			copy(trimmed, e.Observations[:maxObservations])
			e.Observations = trimmed
		}
		matched = append(matched, e)
	}
	return FullGraph{Entities: matched, Relations: relationsAmong(nameSet(matched), g.Relations)}, nil
}

func entityContains(e Entity, lowerNeedle string) bool {
	if strings.Contains(strings.ToLower(e.Name), lowerNeedle) ||
		strings.Contains(strings.ToLower(e.EntityType), lowerNeedle) {
		return true
	}
	for _, o := range e.Observations {
		if strings.Contains(strings.ToLower(o.Content), lowerNeedle) {
			return true
		}
	}
	return false
}

// SemanticSearch is the semantic_search tool: BM25 ranking, returning whole
// entities in rank order with the relations among them.
//
// k <= 0 means the schema default of 10. The default is applied by the MCP SDK
// from the declared schema for tool calls, but the HTTP read surface reaches this
// directly, so the floor lives here too.
func (s *Store) SemanticSearch(sc Scope, query string, k int, threshold float64) (SearchResult, error) {
	if k <= 0 {
		k = 10
	}
	g, err := s.Load(sc)
	if err != nil {
		return SearchResult{}, err
	}
	hits := Rank(g, query, k, threshold)

	byName := make(map[string]Entity, len(g.Entities))
	for _, e := range g.Entities {
		byName[e.Name] = e
	}
	ranked := make([]Entity, 0, len(hits))
	for _, h := range hits {
		if e, ok := byName[h.EntityName]; ok {
			ranked = append(ranked, e)
		}
	}
	return SearchResult{
		Entities:      ranked,
		Relations:     relationsAmong(nameSet(ranked), g.Relations),
		SearchResults: hits,
		SearchType:    "lexical",
	}, nil
}
