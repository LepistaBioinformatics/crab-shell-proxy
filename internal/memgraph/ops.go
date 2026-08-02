package memgraph

import "fmt"

// The graph operations, ported from better-memory-mcp's KnowledgeGraphManager.
//
// Result shapes are upstream's, field names included, because they are what the
// agent reads back and what the reference implementation's prompts were tuned
// against. Slices are always initialized: upstream emits [] where Go would emit
// null, and a UI that has to handle both is a UI with a bug waiting in it.

// CreateEntitiesResult reports which names were taken and which already existed.
type CreateEntitiesResult struct {
	Created []string `json:"created"`
	Skipped []string `json:"skipped"`
}

// CreateRelationsResult counts new and duplicate edges.
type CreateRelationsResult struct {
	Created int `json:"created"`
	Skipped int `json:"skipped"`
}

// AddObservationsResult reports one entity's outcome.
type AddObservationsResult struct {
	EntityName string `json:"entityName"`
	Added      int    `json:"added"`
	Skipped    int    `json:"skipped"`
}

// DeleteEntitiesResult reports the entities removed and the relations that went
// with them.
type DeleteEntitiesResult struct {
	Deleted           int `json:"deleted"`
	CascadedRelations int `json:"cascadedRelations"`
}

// DeleteObservationsResult reports one entity's outcome.
type DeleteObservationsResult struct {
	EntityName string `json:"entityName"`
	Deleted    int    `json:"deleted"`
}

// DeleteRelationsResult counts removed edges.
type DeleteRelationsResult struct {
	Deleted int `json:"deleted"`
}

// StatusResult is upstream's {success, message} pair. archive/unarchive/merge
// report a missing entity or a no-op transition THROUGH this rather than as an
// error, so the agent gets a sentence it can act on instead of a tool failure.
type StatusResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// ObservationInput is one entry of add_observations' payload. A Confidence
// shorter than Contents leaves the remainder at the 1.0 default.
type ObservationInput struct {
	EntityName string
	Contents   []string
	Confidence []float64
}

// RelationKey is the identity of an edge: upstream treats (from, to, relationType)
// as the whole key, so two edges differing only in createdAt are the same edge.
type RelationKey struct {
	From         string
	To           string
	RelationType string
}

func (r Relation) key() RelationKey {
	return RelationKey{From: r.From, To: r.To, RelationType: r.RelationType}
}

// EntityMinimal is read_graph's "minimal" projection.
type EntityMinimal struct {
	Name             string `json:"name"`
	Type             string `json:"type"`
	ObservationCount int    `json:"observationCount"`
}

// EntitySummary is read_graph's default "summary" projection.
type EntitySummary struct {
	Name             string `json:"name"`
	Type             string `json:"type"`
	ObservationCount int    `json:"observationCount"`
	FirstObservation string `json:"firstObservation,omitempty"`
	RelationCount    int    `json:"relationCount"`
}

// MinimalGraph, SummaryGraph and FullGraph are read_graph's three return shapes.
// They are separate types rather than one struct with an `any` field so a test
// can assert which projection was produced, and so the JSON matches upstream's
// (a full graph carries no totalObservations).
type MinimalGraph struct {
	Entities          []EntityMinimal `json:"entities"`
	Relations         []Relation      `json:"relations"`
	TotalObservations int             `json:"totalObservations"`
}

type SummaryGraph struct {
	Entities          []EntitySummary `json:"entities"`
	Relations         []Relation      `json:"relations"`
	TotalObservations int             `json:"totalObservations"`
}

type FullGraph struct {
	Entities  []Entity   `json:"entities"`
	Relations []Relation `json:"relations"`
}

// EntityObservations pairs an entity name with a subset of its observations.
type EntityObservations struct {
	Entity       string        `json:"entity"`
	Observations []Observation `json:"observations"`
}

// RecentChanges is get_recent_changes' result.
type RecentChanges struct {
	RecentEntities     []Entity             `json:"recentEntities"`
	RecentRelations    []Relation           `json:"recentRelations"`
	RecentObservations []EntityObservations `json:"recentObservations"`
}

// Detail levels accepted by read_graph.
const (
	DetailMinimal = "minimal"
	DetailSummary = "summary"
	DetailFull    = "full"
)

// findEntity returns a pointer into the graph so callers mutate in place.
func (g *Graph) findEntity(name string) *Entity {
	for i := range g.Entities {
		if g.Entities[i].Name == name {
			return &g.Entities[i]
		}
	}
	return nil
}

