package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
)

// memberReefServer wires a server at `reefURL`, which the tests point at a
// stub standing in for crab-reef-network.
func memberReefServer(reefURL string) *Server {
	cfg := &config.Config{
		ContainerDataRoot: "/tmp",
		ReefBaseURL:       reefURL,
		ResolvedReefToken: reefTok,
		Agents: map[string]config.Agent{
			"alpha": {Key: "alpha", ServiceName: "picoclaw-alpha", ResolvedToken: "bearer",
				Mode: config.ModeContinuous},
		},
	}
	return &Server{Cfg: cfg, Resolver: identity.NewSDKResolver(), Mgr: newFakeOrch(), Pico: &fakeTurner{}}
}

// stubReef records the last body it was handed and answers 200.
type stubReef struct {
	srv  *httptest.Server
	last map[string]any
}

func newStubReef(t *testing.T) *stubReef {
	t.Helper()
	sr := &stubReef{}
	sr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sr.last = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&sr.last)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(sr.srv.Close)
	return sr
}

func memberReq(t *testing.T, method, path, profileJSON, body string) *http.Request {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	for k, v := range headersFor(t, profileJSON) {
		r.Header.Set(k, v)
	}
	return r
}

const reefScope = "?tenant_id=" + tenantT + "&subs_acc_id=" + subsX

// Unconfigured, none of the member routes exist either. Absent, not refusing —
// the same rule the tools and the membership route follow.
func TestReefMemberRoutesAbsentWhenUnconfigured(t *testing.T) {
	s := memberReefServer("")
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/reef/timeline"},
		{http.MethodGet, "/v1/reef/capabilities"},
		{http.MethodPost, "/v1/reef/admit"},
		{http.MethodPost, "/v1/reef/decide"},
		{http.MethodPost, "/v1/reef/revoke"},
	} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, memberReq(t, tc.method, tc.path+reefScope,
			licensedProfile(accAlice, tenantT, subsX, "alpha", "write", true), "{}"))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s answered %d, want 404", tc.method, tc.path, w.Code)
		}
	}
}

// THE HUMAN PATH IS THE ONLY ONE THAT MAY ACT AS THE PERSON. The MCP path signs
// as the Service; this one signs as the Person, which is what gives the human
// authority over their own bot.
func TestMemberCallsActAsThePerson(t *testing.T) {
	stub := newStubReef(t)
	s := memberReefServer(stub.srv.URL)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, memberReq(t, http.MethodGet, "/v1/reef/timeline"+reefScope,
		licensedProfile(accAlice, tenantT, subsX, "alpha", "read", true), ""))
	if w.Code != http.StatusOK {
		t.Fatalf("timeline answered %d: %s", w.Code, w.Body.String())
	}
	if stub.last["as"] != "person" {
		t.Errorf("the reef saw as=%v, want person", stub.last["as"])
	}
}

// ONLY A GOVERNING ROLE MAY DECIDE, and the role comes from the injected
// profile rather than from anything the caller asserts. A plain member holding
// write on the subscription is not a subscriptions-manager.
func TestDecideRefusesWithoutAGoverningRole(t *testing.T) {
	stub := newStubReef(t)
	s := memberReefServer(stub.srv.URL)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, memberReq(t, http.MethodPost, "/v1/reef/decide"+reefScope,
		licensedProfile(accAlice, tenantT, subsX, "alpha", "write", true),
		`{"activityId":"reef:act:1","accept":true}`))
	if w.Code != http.StatusForbidden {
		t.Fatalf("a plain member decided a scope: %d %s", w.Code, w.Body.String())
	}
	if stub.last != nil {
		t.Error("the refusal still reached the reef; it must be refused here")
	}
}

// And a subscriptions-manager may, with `governs` sent as a resolved fact.
func TestDecideAllowedForASubscriptionsManager(t *testing.T) {
	stub := newStubReef(t)
	s := memberReefServer(stub.srv.URL)

	// Two licensed records: the agent role that authorizes the workspace, and
	// the governing role on the same subscription.
	profile := `{"accId":"` + accAlice + `","owners":[{"email":"u@x","isPrincipal":true}],` +
		`"licensedResources":{"records":[` +
		`{"accId":"` + subsX + `","tenantId":"` + tenantT + `","role":"alpha","perm":"write","verified":true},` +
		`{"accId":"` + subsX + `","tenantId":"` + tenantT + `","role":"subscriptions-manager","perm":"write","verified":true}` +
		`]}}`

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, memberReq(t, http.MethodPost, "/v1/reef/decide"+reefScope,
		profile, `{"activityId":"reef:act:1","accept":true}`))
	if w.Code != http.StatusOK {
		t.Fatalf("a subscriptions-manager could not decide: %d %s", w.Code, w.Body.String())
	}
	if stub.last["governs"] != true {
		t.Errorf("governs=%v reached the reef, want true", stub.last["governs"])
	}
}

