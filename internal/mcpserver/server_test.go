package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/mcptoken"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
)

const testSecret = "test-mcp-signing-secret"

var (
	scopeA = memgraph.Scope{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "userA"}
	scopeB = memgraph.Scope{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "userB"}
)

type harness struct {
	srv   *httptest.Server
	store *memgraph.Store
	logs  *logCapture
}

type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCapture) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, format)
	for _, a := range args {
		if s, ok := a.(string); ok {
			l.lines = append(l.lines, s)
		}
	}
}

func (l *logCapture) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store := memgraph.NewStore(t.TempDir(), func() time.Time {
		return time.UnixMilli(1_800_000_000_000)
	})
	logs := &logCapture{}
	h := NewHandler(Deps{Store: store, Secret: testSecret, Logf: logs.logf})
	mux := http.NewServeMux()
	mux.Handle("/v1/mcp", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{srv: srv, store: store, logs: logs}
}

// connect drives the handler with the SDK's OWN client — the same SDK, at the same
// version, that picoclaw's client is built from. A hand-rolled JSON-RPC request
// would only prove we agree with ourselves about the wire format.
func (h *harness) connect(t *testing.T, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint: h.srv.URL + "/v1/mcp",
		HTTPClient: &http.Client{
			Transport: bearerTransport{token: token, base: http.DefaultTransport},
		},
	}
	sess, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	if b.token != "" {
		clone.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.base.RoundTrip(clone)
}

func mint(t *testing.T, sc memgraph.Scope) string {
	t.Helper()
	tok, err := mcptoken.Mint(testSecret, sc)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return tok
}

// --- handshake (FR-1.1, FR-1.2, FR-1.3) ---

func TestHandshakeAndToolListing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess := h.connect(t, mint(t, scopeA))

	res, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != 15 {
		names := make([]string, len(res.Tools))
		for i, tl := range res.Tools {
			names[i] = tl.Name
		}
		sort.Strings(names)
		t.Fatalf("advertised %d tools, want 15: %v", len(res.Tools), names)
	}
}

func TestServerIdentifiesItself(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess := h.connect(t, mint(t, scopeA))
	got := sess.InitializeResult().ServerInfo
	if got.Name != ServerName {
		t.Errorf("server name = %q, want %q", got.Name, ServerName)
	}
	if got.Version != ServerVersion {
		t.Errorf("server version = %q, want %q", got.Version, ServerVersion)
	}
}

// --- authorization (FR-4.4, NFR-1) ---

func TestUnauthorizedRequestsAreRefusedWithNoDetail(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	forged := mint(t, scopeA)
	forged = forged[:len(forged)-3] + "aaa"

	cases := []struct {
		name   string
		header string
	}{
		{"absent", ""},
		{"empty bearer", "Bearer "},
		{"wrong scheme", "Basic " + mint(t, scopeA)},
		{"garbage", "Bearer not-a-token"},
		{"forged mac", "Bearer " + forged},
		{"bearer only", "Bearer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
				req, err := http.NewRequest(method, h.srv.URL+"/v1/mcp", strings.NewReader(`{}`))
				if err != nil {
					t.Fatalf("NewRequest: %v", err)
				}
				if c.header != "" {
					req.Header.Set("Authorization", c.header)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("Do: %v", err)
				}
				body := make([]byte, 64)
				n, _ := resp.Body.Read(body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusUnauthorized {
					t.Errorf("%s status = %d, want 401", method, resp.StatusCode)
				}
				if n != 0 {
					t.Errorf("%s body = %q, want nothing (it must not explain why it refused)", method, body[:n])
				}
			}
		})
	}
}

// A token in a log line is a token in a log aggregator. Asserted rather than
// reviewed, because it is the kind of thing a later debugging session adds back.
func TestTheTokenIsNeverLogged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tok := mint(t, scopeA)

	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+tok+"-broken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	logs := h.logs.all()
	if logs == "" {
		t.Fatal("nothing was logged for a rejected request; the refusal should be observable")
	}
	if strings.Contains(logs, tok) || strings.Contains(logs, tok+"-broken") {
		t.Errorf("the token appears in the logs:\n%s", logs)
	}
}

