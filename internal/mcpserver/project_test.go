package mcpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// projectHarness is newHarness plus a project resolver and a header the client
// sends on every request -- the shape a ganglion container produces, where one
// server carries the member's token and the project travels per call.
func projectHarness(t *testing.T, owns map[string]bool, project string) *harness {
	t.Helper()
	store := memgraph.NewStore(t.TempDir(), func() time.Time {
		return time.UnixMilli(1_800_000_000_000)
	})
	logs := &logCapture{}
	deps := Deps{Store: store, Secret: testSecret, Logf: logs.logf}
	if owns != nil {
		deps.OwnsProject = func(_ memgraph.Scope, id string) (bool, error) {
			return owns[id], nil
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/mcp", projectHeader(project, NewHandler(deps)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{srv: srv, store: store, logs: logs}
}

func projectHeader(project string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if project != "" {
			r.Header.Set(ProjectHeader, project)
		}
		next.ServeHTTP(w, r)
	})
}

// addEntity writes one entity and reports whether the server refused.
//
// A handler error comes back as a RESULT with IsError set, not as a transport
// error -- that is the MCP contract, and asserting on the transport error alone
// is how a refusal test passes while the write goes through.
func addEntity(t *testing.T, sess *mcp.ClientSession, name string) error {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "create_entities",
		Arguments: map[string]any{"entities": []map[string]any{
			{"name": name, "entityType": "note", "observations": []string{}},
		}},
	})
	if err != nil {
		return err
	}
	if res.IsError {
		return errRefused
	}
	return nil
}

// errRefused is "the server said no", as distinct from "the call did not reach
// the server".
var errRefused = errors.New("refused by the server")

func names(t *testing.T, h *harness, sc memgraph.Scope) []string {
	t.Helper()
	g, err := h.store.Load(sc)
	if err != nil {
		t.Fatalf("Load %+v: %v", sc, err)
	}
	var out []string
	for _, e := range g.Entities {
		out = append(out, e.Name)
	}
	return out
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// The defect: a turn inside a project wrote into the member's GLOBAL graph,
// because the one server a ganglion container is given carries the member's
// token and nothing in the request said which project the work belonged to. A
// project's memory filling up with another's is invisible until someone reads it.
func TestAProjectScopedCallWritesIntoThatProjectsGraph(t *testing.T) {
	t.Parallel()
	h := projectHarness(t, map[string]bool{"seedtrial": true}, "seedtrial")
	sess := h.connect(t, mint(t, scopeA))

	if err := addEntity(t, sess, "in-the-project"); err != nil {
		t.Fatalf("create_entities: %v", err)
	}

	scoped := scopeA
	scoped.Project = "seedtrial"
	if got := names(t, h, scoped); !has(got, "in-the-project") {
		t.Errorf("the project's graph does not hold the write: %v", got)
	}
	if got := names(t, h, scopeA); has(got, "in-the-project") {
		t.Errorf("the write leaked into the member's global graph: %v", got)
	}
}

// A turn outside any project is unchanged, which is the regression bar: every
// picoclaw container and every ganglion turn in the agent's own workspace sends
// no header at all.
func TestACallWithNoProjectHeaderIsUnchanged(t *testing.T) {
	t.Parallel()
	h := projectHarness(t, map[string]bool{"seedtrial": true}, "")
	sess := h.connect(t, mint(t, scopeA))

	if err := addEntity(t, sess, "global"); err != nil {
		t.Fatalf("create_entities: %v", err)
	}
	if got := names(t, h, scopeA); !has(got, "global") {
		t.Errorf("the member's global graph does not hold the write: %v", got)
	}
}

// A token that already names a project WINS. That is picoclaw's shape, where
// each project agent holds its own project-scoped credential -- and a header
// must never be able to move a call out of the project its token names.
func TestATokensOwnProjectBeatsTheHeader(t *testing.T) {
	t.Parallel()
	h := projectHarness(t, map[string]bool{"other": true}, "other")

	tokenScope := scopeA
	tokenScope.Project = "mine"
	sess := h.connect(t, mint(t, tokenScope))

	if err := addEntity(t, sess, "stays-put"); err != nil {
		t.Fatalf("create_entities: %v", err)
	}
	if got := names(t, h, tokenScope); !has(got, "stays-put") {
		t.Errorf("the token's own project did not receive the write: %v", got)
	}
	elsewhere := scopeA
	elsewhere.Project = "other"
	if got := names(t, h, elsewhere); has(got, "stays-put") {
		t.Error("a header moved a call out of the project its token names")
	}
}

// A project the workspace does not have is REFUSED, not served the global graph.
// Falling through would be the original defect with an extra step.
func TestAnUnknownProjectIsRefused(t *testing.T) {
	t.Parallel()
	h := projectHarness(t, map[string]bool{"seedtrial": true}, "someone-elses")
	sess := h.connect(t, mint(t, scopeA))

	if err := addEntity(t, sess, "should-not-exist"); err == nil {
		t.Fatal("a call naming an unknown project succeeded")
	}
	if got := names(t, h, scopeA); has(got, "should-not-exist") {
		t.Error("the refused write landed in the global graph")
	}
	if !strings.Contains(h.logs.all(), "does not have") {
		t.Errorf("the refusal was not explained in the log:\n%s", h.logs.all())
	}
}

// No resolver wired means the header cannot be validated, so it is refused.
// Serving the member's global graph instead is the exact bug this fixes, and it
// is the failure a deployment would never notice.
func TestAProjectHeaderWithNoResolverIsRefused(t *testing.T) {
	t.Parallel()
	h := projectHarness(t, nil, "seedtrial")
	sess := h.connect(t, mint(t, scopeA))

	if err := addEntity(t, sess, "unvalidated"); err == nil {
		t.Fatal("a project-scoped call succeeded with no way to validate it")
	}
	if got := names(t, h, scopeA); has(got, "unvalidated") {
		t.Error("the write fell through to the global graph")
	}
}

// Two projects on one member stay apart. This is the whole point: a shared graph
// pours every other subject back into a project's context.
func TestTwoProjectsDoNotSeeEachOther(t *testing.T) {
	t.Parallel()
	owns := map[string]bool{"alpha-proj": true, "beta-proj": true}

	hA := projectHarness(t, owns, "alpha-proj")
	if err := addEntity(t, hA.connect(t, mint(t, scopeA)), "only-in-alpha"); err != nil {
		t.Fatal(err)
	}
	scopedA := scopeA
	scopedA.Project = "alpha-proj"
	scopedB := scopeA
	scopedB.Project = "beta-proj"

	if got := names(t, hA, scopedA); !has(got, "only-in-alpha") {
		t.Fatalf("alpha-proj graph = %v", got)
	}
	if got := names(t, hA, scopedB); has(got, "only-in-alpha") {
		t.Errorf("beta-proj can see alpha-proj's memory: %v", got)
	}
	if got := names(t, hA, scopeA); has(got, "only-in-alpha") {
		t.Errorf("the global graph can see a project's memory: %v", got)
	}
}
