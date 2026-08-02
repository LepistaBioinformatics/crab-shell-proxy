package memgraph

import (
	"errors"
	"strings"
	"testing"
)

// seed puts a small graph in place without going through the tool operations, so
// a test's arrangement cannot be broken by a bug in the thing under test.
func seed(t *testing.T, s *Store, sc Scope, g Graph) {
	t.Helper()
	if err := s.Update(sc, func(cur *Graph) error {
		*cur = g
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func obs(contents ...string) []Observation {
	out := make([]Observation, len(contents))
	for i, c := range contents {
		out[i] = Observation{Content: c, Timestamp: testNow, Confidence: 1}
	}
	return out
}

// --- create_entities (FR-3.1) ---

func TestCreateEntitiesSkipsExistingAndNeverOverwrites(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	seed(t, s, sc, Graph{Entities: []Entity{
		{Name: "alice", EntityType: "person", Observations: obs("knows go"), CreatedAt: 1},
	}})

	res, err := s.CreateEntities(sc, []Entity{
		{Name: "alice", EntityType: "robot", Observations: obs("wiped?")},
		{Name: "bob", EntityType: "person", Observations: obs("new")},
	}, "")
	if err != nil {
		t.Fatalf("CreateEntities: %v", err)
	}
	if len(res.Created) != 1 || res.Created[0] != "bob" {
		t.Errorf("Created = %v, want [bob]", res.Created)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "alice" {
		t.Errorf("Skipped = %v, want [alice]", res.Skipped)
	}

	g, _ := s.Load(sc)
	alice := g.findEntity("alice")
	if alice.EntityType != "person" {
		t.Errorf("alice.EntityType = %q; a skipped entity must not be rewritten", alice.EntityType)
	}
	if len(alice.Observations) != 1 || alice.Observations[0].Content != "knows go" {
		t.Errorf("alice observations = %+v; a skipped entity must keep what it accumulated", alice.Observations)
	}
}

func TestCreateEntitiesStampsTimestampsAndConfidence(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	if _, err := s.CreateEntities(sc, []Entity{
		{Name: "a", EntityType: "t", Observations: []Observation{{Content: "bare"}}},
	}, ""); err != nil {
		t.Fatalf("CreateEntities: %v", err)
	}
	g, _ := s.Load(sc)
	e := g.findEntity("a")
	if e.CreatedAt != testNow {
		t.Errorf("CreatedAt = %d, want %d", e.CreatedAt, testNow)
	}
	if e.Observations[0].Timestamp != testNow || e.Observations[0].Confidence != 1.0 {
		t.Errorf("observation = %+v, want it stamped with now and confidence 1", e.Observations[0])
	}
}

func TestCreateEntitiesReturnsEmptySlicesNotNil(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	res, err := s.CreateEntities(sc, nil, "")
	if err != nil {
		t.Fatalf("CreateEntities: %v", err)
	}
	if res.Created == nil || res.Skipped == nil {
		t.Errorf("result = %+v; slices must be non-nil so they marshal as [] like upstream", res)
	}
}

// --- create_relations (FR-3.2) ---

func TestCreateRelationsSkipsExactDuplicates(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	seed(t, s, sc, Graph{Relations: []Relation{
		{From: "a", To: "b", RelationType: "knows", CreatedAt: 1},
	}})
	res, err := s.CreateRelations(sc, []Relation{
		{From: "a", To: "b", RelationType: "knows"},   // duplicate
		{From: "a", To: "b", RelationType: "manages"}, // different type, new
		{From: "b", To: "a", RelationType: "knows"},   // reversed, new
		{From: "a", To: "b", RelationType: "manages"}, // duplicate of one added in this call
	}, "")
	if err != nil {
		t.Fatalf("CreateRelations: %v", err)
	}
	if res.Created != 2 || res.Skipped != 2 {
		t.Errorf("result = %+v, want created 2 / skipped 2", res)
	}
	g, _ := s.Load(sc)
	if len(g.Relations) != 3 {
		t.Errorf("relations = %d, want 3", len(g.Relations))
	}
}

// --- add_observations (FR-3.3) ---

func TestAddObservationsErrorsOnMissingEntityAndWritesNothing(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	seed(t, s, sc, Graph{Entities: []Entity{
		{Name: "real", EntityType: "t", Observations: obs("one"), CreatedAt: 1},
	}})
	_, err := s.AddObservations(sc, []ObservationInput{
		{EntityName: "real", Contents: []string{"two"}},
		{EntityName: "ghost", Contents: []string{"three"}},
	}, "")
	if err == nil {
		t.Fatal("AddObservations on a missing entity returned nil, want an error (FR-3.3)")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error = %v, want it to name the missing entity", err)
	}
	g, _ := s.Load(sc)
	if len(g.findEntity("real").Observations) != 1 {
		t.Errorf("a payload naming a missing entity half-applied; want no write at all")
	}
}

func TestAddObservationsDedupesAndDefaultsConfidence(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	seed(t, s, sc, Graph{Entities: []Entity{
		{Name: "a", EntityType: "t", Observations: obs("known"), CreatedAt: 1},
	}})
	res, err := s.AddObservations(sc, []ObservationInput{{
		EntityName: "a",
		Contents:   []string{"known", "fresh", "also fresh"},
		Confidence: []float64{0.1, 0.5}, // shorter than Contents on purpose
	}}, "")
	if err != nil {
		t.Fatalf("AddObservations: %v", err)
	}
	if len(res) != 1 || res[0].Added != 2 || res[0].Skipped != 1 {
		t.Errorf("result = %+v, want added 2 / skipped 1", res)
	}
	g, _ := s.Load(sc)
	got := g.findEntity("a").Observations
	if len(got) != 3 {
		t.Fatalf("observations = %d, want 3", len(got))
	}
	// "known" was skipped, so index 0 of Confidence lines up with "fresh".
	if got[1].Content != "fresh" || got[1].Confidence != 0.1 {
		t.Errorf("second observation = %+v, want fresh at confidence 0.1", got[1])
	}
	if got[2].Content != "also fresh" || got[2].Confidence != 0.5 {
		t.Errorf("third observation = %+v, want 'also fresh' at confidence 0.5", got[2])
	}
}

func TestAddObservationsDefaultsConfidenceToOneWhenAbsent(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	seed(t, s, sc, Graph{Entities: []Entity{{Name: "a", EntityType: "t", CreatedAt: 1}}})
	if _, err := s.AddObservations(sc, []ObservationInput{
		{EntityName: "a", Contents: []string{"x", "y"}},
	}, ""); err != nil {
		t.Fatalf("AddObservations: %v", err)
	}
	g, _ := s.Load(sc)
	for i, o := range g.findEntity("a").Observations {
		if o.Confidence != 1.0 {
			t.Errorf("observation %d confidence = %v, want 1.0", i, o.Confidence)
		}
		if o.Timestamp != testNow {
			t.Errorf("observation %d timestamp = %d, want %d", i, o.Timestamp, testNow)
		}
	}
}

// --- delete_entities (FR-3.4) ---

func TestDeleteEntitiesCascadesRelationsOnEitherEndpoint(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	seed(t, s, sc, Graph{
		Entities: []Entity{
			{Name: "a", EntityType: "t", CreatedAt: 1},
			{Name: "b", EntityType: "t", CreatedAt: 1},
			{Name: "c", EntityType: "t", CreatedAt: 1},
		},
		Relations: []Relation{
			{From: "a", To: "b", RelationType: "out", CreatedAt: 1},
			{From: "c", To: "a", RelationType: "in", CreatedAt: 1},
			{From: "b", To: "c", RelationType: "untouched", CreatedAt: 1},
		},
	})
	res, err := s.DeleteEntities(sc, []string{"a", "nonexistent"})
	if err != nil {
		t.Fatalf("DeleteEntities: %v", err)
	}
	if res.Deleted != 1 || res.CascadedRelations != 2 {
		t.Errorf("result = %+v, want deleted 1 / cascadedRelations 2", res)
	}
	g, _ := s.Load(sc)
	if len(g.Entities) != 2 {
		t.Errorf("entities = %d, want 2", len(g.Entities))
	}
	if len(g.Relations) != 1 || g.Relations[0].RelationType != "untouched" {
		t.Errorf("relations = %+v, want only the one not touching 'a'", g.Relations)
	}
}

// --- delete_observations (FR-3.5) ---

// The asymmetry with AddObservations is upstream's and deliberate. Asserted
// explicitly so nobody "makes these consistent" without failing a test that
// explains why.
func TestDeleteObservationsOnMissingEntityReportsZeroWithoutError(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	seed(t, s, sc, Graph{Entities: []Entity{
		{Name: "a", EntityType: "t", Observations: obs("keep", "drop"), CreatedAt: 1},
	}})
	res, err := s.DeleteObservations(sc, []ObservationInput{
		{EntityName: "a", Contents: []string{"drop", "never existed"}},
		{EntityName: "ghost", Contents: []string{"anything"}},
	})
	if err != nil {
		t.Fatalf("DeleteObservations returned %v; a missing entity must NOT be an error here", err)
	}
	if len(res) != 2 {
		t.Fatalf("results = %d, want 2", len(res))
	}
	if res[0].Deleted != 1 {
		t.Errorf("entity a deleted = %d, want 1", res[0].Deleted)
	}
	if res[1].EntityName != "ghost" || res[1].Deleted != 0 {
		t.Errorf("ghost result = %+v, want {ghost 0}", res[1])
	}
	g, _ := s.Load(sc)
	got := g.findEntity("a").Observations
	if len(got) != 1 || got[0].Content != "keep" {
		t.Errorf("observations = %+v, want only 'keep'", got)
	}
}

// --- delete_relations (FR-3.11) ---

func TestDeleteRelationsRemovesOnlyExactTriples(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	seed(t, s, sc, Graph{Relations: []Relation{
		{From: "a", To: "b", RelationType: "knows", CreatedAt: 1},
		{From: "a", To: "b", RelationType: "manages", CreatedAt: 1},
		{From: "b", To: "a", RelationType: "knows", CreatedAt: 1},
	}})
	res, err := s.DeleteRelations(sc, []Relation{
		{From: "a", To: "b", RelationType: "knows"},
		{From: "x", To: "y", RelationType: "absent"}, // not an error
	})
	if err != nil {
		t.Fatalf("DeleteRelations: %v", err)
	}
	if res.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1", res.Deleted)
	}
	g, _ := s.Load(sc)
	if len(g.Relations) != 2 {
		t.Errorf("relations = %+v, want the other two intact", g.Relations)
	}
}

// --- read_graph (FR-3.6) ---

func readGraphFixture(t *testing.T) (*Store, Scope) {
	t.Helper()
	s, sc := newTestStore(t), testScope()
	seed(t, s, sc, Graph{
		Entities: []Entity{
			{Name: "a", EntityType: "person", Observations: obs("first", "second"), CreatedAt: 1},
			{Name: "b", EntityType: "system", Observations: obs("only"), CreatedAt: 1},
			{Name: "gone", EntityType: "note", Observations: obs("hidden"), CreatedAt: 1, Archived: true},
			{Name: "folded", EntityType: "note", Observations: obs("folded"), CreatedAt: 1, Merged: true, MergedInto: "a"},
		},
		Relations: []Relation{
			{From: "a", To: "b", RelationType: "uses", CreatedAt: 1},
			{From: "a", To: "gone", RelationType: "dangles", CreatedAt: 1},
		},
	})
	return s, sc
}

func TestReadGraphHidesArchivedAndMergedAndDropsDanglingRelations(t *testing.T) {
	t.Parallel()
	s, sc := readGraphFixture(t)
	got, err := s.ReadGraph(sc, DetailFull, nil, false, false)
	if err != nil {
		t.Fatalf("ReadGraph: %v", err)
	}
	full, ok := got.(FullGraph)
	if !ok {
		t.Fatalf("ReadGraph(full) returned %T, want FullGraph", got)
	}
	if len(full.Entities) != 2 {
		t.Errorf("entities = %d, want 2 (archived and merged hidden)", len(full.Entities))
	}
	if len(full.Relations) != 1 || full.Relations[0].RelationType != "uses" {
		t.Errorf("relations = %+v; an edge to a hidden entity must be dropped", full.Relations)
	}
}

func TestReadGraphIncludeFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                           string
		includeArchived, includeMerged bool
		wantEntities                   int
	}{
		{"default hides both", false, false, 2},
		{"includeArchived", true, false, 3},
		{"includeMerged", false, true, 3},
		{"both", true, true, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s, sc := readGraphFixture(t)
			got, err := s.ReadGraph(sc, DetailFull, nil, c.includeArchived, c.includeMerged)
			if err != nil {
				t.Fatalf("ReadGraph: %v", err)
			}
			if n := len(got.(FullGraph).Entities); n != c.wantEntities {
				t.Errorf("entities = %d, want %d", n, c.wantEntities)
			}
		})
	}
}

