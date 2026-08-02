package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
)

// graphServer is testServer plus a real memgraph.Store over a temp root, so these
// tests exercise the same authorization chain every other route test does and read
// real graph bytes rather than a fake.
func graphServer(t *testing.T, secret string) (*Server, *memgraph.Store, memgraph.Scope) {
	t.Helper()
	root := t.TempDir()
	store := memgraph.NewStore(root, func() time.Time { return time.UnixMilli(1_800_000_000_000) })

	s := testServer(scaffoldedOrch(), &fakeTurner{})
	s.Cfg = &config.Config{
		ContainerDataRoot:      root,
		ResolvedMCPTokenSecret: secret,
		Agents:                 s.Cfg.Agents,
	}
	s.MemoryGraph = store

	// The scope the goodHeaders profile resolves to: role comes from the agent the
	// service-name header selects, and the user is the profile's own accId.
	return s, store, memgraph.Scope{
		TenantID: tenantT, SubsAccID: subsX, Role: "alpha", UserAccID: accAlice,
	}
}

func graphReq(t *testing.T, path, query string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path+"?"+query, nil)
	for k, v := range goodHeaders(t) {
		r.Header.Set(k, v)
	}
	return r
}

const graphQuery = "tenant_id=" + tenantT + "&subs_acc_id=" + subsX

func seedGraph(t *testing.T, store *memgraph.Store, sc memgraph.Scope) {
	t.Helper()
	if _, err := store.CreateEntities(sc, []memgraph.Entity{
		{Name: "ledger", EntityType: "system", Observations: []memgraph.Observation{{Content: "written in Rust"}}},
		{Name: "alice", EntityType: "person", Observations: []memgraph.Observation{{Content: "reviews specs"}}},
	}, ""); err != nil {
		t.Fatalf("seed entities: %v", err)
	}
	if _, err := store.CreateRelations(sc, []memgraph.Relation{
		{From: "alice", To: "ledger", RelationType: "maintains"},
	}, ""); err != nil {
		t.Fatalf("seed relations: %v", err)
	}
}

// --- the four read routes (FR-6.1 – FR-6.4) ---

func TestMemoryGraphReadRoutes(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		query    string
		contains []string
	}{
		{"read_graph", "/v1/memory-graph", graphQuery, []string{"ledger", "firstObservation", "totalObservations"}},
		{"read_graph minimal", "/v1/memory-graph", graphQuery + "&detail_level=minimal", []string{"observationCount"}},
		{"read_graph full", "/v1/memory-graph", graphQuery + "&detail_level=full", []string{"written in Rust"}},
		{"open_nodes", "/v1/memory-graph/nodes", graphQuery + "&names=ledger,alice", []string{"ledger", "alice", "maintains"}},
		{"search", "/v1/memory-graph/search", graphQuery + "&query=rust", []string{"ledger", `"searchType"`, "lexical"}},
		{"recent", "/v1/memory-graph/recent", graphQuery + "&hours=48", []string{"recentEntities", "ledger"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, store, sc := graphServer(t, "secret")
			seedGraph(t, store, sc)

			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, graphReq(t, c.path, c.query))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			for _, want := range c.contains {
				if !strings.Contains(body, want) {
					t.Errorf("body is missing %q:\n%s", want, body)
				}
			}
		})
	}
}

func TestMemoryGraphReadRoutesRequireTenantAndSubs(t *testing.T) {
	paths := []string{
		"/v1/memory-graph", "/v1/memory-graph/nodes",
		"/v1/memory-graph/search", "/v1/memory-graph/recent",
	}
	queries := []string{
		"subs_acc_id=" + subsX, // no tenant_id
		"tenant_id=" + tenantT, // no subs_acc_id
		"tenant_id=not-a-uuid&subs_acc_id=" + subsX,
	}
	for _, p := range paths {
		for _, q := range queries {
			s, _, _ := graphServer(t, "secret")
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, graphReq(t, p, q))
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s?%s: status = %d, want 400", p, q, w.Code)
			}
		}
	}
}

// The chain is resolveSecretCaller + authorizeSecret — the same one /v1/memory
// uses. Asserted against handleMemoryGet's own answer rather than a guessed status,
// so the two surfaces cannot drift apart.
func TestMemoryGraphAuthorizationMatchesTheMemoryDocumentRoute(t *testing.T) {
	s, _, _ := graphServer(t, "secret")

	// A profile with no licensed resource for tenantT/subsX.
	unlicensed := headersFor(t, licensedProfile(
		accAlice, tenantT, subsX, "alpha", "read", false))

	memW := httptest.NewRecorder()
	memReq := httptest.NewRequest(http.MethodGet, "/v1/memory?"+graphQuery, nil)
	for k, v := range unlicensed {
		memReq.Header.Set(k, v)
	}
	s.Handler().ServeHTTP(memW, memReq)

	for _, p := range []string{
		"/v1/memory-graph", "/v1/memory-graph/nodes?names=x",
		"/v1/memory-graph/search?query=x", "/v1/memory-graph/recent",
	} {
		sep := "?"
		if strings.Contains(p, "?") {
			sep = "&"
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, p+sep+graphQuery, nil)
		for k, v := range unlicensed {
			r.Header.Set(k, v)
		}
		s.Handler().ServeHTTP(w, r)
		if w.Code != memW.Code {
			t.Errorf("%s: status = %d, but /v1/memory answers %d for the same caller — the two must share one chain",
				p, w.Code, memW.Code)
		}
	}
}

