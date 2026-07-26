package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/restart"
)

const selfQuery = "tenant_id=" + tenantT + "&subs_acc_id=" + subsX

func restartReq(t *testing.T, method, target string, headers map[string]string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON (%s): %v", w.Body.String(), err)
	}
	return out
}

// FR-1.1: the workspace key comes from the profile, never the request. A body or
// query naming someone else must be ignored, not honoured — this is the endpoint
// where getting it wrong lets any member restart anyone's container.
func TestSelfRestartIgnoresACallerSuppliedUserID(t *testing.T) {
	orch := newFakeOrchWithRestarts(t)
	s := testServer(orch, &fakeTurner{})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, restartReq(t, http.MethodPost,
		"/v1/restart?"+selfQuery+"&user_acc_id="+accBob, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(orch.restarts) != 1 {
		t.Fatalf("restarts = %d, want 1", len(orch.restarts))
	}
	if got := orch.restarts[0].UserAccID; got != accAlice {
		t.Fatalf("restarted user = %s, want the caller %s — a request field reached the key", got, accAlice)
	}
}

// FR-1.3: the member is waiting on the answer, so a Docker failure surfaces
// rather than being swallowed the way the best-effort scope paths do.
func TestSelfRestartSurfacesDockerFailure(t *testing.T) {
	orch := newFakeOrchWithRestarts(t)
	orch.restartErr = errors.New("daemon unreachable")
	s := testServer(orch, &fakeTurner{})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, restartReq(t, http.MethodPost, "/v1/restart?"+selfQuery, goodHeaders(t)))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "daemon unreachable") {
		t.Errorf("body %q does not carry the real error", w.Body.String())
	}
}

