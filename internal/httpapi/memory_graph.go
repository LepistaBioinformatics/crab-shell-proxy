package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
)

// Read-only HTTP access to the knowledge-graph memory, so the user interface can
// see and check what the agent has learned.
//
// The bot writes through MCP (POST /v1/mcp); this side only reads. There is no
// write route here — not undocumented, not registered. Curation from the interface
// (archive, delete, merge) is a later, additive change.
//
// Authorization is the SAME chain handleMemoryGet uses for MEMORY_CUSTOM.md:
// resolveSecretCaller then authorizeSecret. That is deliberate rather than
// convenient — the memory graph gets exactly the visibility rules the memory
// document already has, and a future change to that chain applies to both.
//
// Distinct from /v1/memory, which is MEMORY_CUSTOM.md and keeps its current
// meaning. The two surfaces are not unified.

// graphScope resolves the caller down to the workspace whose graph they may read,
// or writes the error response and returns false.
func (s *Server) graphScope(w http.ResponseWriter, r *http.Request) (memgraph.Scope, bool) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return memgraph.Scope{}, false
	}
	tenantID, err := uuid.Parse(r.URL.Query().Get("tenant_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" query parameter is required and must be a UUID`))
		return memgraph.Scope{}, false
	}
	subsAccID, err := uuid.Parse(r.URL.Query().Get("subs_acc_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" query parameter is required and must be a UUID`))
		return memgraph.Scope{}, false
	}
	key, ok := s.authorizeSecret(w, agent, ident, tenantID, subsAccID)
	if !ok {
		return memgraph.Scope{}, false
	}
	// agent-projects: each project keeps its own graph, so the read views have to
	// be told which one. An unknown id 404s here rather than falling back to the
	// main graph — showing one project's memory under another project's name is
	// worse than an error.
	_, projectID, ok := s.workspaceSegmentFor(w, r, key)
	if !ok {
		return memgraph.Scope{}, false
	}
	sc := scopeOf(key)
	sc.Project = projectID
	return sc, true
}

// scopeOf converts the proxy's workspace identity into the graph's. They are the same
// four fields; memgraph redeclares them so it need not import internal/docker.
func scopeOf(key docker.WorkspaceKey) memgraph.Scope {
	return memgraph.Scope{
		TenantID:  key.TenantID,
		SubsAccID: key.SubsAccID,
		Role:      key.Role,
		UserAccID: key.UserAccID,
	}
}

// handleMemoryGraphGet is read_graph.
func (s *Server) handleMemoryGraphGet(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.graphScope(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	detail := q.Get("detail_level")
	if detail == "" {
		detail = memgraph.DetailSummary
	}
	// Rejected rather than silently defaulted: a caller that sent "sumary" wants to
	// know, and quietly answering a different question is how a UI bug becomes a
	// support ticket about missing data.
	switch detail {
	case memgraph.DetailMinimal, memgraph.DetailSummary, memgraph.DetailFull:
	default:
		writeJSON(w, http.StatusBadRequest,
			errBody(`"detail_level" must be one of "minimal", "summary", "full"`))
		return
	}
	includeArchived, ok := boolParam(w, q.Get("include_archived"), "include_archived")
	if !ok {
		return
	}
	includeMerged, ok := boolParam(w, q.Get("include_merged"), "include_merged")
	if !ok {
		return
	}

	graph, err := s.MemoryGraph.ReadGraph(sc, detail, nil, includeArchived, includeMerged)
	if err != nil {
		s.logf("memory graph: read failed user=%s: %v", sc.UserAccID, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, graph)
}

// handleMemoryGraphNodes is open_nodes. Names arrive comma-separated, which keeps
// this a plain GET the UI can link to.
func (s *Server) handleMemoryGraphNodes(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.graphScope(w, r)
	if !ok {
		return
	}
	raw := r.URL.Query().Get("names")
	if strings.TrimSpace(raw) == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"names" query parameter is required`))
		return
	}
	names := make([]string, 0, 4)
	for _, n := range strings.Split(raw, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		writeJSON(w, http.StatusBadRequest, errBody(`"names" query parameter named no entity`))
		return
	}
	graph, err := s.MemoryGraph.OpenNodes(sc, names)
	if err != nil {
		s.logf("memory graph: open nodes failed user=%s: %v", sc.UserAccID, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, graph)
}

// handleMemoryGraphSearch runs the SAME ranking the semantic_search tool uses, so
// what the member sees in the interface and what the agent found are the same
// answer to the same query.
func (s *Server) handleMemoryGraphSearch(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.graphScope(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	query := q.Get("query")
	if strings.TrimSpace(query) == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"query" query parameter is required`))
		return
	}
	k, ok := intParam(w, q.Get("k"), "k", 10)
	if !ok {
		return
	}
	threshold, ok := floatParam(w, q.Get("threshold"), "threshold", 0)
	if !ok {
		return
	}
	res, err := s.MemoryGraph.SemanticSearch(sc, query, k, threshold)
	if err != nil {
		s.logf("memory graph: search failed user=%s: %v", sc.UserAccID, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleMemoryGraphRecent is get_recent_changes.
func (s *Server) handleMemoryGraphRecent(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.graphScope(w, r)
	if !ok {
		return
	}
	hours, ok := floatParam(w, r.URL.Query().Get("hours"), "hours", 24)
	if !ok {
		return
	}
	if hours <= 0 {
		writeJSON(w, http.StatusBadRequest, errBody(`"hours" must be greater than 0`))
		return
	}
	res, err := s.MemoryGraph.GetRecentChanges(sc, hours)
	if err != nil {
		s.logf("memory graph: recent changes failed user=%s: %v", sc.UserAccID, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// The three parameter parsers below all treat an ABSENT value as the default and a
// PRESENT-but-unparseable value as a 400. Coercing "abc" to the default would
// answer a question the caller did not ask.

func boolParam(w http.ResponseWriter, raw, name string) (bool, bool) {
	if raw == "" {
		return false, true
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"`+name+`" must be a boolean`))
		return false, false
	}
	return v, true
}

func intParam(w http.ResponseWriter, raw, name string, def int) (int, bool) {
	if raw == "" {
		return def, true
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"`+name+`" must be an integer`))
		return 0, false
	}
	return v, true
}

func floatParam(w http.ResponseWriter, raw, name string, def float64) (float64, bool) {
	if raw == "" {
		return def, true
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"`+name+`" must be a number`))
		return 0, false
	}
	return v, true
}
