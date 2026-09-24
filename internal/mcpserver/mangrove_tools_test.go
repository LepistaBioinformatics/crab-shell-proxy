package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/mangrove"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
)

// newHarnessWithMangrove mirrors newHarness, with a mangrove client threaded in. It is
// a separate constructor rather than a parameter on the original so that every
// existing test keeps proving the unconfigured shape -- including the golden
// schema test, which counts advertised tools and would otherwise have to be
// taught about a feature it has nothing to do with.
func newHarnessWithMangrove(t *testing.T, rc *mangrove.Client) *harness {
	t.Helper()
	store := memgraph.NewStore(t.TempDir(), func() time.Time {
		return time.UnixMilli(1_800_000_000_000)
	})
	logs := &logCapture{}
	h := NewHandler(Deps{Store: store, Secret: testSecret, Logf: logs.logf, Mangrove: rc})
	mux := http.NewServeMux()
	mux.Handle("/v1/mcp", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{srv: srv, store: store, logs: logs}
}

// callPublish is a minimal valid mangrove_publish call.
func callPublish() *mcp.CallToolParams {
	return &mcp.CallToolParams{
		Name: "mangrove_publish",
		Arguments: map[string]any{
			"type":    "MemoryNote",
			"cell":    "soil-ph",
			"content": "5.8",
		},
	}
}

// mangroveNames is the surface this feature adds. Pinned as a list rather than
// counted, so adding a tool without deciding to is a failing test.
var mangroveNames = []string{
	// FOUR, NOT FIVE. `mangrove_admit` is gone with the hold it cleared -- which
	// was never a hold on anything, since a held item travelled with its object
	// and this tool let an agent clear its own. Emitting a read receipt is
	// `mangrove_react` with kind `read`, signed as the SERVICE, which is a
	// different fact from its person having opened the thing.
	"mangrove_publish", "mangrove_share", "mangrove_timeline", "mangrove_react",
}

// listToolNames drives the real MCP client against a handler built with the
// given mangrove client, and returns every advertised tool name.
func listToolNames(t *testing.T, mangroveClient *mangrove.Client) []string {
	t.Helper()
	h := newHarnessWithMangrove(t, mangroveClient)
	sess := h.connect(t, mint(t, scopeA))
	listed, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make([]string, 0, len(listed.Tools))
	for _, tl := range listed.Tools {
		names = append(names, tl.Name)
	}
	return names
}

// THE OFF SWITCH. Unconfigured, not one mangrove tool exists -- absent, not
// present-and-refusing.
//
// This is not tidiness. Every registered tool is described to the model on
// every turn, so a tool that can only fail costs context forever in every
// deployment that never wanted the feature. It is also the whole of an
// EXPERIMENTAL feature's blast radius: unset the config and the stack is the
// stack it was before.
func TestNoMangroveToolsWhenUnconfigured(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client *mangrove.Client
	}{
		{"nil client", nil},
		{"no base url", mangrove.New("", "secret")},
		{"no token", mangrove.New("http://mangrove:8090", "")},
		{"neither", mangrove.New("", "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			names := listToolNames(t, tc.client)
			for _, got := range names {
				if strings.HasPrefix(got, "mangrove_") {
					t.Errorf("mangrove tool %q was registered with the mangrove %s", got, tc.name)
				}
			}
		})
	}
}

// Configured, exactly the five appear -- no more, no fewer.
func TestMangroveToolsAppearWhenConfigured(t *testing.T) {
	names := listToolNames(t, mangrove.New("http://mangrove:8090", "secret"))

	seen := map[string]bool{}
	for _, n := range names {
		if strings.HasPrefix(n, "mangrove_") {
			seen[n] = true
		}
	}
	for _, want := range mangroveNames {
		if !seen[want] {
			t.Errorf("%s was not advertised", want)
		}
		delete(seen, want)
	}
	for extra := range seen {
		t.Errorf("unexpected mangrove tool %q -- the surface is a budget, not a wish list", extra)
	}
}

// NO NAME COLLIDES, and this is the test that matters most for the delivery
// decision. The harness registers a remote server's tools under their own names
// and REFUSES ITS BOOT on a collision -- so a duplicate here would not fail a
// call, it would stop a member's container from starting, later, for one
// member, with no relation in time to the change that caused it.
func TestMangroveToolNamesDoNotCollide(t *testing.T) {
	names := listToolNames(t, mangrove.New("http://mangrove:8090", "secret"))
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("tool %q is advertised twice", n)
		}
		seen[n] = true
	}
	if len(names) == 0 {
		t.Fatal("no tools advertised at all")
	}
}