// relationsAmong keeps only the edges whose BOTH endpoints are in names. Every
// projection uses it, so a filtered view never carries a dangling edge.
func relationsAmong(names map[string]bool, relations []Relation) []Relation {
	out := make([]Relation, 0, len(relations))
	for _, r := range relations {
		if names[r.From] && names[r.To] {
			out = append(out, r)
		}
	}
	return out
}

func nameSet(entities []Entity) map[string]bool {
	set := make(map[string]bool, len(entities))
	for _, e := range entities {
		set[e.Name] = true
	}
	return set
}

// CreateEntities adds entities whose names are free and skips the rest. An
// existing entity is never overwritten: the agent re-asserting something it
// already knows must not wipe the observations it accumulated about it.
// The `source` argument on the three write operations is the conversation the fact
// came out of, or "" when the proxy could not attribute it unambiguously. Empty is a
// normal outcome, not a failure — see httpapi.turnRegistry.
func (s *Store) CreateEntities(sc Scope, entities []Entity, source string) (CreateEntitiesResult, error) {
	res := CreateEntitiesResult{Created: []string{}, Skipped: []string{}}
	now := s.NowMillis()
	err := s.Update(sc, func(g *Graph) error {
		for _, e := range entities {
			if g.findEntity(e.Name) != nil {
				res.Skipped = append(res.Skipped, e.Name)
				continue
			}
			ne := e
			ne.Observations = stampNew(e.Observations, now, source)
			if ne.CreatedAt == 0 {
				ne.CreatedAt = now
			}
			if ne.SourceSessionID == "" {
				ne.SourceSessionID = source
			}
			g.Entities = append(g.Entities, ne)
			res.Created = append(res.Created, e.Name)
		}
		return nil
	})
	if err != nil {
		return CreateEntitiesResult{Created: []string{}, Skipped: []string{}}, err
	}
	return res, nil
}

// stampNew fills in timestamp and confidence for observations arriving from a
// tool call, where the caller supplies only text.
func stampNew(obs []Observation, now int64, source string) []Observation {
	out := make([]Observation, len(obs))
	for i, o := range obs {
		if o.Timestamp == 0 {
			o.Timestamp = now
		}
		if o.Confidence == 0 {
			o.Confidence = 1.0
		}
		if o.SourceSessionID == "" {
			o.SourceSessionID = source
		}
		o.legacy = false
		out[i] = o
	}
	return out
}

// CreateRelations adds edges, skipping exact (from, to, relationType) duplicates.
func (s *Store) CreateRelations(sc Scope, relations []Relation, source string) (CreateRelationsResult, error) {
	var res CreateRelationsResult
	now := s.NowMillis()
	err := s.Update(sc, func(g *Graph) error {
		existing := make(map[RelationKey]bool, len(g.Relations))
		for _, r := range g.Relations {
			existing[r.key()] = true
		}
		for _, r := range relations {
			if existing[r.key()] {
				res.Skipped++
				continue
			}
			nr := r
			if nr.CreatedAt == 0 {
				nr.CreatedAt = now
			}
			if nr.SourceSessionID == "" {
				nr.SourceSessionID = source
			}
			g.Relations = append(g.Relations, nr)
			existing[nr.key()] = true
			res.Created++
		}
		return nil
	})
	if err != nil {
		return CreateRelationsResult{}, err
	}
	return res, nil
}

