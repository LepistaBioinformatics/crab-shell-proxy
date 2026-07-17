package httpapi

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
)

// Profile fixtures for the admin tier matrix. accAlice is the caller; the
// licensed record's account id (subsX/subsY) only matters for the
// subscriptions-manager tier.
func instanceProfile() string {
	return `{"accId":"` + accAlice + `","isStaff":true,"owners":[{"email":"u@x","isPrincipal":true}]}`
}
func tenantOwnerProfile() string {
	return licensedProfile(accAlice, tenantT, subsX, "tenant-owner", "write", true)
}
func tenantManagerProfile() string {
	return licensedProfile(accAlice, tenantT, subsX, "tenant-manager", "write", true)
}
func subsManagerProfile() string {
	return licensedProfile(accAlice, tenantT, subsX, "subscriptions-manager", "write", true)
}
func userProfile() string {
	return licensedProfile(accAlice, tenantT, subsX, "alpha", "write", true)
}

func adminReq(t *testing.T, method, path string, headers map[string]string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// TestAdminTierMatrix drives every tier against every target and asserts the
// authorization outcome (allow => not 403; deny => 403). This is the core of
// the FR-1/FR-1.1 matrix, enforced server-side (NFR-1).
func TestAdminTierMatrix(t *testing.T) {
	tenantScope := "/v1/admin/shared?scope=tenant&tenant_id=" + tenantT
	subsScope := "/v1/admin/shared?scope=subscription&tenant_id=" + tenantT + "&subs_acc_id=" + subsX
	users := "/v1/admin/users?tenant_id=" + tenantT + "&subs_acc_id=" + subsX
	userFiles := "/v1/admin/users/files?tenant_id=" + tenantT + "&subs_acc_id=" + subsX + "&user_acc_id=" + accBob

	tiers := []struct {
		name    string
		profile string
		// allow[target] expected
		tenant, subs, users, userFiles bool
	}{
		{"instance", instanceProfile(), true, true, true, true},
		{"tenant-owner", tenantOwnerProfile(), true, true, true, true},
		{"tenant-manager", tenantManagerProfile(), true, true, true, true},
		{"subs-manager", subsManagerProfile(), false, true, true, true},
		{"user", userProfile(), false, false, false, false},
	}
	for _, tr := range tiers {
		cases := []struct {
			target string
			path   string
			allow  bool
		}{
			{"tenant-scope", tenantScope, tr.tenant},
			{"subs-scope", subsScope, tr.subs},
			{"users", users, tr.users},
			{"user-files", userFiles, tr.userFiles},
		}
		for _, c := range cases {
			t.Run(tr.name+"/"+c.target, func(t *testing.T) {
				s := testServer(newFakeOrch(), &fakeTurner{})
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, c.path, headersFor(t, tr.profile)))
				if c.allow && w.Code == http.StatusForbidden {
					t.Errorf("expected allow, got 403: %s", w.Body.String())
				}
				if !c.allow && w.Code != http.StatusForbidden {
					t.Errorf("expected 403 deny, got %d: %s", w.Code, w.Body.String())
				}
			})
		}
	}
}

// TestAdminBranchIsolation: a subscription manager of one subscription is not
// authoritative over a different subscription in the same tenant (FR-1.1
// same-branch), nor over the tenant scope.
func TestAdminBranchIsolation(t *testing.T) {
	// subs-manager scoped to subsX (via subsY-account record won't match subsX).
	profile := licensedProfile(accAlice, tenantT, subsX, "subscriptions-manager", "write", true)
	otherSubs := "22222222-2222-2222-2222-999999999999"
	path := "/v1/admin/shared?scope=subscription&tenant_id=" + tenantT + "&subs_acc_id=" + otherSubs
	s := testServer(newFakeOrch(), &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, path, headersFor(t, profile)))
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-subscription access: status = %d, want 403", w.Code)
	}
}

// TestAdminWrongTenantDenied: a tenant manager of tenantT has no authority over
// tenantU's scope.
func TestAdminWrongTenantDenied(t *testing.T) {
	path := "/v1/admin/shared?scope=tenant&tenant_id=" + tenantU
	s := testServer(newFakeOrch(), &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, path, headersFor(t, tenantManagerProfile())))
	if w.Code != http.StatusForbidden {
		t.Errorf("wrong-tenant tenant scope: status = %d, want 403", w.Code)
	}
}

// --- FR-7 privacy invariant ---

// TestNoUserFileContentRoute proves there is NO endpoint that returns a user's
// private file bytes: the route was never registered, so it 404s regardless of
// tier (asserted with an Instance caller — the most privileged).
func TestNoUserFileContentRoute(t *testing.T) {
	path := "/v1/admin/users/files/content?tenant_id=" + tenantT + "&subs_acc_id=" + subsX + "&user_acc_id=" + accBob + "&name=x"
	s := testServer(newFakeOrch(), &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, path, headersFor(t, instanceProfile())))
	if w.Code != http.StatusNotFound {
		t.Errorf("user-file content route must not exist: status = %d, want 404", w.Code)
	}
}

