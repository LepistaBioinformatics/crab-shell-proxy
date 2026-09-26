package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/projects"
)

func TestSkillsListReturnsEveryLayerWithItsOrigin(t *testing.T) {
	orch := scaffoldedOrch()
	orch.memberSkills = []docker.MemberSkill{
		{SkillMeta: docker.SkillMeta{Name: "mine"}, Origin: docker.SkillOriginMember},
		{SkillMeta: docker.SkillMeta{Name: "house-style"}, Origin: docker.SkillOriginShared},
		{SkillMeta: docker.SkillMeta{Name: "skill-creator"}, Origin: docker.SkillOriginManaged},
	}
	s := testServer(orch, &fakeTurner{})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodGet, "/v1/skills?"+projectsQuery(), ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	for _, want := range []string{`"origin":"member"`, `"origin":"shared"`, `"origin":"managed"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("body = %s, missing %s", w.Body.String(), want)
		}
	}
}

// The routes carry NO project, and a project in the query changes nothing.
//
// Not cosmetic: the harness's skill loader is fixed on the main workspace at
// boot, so a skill written into workspace-<id>/skills is read by nothing. A
// project-scoped skills surface would let a member write inert files, which is
// why the Orchestrator methods have no project argument at all.
//
// /v1/sessions/resolve 404s on an unknown project. These answer 200, which is
// the observable difference between "scoped" and "deliberately unscoped".
func TestSkillRoutesAreNotProjectScoped(t *testing.T) {
	orch := scaffoldedOrch()
	orch.projects = []projects.Project{{ID: "seedtrial"}}
	s := testServer(orch, &fakeTurner{})

	for _, path := range []string{
		"/v1/skills?" + projectsQuery() + "&project=nope",
		"/v1/skills/doc?" + projectsQuery() + "&name=mine&project=nope",
	} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, projectsReq(t, http.MethodGet, path, ""))
		if w.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200 (the project is ignored, not validated)", path, w.Code)
		}
	}

	body := `{"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX +
		`","name":"notes","content":"x","project":"seedtrial"}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodPut, "/v1/skills", body))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if orch.memberSkillName != "notes" {
		t.Errorf("WriteMemberSkill name = %q, want notes", orch.memberSkillName)
	}
}

func TestSkillsPutCarriesTheVersionTheCallerRead(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{})

	body := `{"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX +
		`","name":"notes","content":"doc","modifiedAt":"2026-09-01T10:00:00Z"}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodPut, "/v1/skills", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if orch.memberSkillIfMod != "2026-09-01T10:00:00Z" {
		t.Errorf("ifModifiedAt = %q, want the version the caller read — without it a write "+
			"silently replaces whatever the agent's evolution wrote", orch.memberSkillIfMod)
	}
}

// The store's vocabulary has to reach the member as distinguishable statuses:
// 403 is "this layer is not yours", 409 is "it moved under you", 404 is "no such
// skill". Collapsing any of them to 500 makes the panel unable to say which.
func TestSkillErrorsMapToDistinctStatuses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"a read-only layer", docker.ErrSkillReadOnly, http.StatusForbidden},
		{"a concurrent write", docker.ErrSkillConflict, http.StatusConflict},
		{"a name already taken", docker.ErrSkillExists, http.StatusConflict},
		{"an unusable name", docker.ErrInvalidSkillName, http.StatusBadRequest},
		{"an unusable path inside the skill", docker.ErrSkillFilePath, http.StatusBadRequest},
		{"a declared name that disagrees", docker.ErrSkillNameMismatch, http.StatusBadRequest},
		{"no such skill", docker.ErrMediaNotFound, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orch := scaffoldedOrch()
			orch.memberSkillErr = c.err
			s := testServer(orch, &fakeTurner{})

			body := `{"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX +
				`","name":"notes","content":"doc"}`
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, projectsReq(t, http.MethodPut, "/v1/skills", body))
			if w.Code != c.want {
				t.Errorf("status = %d, want %d (%s)", w.Code, c.want, w.Body.String())
			}
		})
	}
}

func TestSkillsDeleteReachesTheStore(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodDelete, "/v1/skills?"+projectsQuery()+"&name=notes", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if orch.memberSkillName != "notes" {
		t.Errorf("DeleteMemberSkill name = %q, want notes", orch.memberSkillName)
	}
}

// The member's key comes from the PROFILE, never from the request. A caller
// cannot address another member's workspace by asking for one.
func TestSkillsAddressTheCallersOwnWorkspace(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, projectsReq(t, http.MethodGet, "/v1/skills?"+projectsQuery(), ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if orch.memberSkillKey.UserAccID == "" {
		t.Error("the store was called without a user-scoped workspace key")
	}
}