func TestReadGraphDetailLevels(t *testing.T) {
	t.Parallel()

	t.Run("minimal", func(t *testing.T) {
		t.Parallel()
		s, sc := readGraphFixture(t)
		got, err := s.ReadGraph(sc, DetailMinimal, nil, false, false)
		if err != nil {
			t.Fatalf("ReadGraph: %v", err)
		}
		m, ok := got.(MinimalGraph)
		if !ok {
			t.Fatalf("got %T, want MinimalGraph", got)
		}
		if m.TotalObservations != 3 {
			t.Errorf("TotalObservations = %d, want 3", m.TotalObservations)
		}
		if m.Entities[0].Name != "a" || m.Entities[0].Type != "person" || m.Entities[0].ObservationCount != 2 {
			t.Errorf("first entity = %+v", m.Entities[0])
		}
	})

	t.Run("summary is the default", func(t *testing.T) {
		t.Parallel()
		s, sc := readGraphFixture(t)
		for _, level := range []string{DetailSummary, "", "nonsense"} {
			got, err := s.ReadGraph(sc, level, nil, false, false)
			if err != nil {
				t.Fatalf("ReadGraph(%q): %v", level, err)
			}
			sum, ok := got.(SummaryGraph)
			if !ok {
				t.Fatalf("ReadGraph(%q) = %T, want SummaryGraph", level, got)
			}
			if sum.Entities[0].FirstObservation != "first" {
				t.Errorf("FirstObservation = %q, want %q", sum.Entities[0].FirstObservation, "first")
			}
			if sum.Entities[0].RelationCount != 1 {
				t.Errorf("RelationCount = %d, want 1 (the dangling edge is already gone)", sum.Entities[0].RelationCount)
			}
			if sum.TotalObservations != 3 {
				t.Errorf("TotalObservations = %d, want 3", sum.TotalObservations)
			}
		}
	})

	t.Run("full carries whole entities", func(t *testing.T) {
		t.Parallel()
		s, sc := readGraphFixture(t)
		got, err := s.ReadGraph(sc, DetailFull, nil, false, false)
		if err != nil {
			t.Fatalf("ReadGraph: %v", err)
		}
		full := got.(FullGraph)
		if len(full.Entities[0].Observations) != 2 {
			t.Errorf("full detail dropped observations: %+v", full.Entities[0])
		}
	})
}

