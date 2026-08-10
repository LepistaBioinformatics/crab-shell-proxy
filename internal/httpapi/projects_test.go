package httpapi

import (
	"bytes"
	"mime/multipart"
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

// --- the upload path (agent-projects-scope-fixes B2) ---

// mediaUploadReq builds the multipart request the browser sends. The project
// travels as a FORM FIELD, beside tenant_id and subs_acc_id — there is no query
// string to put it in, which is how it came to be dropped.
func mediaUploadReq(t *testing.T, project string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for k, v := range map[string]string{"tenant_id": tenantT, "subs_acc_id": subsX} {
		if err := form.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if project != "" {
		if err := form.WriteField("project", project); err != nil {
			t.Fatal(err)
		}
	}
	part, err := form.CreateFormFile("file", "analysis.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("PK\x03\x04zipbytes")); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodPost, "/v1/media", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	for k, v := range goodHeaders(t) {
		r.Header.Set(k, v)
	}
	return r
}

// uploadServer is testServer with the media allowlist and size cap the upload
// handler checks before it ever looks at the project.
func uploadServer(orch *fakeOrch) *Server {
	s := testServer(orch, &fakeTurner{})
	s.Cfg.MediaAllowedExts = []string{"zip"}
	s.Cfg.MediaMaxBytes = 1 << 20
	return s
}

// The reported bug: a file uploaded from inside a project was written to the MAIN
// workspace, so the agent answering the turn could not open the path it was handed
// (`unzip: can't open .../workspace-<project>/uploads/analysis.zip`).
func TestMediaUploadResolvesTheProjectFromTheForm(t *testing.T) {
	orch := scaffoldedOrch()
	orch.projects = []projects.Project{{ID: "seedtrial", Name: "Seed Trial"}}
	s := uploadServer(orch)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, mediaUploadReq(t, "seedtrial"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := orch.mediaProjects; len(got) != 1 || got[0] != "seedtrial" {
		t.Errorf("StoreMedia project = %v, want [seedtrial] — the file went to the wrong workspace", got)
	}
}

// A project-less upload must stay byte-identical to the pre-feature request: no
// project field, and the main workspace resolved.
func TestMediaUploadWithoutAProjectIsUnchanged(t *testing.T) {
	orch := scaffoldedOrch()
	s := uploadServer(orch)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, mediaUploadReq(t, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := orch.mediaProjects; len(got) != 1 || got[0] != "" {
		t.Errorf("StoreMedia project = %v, want [\"\"] (the agent's own workspace)", got)
	}
}

// Refused BEFORE a byte is written. Falling through to the main workspace is the
// defect being fixed, and the failure it produces is invisible until the agent
// cannot find the file days later.
func TestMediaUploadUnknownProjectIs404(t *testing.T) {
	orch := scaffoldedOrch()
	s := uploadServer(orch)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, mediaUploadReq(t, "nope"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
	if len(orch.mediaProjects) != 0 {
		t.Errorf("StoreMedia was called %d time(s) for an unknown project, want none", len(orch.mediaProjects))
	}
}

// --- sessions/resolve (agent-projects-scope-fixes FR-13) ---

// It answered with the UNPREFIXED session key, looked for it in the main
// workspace, and did both for every project conversation — so the one route whose
// job is to say "which file holds this chat" named a file in a directory that
// never held it.
func TestSessionsResolveIsProjectScoped(t *testing.T) {
	orch := scaffoldedOrch()
	orch.projects = []projects.Project{{ID: "seedtrial"}}
	s := testServer(orch, &fakeTurner{})

	base := "/v1/sessions/resolve?" + projectsQuery() + "&session_id=chat-1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodGet, base+"&project=seedtrial", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"sessionKey":"p.seedtrial.`) {
		t.Errorf("body = %s, want the project-prefixed session key", w.Body.String())
	}

	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodGet, base+"&project=nope", ""))
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown project status = %d, want 404", w.Code)
	}

	// Without a project, unchanged: the bare key, no prefix.
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodGet, base, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"sessionKey":"p.`) {
		t.Errorf("body = %s, want an unprefixed session key outside a project", w.Body.String())
	}
}

// --- the memory note (agent-projects-scope-fixes, found while auditing B2) ---

// The write took its project from the QUERY while the client sent it in the BODY, so
// a note edited inside a project overwrote the MAIN workspace's MEMORY_CUSTOM.md. The
// read beside it was correct, which is what made this invisible: the member opened the
// project's note, edited it, and destroyed a different one.
func TestMemoryPutTakesTheProjectFromTheBody(t *testing.T) {
	orch := scaffoldedOrch()
	orch.projects = []projects.Project{{ID: "seedtrial"}}
	s := testServer(orch, &fakeTurner{})

	body := `{"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX +
		`","project":"seedtrial","content":"project note"}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodPut, "/v1/memory", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := orch.memoryProjects; len(got) != 1 || got[0] != "seedtrial" {
		t.Errorf("WriteMemory project = %v, want [seedtrial] — the note went to the wrong workspace", got)
	}
}

func TestMemoryPutWithoutAProjectIsUnchanged(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{})

	body := `{"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX + `","content":"own note"}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodPut, "/v1/memory", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := orch.memoryProjects; len(got) != 1 || got[0] != "" {
		t.Errorf("WriteMemory project = %v, want [\"\"] (the agent's own workspace)", got)
	}
}

// Refused before anything is written: overwriting the main workspace's note because a
// project id was wrong is the failure mode this whole batch exists to remove.
func TestMemoryPutUnknownProjectIs404(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{})

	body := `{"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX +
		`","project":"nope","content":"x"}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodPut, "/v1/memory", body))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
	if len(orch.memoryProjects) != 0 {
		t.Errorf("WriteMemory was called for an unknown project (%v), want not at all", orch.memoryProjects)
	}
}