// --- isolation (FR-4.2, AC-3) ---

func TestATokenReachesOnlyItsOwnWorkspace(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	if _, err := h.store.CreateEntities(scopeB, []memgraph.Entity{
		{Name: "b-secret", EntityType: "note", Observations: []memgraph.Observation{{Content: "belongs to B"}}},
	}, ""); err != nil {
		t.Fatalf("seed B: %v", err)
	}
	if _, err := h.store.CreateEntities(scopeA, []memgraph.Entity{
		{Name: "a-thing", EntityType: "note"},
	}, ""); err != nil {
		t.Fatalf("seed A: %v", err)
	}

	sess := h.connect(t, mint(t, scopeA))
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"read_graph", map[string]any{"detailLevel": "full", "includeArchived": true, "includeMerged": true}},
		{"search_nodes", map[string]any{"query": "b-secret"}},
		{"semantic_search", map[string]any{"query": "belongs to B"}},
		{"open_nodes", map[string]any{"names": []string{"b-secret"}}},
		{"get_entity_details", map[string]any{"entityNames": []string{"b-secret"}}},
		{"get_recent_changes", map[string]any{"hours": 100000}},
	} {
		res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
			Name: call.tool, Arguments: call.args,
		})
		if err != nil {
			t.Fatalf("CallTool %s: %v", call.tool, err)
		}
		text := resultText(res)
		if strings.Contains(text, "b-secret") || strings.Contains(text, "belongs to B") {
			t.Errorf("%s leaked member B's data to member A's token:\n%s", call.tool, text)
		}
	}

	// And A's write lands in A's graph only.
	if _, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "create_entities",
		Arguments: map[string]any{"entities": []map[string]any{{"name": "a-new", "entityType": "note", "observations": []string{}}}},
	}); err != nil {
		t.Fatalf("CallTool create_entities: %v", err)
	}
	bGraph, err := h.store.Load(scopeB)
	if err != nil {
		t.Fatalf("Load B: %v", err)
	}
	for _, e := range bGraph.Entities {
		if e.Name == "a-new" {
			t.Error("a write authorised by A's token landed in B's graph")
		}
	}
}

// FR-4.2's guarantee is structural: if no tool has a scope parameter, no caller can
// name another workspace. This walks every advertised schema rather than trusting a
// reading of tools.go.
func TestNoToolAcceptsAScopeParameter(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess := h.connect(t, mint(t, scopeA))
	res, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	forbidden := []string{
		"tenant", "tenantid", "subscription", "subsaccid", "accid", "account",
		"role", "agent", "user", "useraccid", "scope", "workspace", "email", "path",
	}
	for _, tl := range res.Tools {
		raw, err := json.Marshal(tl.InputSchema)
		if err != nil {
			t.Fatalf("marshal %s schema: %v", tl.Name, err)
		}
		var walk func(any)
		walk = func(v any) {
			switch node := v.(type) {
			case map[string]any:
				if props, ok := node["properties"].(map[string]any); ok {
					for name := range props {
						lower := strings.ToLower(name)
						for _, bad := range forbidden {
							if lower == bad {
								t.Errorf("tool %s takes a %q parameter; scope must come only from the token",
									tl.Name, name)
							}
						}
					}
				}
				for _, child := range node {
					walk(child)
				}
			case []any:
				for _, child := range node {
					walk(child)
				}
			}
		}
		var parsed any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("unmarshal %s schema: %v", tl.Name, err)
		}
		walk(parsed)
	}
}

// --- schema fidelity (FR-2, E-4, E-8) ---

type goldenProperty struct {
	Type           string                    `json:"type"`
	Enum           []any                     `json:"enum,omitempty"`
	Default        *json.RawMessage          `json:"default,omitempty"`
	ItemRequired   []string                  `json:"itemRequired,omitempty"`
	ItemProperties map[string]goldenProperty `json:"itemProperties,omitempty"`
}