// Turning the mangrove on must not disturb the surface that was already there.
func TestEnablingTheMangroveAddsOnlyMangroveTools(t *testing.T) {
	before := listToolNames(t, nil)
	after := listToolNames(t, mangrove.New("http://mangrove:8090", "secret"))

	beforeSet := map[string]bool{}
	for _, n := range before {
		beforeSet[n] = true
	}
	for _, n := range after {
		if !beforeSet[n] && !strings.HasPrefix(n, "mangrove_") {
			t.Errorf("enabling the mangrove added non-mangrove tool %q", n)
		}
	}
	if len(after) != len(before)+len(mangroveNames) {
		t.Errorf("advertised %d tools with the mangrove on and %d with it off; want exactly %d more",
			len(after), len(before), len(mangroveNames))
	}
}

// AN AGENT CANNOT PROVE IT MAY ADDRESS A TENANT, so the MCP path must never
// claim it can. This drives a real tool call through the real client and reads
// what actually went on the wire.
//
// If tenantLicensed ever leaked out of this path, the mangrove would accept a
// tenant-wide broadcast from a turn steered by untrusted text -- which is the
// single worst thing this feature could do.
func TestAgentCallsNeverClaimTenantLicence(t *testing.T) {
	var bodies []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	h := newHarnessWithMangrove(t, mangrove.New(upstream.URL, "secret"))
	sess := h.connect(t, mint(t, scopeA))

	if _, err := sess.CallTool(context.Background(), callPublish()); err != nil {
		t.Fatalf("mangrove_publish: %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("the mangrove saw %d calls, want 1", len(bodies))
	}
	body := bodies[0]
	if _, present := body["tenantLicensed"]; present {
		t.Errorf("an agent call carried tenantLicensed=%v", body["tenantLicensed"])
	}
	if body["as"] != "service" {
		t.Errorf("an agent call acted as %q, want service", body["as"])
	}

	// And the tuple is the one the TOKEN carried, not anything the call named.
	tup, _ := body["tuple"].(map[string]any)
	if tup["userAccId"] != scopeA.UserAccID || tup["subsAccId"] != scopeA.SubsAccID {
		t.Errorf("tuple = %v, want the token's scope %+v", tup, scopeA)
	}
}

// The network exists to share MEMORY. This is the shape that does it: the agent
// names entities from its own graph and the post carries them, with the
// relations among them, as a fragment the recipient can merge.
func TestAnAgentPublishesAGraphFragment(t *testing.T) {
	var bodies []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	h := newHarnessWithMangrove(t, mangrove.New(upstream.URL, "secret"))
	if _, err := h.store.CreateEntities(scopeA, []memgraph.Entity{
		{Name: "Rhizophora", EntityType: "species",
			Observations: []memgraph.Observation{{Content: "salt tolerant"}}},
		{Name: "Mangrove", EntityType: "biome"},
		{Name: "Unrelated", EntityType: "noise"},
	}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CreateRelations(scopeA, []memgraph.Relation{
		{From: "Rhizophora", To: "Mangrove", RelationType: "grows in"},
		{From: "Rhizophora", To: "Unrelated", RelationType: "not shared"},
	}, "test"); err != nil {
		t.Fatal(err)
	}

	sess := h.connect(t, mint(t, scopeA))
	if _, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "mangrove_publish",
		Arguments: map[string]any{"type": "MemoryNote", "entities": []string{"Rhizophora", "Mangrove"}},
	}); err != nil {
		t.Fatalf("publish entities: %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("the mangrove saw %d calls, want 1", len(bodies))
	}
	obj, _ := bodies[0]["object"].(map[string]any)
	if obj["mediaType"] != mangrove.GraphFragmentMediaType {
		t.Errorf("mediaType = %v, want the fragment type", obj["mediaType"])
	}
	if !strings.HasPrefix(obj["cell"].(string), "graph:") {
		t.Errorf("cell = %v; a fragment derives its cell and must not collide with a prose note", obj["cell"])
	}

	var frag struct {
		Entities  []memgraph.Entity   `json:"entities"`
		Relations []memgraph.Relation `json:"relations"`
	}
	if err := json.Unmarshal([]byte(obj["content"].(string)), &frag); err != nil {
		t.Fatalf("content is not a fragment: %v", err)
	}
	if len(frag.Entities) != 2 {
		t.Errorf("shared %d entities, want the two that were named", len(frag.Entities))
	}
	// THE EDGE TO AN UNSHARED NODE MUST NOT TRAVEL. A fragment with a dangling
	// relation would name an entity the recipient was never given.
	if len(frag.Relations) != 1 || frag.Relations[0].To != "Mangrove" {
		t.Errorf("relations = %v, want only the one between the two shared entities", frag.Relations)
	}
}

// EXACTLY ONE KIND. `cell` is the reduction key and each shape derives it
// differently; two at once would leave a reader asking which part is the
// content.
func TestAnAgentCannotPublishTwoKindsAtOnce(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	h := newHarnessWithMangrove(t, mangrove.New(upstream.URL, "secret"))
	if _, err := h.store.CreateEntities(scopeA,
		[]memgraph.Entity{{Name: "Rhizophora", EntityType: "species"}}, "test"); err != nil {
		t.Fatal(err)
	}
	sess := h.connect(t, mint(t, scopeA))

	for _, args := range []map[string]any{
		{"type": "MemoryNote", "cell": "c", "content": "prose", "entities": []string{"Rhizophora"}},
		{"type": "MemoryNote", "cell": "c"}, // neither
	} {
		res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "mangrove_publish", Arguments: args,
		})
		if err == nil && (res == nil || !res.IsError) {
			t.Errorf("publishing %v was accepted", args)
		}
	}
}