func TestReadGraphWithEntityNamesIgnoresDetailLevel(t *testing.T) {
	t.Parallel()
	s, sc := readGraphFixture(t)
	got, err := s.ReadGraph(sc, DetailMinimal, []string{"a"}, false, false)
	if err != nil {
		t.Fatalf("ReadGraph: %v", err)
	}
	full, ok := got.(FullGraph)
	if !ok {
		t.Fatalf("got %T, want FullGraph — naming entities must override detailLevel", got)
	}
	if len(full.Entities) != 1 || len(full.Entities[0].Observations) != 2 {
		t.Errorf("entities = %+v, want just 'a' in full detail", full.Entities)
	}
	if len(full.Relations) != 0 {
		t.Errorf("relations = %+v; 'b' was not requested so a→b must drop", full.Relations)
	}
}

// --- get_entity_details / open_nodes (FR-3.12) ---

func TestNamedRetrievalDoesNotHideArchivedOrMerged(t *testing.T) {
	t.Parallel()
	s, sc := readGraphFixture(t)

	details, err := s.GetEntityDetails(sc, []string{"gone", "folded", "absent"})
	if err != nil {
		t.Fatalf("GetEntityDetails: %v", err)
	}
	if len(details) != 2 {
		t.Errorf("GetEntityDetails returned %d, want 2 — naming an entity is how you inspect one you archived", len(details))
	}

	open, err := s.OpenNodes(sc, []string{"a", "gone"})
	if err != nil {
		t.Fatalf("OpenNodes: %v", err)
	}
	if len(open.Entities) != 2 {
		t.Errorf("OpenNodes entities = %d, want 2", len(open.Entities))
	}
	if len(open.Relations) != 1 || open.Relations[0].RelationType != "dangles" {
		t.Errorf("OpenNodes relations = %+v, want the a→gone edge, both endpoints being requested", open.Relations)
	}
}

