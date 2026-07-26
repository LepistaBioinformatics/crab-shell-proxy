package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// newTestServer builds a Server wired to a real, temp-file-backed registry (the
// handlers here read/write s.Reg directly, not through the Orchestrator fake),
// plus the encoded profile-header values for an admin (instance/staff) caller
// and a plain (non-admin) caller. It reuses testServer/newFakeOrch/headersFor
// from handlers_test.go and instanceProfile/userProfile from admin_test.go
// rather than duplicating that scaffolding.
func newTestServer(t *testing.T) (s *Server, admin, nonAdmin string) {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "model-registry.db"), nil)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	s = testServer(newFakeOrch(), &fakeTurner{})
	s.Reg = reg
	return s, headersFor(t, instanceProfile())[identity.ProfileHeader],
		headersFor(t, userProfile())[identity.ProfileHeader]
}

const (
	profileHeaderName = identity.ProfileHeader
	serviceNameHeader = identity.ServiceNameHeader
	testServiceName   = "picoclaw-alpha"
	testAgentBearer   = "Bearer bearer"
)

func TestAdminModelsRequireProxyAdmin(t *testing.T) {
	s, _, nonAdmin := newTestServer(t)

	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/v1/admin/models", ""},
		{"POST", "/v1/admin/models", `{"model_name":"m","provider":"openai","model":"gpt-5.4","api_base":"https://x","api_key":"sk"}`},
		{"DELETE", "/v1/admin/models/m", ""},
		{"PUT", "/v1/admin/models/order", `{"order":["m"]}`},
		{"GET", "/v1/admin/model-catalog", ""},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set(profileHeaderName, nonAdmin)
		req.Header.Set("Authorization", testAgentBearer)
		req.Header.Set(serviceNameHeader, testServiceName)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		// The inventory holds API keys with instance-wide blast radius, so the gate
		// is proxy-admin and it lives here, not only in the webapp.
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
}

func TestAdminModelCreateListRoundTripNeverReturnsTheKey(t *testing.T) {
	s, admin, _ := newTestServer(t)

	body := `{"model_name":"gpt-5.4","provider":"openai","model":"gpt-5.4",
	  "api_base":"https://api.openai.com/v1","api_key":"sk-super-secret"}`
	rec := doAdmin(t, s, admin, "POST", "/v1/admin/models", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-super-secret") {
		t.Fatalf("create response leaked the key: %s", rec.Body.String())
	}

	rec = doAdmin(t, s, admin, "GET", "/v1/admin/models", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-super-secret") {
		t.Fatalf("list leaked the key: %s", rec.Body.String())
	}
	var listed struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Models) != 1 || listed.Models[0]["has_key"] != true {
		t.Errorf("listed = %#v, want one entry with has_key true", listed.Models)
	}
}

func TestAdminModelCreateDuplicateIsConflict(t *testing.T) {
	s, admin, _ := newTestServer(t)
	body := `{"model_name":"dup","provider":"openai","model":"m","api_base":"https://x","api_key":"sk"}`
	if rec := doAdmin(t, s, admin, "POST", "/v1/admin/models", body); rec.Code != http.StatusOK {
		t.Fatalf("first create = %d", rec.Code)
	}
	rec := doAdmin(t, s, admin, "POST", "/v1/admin/models", body)
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate create = %d, want 409", rec.Code)
	}
}