func TestMemoryGraphRejectsMalformedParameters(t *testing.T) {
	cases := []struct{ path, query string }{
		{"/v1/memory-graph", graphQuery + "&detail_level=sumary"},
		{"/v1/memory-graph", graphQuery + "&include_archived=maybe"},
		{"/v1/memory-graph", graphQuery + "&include_merged=maybe"},
		{"/v1/memory-graph/nodes", graphQuery},
		{"/v1/memory-graph/nodes", graphQuery + "&names=,,"},
		{"/v1/memory-graph/search", graphQuery},
		{"/v1/memory-graph/search", graphQuery + "&query=x&k=lots"},
		{"/v1/memory-graph/search", graphQuery + "&query=x&threshold=high"},
		{"/v1/memory-graph/recent", graphQuery + "&hours=soon"},
		{"/v1/memory-graph/recent", graphQuery + "&hours=0"},
		{"/v1/memory-graph/recent", graphQuery + "&hours=-3"},
	}
	for _, c := range cases {
		s, store, sc := graphServer(t, "secret")
		seedGraph(t, store, sc)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, graphReq(t, c.path, c.query))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s?%s: status = %d, want 400 (a bad value must not be coerced to a default)",
				c.path, c.query, w.Code)
		}
	}
}

func TestMemoryGraphOnAnEmptyGraphIsAnEmptyResult(t *testing.T) {
	s, _, _ := graphServer(t, "secret")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, graphReq(t, "/v1/memory-graph", graphQuery))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got struct {
		Entities  []any `json:"entities"`
		Relations []any `json:"relations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if got.Entities == nil || got.Relations == nil {
		t.Errorf("entities/relations came back as null, not []: %s", w.Body.String())
	}
}

// --- no write surface (FR-6.5) ---

func TestMemoryGraphHasNoWriteRoutes(t *testing.T) {
	s, _, _ := graphServer(t, "secret")
	paths := []string{
		"/v1/memory-graph", "/v1/memory-graph/nodes",
		"/v1/memory-graph/search", "/v1/memory-graph/recent",
	}
	for _, p := range paths {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(method, p+"?"+graphQuery, strings.NewReader("{}"))
			for k, v := range goodHeaders(t) {
				r.Header.Set(k, v)
			}
			s.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusMethodNotAllowed && w.Code != http.StatusNotFound {
				t.Errorf("%s %s: status = %d, want 405 or 404 — the UI has no write surface in v1",
					method, p, w.Code)
			}
		}
	}
}

// A source gate, because the risk is a later change adding a mutating route to this
// file without anybody revisiting FR-6.5.
func TestMemoryGraphFileRegistersNoMutatingHandler(t *testing.T) {
	src, err := os.ReadFile("memory_graph.go")
	if err != nil {
		t.Fatalf("read memory_graph.go: %v", err)
	}
	text := string(src)
	for _, bad := range []string{"WriteMemory", "store.Update", "CreateEntities", "DeleteEntities",
		"MergeEntities", "ArchiveEntity", "AddObservations"} {
		if strings.Contains(text, bad) {
			t.Errorf("memory_graph.go references %s; the UI surface is read-only in v1 (FR-6.5)", bad)
		}
	}
}

// NFR-5: the pre-existing memory document surface is untouched.
func TestTheMemoryDocumentRouteIsUnchanged(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatalf("read handlers.go: %v", err)
	}
	text := string(src)
	for _, want := range []string{
		`mux.HandleFunc("GET /v1/memory", s.handleMemoryGet)`,
		`mux.HandleFunc("PUT /v1/memory", s.handleMemoryPut)`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the MEMORY_CUSTOM.md route changed; %q is gone", want)
		}
	}
}

// --- the MCP endpoint's mounting (FR-4.5, AC-6) ---

func TestMCPEndpointIsMountedOnlyWithASecret(t *testing.T) {
	t.Run("mounted with a secret", func(t *testing.T) {
		s, _, sc := graphServer(t, "a-real-secret")
		_ = sc
		w := httptest.NewRecorder()
		// No token: the handler must answer 401, which proves it is MOUNTED (an
		// unmounted route would 404).
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/mcp", strings.NewReader("{}")))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 (mounted, refusing an unauthenticated caller)", w.Code)
		}
	})

	t.Run("absent without a secret", func(t *testing.T) {
		s, _, _ := graphServer(t, "")
		for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, httptest.NewRequest(method, "/v1/mcp", strings.NewReader("{}")))
			if w.Code != http.StatusNotFound {
				t.Errorf("%s status = %d, want 404 — an unconfigured deployment must expose no endpoint at all",
					method, w.Code)
			}
		}
	})
}

// The read routes must keep working with the secret unset: a member can still
// inspect a graph that already exists, they just get no new memories.
func TestReadRoutesSurviveWithoutASecret(t *testing.T) {
	s, store, sc := graphServer(t, "")
	seedGraph(t, store, sc)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, graphReq(t, "/v1/memory-graph", graphQuery))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ledger") {
		t.Errorf("an existing graph became unreadable when the feature was switched off:\n%s", w.Body.String())
	}
}