// --- archive / unarchive (FR-3.8) ---

func TestArchiveAndUnarchiveReportRefusalsAsStatusNotError(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	seed(t, s, sc, Graph{Entities: []Entity{{Name: "a", EntityType: "t", CreatedAt: 1}}})

	if res, err := s.ArchiveEntity(sc, "a"); err != nil || !res.Success {
		t.Fatalf("ArchiveEntity = %+v, %v; want success", res, err)
	}
	g, _ := s.Load(sc)
	if !g.findEntity("a").Archived {
		t.Error("entity not marked archived")
	}

	cases := []struct {
		name    string
		call    func() (StatusResult, error)
		wantMsg string
	}{
		{"archive twice", func() (StatusResult, error) { return s.ArchiveEntity(sc, "a") }, "already archived"},
		{"archive missing", func() (StatusResult, error) { return s.ArchiveEntity(sc, "ghost") }, "not found"},
		{"unarchive missing", func() (StatusResult, error) { return s.UnarchiveEntity(sc, "ghost") }, "not found"},
	}
	for _, c := range cases {
		res, err := c.call()
		if err != nil {
			t.Errorf("%s returned err %v; refusals travel through StatusResult", c.name, err)
		}
		if res.Success {
			t.Errorf("%s reported success", c.name)
		}
		if !strings.Contains(res.Message, c.wantMsg) {
			t.Errorf("%s message = %q, want it to contain %q", c.name, res.Message, c.wantMsg)
		}
	}

	if res, err := s.UnarchiveEntity(sc, "a"); err != nil || !res.Success {
		t.Fatalf("UnarchiveEntity = %+v, %v; want success", res, err)
	}
	if res, err := s.UnarchiveEntity(sc, "a"); err != nil || res.Success ||
		!strings.Contains(res.Message, "not archived") {
		t.Errorf("second UnarchiveEntity = %+v, %v; want a 'not archived' refusal", res, err)
	}
}

