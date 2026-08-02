package mcpserver

import (
	"encoding/json"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
)

// The 15 tools, with better-memory-mcp v0.8.0's exact names, parameter names,
// required sets and declared defaults.
//
// Every InputSchema is written out by hand rather than inferred from the Go input
// struct. That is deliberate: the SDK only infers when InputSchema is nil, and it
// APPLIES a schema's declared defaults before unmarshalling (context.md E-4). So a
// hand-written schema is what makes "the same tools as the upstream project" true
// at the level a client can observe — same property names, same enums, same
// defaults — instead of merely the same tool names.
//
// testdata/upstream-index-tools.ts.txt is the upstream registration block, kept
// next to testdata/upstream-tools.json so the golden file's provenance can be
// checked by reading rather than by trust.

// --- schema helpers -------------------------------------------------------

func object(props map[string]*jsonschema.Schema, required ...string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Properties: props, Required: required}
}

func str(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: desc}
}

func strArray(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "array", Description: desc, Items: &jsonschema.Schema{Type: "string"}}
}

func numArray(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "array", Description: desc, Items: &jsonschema.Schema{Type: "number"}}
}

func objArray(items *jsonschema.Schema) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "array", Items: items}
}

func numWithDefault(desc string, def float64) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "number", Description: desc, Default: mustJSON(def)}
}

func boolWithDefault(desc string, def bool) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "boolean", Description: desc, Default: mustJSON(def)}
}

func enumWithDefault(desc string, def string, values ...string) *jsonschema.Schema {
	vals := make([]any, len(values))
	for i, v := range values {
		vals[i] = v
	}
	return &jsonschema.Schema{Type: "string", Description: desc, Enum: vals, Default: mustJSON(def)}
}

// mustJSON encodes a schema default. The inputs are all literals in this file, so a
// failure is a programming error rather than a runtime condition.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("mcpserver: cannot encode schema default: " + err.Error())
	}
	return b
}

// --- tool input types ----------------------------------------------------

type entityIn struct {
	Name         string   `json:"name"`
	EntityType   string   `json:"entityType"`
	Observations []string `json:"observations"`
}

type relationIn struct {
	From         string `json:"from"`
	To           string `json:"to"`
	RelationType string `json:"relationType"`
}

type createEntitiesIn struct {
	Entities []entityIn `json:"entities"`
}

type createRelationsIn struct {
	Relations []relationIn `json:"relations"`
}

type addObservationsIn struct {
	Observations []struct {
		EntityName string    `json:"entityName"`
		Contents   []string  `json:"contents"`
		Confidence []float64 `json:"confidence"`
	} `json:"observations"`
}

type deleteEntitiesIn struct {
	EntityNames []string `json:"entityNames"`
}

type deleteObservationsIn struct {
	Deletions []struct {
		EntityName   string   `json:"entityName"`
		Observations []string `json:"observations"`
	} `json:"deletions"`
}

type deleteRelationsIn struct {
	Relations []relationIn `json:"relations"`
}

type readGraphIn struct {
	DetailLevel     string   `json:"detailLevel"`
	EntityNames     []string `json:"entityNames"`
	IncludeArchived bool     `json:"includeArchived"`
	IncludeMerged   bool     `json:"includeMerged"`
}

type entityNamesIn struct {
	EntityNames []string `json:"entityNames"`
}

type searchNodesIn struct {
	Query           string `json:"query"`
	MaxObservations int    `json:"maxObservations"`
}

type semanticSearchIn struct {
	Query     string  `json:"query"`
	K         int     `json:"k"`
	Threshold float64 `json:"threshold"`
}

type namesIn struct {
	Names []string `json:"names"`
}

type mergeIn struct {
	SourceName string `json:"sourceName"`
	TargetName string `json:"targetName"`
}

type entityNameIn struct {
	EntityName string `json:"entityName"`
}

type recentChangesIn struct {
	Hours float64 `json:"hours"`
}

// --- registration --------------------------------------------------------

