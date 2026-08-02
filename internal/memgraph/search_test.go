package memgraph

import (
	"reflect"
	"testing"
)

func searchFixture(t *testing.T) (*Store, Scope) {
	t.Helper()
	s, sc := newTestStore(t), testScope()
	seed(t, s, sc, Graph{
		Entities: []Entity{
			{Name: "alice", EntityType: "person", CreatedAt: 1,
				Observations: obs("writes Rust daily", "common word here")},
			{Name: "bob", EntityType: "person", CreatedAt: 1,
				Observations: obs("reviews specs", "common word here")},
			{Name: "ledger", EntityType: "system", CreatedAt: 1,
				Observations: obs("written in Rust", "common word here")},
			{Name: "archived-rust-note", EntityType: "note", CreatedAt: 1, Archived: true,
				Observations: obs("Rust Rust Rust")},
			{Name: "merged-rust-note", EntityType: "note", CreatedAt: 1, Merged: true,
				Observations: obs("Rust Rust Rust")},
		},
		Relations: []Relation{
			{From: "alice", To: "ledger", RelationType: "maintains", CreatedAt: 1},
			{From: "bob", To: "alice", RelationType: "reviews for", CreatedAt: 1},
		},
	})
	return s, sc
}

// --- search_nodes (FR-3.13) ---

func TestSearchNodesMatchesNameTypeAndObservations(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"by name", "ledg", []string{"ledger"}},
		{"by name case-insensitively", "ALICE", []string{"alice"}},
		{"by entity type", "system", []string{"ledger"}},
		{"by observation content", "reviews specs", []string{"bob"}},
		{"across several entities", "rust", []string{"alice", "ledger"}},
		{"no match", "zzz", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s, sc := searchFixture(t)
			got, err := s.SearchNodes(sc, c.query, 10)
			if err != nil {
				t.Fatalf("SearchNodes: %v", err)
			}
			if len(got.Entities) != len(c.want) {
				t.Fatalf("matched %v, want %v", names(got.Entities), c.want)
			}
			for i, w := range c.want {
				if got.Entities[i].Name != w {
					t.Errorf("match %d = %q, want %q", i, got.Entities[i].Name, w)
				}
			}
		})
	}
}

func TestSearchNodesHidesArchivedAndMerged(t *testing.T) {
	t.Parallel()
	s, sc := searchFixture(t)
	got, err := s.SearchNodes(sc, "rust", 10)
	if err != nil {
		t.Fatalf("SearchNodes: %v", err)
	}
	for _, e := range got.Entities {
		if e.Archived || e.Merged {
			t.Errorf("search returned %q, which is archived/merged", e.Name)
		}
	}
}

func TestSearchNodesTruncatesObservationsWithoutMutatingTheGraph(t *testing.T) {
	t.Parallel()
	s, sc := searchFixture(t)
	got, err := s.SearchNodes(sc, "alice", 1)
	if err != nil {
		t.Fatalf("SearchNodes: %v", err)
	}
	if len(got.Entities[0].Observations) != 1 {
		t.Errorf("observations = %d, want 1 (truncated to maxObservations)", len(got.Entities[0].Observations))
	}
	g, _ := s.Load(sc)
	if len(g.findEntity("alice").Observations) != 2 {
		t.Error("truncation leaked into the stored graph")
	}
}

func TestSearchNodesFiltersRelationsToBothEndpointsPresent(t *testing.T) {
	t.Parallel()
	s, sc := searchFixture(t)
	// "rust" matches alice and ledger, so alice→ledger survives and bob→alice does not.
	got, err := s.SearchNodes(sc, "rust", 10)
	if err != nil {
		t.Fatalf("SearchNodes: %v", err)
	}
	if len(got.Relations) != 1 || got.Relations[0].RelationType != "maintains" {
		t.Errorf("relations = %+v, want only alice→ledger", got.Relations)
	}
}

// --- BM25 (D-1, NFR-4) ---

func TestRankOrdersByRelevance(t *testing.T) {
	t.Parallel()
	s, sc := searchFixture(t)
	g, _ := s.Load(sc)
	hits := Rank(g, "rust", 10, 0)
	if len(hits) != 2 {
		t.Fatalf("hits = %+v, want alice and ledger", hits)
	}
	if hits[0].Score < hits[1].Score {
		t.Errorf("hits are not ordered best first: %+v", hits)
	}
	if hits[0].Score != 1.0 {
		t.Errorf("best score = %v, want it normalised to 1.0", hits[0].Score)
	}
	for _, h := range hits {
		if h.Score <= 0 || h.Score > 1 {
			t.Errorf("score %v out of the (0,1] range normalisation promises", h.Score)
		}
	}
}

// A term every entity carries must not outrank a rare one. This is the whole
// reason to use BM25 rather than counting matches.
func TestRankPrefersARareTermOverAUniversalOne(t *testing.T) {
	t.Parallel()
	s, sc := searchFixture(t)
	g, _ := s.Load(sc)

	rare := Rank(g, "ledger", 10, 0)
	if len(rare) == 0 || rare[0].EntityName != "ledger" {
		t.Fatalf("query 'ledger' ranked %+v, want ledger first", rare)
	}

	// "common" appears in all three visible entities, so it separates them barely;
	// a query mixing it with a rare term must still put the rare match first.
	mixed := Rank(g, "common ledger", 10, 0)
	if len(mixed) == 0 || mixed[0].EntityName != "ledger" {
		t.Errorf("query 'common ledger' ranked %+v, want ledger first — IDF is not discounting the universal term", mixed)
	}
}