func TestRefusedArchiveWritesNothing(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	seed(t, s, sc, Graph{Entities: []Entity{{Name: "a", EntityType: "t", CreatedAt: 1}}})
	before, err := s.Load(sc)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := s.ArchiveEntity(sc, "ghost"); err != nil {
		t.Fatalf("ArchiveEntity: %v", err)
	}
	after, _ := s.Load(sc)
	if len(after.Entities) != len(before.Entities) || after.Entities[0].Archived {
		t.Errorf("a refused archive changed the graph: %+v", after.Entities)
	}
}

// --- merge_entities (FR-3.7) ---

func TestMergeEntitiesFoldsObservationsRedirectsAndDedupesRelations(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	seed(t, s, sc, Graph{
		Entities: []Entity{
			{Name: "dup", EntityType: "person", Observations: obs("shared", "only in dup"), CreatedAt: 1},
			{Name: "canon", EntityType: "person", Observations: obs("shared"), CreatedAt: 1},
			{Name: "other", EntityType: "t", CreatedAt: 1},
		},
		Relations: []Relation{
			// Both of these become other→canon "knows" once dup is redirected.
			{From: "other", To: "dup", RelationType: "knows", CreatedAt: 1},
			{From: "other", To: "canon", RelationType: "knows", CreatedAt: 1},
			{From: "dup", To: "other", RelationType: "leads", CreatedAt: 1},
		},
	})

	res, err := s.MergeEntities(sc, "dup", "canon")
	if err != nil {
		t.Fatalf("MergeEntities: %v", err)
	}
	if !res.Success {
		t.Fatalf("MergeEntities refused: %s", res.Message)
	}
	if !strings.Contains(res.Message, "Merged 1 observations") {
		t.Errorf("message = %q, want it to report 1 merged observation", res.Message)
	}

	g, _ := s.Load(sc)
	canon := g.findEntity("canon")
	if len(canon.Observations) != 2 {
		t.Errorf("canon observations = %+v, want the shared one plus the unique one", canon.Observations)
	}
	dup := g.findEntity("dup")
	if dup == nil {
		t.Fatal("source entity was removed; it must be kept as a merged tombstone")
	}
	if !dup.Merged || dup.MergedInto != "canon" || dup.MergedAt != testNow {
		t.Errorf("dup = %+v, want merged/mergedInto=canon/mergedAt=now", dup)
	}
	if len(g.Relations) != 2 {
		t.Errorf("relations = %+v, want 2 after redirect collapses the duplicate", g.Relations)
	}
	for _, r := range g.Relations {
		if r.From == "dup" || r.To == "dup" {
			t.Errorf("relation %+v still points at the merged source", r)
		}
	}
}

