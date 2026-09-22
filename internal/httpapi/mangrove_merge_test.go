package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/mangrove"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
)

// A stub mangrove that answers `received` with one shared fragment and records
// whether the proxy admitted it afterwards.
type fragmentMangrove struct {
	srv      *httptest.Server
	admitted []string
	content  string
	media    string
	held     bool
}

func newFragmentMangrove(t *testing.T, content, media string, held bool) *fragmentMangrove {
	t.Helper()
	fm := &fragmentMangrove{content: content, media: media, held: held}
	fm.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/v1/timeline":
			obj := map[string]any{
				"id": "mangrove:obj:frag", "type": "MemoryNote",
				"cell": "graph:abc", "content": fm.content, "mediaType": fm.media,
			}
			out := map[string]any{"reading": "received", "claims": []any{}, "held": []any{}}
			if fm.held {
				out["held"] = []any{map[string]any{"activityId": "mangrove:act:7", "object": obj}}
			} else {
				out["claims"] = []any{map[string]any{"object": obj}}
			}
			_ = json.NewEncoder(w).Encode(out)
		case "/internal/v1/admit":
			var body struct {
				ActivityID string `json:"activityId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			fm.admitted = append(fm.admitted, body.ActivityID)
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(fm.srv.Close)
	return fm
}

func mergeServer(t *testing.T, fm *fragmentMangrove) (*Server, *memgraph.Store, memgraph.Scope) {
	t.Helper()
	root := t.TempDir()
	store := memgraph.NewStore(root, func() time.Time { return time.UnixMilli(1_800_000_000_000) })
	s := &Server{
		Cfg: &config.Config{
			ContainerDataRoot:     root,
			MangroveBaseURL:       fm.srv.URL,
			ResolvedMangroveToken: mangroveTok,
			Agents: map[string]config.Agent{
				"alpha": {Key: "alpha", ServiceName: "picoclaw-alpha", ResolvedToken: "bearer",
					Mode: config.ModeContinuous},
			},
		},
		Resolver: identity.NewSDKResolver(), Mgr: newFakeOrch(), Pico: &fakeTurner{},
		MemoryGraph: store,
	}
	return s, store, memgraph.Scope{
		TenantID: tenantT, SubsAccID: subsX, Role: "alpha", UserAccID: accAlice,
	}
}

func fragmentJSON(t *testing.T, ents []memgraph.Entity, rels []memgraph.Relation) string {
	t.Helper()
	b, err := json.Marshal(graphFragment{Entities: ents, Relations: rels})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func merge(t *testing.T, s *Server, objectID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, memberReq(t, http.MethodPost, "/v1/mangrove/merge"+mangroveScope,
		plainMember(), `{"objectId":"`+objectID+`"}`))
	return w
}

// THE HALF THAT MAKES THE FEATURE WORTH HAVING.
func TestAPersonMergingAFragmentChangesTheirGraph(t *testing.T) {
	frag := fragmentJSON(t,
		[]memgraph.Entity{
			{Name: "Rhizophora", EntityType: "species", Observations: []memgraph.Observation{{Content: "salt tolerant"}}},
			{Name: "Mangrove", EntityType: "biome", Observations: []memgraph.Observation{{Content: "tidal"}}},
		},
		[]memgraph.Relation{{From: "Rhizophora", To: "Mangrove", RelationType: "grows in"}},
	)
	fm := newFragmentMangrove(t, frag, mangrove.GraphFragmentMediaType, true)
	s, store, sc := mergeServer(t, fm)

	w := merge(t, s, "mangrove:obj:frag")
	if w.Code != http.StatusOK {
		t.Fatalf("merge: %d %s", w.Code, w.Body.String())
	}
	// The member is told what actually landed, so "merged" is not a word they
	// have to take on trust.
	var report map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]float64{
		"entitiesCreated": 2, "observationsAdded": 2, "relationsCreated": 1,
	} {
		if got, _ := report[field].(float64); got != want {
			t.Errorf("%s = %v, want %v (%v)", field, report[field], want, report)
		}
	}

	g, err := store.Load(sc)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Entities) != 2 {
		t.Fatalf("graph holds %d entities, want 2", len(g.Entities))
	}
	if len(g.Relations) != 1 {
		t.Fatalf("graph holds %d relations, want 1", len(g.Relations))
	}
	// Borrowed memory has to be tellable from your own.
	for _, e := range g.Entities {
		if e.SourceSessionID == "" {
			t.Errorf("%s carries no source; a later reader cannot tell it was borrowed", e.Name)
		}
		for _, o := range e.Observations {
			if o.SourceSessionID == "" {
				t.Errorf("an observation on %s carries no source", e.Name)
			}
		}
	}
	// Taking it also admits it: leaving it held would go on offering an Admit
	// for something already in the member's graph.
	if len(fm.admitted) != 1 || fm.admitted[0] != "mangrove:act:7" {
		t.Errorf("admitted = %v, want the held activity", fm.admitted)
	}
}

// THE HALF THAT KEEPS IT SAFE, and neither proves the property alone.
//
// `mangrove_admit` lets an agent admit for itself with no human in the loop. If
// admitting merged, an agent steered by untrusted text could publish entities,
// address a peer agent, and have that peer write them into its own graph -- and
// the graph is what steers later turns.
//
// There is no agent path to the merge route at all: it is a member route behind
// the gateway, and an agent has no profile to present. This asserts the part
// that could silently change -- that the agent's own admit still writes nothing.
func TestAnAgentAdmittingTheSameFragmentChangesNoGraph(t *testing.T) {
	frag := fragmentJSON(t,
		[]memgraph.Entity{{Name: "Rhizophora", EntityType: "species"}}, nil)
	fm := newFragmentMangrove(t, frag, mangrove.GraphFragmentMediaType, true)
	s, store, sc := mergeServer(t, fm)

	// The agent's admit, exactly as mangrove_admit performs it.
	key := docker.WorkspaceKey{TenantID: tenantT, SubsAccID: subsX, Role: "alpha", UserAccID: accAlice}
	if _, err := s.mangroveClient().Admit(context.Background(),
		s.mangroveTuple(key), mangrove.AsService, "mangrove:act:7"); err != nil {
		t.Fatalf("admit: %v", err)
	}

	g, err := store.Load(sc)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Entities) != 0 {
		t.Fatalf("an agent's admit wrote %d entities into the graph; only a person may merge", len(g.Entities))
	}
}

// FR-D1: the request body cannot carry an entity. That is the whole reason this
// route is allowed to exist beside a file that says the graph has no write route.
func TestTheMergeRouteWillNotTakeGraphContentFromTheCaller(t *testing.T) {
	fm := newFragmentMangrove(t, fragmentJSON(t, nil, nil), mangrove.GraphFragmentMediaType, false)
	s, store, sc := mergeServer(t, fm)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, memberReq(t, http.MethodPost, "/v1/mangrove/merge"+mangroveScope,
		plainMember(),
		`{"objectId":"mangrove:obj:frag","entities":[{"name":"Injected","entityType":"x"}]}`))

	g, err := store.Load(sc)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range g.Entities {
		if e.Name == "Injected" {
			t.Fatal("an entity travelled in the request body into the graph")
		}
	}
}

// A post that is not a fragment is refused rather than half-parsed.
func TestMergingSomethingThatIsNotAFragmentIsRefused(t *testing.T) {
	// DELIBERATELY VALID JSON that would parse into an empty fragment. Prose
	// would be refused by the JSON decoder instead, and the test would pass
	// while the media-type check did nothing -- which is what mutation caught.
	fm := newFragmentMangrove(t, `{"entities":[{"name":"Sneaky"}]}`, "text/markdown", false)
	s, store, sc := mergeServer(t, fm)

	w := merge(t, s, "mangrove:obj:frag")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	g, err := store.Load(sc)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Entities) != 0 {
		t.Errorf("a post that was not labelled a fragment was merged anyway: %d entities", len(g.Entities))
	}
}

// Something the member cannot see is not mergeable, and says so the same way an
// object that never existed does.
func TestMergingSomethingNotSharedWithYouIsNotFound(t *testing.T) {
	fm := newFragmentMangrove(t, fragmentJSON(t, nil, nil), mangrove.GraphFragmentMediaType, false)
	s, _, _ := mergeServer(t, fm)
	if w := merge(t, s, "mangrove:obj:somebody-elses"); w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
}

// THE CASE THAT DISTINGUISHES A MERGE FROM A SKIP.
//
// CreateEntities skips a name that already exists. A recipient who already knows
// `Rhizophora` would therefore receive NONE of the shared observations about it
// -- and the route would report success. "Merged into your graph" has to mean
// something for the entities you already have, or it is the wrong word.
func TestMergingReachesAnEntityTheRecipientAlreadyHas(t *testing.T) {
	frag := fragmentJSON(t,
		[]memgraph.Entity{{
			Name: "Rhizophora", EntityType: "species",
			Observations: []memgraph.Observation{
				{Content: "salt tolerant"},
				{Content: "prop roots"},
			},
		}}, nil)
	fm := newFragmentMangrove(t, frag, mangrove.GraphFragmentMediaType, false)
	s, store, sc := mergeServer(t, fm)

	// The recipient already holds it, with an observation of their own and one
	// the fragment repeats.
	if _, err := store.CreateEntities(sc, []memgraph.Entity{{
		Name: "Rhizophora", EntityType: "species",
		Observations: []memgraph.Observation{
			{Content: "mine already"},
			{Content: "salt tolerant"},
		},
	}}, "their own work"); err != nil {
		t.Fatal(err)
	}

	if w := merge(t, s, "mangrove:obj:frag"); w.Code != http.StatusOK {
		t.Fatalf("merge: %d %s", w.Code, w.Body.String())
	}

	g, err := store.Load(sc)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Entities) != 1 {
		t.Fatalf("the merge duplicated the entity: %d", len(g.Entities))
	}
	have := map[string]string{}
	for _, o := range g.Entities[0].Observations {
		have[o.Content] = o.SourceSessionID
	}
	if _, ok := have["prop roots"]; !ok {
		t.Error("the shared observation never arrived; CreateEntities skipped the entity and nothing followed up")
	}
	if _, ok := have["mine already"]; !ok {
		t.Error("the merge destroyed an observation the recipient had")
	}
	if len(have) != 3 {
		t.Errorf("observations = %v, want three: theirs, the shared one, and the one both held once", have)
	}
	if have["mine already"] == have["prop roots"] {
		t.Error("borrowed and own material carry the same source; they must be tellable apart")
	}
}
