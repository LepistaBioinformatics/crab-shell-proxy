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
	"mangrove_publish", "mangrove_share", "mangrove_timeline", "mangrove_react", "mangrove_admit",
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