func TestMergeEntitiesRefusals(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	seed(t, s, sc, Graph{Entities: []Entity{
		{Name: "a", EntityType: "t", CreatedAt: 1},
		{Name: "b", EntityType: "t", CreatedAt: 1},
		{Name: "already", EntityType: "t", CreatedAt: 1, Merged: true, MergedInto: "a"},
	}})
	cases := []struct {
		name, source, target, wantMsg string
	}{
		{"missing source", "ghost", "a", "Source entity 'ghost' not found"},
		{"missing target", "a", "ghost", "Target entity 'ghost' not found"},
		{"already merged", "already", "b", "has already been merged"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := s.MergeEntities(sc, c.source, c.target)
			if err != nil {
				t.Fatalf("MergeEntities returned err %v; refusals travel through StatusResult", err)
			}
			if res.Success {
				t.Errorf("merge succeeded, want a refusal")
			}
			if !strings.Contains(res.Message, c.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", res.Message, c.wantMsg)
			}
		})
	}
}

// --- get_recent_changes (FR-3.9) ---

func TestGetRecentChangesWindowsEntitiesRelationsAndObservations(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	const hour = int64(60 * 60 * 1000)
	old := testNow - 48*hour
	recent := testNow - 1*hour

	seed(t, s, sc, Graph{
		Entities: []Entity{
			{Name: "fresh", EntityType: "t", CreatedAt: recent,
				Observations: []Observation{{Content: "new", Timestamp: recent, Confidence: 1}}},
			{Name: "veteran", EntityType: "t", CreatedAt: old, Observations: []Observation{
				{Content: "ancient", Timestamp: old, Confidence: 1},
				{Content: "learned today", Timestamp: recent, Confidence: 1},
			}},
			{Name: "stale", EntityType: "t", CreatedAt: old,
				Observations: []Observation{{Content: "ancient", Timestamp: old, Confidence: 1}}},
			{Name: "hidden", EntityType: "t", CreatedAt: recent, Archived: true,
				Observations: []Observation{{Content: "new but archived", Timestamp: recent, Confidence: 1}}},
			{Name: "folded", EntityType: "t", CreatedAt: recent, Merged: true,
				Observations: []Observation{{Content: "new but merged", Timestamp: recent, Confidence: 1}}},
		},
		Relations: []Relation{
			{From: "fresh", To: "veteran", RelationType: "new edge", CreatedAt: recent},
			{From: "veteran", To: "stale", RelationType: "old edge", CreatedAt: old},
		},
	})

	res, err := s.GetRecentChanges(sc, 24)
	if err != nil {
		t.Fatalf("GetRecentChanges: %v", err)
	}
	if len(res.RecentEntities) != 1 || res.RecentEntities[0].Name != "fresh" {
		t.Errorf("RecentEntities = %+v, want only 'fresh' (archived and merged excluded)", names(res.RecentEntities))
	}
	if len(res.RecentRelations) != 1 || res.RecentRelations[0].RelationType != "new edge" {
		t.Errorf("RecentRelations = %+v, want only the new edge", res.RecentRelations)
	}
	if len(res.RecentObservations) != 2 {
		t.Fatalf("RecentObservations = %+v, want fresh and veteran", res.RecentObservations)
	}
	for _, eo := range res.RecentObservations {
		if eo.Entity == "veteran" {
			if len(eo.Observations) != 1 || eo.Observations[0].Content != "learned today" {
				t.Errorf("veteran observations = %+v, want ONLY the one inside the window", eo.Observations)
			}
		}
	}
}

