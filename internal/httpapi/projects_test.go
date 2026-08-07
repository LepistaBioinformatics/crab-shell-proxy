package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/projects"
)

func projectsQuery() string {
	return "tenant_id=" + tenantT + "&subs_acc_id=" + subsX
}

func projectsReq(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range goodHeaders(t) {
		r.Header.Set(k, v)
	}
	return r
}

func TestProjectsListEmpty(t *testing.T) {
	s := testServer(scaffoldedOrch(), &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodGet, "/v1/projects?"+projectsQuery(), ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	// An empty ARRAY, never null: the frontend maps over this.
	if !strings.Contains(w.Body.String(), `"projects":[]`) {
		t.Errorf("body = %s, want an empty array", w.Body.String())
	}
}

func TestProjectsCreateAndList(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodPost, "/v1/projects?"+projectsQuery(),
		`{"name":"Seed Trial","instructions":"Cite the protocol."}`))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
	}
	// Creating adds a bind, which needs a container recreate. The caller has to
	// be told so the UI can warn before the agent bounces.
	if !strings.Contains(w.Body.String(), `"restart_pending":true`) {
		t.Errorf("create response hides the pending restart: %s", w.Body.String())
	}

	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodGet, "/v1/projects?"+projectsQuery(), ""))
	if !strings.Contains(w.Body.String(), "Seed Trial") {
		t.Errorf("created project missing from list: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Cite the protocol.") {
		t.Errorf("instructions missing from list: %s", w.Body.String())
	}
}

func TestProjectsCreateRequiresName(t *testing.T) {
	s := testServer(scaffoldedOrch(), &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodPost, "/v1/projects?"+projectsQuery(), `{}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestProjectsDuplicateIsConflict(t *testing.T) {
	orch := scaffoldedOrch()
	orch.projectErr = projects.ErrDuplicate
	s := testServer(orch, &fakeTurner{})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodPost, "/v1/projects?"+projectsQuery(),
		`{"name":"Seed Trial"}`))
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
}

func TestProjectsPatchRenames(t *testing.T) {
	orch := scaffoldedOrch()
	orch.projects = []projects.Project{{ID: "seedtrial", Name: "Seed Trial"}}
	s := testServer(orch, &fakeTurner{})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodPatch, "/v1/projects/seedtrial?"+projectsQuery(),
		`{"name":"Field Trial 2026"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Field Trial 2026") {
		t.Errorf("rename not reflected: %s", w.Body.String())
	}
	// A rename touches no mount, so it must NOT claim a restart is coming.
	if strings.Contains(w.Body.String(), "restart_pending") {
		t.Errorf("rename claimed a restart: %s", w.Body.String())
	}
}

func TestProjectsPatchRequiresAField(t *testing.T) {
	orch := scaffoldedOrch()
	orch.projects = []projects.Project{{ID: "seedtrial", Name: "Seed Trial"}}
	s := testServer(orch, &fakeTurner{})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodPatch, "/v1/projects/seedtrial?"+projectsQuery(), `{}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestProjectsPatchUnknownIs404(t *testing.T) {
	s := testServer(scaffoldedOrch(), &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodPatch, "/v1/projects/nope?"+projectsQuery(),
		`{"name":"x"}`))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestProjectsDelete(t *testing.T) {
	orch := scaffoldedOrch()
	orch.projects = []projects.Project{{ID: "seedtrial", Name: "Seed Trial"}}
	s := testServer(orch, &fakeTurner{})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodDelete, "/v1/projects/seedtrial?"+projectsQuery(), ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(orch.projects) != 0 {
		t.Errorf("project survived delete: %+v", orch.projects)
	}
}

func TestProjectsRequireProfile(t *testing.T) {
	s := testServer(scaffoldedOrch(), &fakeTurner{})
	r := httptest.NewRequest(http.MethodGet, "/v1/projects?"+projectsQuery(), nil)
	r.Header.Set("Authorization", "Bearer bearer")
	r.Header.Set("x-mycelium-service-name", "picoclaw-alpha")

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401: %s", w.Code, w.Body.String())
	}
}

func TestProjectsRequireWorkspaceParams(t *testing.T) {
	s := testServer(scaffoldedOrch(), &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodGet, "/v1/projects", ""))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// TestHistoryUnknownProjectIs404 is AC-8. Falling through to the main workspace
// would return an empty history and read as data loss rather than a bad request.
func TestHistoryUnknownProjectIs404(t *testing.T) {
	s := testServer(scaffoldedOrch(), &fakeTurner{})
	q := "session_id=s&" + projectsQuery() + "&project=nope"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, historyReq(t, q, goodHeaders(t)))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestHistoryKnownProjectIsAccepted(t *testing.T) {
	orch := scaffoldedOrch()
	orch.projects = []projects.Project{{ID: "seedtrial", Name: "Seed Trial"}}
	s := testServer(orch, &fakeTurner{})

	q := "session_id=s&" + projectsQuery() + "&project=seedtrial"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, historyReq(t, q, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
}

// TestChatUnknownProjectIs404 is the same guard on the write path, and the one
// that actually prevents misplaced turns.
func TestChatUnknownProjectIs404(t *testing.T) {
	s := testServer(scaffoldedOrch(), &fakeTurner{})
	body := `{"model":"m","session_id":"s","tenant_id":"` + tenantT +
		`","subs_acc_id":"` + subsX + `","project":"nope",` +
		`"messages":[{"role":"user","content":"hi"}]}`

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodPost, "/v1/chat/completions", body))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
}
