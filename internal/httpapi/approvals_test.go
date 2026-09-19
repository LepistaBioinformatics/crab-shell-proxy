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
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/mcptoken"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
)

const approvalSecret = "test-mcp-secret"

// approvalServer is a server whose MCP secret is set, which is what makes the
// container-facing half of the endpoint verifiable at all.
func approvalServer(t *testing.T) (*Server, string) {
	t.Helper()
	s, root := cronServer(t)
	s.Cfg.ResolvedMCPTokenSecret = approvalSecret
	// Built here rather than left to Handler's lazy init: these tests call
	// Handler once per request and one of those calls is on another goroutine,
	// so the init would be a race and, on the losing side, a second store the
	// member's answer would look into and not find.
	s.Approvals = newApprovalStore()
	return s, root
}

// callerScope is the workspace goodHeaders authorizes: role alpha, user alice.
func callerScope() memgraph.Scope {
	return memgraph.Scope{TenantID: tenantT, SubsAccID: subsX, Role: "alpha", UserAccID: accAlice}
}

func scopeToken(t *testing.T, sc memgraph.Scope) string {
	t.Helper()
	tok, err := mcptoken.Mint(approvalSecret, sc)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// ask fires the container's half in the background and hands back the channel the
// decision will arrive on. The request BLOCKS until a member answers, which is
// the whole shape of this endpoint.
func ask(t *testing.T, s *Server, token, body string) chan *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest(http.MethodPost, "/v1/approvals?t="+token, strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, r)
		done <- rec
	}()
	return done
}

// waitPending spins until the request has registered. The container's handler
// registers before it blocks, but "before" is not "already" across goroutines.
func waitPending(t *testing.T, s *Server, sc memgraph.Scope) []pendingApproval {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p := s.Approvals.list(sc); len(p) > 0 {
			return p
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no approval request registered")
	return nil
}

func TestAMemberAnswersAndTheBlockedCallGetsTheDecision(t *testing.T) {
	s, root := approvalServer(t)
	sc := callerScope()

	done := ask(t, s, scopeToken(t, sc),
		`{"session_key":"alice:alpha","session_id":"s1","tool_call_id":"c1","tool":"schedule_create","arguments":{"message":"weekly report"}}`)

	pending := waitPending(t, s, sc)
	if pending[0].Tool != "schedule_create" {
		t.Fatalf("tool = %q", pending[0].Tool)
	}

	rec := cronWrite(t, s, http.MethodPost, "/v1/approvals/answer?"+cronQuery,
		`{"id":"`+pending[0].ID+`","allowed":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("answer = %d: %s", rec.Code, rec.Body.String())
	}

	var dec approvalDecision
	out := <-done
	if err := json.Unmarshal(out.Body.Bytes(), &dec); err != nil {
		t.Fatal(err)
	}
	if !dec.Allowed {
		t.Fatalf("decision = %+v, want allowed", dec)
	}
	// BY IS THE PROXY'S. A container that could set this could tell the harness a
	// member it never spoke to had agreed.
	if dec.By != accAlice {
		t.Fatalf("by = %q, want the answering member's account id", dec.By)
	}

	// FR-5: the answer is auditable afterwards, because the member decided it in
	// seconds with a turn blocked on them.
	line, err := os.ReadFile(config.ApprovalsFile(root, tenantT, subsX, "alpha", accAlice))
	if err != nil {
		t.Fatalf("no audit record: %v", err)
	}
	if !strings.Contains(string(line), `"tool":"schedule_create"`) ||
		!strings.Contains(string(line), `"allowed":true`) {
		t.Fatalf("audit record does not describe the decision: %s", line)
	}
}

func TestARefusalCarriesItsReasonBackToTheAgent(t *testing.T) {
	s, _ := approvalServer(t)
	sc := callerScope()
	done := ask(t, s, scopeToken(t, sc), `{"session_id":"s1","tool":"schedule_create"}`)
	pending := waitPending(t, s, sc)

	cronWrite(t, s, http.MethodPost, "/v1/approvals/answer?"+cronQuery,
		`{"id":"`+pending[0].ID+`","allowed":false,"reason":"not this week"}`)

	var dec approvalDecision
	_ = json.Unmarshal((<-done).Body.Bytes(), &dec)
	if dec.Allowed {
		t.Fatal("a refusal was reported as allowed")
	}
	// DEC-2: the model reads this and can tell the member why, so the reason has
	// to survive the round trip verbatim.
	if dec.Reason != "not this week" {
		t.Fatalf("reason = %q", dec.Reason)
	}
}

// The security property this endpoint exists to hold: the id is the only thing an
// answer names, so without the scope check a member who learned another member's
// id could answer on their behalf.
func TestAMemberCannotAnswerAnotherWorkspacesRequest(t *testing.T) {
	s, _ := approvalServer(t)
	other := memgraph.Scope{TenantID: tenantT, SubsAccID: subsX, Role: "alpha", UserAccID: "someone-else"}

	done := ask(t, s, scopeToken(t, other), `{"session_id":"s1","tool":"schedule_create"}`)
	pending := waitPending(t, s, other)

	rec := cronWrite(t, s, http.MethodPost, "/v1/approvals/answer?"+cronQuery,
		`{"id":"`+pending[0].ID+`","allowed":true}`)
	// 404, the same answer an unknown id gets -- so the route cannot be used to
	// discover that someone else's request exists.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("answer = %d, want 404", rec.Code)
	}
	if got := s.Approvals.list(other); len(got) != 1 {
		t.Fatalf("the other workspace's request was disturbed: %d left", len(got))
	}
	_ = done
}

func TestAMemberOnlySeesTheirOwnPendingRequests(t *testing.T) {
	s, _ := approvalServer(t)
	other := memgraph.Scope{TenantID: tenantT, SubsAccID: subsX, Role: "alpha", UserAccID: "someone-else"}
	_ = ask(t, s, scopeToken(t, other), `{"session_id":"s1","tool":"schedule_create"}`)
	waitPending(t, s, other)

	rec := cronWrite(t, s, http.MethodGet, "/v1/approvals/pending?"+cronQuery, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("pending = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Pending []pendingApproval `json:"pending"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Pending) != 0 {
		t.Fatalf("saw %d requests belonging to another workspace", len(body.Pending))
	}
}

// This route is reachable by any container on the proxy's network, so an
// unverifiable scope gets the /v1/mcp posture: 401, no body, nothing logged.
func TestAnUnverifiableScopeTokenIsRefusedWithNothing(t *testing.T) {
	s, _ := approvalServer(t)
	for _, token := range []string{"", "garbage", "a.b"} {
		r := httptest.NewRequest(http.MethodPost, "/v1/approvals?t="+token,
			strings.NewReader(`{"tool":"schedule_create"}`))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, r)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q = %d, want 401", token, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("token %q leaked a body: %s", token, rec.Body.String())
		}
	}
}