// TestNoUserFileWriteRoute proves no endpoint edits/writes a user's private
// file: only GET and DELETE are registered for /v1/admin/users/files, so a
// write method is rejected (405) — even for an Instance caller.
func TestNoUserFileWriteRoute(t *testing.T) {
	path := "/v1/admin/users/files?tenant_id=" + tenantT + "&subs_acc_id=" + subsX + "&user_acc_id=" + accBob
	s := testServer(newFakeOrch(), &fakeTurner{})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, adminReq(t, method, path, headersFor(t, instanceProfile())))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s user-file write must be rejected: status = %d, want 405", method, w.Code)
		}
	}
}

// TestUserFilesMetadataOnly proves the user-file list returns metadata only
// (name/size/modifiedAt) and never carries file bytes/content.
func TestUserFilesMetadataOnly(t *testing.T) {
	orch := newFakeOrch()
	orch.userFiles = []docker.FileMeta{{Name: "doc.pdf", Size: 1234, ModifiedAt: "2026-07-17T00:00:00Z"}}
	s := testServer(orch, &fakeTurner{})
	path := "/v1/admin/users/files?tenant_id=" + tenantT + "&subs_acc_id=" + subsX + "&user_acc_id=" + accBob
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, path, headersFor(t, instanceProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, key := range []string{`"name"`, `"size"`, `"modifiedAt"`, "doc.pdf"} {
		if !strings.Contains(body, key) {
			t.Errorf("metadata missing %s: %s", key, body)
		}
	}
	// The FileMeta shape carries no content/bytes field. A sentinel that would
	// only appear if bytes leaked.
	for _, leak := range []string{`"content"`, `"bytes"`, `"data"`} {
		if strings.Contains(body, leak) {
			t.Errorf("user-file list leaked content field %s: %s", leak, body)
		}
	}
}

// TestSharedSecretsListNamesOnly: the shared-secrets list returns names, never
// values (write-only API, FR-5.1).
func TestSharedSecretsListNamesOnly(t *testing.T) {
	orch := newFakeOrch()
	orch.listResult = docker.SecretNames{Dotenv: []string{"BRAVE_KEY"}, JSON: []string{"OPENAI_KEY"}}
	s := testServer(orch, &fakeTurner{})
	path := "/v1/admin/shared-secrets?scope=tenant&tenant_id=" + tenantT
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, path, headersFor(t, instanceProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "BRAVE_KEY") || !strings.Contains(body, "OPENAI_KEY") {
		t.Errorf("names missing: %s", body)
	}
	if strings.Contains(body, "SECRET-VALUE") {
		t.Errorf("shared-secrets list leaked a value: %s", body)
	}
}

// TestSharedSecretFormatRestricted: only dotenv/json are accepted for shared
// secrets; file/native are rejected with 400.
func TestSharedSecretFormatRestricted(t *testing.T) {
	for _, tc := range []struct {
		format string
		want   int
	}{
		{"dotenv", http.StatusOK},
		{"json", http.StatusOK},
		{"file", http.StatusBadRequest},
		{"native", http.StatusBadRequest},
	} {
		orch := newFakeOrch()
		s := testServer(orch, &fakeTurner{})
		body := `{"scope":"tenant","tenant_id":"` + tenantT + `","format":"` + tc.format + `","name":"K","value":"v"}`
		r := httptest.NewRequest(http.MethodPost, "/v1/admin/shared-secrets", strings.NewReader(body))
		for k, v := range headersFor(t, instanceProfile()) {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("format %q: status = %d, want %d: %s", tc.format, w.Code, tc.want, w.Body.String())
		}
	}
}

// Propagation policy: a shared FILE is a read-only bind-mount, so it reaches
// running containers live and must NOT trigger a recreate (recreating truncates
// picoclaw's live session — the "conversation cut after injection" bug). A
// shared SECRET is baked into env at create time, so it still needs a
// RestartScope to take effect.
func TestSharedWriteTriggersRestartScope(t *testing.T) {
	t.Run("file-no-restart", func(t *testing.T) {
		orch := newFakeOrch()
		s := testServer(orch, &fakeTurner{})

		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("scope", "tenant")
		_ = mw.WriteField("tenant_id", tenantT)
		fw, _ := mw.CreateFormFile("file", "policy.txt")
		_, _ = fw.Write([]byte("hello"))
		_ = mw.Close()

		r := httptest.NewRequest(http.MethodPost, "/v1/admin/shared", &buf)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		for k, v := range headersFor(t, instanceProfile()) {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if len(orch.sharedWrites) != 1 || len(orch.restartScopes) != 0 {
			t.Errorf("writes=%d restarts=%d, want 1/0 (files are live via RO mount)",
				len(orch.sharedWrites), len(orch.restartScopes))
		}
	})

	t.Run("secret-restarts", func(t *testing.T) {
		orch := newFakeOrch()
		s := testServer(orch, &fakeTurner{})

		body := `{"scope":"tenant","tenant_id":"` + tenantT + `","format":"dotenv","name":"K","value":"v"}`
		r := httptest.NewRequest(http.MethodPost, "/v1/admin/shared-secrets", strings.NewReader(body))
		for k, v := range headersFor(t, instanceProfile()) {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if len(orch.restartScopes) != 1 {
			t.Errorf("restarts=%d, want 1 (env secrets need a recreate)", len(orch.restartScopes))
		}
	})
}

// TestAdminWriteForbiddenNoMutation proves the "can't write" half of the
// invariant: a sub-tier caller is denied the shared mutations AND no write
// reaches the orchestrator (403 before the Mgr call), mirroring
// TestSecretsPostForbidden. A subscriptions-manager has no tenant-scope
// authority.
func TestAdminWriteForbiddenNoMutation(t *testing.T) {
	// Shared-file upload at tenant scope by a subs-manager -> 403, no write.
	t.Run("shared-upload", func(t *testing.T) {
		orch := newFakeOrch()
		s := testServer(orch, &fakeTurner{})
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("scope", "tenant")
		_ = mw.WriteField("tenant_id", tenantT)
		fw, _ := mw.CreateFormFile("file", "x.txt")
		_, _ = fw.Write([]byte("x"))
		_ = mw.Close()
		r := httptest.NewRequest(http.MethodPost, "/v1/admin/shared", &buf)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		for k, v := range headersFor(t, subsManagerProfile()) {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", w.Code)
		}
		if len(orch.sharedWrites) != 0 || len(orch.restartScopes) != 0 {
			t.Errorf("mutation ran despite 403: writes=%d restarts=%d", len(orch.sharedWrites), len(orch.restartScopes))
		}
	})
	// Shared-secret write + delete at tenant scope by a subs-manager -> 403.
	t.Run("shared-secret-write", func(t *testing.T) {
		orch := newFakeOrch()
		s := testServer(orch, &fakeTurner{})
		body := `{"scope":"tenant","tenant_id":"` + tenantT + `","format":"dotenv","name":"K","value":"v"}`
		r := httptest.NewRequest(http.MethodPost, "/v1/admin/shared-secrets", strings.NewReader(body))
		for k, v := range headersFor(t, subsManagerProfile()) {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", w.Code)
		}
		if len(orch.sharedWrites) != 0 {
			t.Errorf("secret write ran despite 403: %d", len(orch.sharedWrites))
		}
	})
	// User-file delete by a plain user (no admin tier) -> 403, no delete.
	t.Run("user-file-delete", func(t *testing.T) {
		orch := newFakeOrch()
		s := testServer(orch, &fakeTurner{})
		path := "/v1/admin/users/files?tenant_id=" + tenantT + "&subs_acc_id=" + subsX +
			"&user_acc_id=" + accBob + "&name=x"
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, adminReq(t, http.MethodDelete, path, headersFor(t, userProfile())))
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", w.Code)
		}
		if len(orch.userFileDeletes) != 0 {
			t.Errorf("user-file delete ran despite 403: %d", len(orch.userFileDeletes))
		}
	})
}

// TestAdminNoProfile401: without a profile the admin routes 401 (they still run
// the resolveAgent anti-bypass + profile decode).
func TestAdminNoProfile401(t *testing.T) {
	s := testServer(newFakeOrch(), &fakeTurner{})
	h := headersFor(t, instanceProfile())
	delete(h, "x-mycelium-profile")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, "/v1/admin/scopes", h))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// TestAdminScopesDiscovery: scopes reflects the caller's tier roles (FR-8).
