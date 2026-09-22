package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
)

const mangroveTok = "mangrove-secret"

// mangroveServer builds a server with the mangrove configured or not. BOTH halves are
// required, so the two arguments are separate: a base URL with no token and a
// token with no base URL are each a misconfiguration, and each must read as
// off rather than as half-on.
func mangroveServer(orch Orchestrator, baseURL, tok string) *Server {
	cfg := &config.Config{
		ContainerDataRoot:     "/tmp",
		MangroveBaseURL:       baseURL,
		ResolvedMangroveToken: tok,
		Agents: map[string]config.Agent{
			"alpha": {Key: "alpha", ServiceName: "picoclaw-alpha", ResolvedToken: "bearer",
				Mode: config.ModeContinuous},
		},
	}
	return &Server{Cfg: cfg, Resolver: identity.NewSDKResolver(), Mgr: orch, Pico: &fakeTurner{}}
}

func mangroveReq(auth, query string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/mangrove/subscription-members"+query, nil)
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	return r
}

// ABSENT, NOT 401, when the mangrove is not configured. This route discloses a
// subscription's whole membership roll -- account ids and emails -- so a
// deployment that has not opted in must grow no new surface at all. It is the
// same rule /v1/instances follows for the same reason.
func TestMangroveMembersRouteAbsentWhenUnconfigured(t *testing.T) {
	for _, tc := range []struct{ name, base, tok string }{
		{"neither half", "", ""},
		{"base url only", "http://mangrove:8090", ""},
		{"token only", "", mangroveTok},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mangroveServer(newFakeOrch(), tc.base, tc.tok)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, mangroveReq("Bearer "+mangroveTok, "?tenant_id=t1&subs_acc_id=s1"))
			if w.Code != http.StatusNotFound {
				t.Errorf("answered %d, want 404 -- the route must be absent, not refusing", w.Code)
			}
		})
	}
}

func TestMangroveMembersRefusesABadToken(t *testing.T) {
	s := mangroveServer(newFakeOrch(), "http://mangrove:8090", mangroveTok)
	for _, auth := range []string{"", "Bearer wrong", mangroveTok, "Bearer " + mangroveTok + "x"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, mangroveReq(auth, "?tenant_id=t1&subs_acc_id=s1"))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("auth %q answered %d, want 401", auth, w.Code)
		}
	}
}

// An agent's own bearer must not open this route. The mangrove credential gates a
// different capability, and a credential that gates two things cannot be
// revoked for one of them.
func TestMangroveMembersDoesNotAcceptAnAgentToken(t *testing.T) {
	s := mangroveServer(newFakeOrch(), "http://mangrove:8090", mangroveTok)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, mangroveReq("Bearer bearer", "?tenant_id=t1&subs_acc_id=s1"))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("an agent token answered %d, want 401", w.Code)
	}
}

func TestMangroveMembersNeedsBothQueryParameters(t *testing.T) {
	s := mangroveServer(newFakeOrch(), "http://mangrove:8090", mangroveTok)
	for _, q := range []string{"", "?tenant_id=t1", "?subs_acc_id=s1"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, mangroveReq("Bearer "+mangroveTok, q))
		if w.Code != http.StatusBadRequest {
			t.Errorf("query %q answered %d, want 400", q, w.Code)
		}
	}
}

func TestMangroveMembersReturnsTheRoll(t *testing.T) {
	orch := newFakeOrch()
	orch.users = []docker.UserRef{
		{AccID: "alice", Role: "alpha", Email: "alice@example.test"},
		{AccID: "bob", Role: "alpha", Email: "bob@example.test"},
	}
	s := mangroveServer(orch, "http://mangrove:8090", mangroveTok)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, mangroveReq("Bearer "+mangroveTok, "?tenant_id=t1&subs_acc_id=s1"))
	if w.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", w.Code, w.Body.String())
	}

	var got mangroveMembersResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// accIds is what the reachability gate actually consumes, so it is the
	// field whose shape must not drift.
	if len(got.AccIDs) != 2 || got.AccIDs[0] != "alice" || got.AccIDs[1] != "bob" {
		t.Errorf("accIds = %v", got.AccIDs)
	}
	if len(got.Members) != 2 || got.Members[0].Email != "alice@example.test" {
		t.Errorf("members = %+v", got.Members)
	}
}

// A subscription nobody has a workspace under is a normal state. Answering it
// as an error would turn "nobody to share with yet" into "the service is
// broken", which is the distinction FR-J0 exists to keep.
func TestMangroveMembersEmptySubscriptionIsNotAnError(t *testing.T) {
	orch := newFakeOrch()
	orch.users = nil
	s := mangroveServer(orch, "http://mangrove:8090", mangroveTok)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, mangroveReq("Bearer "+mangroveTok, "?tenant_id=t1&subs_acc_id=s1"))
	if w.Code != http.StatusOK {
		t.Fatalf("answered %d, want 200", w.Code)
	}
	// Empty arrays, never null: a client that does `for x of accIds` must not
	// have to special-case the empty subscription.
	if got := w.Body.String(); !strings.Contains(got, `"accIds":[]`) || !strings.Contains(got, `"members":[]`) {
		t.Errorf("body = %s, want empty arrays rather than null", got)
	}
}
