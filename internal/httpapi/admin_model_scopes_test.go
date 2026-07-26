package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
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
	// A tenant-level change DOES re-apply, unlike global/agent — and it must
	// name the right scope, or every workspace under a different tenant would
	// be swept too.
	orch := s.Mgr.(*fakeOrch)
	if len(orch.reapplyScopes) != 1 || orch.reapplyScopes[0].Kind != docker.ScopeTenant ||
		orch.reapplyScopes[0].TenantID != tenantT {
		t.Errorf("reapplyScopes = %+v, want one ScopeTenant entry for %s", orch.reapplyScopes, tenantT)
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
	// global has no docker.Scope to express: neither the set nor the clear above
	// may re-apply anything, unlike a tenant/subscription change.
	if n := s.Mgr.(*fakeOrch).reapplyCalls; n != 0 {
		t.Errorf("global default set+clear triggered %d reapply calls, want 0", n)
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

func TestModelAssignmentSetAndClearRoundTrip(t *testing.T) {
	s, admin, _ := newTestServer(t)
	doAdmin(t, s, admin, "POST", "/v1/admin/models",
		`{"model_name":"m","provider":"openai","model":"m","api_base":"https://x","api_key":"sk"}`)

	body := `{"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX +
		`","user_acc_id":"` + accBob + `","model_name":"m"}`
	set := doAdmin(t, s, admin, "POST", "/v1/admin/model-assignments", body)
	if set.Code != http.StatusOK {
		t.Fatalf("set = %d: %s", set.Code, set.Body.String())
	}

	ref := registry.WorkspaceRef{TenantID: tenantT, SubsAccID: subsX, Agent: "alpha", UserAccID: accBob}
	a, err := s.Reg.GetAssignment(ref)
	if err != nil {
		t.Fatalf("GetAssignment after set: %v", err)
	}
	if a.ModelName != "m" || a.Source != registry.SourceExplicit {
		t.Errorf("assignment after set = %+v, want model m, source explicit", a)
	}

	clear := doAdmin(t, s, admin, "DELETE", "/v1/admin/model-assignments", body)
	if clear.Code != http.StatusOK {
		t.Fatalf("clear = %d: %s", clear.Code, clear.Body.String())
	}
	if _, err := s.Reg.GetAssignment(ref); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("GetAssignment after clear = %v, want ErrNotFound", err)
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