// registerTools adds all 15 tools to srv.
//
// None is registered `deferred`. Deferred tools stay hidden until the agent thinks
// to search for them, which for memory means the agent only remembers when it
// already suspected it should — the opposite of what a memory is for. The per-turn
// token cost is real and accepted; read_graph's default "summary" detail level is
// what keeps the RESPONSES small.
func (s *server) registerTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_entities",
		Description: "Create new entities in the knowledge graph",
		InputSchema: object(map[string]*jsonschema.Schema{
			"entities": objArray(object(map[string]*jsonschema.Schema{
				"name":         str("Entity identifier"),
				"entityType":   str("Entity type"),
				"observations": strArray("Observations about the entity"),
			}, "name", "entityType", "observations")),
		}, "entities"),
	}, tool(s, func(sc memgraph.Scope, in createEntitiesIn) (any, error) {
		entities := make([]memgraph.Entity, 0, len(in.Entities))
		for _, e := range in.Entities {
			obs := make([]memgraph.Observation, 0, len(e.Observations))
			for _, o := range e.Observations {
				obs = append(obs, memgraph.Observation{Content: o})
			}
			entities = append(entities, memgraph.Entity{
				Name: e.Name, EntityType: e.EntityType, Observations: obs,
			})
		}
		return s.store.CreateEntities(sc, entities, s.source(sc))
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_relations",
		Description: "Create relations between entities (active voice)",
		InputSchema: object(map[string]*jsonschema.Schema{
			"relations": objArray(object(map[string]*jsonschema.Schema{
				"from":         str("Source entity"),
				"to":           str("Target entity"),
				"relationType": str("Relation type"),
			}, "from", "to", "relationType")),
		}, "relations"),
	}, tool(s, func(sc memgraph.Scope, in createRelationsIn) (any, error) {
		return s.store.CreateRelations(sc, toRelations(in.Relations), s.source(sc))
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "add_observations",
		Description: "Add observations to existing entities",
		InputSchema: object(map[string]*jsonschema.Schema{
			"observations": objArray(object(map[string]*jsonschema.Schema{
				"entityName": str("Target entity"),
				"contents":   strArray("New observations"),
				"confidence": numArray("Confidence scores (0-1) for each observation (optional)"),
			}, "entityName", "contents")),
		}, "observations"),
	}, tool(s, func(sc memgraph.Scope, in addObservationsIn) (any, error) {
		inputs := make([]memgraph.ObservationInput, 0, len(in.Observations))
		for _, o := range in.Observations {
			inputs = append(inputs, memgraph.ObservationInput{
				EntityName: o.EntityName, Contents: o.Contents, Confidence: o.Confidence,
			})
		}
		return s.store.AddObservations(sc, inputs, s.source(sc))
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_entities",
		Description: "Delete entities and their relations",
		InputSchema: object(map[string]*jsonschema.Schema{
			"entityNames": strArray("Entity names to delete"),
		}, "entityNames"),
	}, tool(s, func(sc memgraph.Scope, in deleteEntitiesIn) (any, error) {
		return s.store.DeleteEntities(sc, in.EntityNames)
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_observations",
		Description: "Delete specific observations from entities",
		InputSchema: object(map[string]*jsonschema.Schema{
			"deletions": objArray(object(map[string]*jsonschema.Schema{
				"entityName":   {Type: "string"},
				"observations": {Type: "array", Items: &jsonschema.Schema{Type: "string"}},
			}, "entityName", "observations")),
		}, "deletions"),
	}, tool(s, func(sc memgraph.Scope, in deleteObservationsIn) (any, error) {
		deletions := make([]memgraph.ObservationInput, 0, len(in.Deletions))
		for _, d := range in.Deletions {
			deletions = append(deletions, memgraph.ObservationInput{
				EntityName: d.EntityName, Contents: d.Observations,
			})
		}
		return s.store.DeleteObservations(sc, deletions)
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_relations",
		Description: "Delete specific relations",
		InputSchema: object(map[string]*jsonschema.Schema{
			"relations": objArray(object(map[string]*jsonschema.Schema{
				"from":         {Type: "string"},
				"to":           {Type: "string"},
				"relationType": {Type: "string"},
			}, "from", "to", "relationType")),
		}, "relations"),
	}, tool(s, func(sc memgraph.Scope, in deleteRelationsIn) (any, error) {
		return s.store.DeleteRelations(sc, toRelations(in.Relations))
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "read_graph",
		Description: "Read graph with controllable detail. Default returns summary with entity names, types, and first observation only.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"detailLevel": enumWithDefault(
				"Level of detail: minimal (names+counts), summary (default, includes first observation), full (everything)",
				memgraph.DetailSummary,
				memgraph.DetailMinimal, memgraph.DetailSummary, memgraph.DetailFull),
			"entityNames":     strArray("Optional: Get full details for specific entities only"),
			"includeArchived": boolWithDefault("Include archived entities (default false)", false),
			"includeMerged":   boolWithDefault("Include merged entities (default false)", false),
		}),
	}, tool(s, func(sc memgraph.Scope, in readGraphIn) (any, error) {
		return s.store.ReadGraph(sc, in.DetailLevel, in.EntityNames, in.IncludeArchived, in.IncludeMerged)
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_entity_details",
		Description: "Get complete details including all observations for specific entities",
		InputSchema: object(map[string]*jsonschema.Schema{
			"entityNames": strArray("Entity names to get full details for"),
		}, "entityNames"),
	}, tool(s, func(sc memgraph.Scope, in entityNamesIn) (any, error) {
		return s.store.GetEntityDetails(sc, in.EntityNames)
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "search_nodes",
		Description: "Search for entities strictly matching a query",
		InputSchema: object(map[string]*jsonschema.Schema{
			"query": str("Search query"),
			// 5, not 3. Upstream's advertised schema says 5 while its implementation
			// signature defaults to 3 — the two disagree (context.md E-8). The schema
			// is the contract a client can observe and the number the model reads, so
			// the schema wins. Do not "fix" this to 3.
			"maxObservations": numWithDefault("Max observations per entity (default 5)", 5),
		}, "query"),
	}, tool(s, func(sc memgraph.Scope, in searchNodesIn) (any, error) {
		return s.store.SearchNodes(sc, in.Query, in.MaxObservations)
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name: "semantic_search",
		// Upstream's description claims ModernColBERT embeddings. Ours cannot and
		// must not: the ranking is BM25 over entity names, types and observations
		// (context.md D-1). A description promising semantic understanding the
		// implementation does not have is a prompt-level falsehood that would make
		// the agent choose this tool for the wrong queries. tools_test.go fails the
		// build if "embedding" or "ColBERT" appears here.
		Description: "Ranked search across entity names, types, and observations using BM25 lexical relevance. " +
			"Better than search_nodes when you do not know the exact stored wording, since it ranks by term " +
			"relevance instead of requiring a literal substring match. Does not understand synonyms or paraphrase.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"query":     str("Natural language search query"),
			"k":         numWithDefault("Number of results to return (default 10)", 10),
			"threshold": numWithDefault("Minimum relevance score (0-1, default 0)", 0),
		}, "query"),
	}, tool(s, func(sc memgraph.Scope, in semanticSearchIn) (any, error) {
		return s.store.SemanticSearch(sc, in.Query, in.K, in.Threshold)
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "open_nodes",
		Description: "Open specific nodes by name with full details",
		InputSchema: object(map[string]*jsonschema.Schema{
			"names": strArray("Entity names to retrieve"),
		}, "names"),
	}, tool(s, func(sc memgraph.Scope, in namesIn) (any, error) {
		return s.store.OpenNodes(sc, in.Names)
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "merge_entities",
		Description: "Merge one entity into another, combining observations and updating relations",
		InputSchema: object(map[string]*jsonschema.Schema{
			"sourceName": str("Entity to merge from (will be deleted)"),
			"targetName": str("Entity to merge into (will be preserved)"),
		}, "sourceName", "targetName"),
	}, tool(s, func(sc memgraph.Scope, in mergeIn) (any, error) {
		return s.store.MergeEntities(sc, in.SourceName, in.TargetName)
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "archive_entity",
		Description: "Archive an entity (soft delete - hidden from normal queries)",
		InputSchema: object(map[string]*jsonschema.Schema{
			"entityName": str("Entity to archive"),
		}, "entityName"),
	}, tool(s, func(sc memgraph.Scope, in entityNameIn) (any, error) {
		return s.store.ArchiveEntity(sc, in.EntityName)
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "unarchive_entity",
		Description: "Unarchive a previously archived entity",
		InputSchema: object(map[string]*jsonschema.Schema{
			"entityName": str("Entity to unarchive"),
		}, "entityName"),
	}, tool(s, func(sc memgraph.Scope, in entityNameIn) (any, error) {
		return s.store.UnarchiveEntity(sc, in.EntityName)
	}))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_recent_changes",
		Description: "Get entities, relations, and observations created/modified within specified hours",
		InputSchema: object(map[string]*jsonschema.Schema{
			"hours": numWithDefault("Number of hours to look back (default 24)", 24),
		}),
	}, tool(s, func(sc memgraph.Scope, in recentChangesIn) (any, error) {
		return s.store.GetRecentChanges(sc, in.Hours)
	}))
}

func toRelations(in []relationIn) []memgraph.Relation {
	out := make([]memgraph.Relation, 0, len(in))
	for _, r := range in {
		out = append(out, memgraph.Relation{From: r.From, To: r.To, RelationType: r.RelationType})
	}
	return out
}