// Naming entities that are not in the graph is an error the agent can act on,
// not an empty post nobody notices.
func TestPublishingEntitiesNobodyHasIsRefused(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	h := newHarnessWithMangrove(t, mangrove.New(upstream.URL, "secret"))
	sess := h.connect(t, mint(t, scopeA))
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "mangrove_publish",
		Arguments: map[string]any{"type": "MemoryNote", "entities": []string{"Nothing"}},
	})
	if err == nil && (res == nil || !res.IsError) {
		t.Error("publishing entities nobody has was accepted")
	}
}

// WHAT THE TOOL SAYS IT DOES IS WHAT THE MODEL WILL REPORT HAVING DONE.
//
// `mangrove_admit`'s description claimed for months that it took a memory "into
// your own memory". It never did, and the claim became actively harmful once
// merging a shared fragment into the graph shipped as a PERSON's act (AD-031):
// a model told a tool writes to its memory will report a merge that did not
// happen. The tool is gone now, and the trap it fell into has simply moved to
// the tool beside it -- `mangrove_timeline` is what an agent calls to see
// somebody else's memory, and reading is the thing most easily mistaken for
// remembering.
//
// Prose is the one part of a tool nothing else checks, which is how it stayed
// wrong for months. This pins the correction rather than the wording.
func TestTheTimelineDoesNotClaimToWriteMemory(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	h := newHarnessWithMangrove(t, mangrove.New(upstream.URL, "secret"))
	sess := h.connect(t, mint(t, scopeA))
	tools, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var timeline string
	for _, tl := range tools.Tools {
		if tl.Name == "mangrove_admit" {
			t.Error("mangrove_admit is advertised again; the hold it cleared no longer exists")
		}
		if tl.Name == "mangrove_timeline" {
			timeline = tl.Description
		}
	}
	if timeline == "" {
		t.Fatal("mangrove_timeline is not advertised")
	}
	if !strings.Contains(strings.ToLower(timeline), "not in your memory") {
		t.Errorf("the disclaimer is gone; a model will report remembering what it only read:\n%s", timeline)
	}
	// AND NOTHING LEFT SAYS THE OLD THING. The phrase is what the deleted tool
	// claimed, and it must not reappear on whatever inherits the job.
	for _, tl := range tools.Tools {
		if strings.Contains(strings.ToLower(tl.Description), "into your own memory") {
			t.Errorf("%s makes the claim mangrove_admit was deleted for:\n%s", tl.Name, tl.Description)
		}
	}
}

// The agent's publish tool must not offer a group as an addressee, because the
// gate refuses one -- a suggestion the model cannot act on is one it will keep
// trying.
func TestPublishToolDoesNotOfferAGroup(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	h := newHarnessWithMangrove(t, mangrove.New(upstream.URL, "secret"))
	sess := h.connect(t, mint(t, scopeA))
	tools, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		if tl.Name != "mangrove_publish" {
			continue
		}
		if !strings.Contains(tl.Description, "cannot address a group") {
			t.Errorf("publish does not say groups are closed to it:\n%s", tl.Description)
		}
		if strings.Contains(tl.Description, "mangrove:group:subscription") {
			t.Errorf("publish still offers a group id as an example:\n%s", tl.Description)
		}
	}
}