func TestAdminModelUpdateStaleVersionIsConflict(t *testing.T) {
	s, admin, _ := newTestServer(t)
	create := `{"model_name":"m","provider":"openai","model":"m","api_base":"https://x","api_key":"sk"}`
	doAdmin(t, s, admin, "POST", "/v1/admin/models", create)

	ok := doAdmin(t, s, admin, "PUT", "/v1/admin/models/m",
		`{"version":1,"provider":"openai","model":"m","api_base":"https://y"}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", ok.Code, ok.Body.String())
	}
	stale := doAdmin(t, s, admin, "PUT", "/v1/admin/models/m",
		`{"version":1,"provider":"openai","model":"m","api_base":"https://z"}`)
	if stale.Code != http.StatusConflict {
		t.Errorf("stale update = %d, want 409", stale.Code)
	}
}

func TestAdminModelUpdateOmittingTheKeyKeepsIt(t *testing.T) {
	s, admin, _ := newTestServer(t)
	doAdmin(t, s, admin, "POST", "/v1/admin/models",
		`{"model_name":"m","provider":"openai","model":"m","api_base":"https://x","api_key":"sk-keep"}`)

	doAdmin(t, s, admin, "PUT", "/v1/admin/models/m",
		`{"version":1,"provider":"openai","model":"m","api_base":"https://y"}`)

	rec := doAdmin(t, s, admin, "GET", "/v1/admin/models", "")
	var listed struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	// A client that never receives the key must be able to edit other fields
	// without wiping it.
	if listed.Models[0]["has_key"] != true {
		t.Errorf("key lost on an update that omitted api_key: %#v", listed.Models[0])
	}
}

func TestAdminModelDeleteInUseReturnsTheReferrers(t *testing.T) {
	s, admin, _ := newTestServer(t)
	doAdmin(t, s, admin, "POST", "/v1/admin/models",
		`{"model_name":"fb","provider":"openai","model":"fb","api_base":"https://x","api_key":"sk"}`)
	doAdmin(t, s, admin, "POST", "/v1/admin/models",
		`{"model_name":"main","provider":"openai","model":"main","api_base":"https://x","api_key":"sk","fallbacks":["fb"]}`)

	rec := doAdmin(t, s, admin, "DELETE", "/v1/admin/models/fb", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete in use = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error     string `json:"error"`
		Referrers []struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"referrers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// The rejection must name what to detach, or the admin has no next action.
	if len(body.Referrers) == 0 || body.Referrers[0].Kind != "fallback" || body.Referrers[0].ID != "main" {
		t.Errorf("referrers = %+v, want the fallback holder named", body.Referrers)
	}
}

func TestAdminModelDeprecateRequiresAReplacement(t *testing.T) {
	s, admin, _ := newTestServer(t)
	doAdmin(t, s, admin, "POST", "/v1/admin/models",
		`{"model_name":"old","provider":"openai","model":"old","api_base":"https://x","api_key":"sk"}`)

	bad := doAdmin(t, s, admin, "POST", "/v1/admin/models/old/deprecate", `{"version":1}`)
	if bad.Code != http.StatusBadRequest {
		t.Errorf("deprecate without a replacement = %d, want 400", bad.Code)
	}

	doAdmin(t, s, admin, "POST", "/v1/admin/models",
		`{"model_name":"new","provider":"openai","model":"new","api_base":"https://x","api_key":"sk"}`)
	good := doAdmin(t, s, admin, "POST", "/v1/admin/models/old/deprecate",
		`{"version":1,"replaced_by":"new"}`)
	if good.Code != http.StatusOK {
		t.Errorf("deprecate = %d: %s", good.Code, good.Body.String())
	}
}

func TestAdminModelCatalogReturnsSuggestionsWithoutKeys(t *testing.T) {
	s, admin, _ := newTestServer(t)
	rec := doAdmin(t, s, admin, "GET", "/v1/admin/model-catalog", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "api_key") {
		t.Errorf("catalog must never carry keys: %s", rec.Body.String())
	}
	var body struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Entries) < 20 {
		t.Errorf("catalog has %d entries, want the full set", len(body.Entries))
	}
	// model_name is the admin's choice and must be unique, so suggesting one would
	// invite a duplicate.
	if _, present := body.Entries[0]["model_name"]; present {
		t.Errorf("catalog entry suggests a model_name: %#v", body.Entries[0])
	}
}

// doAdmin issues an authenticated admin request and returns the recorder.
func doAdmin(t *testing.T, s *Server, profile, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(profileHeaderName, profile)
	req.Header.Set("Authorization", testAgentBearer)
	req.Header.Set(serviceNameHeader, testServiceName)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}