type goldenTool struct {
	Required   []string                  `json:"required"`
	Properties map[string]goldenProperty `json:"properties"`
}

// wireSchema is a tool's InputSchema AS A CLIENT SEES IT.
//
// The comparison deliberately runs against the JSON that came back over the
// transport, not against the *jsonschema.Schema value in our own process: the
// contract we promise is what a client can observe. A first version of this test
// type-asserted the server-side Go type and failed, because the client decodes the
// schema into a generic map — the assertion was on the wrong side of the wire.
type wireSchema struct {
	Type       string              `json:"type"`
	Required   []string            `json:"required"`
	Properties map[string]wireProp `json:"properties"`
}

type wireProp struct {
	Type    string           `json:"type"`
	Enum    []any            `json:"enum"`
	Default *json.RawMessage `json:"default"`
	Items   *wireSchema      `json:"items"`
}

// decodeWireSchema re-encodes whatever the client handed us and decodes it into the
// shape above, so the test works the same whether the value arrived over HTTP or
// was built in process.
func decodeWireSchema(t *testing.T, tool string, v any) wireSchema {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal schema: %v", tool, err)
	}
	var out wireSchema
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: decode schema: %v", tool, err)
	}
	return out
}

func TestAdvertisedSchemasMatchUpstream(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "upstream-tools.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var golden map[string]json.RawMessage
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	delete(golden, "_comment")

	h := newHarness(t)
	sess := h.connect(t, mint(t, scopeA))
	listed, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	advertised := make(map[string]*mcp.Tool, len(listed.Tools))
	for _, tl := range listed.Tools {
		advertised[tl.Name] = tl
	}

	if len(golden) != len(advertised) {
		t.Errorf("golden has %d tools, server advertises %d", len(golden), len(advertised))
	}
	for name, gRaw := range golden {
		t.Run(name, func(t *testing.T) {
			tl, ok := advertised[name]
			if !ok {
				t.Fatalf("tool %q is in upstream but not advertised", name)
			}
			var want goldenTool
			if err := json.Unmarshal(gRaw, &want); err != nil {
				t.Fatalf("parse golden entry: %v", err)
			}
			compareSchema(t, name, want, decodeWireSchema(t, name, tl.InputSchema))
		})
	}
	for name := range advertised {
		if _, ok := golden[name]; !ok {
			t.Errorf("tool %q is advertised but not in the upstream contract", name)
		}
	}
}

func compareSchema(t *testing.T, tool string, want goldenTool, got wireSchema) {
	t.Helper()
	if got.Type != "object" {
		t.Errorf("%s: schema type = %q, want object", tool, got.Type)
	}
	if !sameStrings(got.Required, want.Required) {
		t.Errorf("%s: required = %v, want %v", tool, got.Required, want.Required)
	}
	if len(got.Properties) != len(want.Properties) {
		t.Errorf("%s: has %d properties, want %d (%v vs %v)",
			tool, len(got.Properties), len(want.Properties),
			propNames(got.Properties), wantNames(want.Properties))
	}
	for name, wp := range want.Properties {
		gp, ok := got.Properties[name]
		if !ok {
			t.Errorf("%s: missing property %q", tool, name)
			continue
		}
		compareProperty(t, tool+"."+name, wp, gp)
	}
	for name := range got.Properties {
		if _, ok := want.Properties[name]; !ok {
			t.Errorf("%s: advertises property %q that upstream does not have", tool, name)
		}
	}
}

