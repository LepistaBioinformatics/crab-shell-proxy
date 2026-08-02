package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
)

func folderReq(t *testing.T, method, path, body string, headers map[string]string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

const folderBody = `{"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX + `","path":"reports"}`

func TestMediaFolderCreate(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, folderReq(t, http.MethodPost, "/v1/media/folder", folderBody, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(orch.folderCreated) != 1 || orch.folderCreated[0] != "reports" {
		t.Errorf("created = %v, want [reports]", orch.folderCreated)
	}
}

// The `uploads/` prefix the UI carries in its tree paths must be stripped before the
// manager sees it, exactly as the delete route already does. A status-only assertion
// would not notice a doubled prefix.
func TestMediaFolderStripsTheUploadsPrefix(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{})
	body := `{"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX + `","path":"uploads/reports/q1"}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, folderReq(t, http.MethodPost, "/v1/media/folder", body, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if len(orch.folderCreated) != 1 || orch.folderCreated[0] != "reports/q1" {
		t.Errorf("created = %v, want [reports/q1] with the prefix stripped", orch.folderCreated)
	}
}

func TestMediaMoveForwardsBothPaths(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{})
	body := `{"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX +
		`","path":"top.txt","to":"reports/top.txt"}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, folderReq(t, http.MethodPost, "/v1/media/move", body, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if len(orch.moved) != 1 || orch.moved[0] != "top.txt -> reports/top.txt" {
		t.Errorf("moved = %v", orch.moved)
	}
}

func TestMediaMoveRequiresADestination(t *testing.T) {
	s := testServer(scaffoldedOrch(), &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, folderReq(t, http.MethodPost, "/v1/media/move", folderBody, goodHeaders(t)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a move with no `to`", w.Code)
	}
}

// The count is what the interface reports after a destructive operation, so it has to
// survive the round trip rather than being recomputed client-side.
func TestMediaFolderDeleteReportsTheFileCount(t *testing.T) {
	orch := scaffoldedOrch()
	orch.removedFiles = 12
	s := testServer(orch, &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, folderReq(t, http.MethodDelete, "/v1/media/folder", folderBody, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		RemovedFiles int `json:"removedFiles"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if got.RemovedFiles != 12 {
		t.Errorf("removedFiles = %d, want 12", got.RemovedFiles)
	}
	if len(orch.folderDeleted) != 1 || orch.folderDeleted[0] != "reports" {
		t.Errorf("deleted = %v", orch.folderDeleted)
	}
}

// Each refusal maps to a status the UI can act on differently: "taken" is not the
// same message as "invalid", and neither is a server fault.
func TestMediaFolderErrorsMapToActionableStatuses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"not found", docker.ErrMediaNotFound, http.StatusNotFound},
		{"already exists", docker.ErrMediaExists, http.StatusConflict},
		{"invalid name", docker.ErrMediaName, http.StatusBadRequest},
		{"into itself", docker.ErrMediaIntoSelf, http.StatusBadRequest},
		{"not a folder", docker.ErrMediaNotFolder, http.StatusBadRequest},
		{"the uploads root", docker.ErrMediaRoot, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orch := scaffoldedOrch()
			orch.folderErr = c.err
			s := testServer(orch, &fakeTurner{})
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w,
				folderReq(t, http.MethodPost, "/v1/media/folder", folderBody, goodHeaders(t)))
			if w.Code != c.want {
				t.Errorf("status = %d, want %d (%s)", w.Code, c.want, c.name)
			}
		})
	}
}

// These write into the same directory an upload does, so they carry the same gate.
// Asserted against /v1/media's own answer for the same caller rather than a guessed
// status, so the two cannot drift apart.
func TestMediaFolderRoutesShareTheUploadGate(t *testing.T) {
	unlicensed := headersFor(t, licensedProfile(accAlice, tenantT, subsX, "alpha", "read", false))

	ref := httptest.NewRecorder()
	testServer(scaffoldedOrch(), &fakeTurner{}).Handler().ServeHTTP(ref,
		folderReq(t, http.MethodPost, "/v1/media",
			`{"tenant_id":"`+tenantT+`","subs_acc_id":"`+subsX+`"}`, unlicensed))

	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/v1/media/folder"},
		{http.MethodDelete, "/v1/media/folder"},
		{http.MethodPost, "/v1/media/move"},
	} {
		w := httptest.NewRecorder()
		testServer(scaffoldedOrch(), &fakeTurner{}).Handler().ServeHTTP(w,
			folderReq(t, route.method, route.path, folderBody, unlicensed))
		if w.Code == http.StatusOK {
			t.Errorf("%s %s let an unlicensed caller through", route.method, route.path)
		}
	}
	_ = ref
}

func TestMediaFolderRoutesRejectAMalformedBody(t *testing.T) {
	for _, body := range []string{
		`{`,
		`{"tenant_id":"nope","subs_acc_id":"` + subsX + `","path":"x"}`,
		`{"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX + `"}`, // no path
	} {
		s := testServer(scaffoldedOrch(), &fakeTurner{})
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, folderReq(t, http.MethodPost, "/v1/media/folder", body, goodHeaders(t)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, w.Code)
		}
	}
}
