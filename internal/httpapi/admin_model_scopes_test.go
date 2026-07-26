package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestModelDefaultGlobalAndAgentRequireProxyAdmin(t *testing.T) {
	s, admin, nonAdmin := newTestServer(t)
	doAdmin(t, s, admin, "POST", "/v1/admin/models",
		`{"model_name":"m","provider":"openai","model":"m","api_base":"https://x","api_key":"sk"}`)

	for _, path := range []string{
		"/v1/admin/model-defaults?scope=global",
		"/v1/admin/model-defaults?scope=agent",
	} {
		rec := doAdmin(t, s, nonAdmin, "PUT", path, `{"model_name":"m"}`)
		// global and agent are instance-wide, and AuthorizeSharedScope has no level
		// above tenant to express — so they take the proxy-admin gate.
		if rec.Code != http.StatusForbidden {
			t.Errorf("PUT %s as non-admin = %d, want 403", path, rec.Code)
		}
		// The discriminating half: AuthorizeSharedScope's default branch returns
		// false for ANY kind outside tenant/subscription, so a fall-through would
		// 403 even the proxy-admin caller. Proving admin succeeds here is what
		// tells the two code paths apart.
		allow := doAdmin(t, s, admin, "PUT", path, `{"model_name":"m"}`)
		if allow.Code != http.StatusOK {
			t.Errorf("PUT %s as proxy-admin = %d, want 200: %s", path, allow.Code, allow.Body.String())
		}
	}
}

func TestModelDefaultTenantUsesTheSharedScopeGate(t *testing.T) {
	s, admin, nonAdmin := newTestServer(t)
	doAdmin(t, s, admin, "POST", "/v1/admin/models",
		`{"model_name":"m","provider":"openai","model":"m","api_base":"https://x","api_key":"sk"}`)

	deny := doAdmin(t, s, nonAdmin, "PUT",
		"/v1/admin/model-defaults?scope=tenant&tenant_id="+tenantU, `{"model_name":"m"}`)
	if deny.Code != http.StatusForbidden {
		t.Errorf("tenant default for a foreign tenant = %d, want 403", deny.Code)
	}

	allow := doAdmin(t, s, admin, "PUT",
		"/v1/admin/model-defaults?scope=tenant&tenant_id="+tenantT, `{"model_name":"m"}`)
	if allow.Code != http.StatusOK {
		t.Errorf("tenant default for an owned tenant = %d: %s", allow.Code, allow.Body.String())
	}
}

func TestModelDefaultRoundTripAndClear(t *testing.T) {
	s, admin, _ := newTestServer(t)
	doAdmin(t, s, admin, "POST", "/v1/admin/models",
		`{"model_name":"m","provider":"openai","model":"m","api_base":"https://x","api_key":"sk"}`)

	if rec := doAdmin(t, s, admin, "PUT", "/v1/admin/model-defaults?scope=global", `{"model_name":"m"}`); rec.Code != http.StatusOK {
		t.Fatalf("set = %d: %s", rec.Code, rec.Body.String())
	}
	rec := doAdmin(t, s, admin, "GET", "/v1/admin/model-defaults?scope=global", "")
	var body struct {
		Default *struct {
			ModelName string `json:"model_name"`
		} `json:"default"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Default == nil || body.Default.ModelName != "m" {
		t.Errorf("get = %s, want model m", rec.Body.String())
	}

	if rec := doAdmin(t, s, admin, "DELETE", "/v1/admin/model-defaults?scope=global", ""); rec.Code != http.StatusOK {
		t.Fatalf("clear = %d", rec.Code)
	}
	rec = doAdmin(t, s, admin, "GET", "/v1/admin/model-defaults?scope=global", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// An absent default is a null, not a 404: "no default here" is a normal state
	// the UI renders, not an error.
	if body.Default != nil {
		t.Errorf("cleared default = %s, want null", rec.Body.String())
	}
}

func TestModelDefaultRejectsAnInactiveModel(t *testing.T) {
	s, admin, _ := newTestServer(t)
	doAdmin(t, s, admin, "POST", "/v1/admin/models",
		`{"model_name":"m","provider":"openai","model":"m","api_base":"https://x","api_key":"sk"}`)
	doAdmin(t, s, admin, "PUT", "/v1/admin/models/m/status", `{"version":1,"status":"disabled"}`)

	rec := doAdmin(t, s, admin, "PUT", "/v1/admin/model-defaults?scope=global", `{"model_name":"m"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("disabled model as a default = %d, want 400", rec.Code)
	}
}

func TestModelAssignmentSetRequiresUserManagementAuthority(t *testing.T) {
	s, admin, nonAdmin := newTestServer(t)
	doAdmin(t, s, admin, "POST", "/v1/admin/models",
		`{"model_name":"m","provider":"openai","model":"m","api_base":"https://x","api_key":"sk"}`)

	body := `{"tenant_id":"` + tenantU + `","subs_acc_id":"` + subsX +
		`","user_acc_id":"` + accBob + `","model_name":"m"}`
	rec := doAdmin(t, s, nonAdmin, "POST", "/v1/admin/model-assignments", body)
	if rec.Code != http.StatusForbidden {
		t.Errorf("assignment outside authority = %d, want 403", rec.Code)
	}
}

func TestModelAssignmentSetUnknownModelIs400(t *testing.T) {
	s, admin, _ := newTestServer(t)
	body := `{"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX +
		`","user_acc_id":"` + accBob + `","model_name":"ghost"}`
	rec := doAdmin(t, s, admin, "POST", "/v1/admin/model-assignments", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown model = %d, want 400", rec.Code)
	}
}

func TestReorderDoesNotReapplyAnything(t *testing.T) {
	s, admin, _ := newTestServer(t)
	for _, n := range []string{"a", "b"} {
		doAdmin(t, s, admin, "POST", "/v1/admin/models",
			`{"model_name":"`+n+`","provider":"openai","model":"`+n+`","api_base":"https://x","api_key":"sk"}`)
	}

	rec := doAdmin(t, s, admin, "PUT", "/v1/admin/models/order", `{"order":["b","a"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder = %d: %s", rec.Code, rec.Body.String())
	}
	// Position is presentation only. A drag must not re-materialize or restart
	// anything, so the fake must have recorded no reapply call at all.
	if n := s.Mgr.(*fakeOrch).reapplyCalls; n != 0 {
		t.Errorf("reorder triggered %d reapply calls, want 0", n)
	}
}