func TestAdminScopesDiscovery(t *testing.T) {
	s := testServer(newFakeOrch(), &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, "/v1/admin/scopes", headersFor(t, subsManagerProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"scopes"`) || !strings.Contains(body, `"kind":"subscription"`) ||
		!strings.Contains(body, subsX) {
		t.Errorf("subscription scope not reported: %s", body)
	}
}

// TestAdminScopesInstanceEnumeratesDisk: an Instance caller's scopes are
// enumerated from disk — every tenant plus its subscriptions (FR-8), so the
// Members panel is reachable for tenants the caller holds no explicit record on.
func TestAdminScopesInstanceEnumeratesDisk(t *testing.T) {
	orch := newFakeOrch()
	orch.tenants = []string{tenantT}
	orch.tenantSubs = []string{subsX}
	s := testServer(orch, &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, "/v1/admin/scopes", headersFor(t, instanceProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"kind":"tenant"`) || !strings.Contains(body, `"kind":"subscription"`) {
		t.Errorf("instance scopes must enumerate tenant + subscription: %s", body)
	}
}

// TestAdminScopesTenantEnumeratesSubscriptions: a tenant admin gets the tenant
// scope AND the subscription scopes under it (enumerated on disk) so the Members
// panel is reachable (coordinator FR-8 clarification).
func TestAdminScopesTenantEnumeratesSubscriptions(t *testing.T) {
	orch := newFakeOrch()
	orch.tenantSubs = []string{subsX}
	s := testServer(orch, &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, "/v1/admin/scopes", headersFor(t, tenantManagerProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"kind":"tenant"`) {
		t.Errorf("tenant scope missing: %s", body)
	}
	if !strings.Contains(body, `"kind":"subscription"`) || !strings.Contains(body, subsX) {
		t.Errorf("tenant admin must get subscription scopes under the tenant: %s", body)
	}
}