func compareProperty(t *testing.T, path string, want goldenProperty, got wireProp) {
	t.Helper()
	if got.Type != want.Type {
		t.Errorf("%s: type = %q, want %q", path, got.Type, want.Type)
	}
	if want.Enum != nil && !reflect.DeepEqual(got.Enum, want.Enum) {
		t.Errorf("%s: enum = %v, want %v", path, got.Enum, want.Enum)
	}
	if want.Default != nil {
		if got.Default == nil {
			t.Errorf("%s: no default declared, want %s — the SDK applies schema defaults, so an omitted one silently changes behaviour",
				path, string(*want.Default))
		} else if !jsonEqual(*want.Default, *got.Default) {
			t.Errorf("%s: default = %s, want %s", path, string(*got.Default), string(*want.Default))
		}
	}
	if len(want.ItemProperties) > 0 || len(want.ItemRequired) > 0 {
		if got.Items == nil {
			t.Fatalf("%s: no items schema, want an object item", path)
		}
		if !sameStrings(got.Items.Required, want.ItemRequired) {
			t.Errorf("%s: item required = %v, want %v", path, got.Items.Required, want.ItemRequired)
		}
		if len(got.Items.Properties) != len(want.ItemProperties) {
			t.Errorf("%s: item has %d properties, want %d", path,
				len(got.Items.Properties), len(want.ItemProperties))
		}
		for name, wip := range want.ItemProperties {
			gip, ok := got.Items.Properties[name]
			if !ok {
				t.Errorf("%s: item missing property %q", path, name)
				continue
			}
			compareProperty(t, path+"[]."+name, wip, gip)
		}
	}
}

func sameStrings(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return reflect.DeepEqual(x, y)
}

func jsonEqual(a, b json.RawMessage) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func propNames(m map[string]wireProp) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func wantNames(m map[string]goldenProperty) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- D-1: the description must not promise what BM25 cannot deliver ---

func TestSemanticSearchDoesNotClaimEmbeddings(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess := h.connect(t, mint(t, scopeA))
	listed, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tl := range listed.Tools {
		if tl.Name != "semantic_search" {
			continue
		}
		lower := strings.ToLower(tl.Description)
		for _, banned := range []string{"colbert", "embedding", "neural", "vector"} {
			if strings.Contains(lower, banned) {
				t.Errorf("semantic_search's description mentions %q; the ranking is BM25 and the description must not imply otherwise:\n%s",
					banned, tl.Description)
			}
		}
		if !strings.Contains(lower, "bm25") {
			t.Errorf("semantic_search's description does not say what the ranking actually is:\n%s", tl.Description)
		}
		return
	}
	t.Fatal("semantic_search was not advertised")
}

func TestSemanticSearchResponseReportsLexicalRanking(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if _, err := h.store.CreateEntities(scopeA, []memgraph.Entity{
		{Name: "ledger", EntityType: "system", Observations: []memgraph.Observation{{Content: "written in Rust"}}},
	}, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sess := h.connect(t, mint(t, scopeA))
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "semantic_search", Arguments: map[string]any{"query": "rust"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	text := resultText(res)
	if !strings.Contains(text, `"searchType":"lexical"`) && !strings.Contains(text, `"searchType": "lexical"`) {
		t.Errorf("response does not report lexical ranking:\n%s", text)
	}
	if !strings.Contains(text, "ledger") {
		t.Errorf("response did not find the seeded entity:\n%s", text)
	}
}

// --- defaults actually applied (E-4) ---

// The whole reason for hand-written schemas is that the SDK applies their declared
// defaults before the handler runs. If that stopped working, read_graph would fall
// through to a Go zero value instead of "summary" — a silent behaviour change with
// no compile error.
func TestSchemaDefaultsAreAppliedByTheSDK(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if _, err := h.store.CreateEntities(scopeA, []memgraph.Entity{
		{Name: "a", EntityType: "person", Observations: []memgraph.Observation{
			{Content: "first"}, {Content: "second"},
		}},
	}, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sess := h.connect(t, mint(t, scopeA))

	// No arguments at all: detailLevel must default to "summary", which is the only
	// projection carrying firstObservation.
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "read_graph", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool read_graph: %v", err)
	}
	text := resultText(res)
	if !strings.Contains(text, "firstObservation") {
		t.Errorf("read_graph with no arguments did not produce the summary projection:\n%s", text)
	}

	// get_recent_changes' 24h default must exclude an entity stamped long ago.
	if err := h.store.Update(scopeA, func(g *memgraph.Graph) error {
		g.Entities = append(g.Entities, memgraph.Entity{
			Name: "ancient", EntityType: "note", CreatedAt: 1,
		})
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	res, err = sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "get_recent_changes", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool get_recent_changes: %v", err)
	}
	if strings.Contains(resultText(res), "ancient") {
		t.Errorf("get_recent_changes ignored its 24h default:\n%s", resultText(res))
	}
}

// --- tool errors reach the agent as errors (FR-3.3) ---

func TestAddObservationsToAMissingEntityIsAToolError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess := h.connect(t, mint(t, scopeA))
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "add_observations",
		Arguments: map[string]any{
			"observations": []map[string]any{{"entityName": "ghost", "contents": []string{"x"}}},
		},
	})
	if err != nil {
		t.Fatalf("CallTool returned a protocol error, want a tool error: %v", err)
	}
	if !res.IsError {
		t.Errorf("result is not marked as an error:\n%s", resultText(res))
	}
	if !strings.Contains(resultText(res), "ghost") {
		t.Errorf("the error does not name the missing entity:\n%s", resultText(res))
	}
}