func TestGetRecentChangesReturnsEmptySlicesNotNil(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	res, err := s.GetRecentChanges(sc, 24)
	if err != nil {
		t.Fatalf("GetRecentChanges: %v", err)
	}
	if res.RecentEntities == nil || res.RecentRelations == nil || res.RecentObservations == nil {
		t.Errorf("result = %+v; slices must be non-nil so they marshal as []", res)
	}
}

// --- cross-cutting ---

// Every mutating operation must reach disk through Store.Update, which is the
// only thing enforcing the lock, the size cap and the atomic write. A regression
// here would be invisible until two turns overlapped in production.
func TestMutatingOperationsPersist(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	if _, err := s.CreateEntities(sc, []Entity{{Name: "a", EntityType: "t", Observations: obs("x")}}, ""); err != nil {
		t.Fatalf("CreateEntities: %v", err)
	}
	fresh := NewStore(s.root, s.now)
	g, err := fresh.Load(sc)
	if err != nil {
		t.Fatalf("Load from a new Store: %v", err)
	}
	if g.findEntity("a") == nil {
		t.Error("a created entity was not durable across Store instances")
	}
}

func TestOperationsOnAnEmptyGraphDoNotError(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	if _, err := s.DeleteEntities(sc, []string{"nope"}); err != nil {
		t.Errorf("DeleteEntities on empty: %v", err)
	}
	if _, err := s.DeleteRelations(sc, []Relation{{From: "a", To: "b", RelationType: "c"}}); err != nil {
		t.Errorf("DeleteRelations on empty: %v", err)
	}
	if _, err := s.OpenNodes(sc, []string{"nope"}); err != nil {
		t.Errorf("OpenNodes on empty: %v", err)
	}
	if _, err := s.ReadGraph(sc, DetailSummary, nil, false, false); err != nil {
		t.Errorf("ReadGraph on empty: %v", err)
	}
	if _, err := s.AddObservations(sc, []ObservationInput{{EntityName: "nope", Contents: []string{"x"}}}, ""); err == nil {
		t.Error("AddObservations on an empty graph must still error (FR-3.3)")
	}
}

func TestErrNoChangeNeverEscapes(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	_, err := s.ArchiveEntity(sc, "ghost")
	if errors.Is(err, errNoChange) {
		t.Error("errNoChange leaked out of ArchiveEntity")
	}
	_, err = s.MergeEntities(sc, "ghost", "other")
	if errors.Is(err, errNoChange) {
		t.Error("errNoChange leaked out of MergeEntities")
	}
}

func names(entities []Entity) []string {
	out := make([]string, len(entities))
	for i, e := range entities {
		out[i] = e.Name
	}
	return out
}

// --- provenance (the source conversation a fact came out of) ---

func TestCreateEntitiesRecordsTheSourceConversation(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	if _, err := s.CreateEntities(sc, []Entity{
		{Name: "a", EntityType: "t", Observations: []Observation{{Content: "x"}, {Content: "y"}}},
	}, "conv-42"); err != nil {
		t.Fatalf("CreateEntities: %v", err)
	}
	g, _ := s.Load(sc)
	e := g.findEntity("a")
	if e.SourceSessionID != "conv-42" {
		t.Errorf("entity source = %q, want conv-42", e.SourceSessionID)
	}
	for i, o := range e.Observations {
		if o.SourceSessionID != "conv-42" {
			t.Errorf("observation %d source = %q, want conv-42", i, o.SourceSessionID)
		}
	}
}