func TestRankAppliesThresholdThenK(t *testing.T) {
	t.Parallel()
	s, sc := searchFixture(t)
	g, _ := s.Load(sc)

	all := Rank(g, "common", 10, 0)
	if len(all) != 3 {
		t.Fatalf("hits = %+v, want all three visible entities", all)
	}

	limited := Rank(g, "common", 1, 0)
	if len(limited) != 1 {
		t.Errorf("k=1 returned %d hits", len(limited))
	}
	if limited[0].EntityName != all[0].EntityName {
		t.Errorf("k=1 returned %q, want the top hit %q", limited[0].EntityName, all[0].EntityName)
	}

	// Nothing can beat the normalised best, so a threshold above 1 empties the set.
	if got := Rank(g, "common", 10, 1.01); len(got) != 0 {
		t.Errorf("threshold 1.01 returned %+v, want nothing", got)
	}

	// A threshold of exactly 1 keeps every maximal-scoring hit — which may be more
	// than one. "rust" appears once in both alice and ledger, in documents of
	// similar length, so they genuinely tie at 1.0 and dropping one would be
	// arbitrary. A query with a strict winner keeps exactly that winner.
	if got := Rank(g, "rust", 10, 1.0); len(got) != 2 {
		t.Errorf("threshold 1.0 on a tie returned %d hits, want both tied hits", len(got))
	}
	got := Rank(g, "common ledger", 10, 1.0)
	if len(got) != 1 || got[0].EntityName != "ledger" {
		t.Errorf("threshold 1.0 with a strict winner returned %+v, want only ledger", got)
	}
}

func TestRankHidesArchivedAndMerged(t *testing.T) {
	t.Parallel()
	s, sc := searchFixture(t)
	g, _ := s.Load(sc)
	// The archived and merged notes say "Rust" three times each; if they were
	// indexed they would dominate.
	for _, h := range Rank(g, "rust", 10, 0) {
		if h.EntityName == "archived-rust-note" || h.EntityName == "merged-rust-note" {
			t.Errorf("Rank returned %q, which is archived/merged", h.EntityName)
		}
	}
}

func TestRankEdgeCases(t *testing.T) {
	t.Parallel()
	s, sc := searchFixture(t)
	g, _ := s.Load(sc)
	cases := []struct {
		name  string
		graph *Graph
		query string
	}{
		{"empty graph", &Graph{}, "anything"},
		{"empty query", g, ""},
		{"punctuation-only query", g, "!!! ??? ---"},
		{"no match", g, "zzzznothing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := Rank(c.graph, c.query, 10, 0)
			if got == nil {
				t.Error("Rank returned nil; want an empty slice so it marshals as []")
			}
			if len(got) != 0 {
				t.Errorf("Rank = %+v, want no hits", got)
			}
		})
	}
}

func TestRankDoesNotMutateTheGraph(t *testing.T) {
	t.Parallel()
	s, sc := searchFixture(t)
	g, _ := s.Load(sc)
	before, err := encodeJSONL(g)
	if err != nil {
		t.Fatalf("encodeJSONL: %v", err)
	}
	_ = Rank(g, "rust common ledger", 10, 0)
	after, err := encodeJSONL(g)
	if err != nil {
		t.Fatalf("encodeJSONL: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Error("Rank mutated the graph")
	}
}

func TestTokenizeSplitsOnPunctuationAndLowercases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []string
	}{
		{"DeepSeek-Chat", []string{"deepseek", "chat"}},
		{"a.b_c", []string{"a", "b", "c"}},
		{"gpt5.4", []string{"gpt5", "4"}},
		{"   ", nil},
	}
	for _, c := range cases {
		got := tokenize(c.in)
		if len(got) != len(c.want) {
			t.Errorf("tokenize(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("tokenize(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

// --- semantic_search (D-1) ---

func TestSemanticSearchReportsLexicalRankingAndRankedEntities(t *testing.T) {
	t.Parallel()
	s, sc := searchFixture(t)
	res, err := s.SemanticSearch(sc, "rust", 10, 0)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	if res.SearchType != "lexical" {
		t.Errorf("SearchType = %q, want %q — the response must not imply embeddings", res.SearchType, "lexical")
	}
	if len(res.Entities) != 2 {
		t.Fatalf("entities = %v, want alice and ledger", names(res.Entities))
	}
	if len(res.SearchResults) != len(res.Entities) {
		t.Errorf("searchResults = %d but entities = %d; they must correspond",
			len(res.SearchResults), len(res.Entities))
	}
	if res.Entities[0].Name != res.SearchResults[0].EntityName {
		t.Errorf("entities are not in rank order: %v vs %+v", names(res.Entities), res.SearchResults)
	}
	if len(res.Entities[0].Observations) == 0 {
		t.Error("ranked entities must carry full detail")
	}
	if len(res.Relations) != 1 {
		t.Errorf("relations = %+v, want the one among the matched entities", res.Relations)
	}
}

func TestSemanticSearchDefaultsKToTen(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	entities := make([]Entity, 0, 15)
	for i := 0; i < 15; i++ {
		entities = append(entities, Entity{
			Name: string(rune('a' + i)), EntityType: "t", CreatedAt: 1,
			Observations: obs("shared term"),
		})
	}
	seed(t, s, sc, Graph{Entities: entities})

	for _, k := range []int{0, -1} {
		res, err := s.SemanticSearch(sc, "shared", k, 0)
		if err != nil {
			t.Fatalf("SemanticSearch(k=%d): %v", k, err)
		}
		if len(res.Entities) != 10 {
			t.Errorf("k=%d returned %d entities, want the schema default of 10", k, len(res.Entities))
		}
	}
}

func TestSemanticSearchOnEmptyGraphReturnsEmptySlices(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	res, err := s.SemanticSearch(sc, "anything", 10, 0)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	if res.Entities == nil || res.Relations == nil || res.SearchResults == nil {
		t.Errorf("result = %+v; slices must be non-nil so they marshal as []", res)
	}
}
