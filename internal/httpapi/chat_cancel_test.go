package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/projects"
)

func cancelReq(body string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/cancel", strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// TestCancelAddressesTheSessionTheTurnRanOn is the reason cancel and completions
// share resolveChatScope.
//
// picoclaw looks an abort up by the session it derives from the id it is given.
// A cancel that computed its session id even slightly differently -- forgetting
// the project prefix is the obvious way -- would abort nothing and still answer
// 204, and the member would meet that as a Stop button that does nothing. So the
// assertion is not "cancel was called", it is "cancel was called with the same
// id the turn was".
func TestCancelAddressesTheSessionTheTurnRanOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"main workspace", goodBody},
		{"inside a project", `{"messages":[{"role":"user","content":"hi"}],"session_id":"s",` +
			`"project":"p1","tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orch := scaffoldedOrch()
			orch.projects = []projects.Project{{ID: "p1", Name: "P1"}}
			turner := &fakeTurner{content: "ok"}
			s := testServer(orch, turner)

			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, chatReq(t, tc.body, goodHeaders(t)))
			if w.Code != http.StatusOK {
				t.Fatalf("turn status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}

			w = httptest.NewRecorder()
			s.Handler().ServeHTTP(w, cancelReq(tc.body, goodHeaders(t)))
			if w.Code != http.StatusNoContent {
				t.Fatalf("cancel status = %d, want 204 (body %s)", w.Code, w.Body.String())
			}

			if len(turner.ran) != 1 || len(turner.cancelled) != 1 {
				t.Fatalf("ran %d turns, cancelled %d, want 1 each", len(turner.ran), len(turner.cancelled))
			}
			if turner.cancelled[0] != turner.ran[0] {
				t.Errorf("cancelled session %q, turn ran on %q -- the stop would abort nothing",
					turner.cancelled[0], turner.ran[0])
			}
		})
	}
}

// TestCancelWithNoTurnRunningSucceeds: stopping is idempotent from the caller's
// side. The turn finishing while the member clicks is an ordinary race, and the
// runner reports it as success (see pico.Client.Cancel).
func TestCancelWithNoTurnRunningSucceeds(t *testing.T) {
	turner := &fakeTurner{}
	s := testServer(scaffoldedOrch(), turner)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, cancelReq(goodBody, goodHeaders(t)))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
}

// TestCancelReportsAnUnreachableAgent: failing to reach the container is a real
// failure, and the webapp must not clear its bands for a turn still running.
func TestCancelReportsAnUnreachableAgent(t *testing.T) {
	turner := &fakeTurner{cancelErr: errors.New("connection refused")}
	s := testServer(scaffoldedOrch(), turner)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, cancelReq(goodBody, goodHeaders(t)))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

// TestCancelIsAuthorized: stopping someone else's turn is not a lesser act than
// running one. The gate is the same filtering chain, reached through the same
// helper -- these cases exist so a future edit cannot loosen it for cancel alone.
func TestCancelIsAuthorized(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers func(*testing.T) map[string]string
		body    string
		want    int
	}{
		{
			name:    "unknown service",
			headers: func(*testing.T) map[string]string { return nil },
			body:    goodBody,
			want:    http.StatusNotFound,
		},
		{
			name: "not licensed for this subscription",
			headers: func(t *testing.T) map[string]string {
				return headersFor(t, licensedProfile(accAlice, tenantT, subsX, "alpha", "read", true))
			},
			body: goodBody,
			want: http.StatusForbidden,
		},
		{
			name:    "unknown project",
			headers: goodHeaders,
			body: `{"session_id":"s","project":"nope","tenant_id":"` + tenantT +
				`","subs_acc_id":"` + subsX + `"}`,
			want: http.StatusNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turner := &fakeTurner{}
			s := testServer(scaffoldedOrch(), turner)

			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, cancelReq(tc.body, tc.headers(t)))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body.String())
			}
			if len(turner.cancelled) != 0 {
				t.Errorf("reached the agent on a refused request (sessions %v)", turner.cancelled)
			}
		})
	}
}

// TestCancelNeedsNoMessages: a stop carries the scope, not the content. Demanding
// "messages" would only make the webapp invent one.
func TestCancelNeedsNoMessages(t *testing.T) {
	turner := &fakeTurner{}
	s := testServer(scaffoldedOrch(), turner)

	body := `{"session_id":"s","tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX + `"}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, cancelReq(body, goodHeaders(t)))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %s)", w.Code, w.Body.String())
	}
}