// A token minted with a different secret must not verify -- the MAC is the whole
// guarantee that a caller cannot name a workspace it does not own.
func TestATokenFromAnotherSecretDoesNotVerify(t *testing.T) {
	s, _ := approvalServer(t)
	foreign, err := mcptoken.Mint("a-different-secret", callerScope())
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/approvals?t="+foreign,
		strings.NewReader(`{"tool":"schedule_create"}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("= %d, want 401", rec.Code)
	}
}

// A caller that gave up leaves nothing behind for a member to answer into.
func TestAnAbandonedRequestIsDropped(t *testing.T) {
	s, _ := approvalServer(t)
	sc := callerScope()
	p := &pendingApproval{ID: "x", scope: sc, answer: make(chan approvalDecision, 1)}
	s.Approvals.add(p)
	if len(s.Approvals.list(sc)) != 1 {
		t.Fatal("not registered")
	}
	s.Approvals.remove(p.ID)
	if len(s.Approvals.list(sc)) != 0 {
		t.Fatal("still listed after the caller went away")
	}
}

// Answering a request whose waiter is already gone must not block the member's
// request -- the deadline may have passed between the list and the answer.
func TestAnsweringAVanishedWaiterDoesNotBlock(t *testing.T) {
	s, _ := approvalServer(t)
	sc := callerScope()
	p := &pendingApproval{ID: "x", scope: sc, answer: make(chan approvalDecision, 1)}
	p.answer <- approvalDecision{} // fill the buffer, as a prior answer would
	s.Approvals.add(p)

	done := make(chan bool, 1)
	go func() { done <- s.Approvals.answer("x", sc, approvalDecision{Allowed: true}) }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("the answer was refused")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("answering blocked on a waiter that had gone")
	}
}

// THE BUG THIS COVERS SHIPPED, and it looked like nothing at all: the turn sat
// on "waiting for approval to run schedule_create" forever, because the member
// was chatting inside a project.
//
// The container registers with the scope its endpoint token carries, and that
// token is minted once per container and cannot name a project. The member asks
// from wherever they are, and inside a project the query carries `project=<id>`.
// Keyed on the narrowed scope, the two never met.
func TestAPendingRequestIsVisibleFromInsideAProject(t *testing.T) {
	s, _ := approvalServer(t)
	sc := callerScope() // as the container registers it: no project
	done := ask(t, s, scopeToken(t, sc), `{"session_id":"s1","tool":"schedule_create"}`)
	waitPending(t, s, sc)

	// The member's own query, from inside a project.
	rec := cronWrite(t, s, http.MethodGet,
		"/v1/approvals/pending?"+cronQuery+"&project=teste-de-texto", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("pending = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Pending []pendingApproval `json:"pending"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Pending) != 1 {
		t.Fatalf("a member inside a project saw %d of their own pending requests, want 1",
			len(body.Pending))
	}

	// And can answer it from there.
	ans := cronWrite(t, s, http.MethodPost,
		"/v1/approvals/answer?"+cronQuery+"&project=teste-de-texto",
		`{"id":"`+body.Pending[0].ID+`","allowed":true}`)
	if ans.Code != http.StatusOK {
		t.Fatalf("answer from inside a project = %d: %s", ans.Code, ans.Body.String())
	}
	var dec approvalDecision
	_ = json.Unmarshal((<-done).Body.Bytes(), &dec)
	if !dec.Allowed {
		t.Fatal("the answer did not reach the blocked call")
	}
}
