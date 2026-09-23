package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
)

func shareServer(t *testing.T, stub *stubMangrove) *Server {
	t.Helper()
	s := memberMangroveServer(stub.srv.URL)
	s.Mgr.(*fakeOrch).users = []docker.UserRef{
		{AccID: accAlice, Role: "alpha", Email: "alice@x.test"},
		{AccID: accBob, Role: "alpha", Email: "bob@x.test"},
	}
	return s
}

func share(t *testing.T, s *Server, profile, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, memberReq(t, http.MethodPost, "/v1/mangrove/share"+mangroveScope, profile, body))
	return w
}

// SHARE IS NOT PUBLISH AGAIN. It names an object that already exists, so the
// object keeps its id and its cell -- a memory that travelled further is still
// the same memory.
func TestSharingNamesTheObjectRatherThanRepublishingIt(t *testing.T) {
	stub := newStubMangrove(t)
	s := shareServer(t, stub)

	w := share(t, s, plainMember(),
		`{"objectId":"mangrove:obj:9","toEmails":[{"email":"bob@x.test","agent":true}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("share: %d %s", w.Code, w.Body.String())
	}
	if stub.last["objectId"] != "mangrove:obj:9" {
		t.Errorf("objectId = %v", stub.last["objectId"])
	}
	if stub.last["target"] != "mangrove:actor:"+accBob+":service" {
		t.Errorf("target = %v, want bob's agent", stub.last["target"])
	}
	if _, present := stub.last["object"]; present {
		t.Error("a share carried an object; it must name one, not make one")
	}
	if stub.last["as"] != "person" {
		t.Errorf("as = %v, want person", stub.last["as"])
	}
}

// The same audience shape publish takes, so a member who learned one learned the
// other -- and a group still needs the role that publishing to it needs.
func TestSharingIntoAGroupCarriesTheLicences(t *testing.T) {
	for _, tc := range []struct {
		name               string
		profile            string
		wantGroups, wantTn bool
	}{
		{"a plain member holds neither", plainMember(), false, false},
		{"a subscriptions-manager governs", withRole("subscriptions-manager"), true, false},
		{"a tenant-manager holds both", withRole("tenant-manager"), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubMangrove(t)
			s := shareServer(t, stub)
			w := share(t, s, tc.profile,
				`{"objectId":"mangrove:obj:9","to":["mangrove:group:subscription:`+subsX+`"]}`)
			if w.Code != http.StatusOK {
				t.Fatalf("share: %d %s", w.Code, w.Body.String())
			}
			if got, _ := stub.last["groupsLicensed"].(bool); got != tc.wantGroups {
				t.Errorf("groupsLicensed = %v, want %v", got, tc.wantGroups)
			}
			if got, _ := stub.last["tenantLicensed"].(bool); got != tc.wantTn {
				t.Errorf("tenantLicensed = %v, want %v", got, tc.wantTn)
			}
		})
	}
}

// ONE ACTIVITY PER ADDRESSEE, because that is what the mangrove's share is.
func TestSharingWithSeveralPeopleEmitsOnePerTarget(t *testing.T) {
	var targets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if tgt, ok := body["target"].(string); ok {
			targets = append(targets, tgt)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	s := memberMangroveServer(srv.URL)
	s.Mgr.(*fakeOrch).users = []docker.UserRef{
		{AccID: accBob, Role: "alpha", Email: "bob@x.test"},
	}
	w := share(t, s, plainMember(),
		`{"objectId":"mangrove:obj:9","to":["mangrove:actor:`+accAlice+`:person"],`+
			`"toEmails":[{"email":"bob@x.test","person":true,"agent":true}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("share: %d %s", w.Code, w.Body.String())
	}
	if len(targets) != 3 {
		t.Fatalf("emitted %d shares for three addressees: %v", len(targets), targets)
	}
}

// A member told "shared" about a list where one name was out of reach would stop
// looking for the problem. The first refusal stops the rest and is forwarded
// whole, because the mangrove names the addressee that failed.
func TestARefusalStopsTheRestAndKeepsItsWords(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"out of reach: mangrove:actor:mallory:service (no workspace under a subscription this caller shares)"}`))
	}))
	defer srv.Close()

	s := memberMangroveServer(srv.URL)
	s.Mgr.(*fakeOrch).users = []docker.UserRef{{AccID: accBob, Role: "alpha", Email: "bob@x.test"}}
	w := share(t, s, plainMember(),
		`{"objectId":"mangrove:obj:9","to":["mangrove:actor:mallory:service","mangrove:actor:`+accBob+`:person"]}`)

	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want the mangrove's own 403", w.Code)
	}
	if calls != 1 {
		t.Errorf("kept going after a refusal: %d calls", calls)
	}
	if body := w.Body.String(); !contains(body, "out of reach") || !contains(body, "mallory") {
		t.Errorf("the refusal lost the addressee it named: %s", body)
	}
}

func TestShareValidatesItsBody(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no object", `{"toEmails":[{"email":"bob@x.test","agent":true}]}`},
		{"nobody to share with", `{"objectId":"mangrove:obj:9"}`},
		{"malformed", `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubMangrove(t)
			s := shareServer(t, stub)
			if w := share(t, s, plainMember(), tc.body); w.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400 (%s)", w.Code, w.Body.String())
			}
			if stub.last != nil {
				t.Error("a rejected body reached the mangrove")
			}
		})
	}
}

// Absent, not refusing, when the operator never enabled the mangrove.
func TestShareAbsentWhenMangroveUnconfigured(t *testing.T) {
	s := memberMangroveServer("")
	if w := share(t, s, plainMember(), `{"objectId":"o","to":["x"]}`); w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