// AddObservations appends facts to existing entities, deduping against what each
// entity already holds.
//
// A missing entity is an ERROR here, unlike DeleteObservations, which reports
// zero. That asymmetry is upstream's and it is deliberate: adding to something
// that does not exist means the agent's model of the graph is wrong and it should
// find out, while deleting from something absent is already the desired end state.
// Do not "make these consistent".
func (s *Store) AddObservations(sc Scope, inputs []ObservationInput, source string) ([]AddObservationsResult, error) {
	results := make([]AddObservationsResult, 0, len(inputs))
	now := s.NowMillis()
	err := s.Update(sc, func(g *Graph) error {
		// Validate every entity before mutating any, so a payload naming one bad
		// entity does not half-apply.
		for _, in := range inputs {
			if g.findEntity(in.EntityName) == nil {
				return fmt.Errorf("entity %q not found", in.EntityName)
			}
		}
		for _, in := range inputs {
			e := g.findEntity(in.EntityName)
			have := make(map[string]bool, len(e.Observations))
			for _, o := range e.Observations {
				have[o.Content] = true
			}
			added := 0
			for _, content := range in.Contents {
				if have[content] {
					continue
				}
				// Confidence is indexed by position among the observations actually
				// ADDED, not by position in Contents. That is upstream's behaviour: it
				// filters duplicates out first and then maps confidence over the
				// filtered list. Debatable as a contract — a caller would reasonably
				// expect the arrays to line up as given — but this is a port, and
				// silently re-aligning it would change what the same call means.
				confidence := 1.0
				if added < len(in.Confidence) {
					confidence = in.Confidence[added]
				}
				e.Observations = append(e.Observations, Observation{
					Content:         content,
					Timestamp:       now,
					Confidence:      confidence,
					SourceSessionID: source,
				})
				have[content] = true
				added++
			}
			results = append(results, AddObservationsResult{
				EntityName: in.EntityName,
				Added:      added,
				Skipped:    len(in.Contents) - added,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// DeleteEntities removes entities and cascades to every relation touching one on
// either side — a graph must not keep an edge to a node that is gone.
func (s *Store) DeleteEntities(sc Scope, names []string) (DeleteEntitiesResult, error) {
	var res DeleteEntitiesResult
	doomed := make(map[string]bool, len(names))
	for _, n := range names {
		doomed[n] = true
	}
	err := s.Update(sc, func(g *Graph) error {
		keptEntities := make([]Entity, 0, len(g.Entities))
		for _, e := range g.Entities {
			if doomed[e.Name] {
				res.Deleted++
				continue
			}
			keptEntities = append(keptEntities, e)
		}
		keptRelations := make([]Relation, 0, len(g.Relations))
		for _, r := range g.Relations {
			if doomed[r.From] || doomed[r.To] {
				res.CascadedRelations++
				continue
			}
			keptRelations = append(keptRelations, r)
		}
		g.Entities = keptEntities
		g.Relations = keptRelations
		return nil
	})
	if err != nil {
		return DeleteEntitiesResult{}, err
	}
	return res, nil
}

// DeleteObservations removes named facts. A missing entity reports zero rather
// than erroring — see AddObservations for why the two differ.
func (s *Store) DeleteObservations(sc Scope, deletions []ObservationInput) ([]DeleteObservationsResult, error) {
	results := make([]DeleteObservationsResult, 0, len(deletions))
	err := s.Update(sc, func(g *Graph) error {
		for _, d := range deletions {
			e := g.findEntity(d.EntityName)
			if e == nil {
				results = append(results, DeleteObservationsResult{EntityName: d.EntityName, Deleted: 0})
				continue
			}
			doomed := make(map[string]bool, len(d.Contents))
			for _, c := range d.Contents {
				doomed[c] = true
			}
			kept := make([]Observation, 0, len(e.Observations))
			for _, o := range e.Observations {
				if doomed[o.Content] {
					continue
				}
				kept = append(kept, o)
			}
			results = append(results, DeleteObservationsResult{
				EntityName: d.EntityName,
				Deleted:    len(e.Observations) - len(kept),
			})
			e.Observations = kept
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// DeleteRelations removes exact triple matches. A triple naming no existing edge
// is not an error; the end state is what was asked for.
func (s *Store) DeleteRelations(sc Scope, relations []Relation) (DeleteRelationsResult, error) {
	var res DeleteRelationsResult
	doomed := make(map[RelationKey]bool, len(relations))
	for _, r := range relations {
		doomed[r.key()] = true
	}
	err := s.Update(sc, func(g *Graph) error {
		kept := make([]Relation, 0, len(g.Relations))
		for _, r := range g.Relations {
			if doomed[r.key()] {
				res.Deleted++
				continue
			}
			kept = append(kept, r)
		}
		g.Relations = kept
		return nil
	})
	if err != nil {
		return DeleteRelationsResult{}, err
	}
	return res, nil
}

// ReadGraph returns one of MinimalGraph, SummaryGraph or FullGraph.
//
// Naming entities explicitly returns full detail and IGNORES detailLevel — asking
// for specific things means you want them, not a summary of them.
func (s *Store) ReadGraph(sc Scope, detailLevel string, entityNames []string, includeArchived, includeMerged bool) (any, error) {
	g, err := s.Load(sc)
	if err != nil {
		return nil, err
	}
	visible := make([]Entity, 0, len(g.Entities))
	for _, e := range g.Entities {
		if e.hidden(includeArchived, includeMerged) {
			continue
		}
		visible = append(visible, e)
	}

	if len(entityNames) > 0 {
		want := make(map[string]bool, len(entityNames))
		for _, n := range entityNames {
			want[n] = true
		}
		picked := make([]Entity, 0, len(entityNames))
		for _, e := range visible {
			if want[e.Name] {
				picked = append(picked, e)
			}
		}
		return FullGraph{Entities: picked, Relations: relationsAmong(nameSet(picked), g.Relations)}, nil
	}

	rels := relationsAmong(nameSet(visible), g.Relations)
	total := 0
	for _, e := range visible {
		total += len(e.Observations)
	}

	switch detailLevel {
	case DetailMinimal:
		out := MinimalGraph{Entities: make([]EntityMinimal, 0, len(visible)), Relations: rels, TotalObservations: total}
		for _, e := range visible {
			out.Entities = append(out.Entities, EntityMinimal{
				Name: e.Name, Type: e.EntityType, ObservationCount: len(e.Observations),
			})
		}
		return out, nil
	case DetailFull:
		return FullGraph{Entities: visible, Relations: rels}, nil
	default: // DetailSummary and anything unrecognised, matching upstream's default branch
		out := SummaryGraph{Entities: make([]EntitySummary, 0, len(visible)), Relations: rels, TotalObservations: total}
		for _, e := range visible {
			var first string
			if len(e.Observations) > 0 {
				first = e.Observations[0].Content
			}
			relCount := 0
			for _, r := range rels {
				if r.From == e.Name || r.To == e.Name {
					relCount++
				}
			}
			out.Entities = append(out.Entities, EntitySummary{
				Name:             e.Name,
				Type:             e.EntityType,
				ObservationCount: len(e.Observations),
				FirstObservation: first,
				RelationCount:    relCount,
			})
		}
		return out, nil
	}
}

// GetEntityDetails returns the named entities in full, INCLUDING archived and
// merged ones. Naming an entity is how you inspect one you archived; only the
// browsing operations hide them.
func (s *Store) GetEntityDetails(sc Scope, names []string) ([]Entity, error) {
	g, err := s.Load(sc)
	if err != nil {
		return nil, err
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	out := make([]Entity, 0, len(names))
	for _, e := range g.Entities {
		if want[e.Name] {
			out = append(out, e)
		}
	}
	return out, nil
}

// OpenNodes returns the named entities with the relations among them. Like
// GetEntityDetails it does not hide archived or merged entities.
func (s *Store) OpenNodes(sc Scope, names []string) (FullGraph, error) {
	g, err := s.Load(sc)
	if err != nil {
		return FullGraph{}, err
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	picked := make([]Entity, 0, len(names))
	for _, e := range g.Entities {
		if want[e.Name] {
			picked = append(picked, e)
		}
	}
	return FullGraph{Entities: picked, Relations: relationsAmong(nameSet(picked), g.Relations)}, nil
}

// ArchiveEntity soft-deletes an entity, hiding it from the browsing operations
// while keeping it retrievable by name.
func (s *Store) ArchiveEntity(sc Scope, name string) (StatusResult, error) {
	return s.setArchived(sc, name, true)
}

// UnarchiveEntity restores an archived entity.
func (s *Store) UnarchiveEntity(sc Scope, name string) (StatusResult, error) {
	return s.setArchived(sc, name, false)
}

func (s *Store) setArchived(sc Scope, name string, archived bool) (StatusResult, error) {
	var res StatusResult
	err := s.Update(sc, func(g *Graph) error {
		e := g.findEntity(name)
		if e == nil {
			res = StatusResult{Message: fmt.Sprintf("Entity '%s' not found", name)}
			return errNoChange
		}
		if e.Archived == archived {
			if archived {
				res = StatusResult{Message: fmt.Sprintf("Entity '%s' is already archived", name)}
			} else {
				res = StatusResult{Message: fmt.Sprintf("Entity '%s' is not archived", name)}
			}
			return errNoChange
		}
		e.Archived = archived
		verb := "archived"
		if !archived {
			verb = "unarchived"
		}
		res = StatusResult{Success: true, Message: fmt.Sprintf("Successfully %s entity '%s'", verb, name)}
		return nil
	})
	if err != nil {
		if err == errNoChange {
			// A refusal is reported through StatusResult, not as a tool failure, and
			// nothing was written — which is exactly what errNoChange bought us.
			return res, nil
		}
		return StatusResult{}, err
	}
	return res, nil
}

// errNoChange aborts an Update without writing, for the operations that report a
// refusal as {success: false} rather than as an error. It never escapes the
// package.
var errNoChange = fmt.Errorf("memgraph: no change")

// MergeEntities folds source into target: non-duplicate observations move across,
// every relation endpoint is redirected, the duplicates that redirect creates are
// collapsed, and the source is kept as a merged tombstone so a dangling reference
// to it can still be resolved.
func (s *Store) MergeEntities(sc Scope, sourceName, targetName string) (StatusResult, error) {
	var res StatusResult
	now := s.NowMillis()
	err := s.Update(sc, func(g *Graph) error {
		source := g.findEntity(sourceName)
		if source == nil {
			res = StatusResult{Message: fmt.Sprintf("Source entity '%s' not found", sourceName)}
			return errNoChange
		}
		if g.findEntity(targetName) == nil {
			res = StatusResult{Message: fmt.Sprintf("Target entity '%s' not found", targetName)}
			return errNoChange
		}
		if source.Merged {
			res = StatusResult{Message: fmt.Sprintf("Source entity '%s' has already been merged", sourceName)}
			return errNoChange
		}

		// Copy the observations out before touching anything: findEntity returns
		// pointers into the same slice, so target and source must not be held
		// simultaneously across an append that could reallocate it.
		sourceObs := make([]Observation, len(source.Observations))
		copy(sourceObs, source.Observations)

		target := g.findEntity(targetName)
		have := make(map[string]bool, len(target.Observations))
		for _, o := range target.Observations {
			have[o.Content] = true
		}
		moved := 0
		for _, o := range sourceObs {
			if have[o.Content] {
				continue
			}
			target.Observations = append(target.Observations, o)
			have[o.Content] = true
			moved++
		}

		// Redirect, then dedupe: two edges that differed only by endpoint can become
		// the same edge once both point at the target.
		seen := make(map[RelationKey]bool, len(g.Relations))
		kept := make([]Relation, 0, len(g.Relations))
		for _, r := range g.Relations {
			if r.From == sourceName {
				r.From = targetName
			}
			if r.To == sourceName {
				r.To = targetName
			}
			if seen[r.key()] {
				continue
			}
			seen[r.key()] = true
			kept = append(kept, r)
		}
		g.Relations = kept

		source = g.findEntity(sourceName)
		source.Merged = true
		source.MergedInto = targetName
		source.MergedAt = now

		touching := 0
		for _, r := range g.Relations {
			if r.From == targetName || r.To == targetName {
				touching++
			}
		}
		res = StatusResult{
			Success: true,
			Message: fmt.Sprintf("Successfully merged '%s' into '%s'. Merged %d observations and updated %d relations.",
				sourceName, targetName, moved, touching),
		}
		return nil
	})
	if err != nil {
		if err == errNoChange {
			return res, nil
		}
		return StatusResult{}, err
	}
	return res, nil
}

// GetRecentChanges reports what happened in the last `hours`: entities and
// relations created in the window, plus — per surviving entity — only the
// observations whose own timestamp falls inside it, so a long-lived entity that
// learned one new fact shows that one fact.
func (s *Store) GetRecentChanges(sc Scope, hours float64) (RecentChanges, error) {
	g, err := s.Load(sc)
	if err != nil {
		return RecentChanges{}, err
	}
	cutoff := s.NowMillis() - int64(hours*60*60*1000)
	out := RecentChanges{
		RecentEntities:     []Entity{},
		RecentRelations:    []Relation{},
		RecentObservations: []EntityObservations{},
	}
	for _, e := range g.Entities {
		if e.Archived || e.Merged {
			continue
		}
		if e.CreatedAt >= cutoff {
			out.RecentEntities = append(out.RecentEntities, e)
		}
		recent := make([]Observation, 0, len(e.Observations))
		for _, o := range e.Observations {
			if o.Timestamp >= cutoff {
				recent = append(recent, o)
			}
		}
		if len(recent) > 0 {
			out.RecentObservations = append(out.RecentObservations,
				EntityObservations{Entity: e.Name, Observations: recent})
		}
	}
	for _, r := range g.Relations {
		if r.CreatedAt >= cutoff {
			out.RecentRelations = append(out.RecentRelations, r)
		}
	}
	return out, nil
}