// FR-1.4: a stopped container reports "noop" — nothing was bounced, but the
// notice is resolved because the next cold start begins from the new state.
func TestSelfRestartReportsNoopWhenNotRunning(t *testing.T) {
	orch := newFakeOrchWithRestarts(t)
	orch.statusRunning = false
	s := testServer(orch, &fakeTurner{})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, restartReq(t, http.MethodPost, "/v1/restart?"+selfQuery, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := decodeBody(t, w)["status"]; got != "noop" {
		t.Errorf("status = %v, want \"noop\"", got)
	}
}

// FR-2.1 + FR-3.3: the status endpoint reports a live scope notice, and the
// member's own restart clears it.
func TestRestartStatusReportsAndClearsPending(t *testing.T) {
	orch := newFakeOrchWithRestarts(t)
	orch.statusRunning = true
	s := testServer(orch, &fakeTurner{})

	if err := orch.restarts_().Raise(tenantT, subsX, "",
		restart.Notice{NoticeAt: time.Now().UTC(), Reason: restart.ReasonSharedSecret}); err != nil {
		t.Fatalf("Raise: %v", err)
	}

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, restartReq(t, http.MethodGet, "/v1/restart?"+selfQuery, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["pending"] != true {
		t.Fatalf("pending = %v, want true: %s", body["pending"], w.Body.String())
	}
	if body["reason"] != string(restart.ReasonSharedSecret) {
		t.Errorf("reason = %v, want %q", body["reason"], restart.ReasonSharedSecret)
	}

	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, restartReq(t, http.MethodPost, "/v1/restart?"+selfQuery, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("restart status = %d, want 200: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, restartReq(t, http.MethodGet, "/v1/restart?"+selfQuery, goodHeaders(t)))
	if decodeBody(t, w)["pending"] == true {
		t.Error("still pending after the member restarted")
	}
}

// FR-2.2 vs FR-1.2: a read-only member sees the notice but cannot trigger the
// bounce. The gateway gates the route too, but the check lives here as well so
// it holds regardless of how the route is declared.
func TestReadOnlyMemberSeesStatusButCannotRestart(t *testing.T) {
	orch := newFakeOrchWithRestarts(t)
	s := testServer(orch, &fakeTurner{})
	readOnly := headersFor(t, licensedProfile(accAlice, tenantT, subsX, "alpha", "read", true))

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, restartReq(t, http.MethodGet, "/v1/restart?"+selfQuery, readOnly))
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200 for a read-only member: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, restartReq(t, http.MethodPost, "/v1/restart?"+selfQuery, readOnly))
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST = %d, want 403 for a read-only member: %s", w.Code, w.Body.String())
	}
	if len(orch.restarts) != 0 {
		t.Error("a read-only member's restart reached the orchestrator")
	}
}

// FR-4.1: the change must reach disk under every policy. Deferring propagation
// would mean the member presses "restart now" and gets a bounce with nothing
// new to load.
func TestNoticePolicyPropagatesWithoutBouncing(t *testing.T) {
	orch := newFakeOrch()
	s := testServer(orch, &fakeTurner{})

	body := `{"scope":"tenant","tenant_id":"` + tenantT + `","format":"dotenv","name":"K","value":"v"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/admin/shared-secrets?restart=notice", strings.NewReader(body))
	for k, v := range headersFor(t, instanceProfile()) {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(orch.propagatedScopes) != 1 {
		t.Errorf("propagated = %d, want 1 — propagation is never deferred", len(orch.propagatedScopes))
	}
	if len(orch.bouncedScopes) != 0 {
		t.Errorf("bounced = %d, want 0 under the notice policy", len(orch.bouncedScopes))
	}
	if n, ok, err := orch.restarts_().Get(tenantT, "", ""); err != nil || !ok {
		t.Fatalf("no notice raised: ok=%v err=%v", ok, err)
	} else if n.Reason != restart.ReasonSharedSecret {
		t.Errorf("reason = %q, want %q", n.Reason, restart.ReasonSharedSecret)
	}
}

// FR-4.2: a client that sends no policy keeps the pre-feature behaviour, and
// "now" raises no notice — bouncing IS the notification, and a notice would
// banner every member whose container happened to be down.
func TestNowPolicyBouncesAndRaisesNoNotice(t *testing.T) {
	orch := newFakeOrch()
	s := testServer(orch, &fakeTurner{})

	body := `{"scope":"tenant","tenant_id":"` + tenantT + `","format":"dotenv","name":"K","value":"v"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/admin/shared-secrets?restart=now", strings.NewReader(body))
	for k, v := range headersFor(t, instanceProfile()) {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(orch.bouncedScopes) != 1 {
		t.Errorf("bounced = %d, want 1", len(orch.bouncedScopes))
	}
	if _, ok, _ := orch.restarts_().Get(tenantT, "", ""); ok {
		t.Error("the now policy must raise no notice")
	}
}

// FR-5.5: the schedule window is validated. A past time is a mistake, and an
// unbounded one would park an armed timer in memory forever.
func TestAdminRestartRejectsBadSchedule(t *testing.T) {
	cases := []struct {
		name, at string
	}{
		{"past", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)},
		{"beyond the horizon", time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)},
		{"not a timestamp", "tomorrow"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orch := newFakeOrchWithRestarts(t)
			s := testServer(orch, &fakeTurner{})
			body := `{"tenant_id":"` + tenantT + `","mode":"schedule","at":"` + tc.at + `"}`
			r := httptest.NewRequest(http.MethodPost, "/v1/admin/restart", strings.NewReader(body))
			for k, v := range headersFor(t, instanceProfile()) {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			if len(orch.armedSchedules) != 0 {
				t.Error("a rejected schedule was armed anyway")
			}
		})
	}
}

// A valid schedule raises the notice and arms the timer.
func TestAdminRestartSchedules(t *testing.T) {
	orch := newFakeOrchWithRestarts(t)
	s := testServer(orch, &fakeTurner{})

	at := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	body := `{"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX +
		`","mode":"schedule","at":"` + at + `","note":"nightly window"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/admin/restart", strings.NewReader(body))
	for k, v := range headersFor(t, instanceProfile()) {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(orch.armedSchedules) != 1 {
		t.Fatalf("armed = %d, want 1", len(orch.armedSchedules))
	}
	n, ok, err := orch.restarts_().Get(tenantT, subsX, "")
	if err != nil || !ok {
		t.Fatalf("no notice: ok=%v err=%v", ok, err)
	}
	if n.ScheduledAt == nil {
		t.Error("notice carries no scheduledAt")
	}
	if n.Note != "nightly window" {
		t.Errorf("note = %q, want the admin's text", n.Note)
	}
}

// FR-5.4: authority over the target, exactly as the shared-content endpoints
// require. A subscriptions-manager owns their subscription and nothing wider.
func TestAdminRestartAuthority(t *testing.T) {
	cases := []struct {
		name, profile, body string
		want                int
	}{
		{"subs-manager on own subscription", subsManagerProfile(),
			`{"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX + `","mode":"notice"}`, http.StatusOK},
		{"subs-manager on the tenant", subsManagerProfile(),
			`{"tenant_id":"` + tenantT + `","mode":"notice"}`, http.StatusForbidden},
		{"plain member", userProfile(),
			`{"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX + `","mode":"notice"}`, http.StatusForbidden},
		{"tenant manager on the tenant", tenantManagerProfile(),
			`{"tenant_id":"` + tenantT + `","mode":"notice"}`, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orch := newFakeOrchWithRestarts(t)
			s := testServer(orch, &fakeTurner{})
			r := httptest.NewRequest(http.MethodPost, "/v1/admin/restart", strings.NewReader(tc.body))
			for k, v := range headersFor(t, tc.profile) {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// FR-5.3: withdrawing removes the notice so members stop being told to restart.
func TestAdminRestartWithdraw(t *testing.T) {
	orch := newFakeOrchWithRestarts(t)
	s := testServer(orch, &fakeTurner{})
	if err := orch.restarts_().Raise(tenantT, subsX, "",
		restart.Notice{NoticeAt: time.Now().UTC(), Reason: restart.ReasonAdminRequest}); err != nil {
		t.Fatalf("Raise: %v", err)
	}

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, restartReq(t, http.MethodDelete,
		"/v1/admin/restart?tenant_id="+tenantT+"&subs_acc_id="+subsX, headersFor(t, instanceProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if _, ok, _ := orch.restarts_().Get(tenantT, subsX, ""); ok {
		t.Error("notice survived the withdrawal")
	}
}