// capabilities is what lets the UI make the pending reading ABSENT rather than
// empty for somebody who governs nothing.
func TestCapabilitiesReportsTheCallersAuthority(t *testing.T) {
	stub := newStubReef(t)
	s := memberReefServer(stub.srv.URL)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, memberReq(t, http.MethodGet, "/v1/reef/capabilities"+reefScope,
		licensedProfile(accAlice, tenantT, subsX, "alpha", "read", true), ""))
	if w.Code != http.StatusOK {
		t.Fatalf("capabilities answered %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Governs        bool `json:"governs"`
		TenantLicensed bool `json:"tenantLicensed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Governs || got.TenantLicensed {
		t.Errorf("a plain member reported governs=%v tenantLicensed=%v, want both false",
			got.Governs, got.TenantLicensed)
	}
}

// A TENANT MANAGER IS LICENSED ON THE TENANT, and that is the only way the
// reef ever hears tenantLicensed=true. The MCP path cannot produce it.
func TestTenantManagerIsTenantLicensed(t *testing.T) {
	stub := newStubReef(t)
	s := memberReefServer(stub.srv.URL)

	profile := `{"accId":"` + accAlice + `","owners":[{"email":"u@x","isPrincipal":true}],` +
		`"licensedResources":{"records":[` +
		`{"accId":"` + subsX + `","tenantId":"` + tenantT + `","role":"alpha","perm":"write","verified":true},` +
		`{"accId":"` + subsX + `","tenantId":"` + tenantT + `","role":"tenant-manager","perm":"write","verified":true}` +
		`]}}`

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, memberReq(t, http.MethodGet, "/v1/reef/capabilities"+reefScope, profile, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("capabilities answered %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Governs        bool `json:"governs"`
		TenantLicensed bool `json:"tenantLicensed"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.TenantLicensed || !got.Governs {
		t.Errorf("tenant-manager reported governs=%v tenantLicensed=%v, want both true",
			got.Governs, got.TenantLicensed)
	}
}

func TestAdmitAndRevokeValidateTheirBodies(t *testing.T) {
	stub := newStubReef(t)
	s := memberReefServer(stub.srv.URL)
	profile := licensedProfile(accAlice, tenantT, subsX, "alpha", "write", true)

	for _, tc := range []struct{ path, body string }{
		{"/v1/reef/admit", `{}`},
		{"/v1/reef/revoke", `{"objectId":"reef:obj:1"}`},
		{"/v1/reef/revoke", `{"cell":"soil-ph"}`},
	} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, memberReq(t, http.MethodPost, tc.path+reefScope, profile, tc.body))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s with %s answered %d, want 400", tc.path, tc.body, w.Code)
		}
	}
}

// AN UNREACHABLE REEF IS ITS OWN STATE, distinct from a refusal and from an
// empty answer. The UI has to tell "nothing shared yet" from "the service is
// down", and it can only do that if this layer keeps them apart.
func TestUnreachableReefIsABadGateway(t *testing.T) {
	s := memberReefServer("http://127.0.0.1:1")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, memberReq(t, http.MethodGet, "/v1/reef/timeline"+reefScope,
		licensedProfile(accAlice, tenantT, subsX, "alpha", "read", true), ""))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("answered %d, want 502", w.Code)
	}
	if !strings.Contains(w.Body.String(), "unreachable") {
		t.Errorf("body does not name the condition: %s", w.Body.String())
	}
}

// A REFUSAL KEEPS ITS BODY. The reef names the addressee that was out of reach;
// flattening it would leave the member nothing to act on.
func TestReefRefusalIsForwardedIntact(t *testing.T) {
	reefSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"out of reach: reef:group:tenant:t1","addressee":"reef:group:tenant:t1","delivered":false}`)
	}))
	defer reefSrv.Close()
	s := memberReefServer(reefSrv.URL)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, memberReq(t, http.MethodPost, "/v1/reef/admit"+reefScope,
		licensedProfile(accAlice, tenantT, subsX, "alpha", "write", true),
		`{"activityId":"reef:act:1"}`))
	if w.Code != http.StatusForbidden {
		t.Fatalf("answered %d, want the reef's own 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "reef:group:tenant:t1") {
		t.Errorf("the named addressee was lost: %s", w.Body.String())
	}
}
