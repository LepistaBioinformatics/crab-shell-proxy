package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

var errTest = errors.New("boom")

// namedProfile is goodHeaders' profile with the owner's NAME filled in -- which is
// what mycelium actually sends and what every turn was dropping.
func namedProfile(accID, tenantID, subsAccID string) string {
	return `{"accId":"` + accID + `","owners":[{"email":"ada@example.com",` +
		`"firstName":"Ada","lastName":"Lovelace","username":"ada","isPrincipal":true}],` +
		`"licensedResources":{"records":[{"accId":"` + subsAccID + `","tenantId":"` + tenantID +
		`","role":"alpha","perm":"write","verified":true}]}}`
}

// THE GAP THIS CLOSES. The name reached the handler on every turn and stopped
// there, so the agent opened each conversation knowing nothing about a member who
// was signed in.
func TestATurnRecordsTheSignedInAccount(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{content: "hello back"})
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, chatReq(t, goodBody,
		headersFor(t, namedProfile(accAlice, tenantT, subsX))))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(orch.seeded) != 1 {
		t.Fatalf("the turn recorded the account %d times, want once", len(orch.seeded))
	}
	got := orch.seeded[0]
	if got.Name() != "Ada Lovelace" {
		t.Errorf("name = %q, want the profile's", got.Name())
	}
	if got.Email != "ada@example.com" || got.Username != "ada" {
		t.Errorf("seeded = %+v, want the principal owner's e-mail and username", got)
	}
}

// THE PATH THAT ACTUALLY SHIPS. The webapp always streams, so the synchronous
// handler above is the one nobody uses -- and the two reach EnsureRunning through
// different frames, `streamTurn` carrying the owner as a parameter of its own. A
// guard on the first proves nothing about the second.
func TestAStreamingTurnRecordsItToo(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{content: "hello back"})
	w := httptest.NewRecorder()

	body := `{"messages":[{"role":"user","content":"hi"}],"session_id":"s","stream":true,` +
		`"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX + `"}`
	s.Handler().ServeHTTP(w, chatReq(t, body,
		headersFor(t, namedProfile(accAlice, tenantT, subsX))))

	if len(orch.seeded) != 1 {
		t.Fatalf("a streaming turn recorded the account %d times, want once", len(orch.seeded))
	}
	if got := orch.seeded[0].Name(); got != "Ada Lovelace" {
		t.Errorf("name = %q, want the profile's", got)
	}
}

// A workspace that could not be written is one this turn is about to fail on
// anyway, with a better message. Trading the member's answer for the note about
// who asked for it is the wrong way round.
func TestAFailedRecordDoesNotFailTheTurn(t *testing.T) {
	orch := scaffoldedOrch()
	orch.seedErr = errTest
	s := testServer(orch, &fakeTurner{content: "hello back"})
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, chatReq(t, goodBody,
		headersFor(t, namedProfile(accAlice, tenantT, subsX))))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want the turn to have succeeded: %s", w.Code, w.Body.String())
	}
}

// An ensure that failed created no workspace, so there is nowhere to write and
// nothing to write about -- the turn is already over.
func TestNothingIsRecordedWhenTheWorkspaceCouldNotBeEnsured(t *testing.T) {
	orch := scaffoldedOrch()
	orch.ensureErr = errTest
	s := testServer(orch, &fakeTurner{content: "x"})
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, chatReq(t, goodBody,
		headersFor(t, namedProfile(accAlice, tenantT, subsX))))

	if len(orch.seeded) != 0 {
		t.Errorf("recorded %d accounts against a workspace that does not exist", len(orch.seeded))
	}
}