func TestCreateRelationsRecordsTheSourceConversation(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	if _, err := s.CreateRelations(sc, []Relation{
		{From: "a", To: "b", RelationType: "knows"},
	}, "conv-7"); err != nil {
		t.Fatalf("CreateRelations: %v", err)
	}
	g, _ := s.Load(sc)
	if g.Relations[0].SourceSessionID != "conv-7" {
		t.Errorf("relation source = %q, want conv-7", g.Relations[0].SourceSessionID)
	}
}

func TestAddObservationsRecordsTheSourcePerObservation(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	seed(t, s, sc, Graph{Entities: []Entity{
		{Name: "a", EntityType: "t", CreatedAt: 1,
			Observations: []Observation{{Content: "old", Timestamp: 1, SourceSessionID: "conv-old"}}},
	}})
	if _, err := s.AddObservations(sc, []ObservationInput{
		{EntityName: "a", Contents: []string{"new"}},
	}, "conv-new"); err != nil {
		t.Fatalf("AddObservations: %v", err)
	}
	g, _ := s.Load(sc)
	got := g.findEntity("a").Observations
	if got[0].SourceSessionID != "conv-old" {
		t.Errorf("the existing observation's source changed to %q", got[0].SourceSessionID)
	}
	if got[1].SourceSessionID != "conv-new" {
		t.Errorf("new observation source = %q, want conv-new", got[1].SourceSessionID)
	}
}

// An empty source is the NORMAL outcome for a cron job, the heartbeat, post-turn
// evolution, or two concurrent conversations. It must write no field at all rather
// than an empty string, so the line stays byte-identical to what upstream writes.
func TestAnUnattributedWriteRecordsNoSourceField(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	if _, err := s.CreateEntities(sc, []Entity{
		{Name: "a", EntityType: "t", Observations: []Observation{{Content: "x"}}},
	}, ""); err != nil {
		t.Fatalf("CreateEntities: %v", err)
	}
	g, _ := s.Load(sc)
	out, err := encodeJSONL(g)
	if err != nil {
		t.Fatalf("encodeJSONL: %v", err)
	}
	if strings.Contains(string(out), "sourceSessionId") {
		t.Errorf("an unattributed write emitted the provenance key:\n%s", out)
	}
}

// MergeEntities copies Observation structs wholesale, so provenance rides along for
// free. Pinned before someone "simplifies" that copy into a rebuild that drops it —
// the observations would keep their text and silently lose where they came from.
func TestMergeCarriesProvenanceAcross(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	seed(t, s, sc, Graph{Entities: []Entity{
		{Name: "dup", EntityType: "t", CreatedAt: 1, Observations: []Observation{
			{Content: "from the dup", Timestamp: 1, SourceSessionID: "conv-dup"},
		}},
		{Name: "canon", EntityType: "t", CreatedAt: 1, Observations: []Observation{
			{Content: "from canon", Timestamp: 1, SourceSessionID: "conv-canon"},
		}},
	}})
	res, err := s.MergeEntities(sc, "dup", "canon")
	if err != nil || !res.Success {
		t.Fatalf("MergeEntities = %+v, %v", res, err)
	}
	g, _ := s.Load(sc)
	byContent := map[string]string{}
	for _, o := range g.findEntity("canon").Observations {
		byContent[o.Content] = o.SourceSessionID
	}
	if byContent["from the dup"] != "conv-dup" {
		t.Errorf("merged observation lost its source: %q", byContent["from the dup"])
	}
	if byContent["from canon"] != "conv-canon" {
		t.Errorf("target's own observation source changed: %q", byContent["from canon"])
	}
}

// Provenance survives the round trip, and an upstream file that never had it still
// reads (the field is omitempty on both sides).
func TestProvenanceRoundTripsThroughJSONL(t *testing.T) {
	t.Parallel()
	s, sc := newTestStore(t), testScope()
	if _, err := s.CreateEntities(sc, []Entity{
		{Name: "a", EntityType: "t", Observations: []Observation{{Content: "x"}}},
	}, "conv-rt"); err != nil {
		t.Fatalf("CreateEntities: %v", err)
	}
	fresh := NewStore(s.root, s.now)
	g, err := fresh.Load(sc)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if g.findEntity("a").Observations[0].SourceSessionID != "conv-rt" {
		t.Error("provenance did not survive a reload")
	}
}