// --- round trip through the tools (AC-2 at unit level) ---

func TestStoreThenRecallThroughTheTools(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess := h.connect(t, mint(t, scopeA))
	ctx := context.Background()

	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: "create_entities",
		Arguments: map[string]any{"entities": []map[string]any{
			{"name": "samuel", "entityType": "person", "observations": []string{"prefers pt-BR"}},
		}},
	}); err != nil {
		t.Fatalf("create_entities: %v", err)
	}
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: "add_observations",
		Arguments: map[string]any{"observations": []map[string]any{
			{"entityName": "samuel", "contents": []string{"runs the crab stack"}, "confidence": []float64{0.9}},
		}},
	}); err != nil {
		t.Fatalf("add_observations: %v", err)
	}

	// A brand-new session, as a restarted container would open — the graph is on
	// disk, not in the session.
	fresh := h.connect(t, mint(t, scopeA))
	res, err := fresh.CallTool(ctx, &mcp.CallToolParams{
		Name: "open_nodes", Arguments: map[string]any{"names": []string{"samuel"}},
	})
	if err != nil {
		t.Fatalf("open_nodes: %v", err)
	}
	text := resultText(res)
	for _, want := range []string{"prefers pt-BR", "runs the crab stack"} {
		if !strings.Contains(text, want) {
			t.Errorf("recall is missing %q:\n%s", want, text)
		}
	}
}

func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	if b.Len() == 0 && res.StructuredContent != nil {
		raw, _ := json.Marshal(res.StructuredContent)
		b.Write(raw)
	}
	return b.String()
}

// NFR-1: the body is capped BEFORE anything parses it, because this route is the
// only one on the proxy reachable from the container network without mycelium in
// front. A valid token does not buy the right to send an unbounded body.
func TestAnOversizedBodyIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tok := mint(t, scopeA)

	oversized := strings.Repeat("x", MaxRequestBytes+1024)
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"pad":"`+oversized+`"}}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// A connection-level refusal is an acceptable outcome for an over-cap body.
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		t.Errorf("status = %d; an over-cap body must not be processed successfully", resp.StatusCode)
	}
}

// And a normal-sized call still works, so the cap is not simply breaking the route.
func TestABodyUnderTheCapStillWorks(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess := h.connect(t, mint(t, scopeA))
	// ~64 KiB of observations: comfortably real, comfortably under the cap.
	contents := make([]string, 0, 64)
	for i := 0; i < 64; i++ {
		contents = append(contents, strings.Repeat("o", 1000))
	}
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "create_entities",
		Arguments: map[string]any{"entities": []map[string]any{
			{"name": "bulky", "entityType": "note", "observations": contents},
		}},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Errorf("a legitimate batch was refused:\n%s", resultText(res))
	}
}
