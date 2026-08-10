package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/projects"
	"github.com/google/uuid"
)

// Projects are a member-facing surface, not an admin one: a user carves their
// own agent into projects the way they would in Claude or ChatGPT. So the whole
// file authorizes against the CALLER's own workspace — reads need `read` on the
// agent, mutations need `write`, matching /v1/restart and /v1/secrets
// respectively.
//
// REST rather than JSON-RPC. The monorepo rule ("always call mycelium over
// JSON-RPC") governs calls TO mycelium; this is the proxy's own HTTP API, which
// the rest of /v1 already speaks.

type projectResponse struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Instructions string `json:"instructions"`
	CreatedAt    string `json:"created_at"`
}

func toProjectResponse(p projects.Project) projectResponse {
	return projectResponse{
		ID:           p.ID,
		Name:         p.Name,
		Instructions: p.Instructions,
		CreatedAt:    p.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

type projectWriteRequest struct {
	Name         *string `json:"name"`
	Instructions *string `json:"instructions"`
}

// resolveProjectCaller runs the shared preamble: agent + identity + the two
// workspace query parameters. write selects which permission the profile must
// carry.
func (s *Server) resolveProjectCaller(
	w http.ResponseWriter, r *http.Request, write bool,
) (docker.WorkspaceKey, bool) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return docker.WorkspaceKey{}, false
	}
	tenantID, err := uuid.Parse(r.URL.Query().Get("tenant_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" query parameter is required and must be a UUID`))
		return docker.WorkspaceKey{}, false
	}
	subsAccID, err := uuid.Parse(r.URL.Query().Get("subs_acc_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" query parameter is required and must be a UUID`))
		return docker.WorkspaceKey{}, false
	}
	if write {
		return s.authorizeSecret(w, agent, ident, tenantID, subsAccID)
	}
	return s.authorizeRestartRead(w, agent, ident, tenantID, subsAccID)
}

func (s *Server) handleProjectsList(w http.ResponseWriter, r *http.Request) {
	key, ok := s.resolveProjectCaller(w, r, false)
	if !ok {
		return
	}
	list, err := s.Mgr.ListProjects(key)
	if err != nil {
		s.logf("projects: list failed user=%s: %v", key.UserAccID, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	out := make([]projectResponse, 0, len(list))
	for _, p := range list {
		out = append(out, toProjectResponse(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": out})
}

func (s *Server) handleProjectsPost(w http.ResponseWriter, r *http.Request) {
	key, ok := s.resolveProjectCaller(w, r, true)
	if !ok {
		return
	}
	var req projectWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("body must be a JSON object"))
		return
	}
	if req.Name == nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"name" is required`))
		return
	}
	instructions := ""
	if req.Instructions != nil {
		instructions = *req.Instructions
	}

	p, err := s.Mgr.CreateProject(key, *req.Name, instructions)
	if err != nil {
		writeProjectError(w, s, key, err)
		return
	}
	// The container gains this project's .secrets bind on its next request, when
	// drift detection recreates it. Told to the caller rather than left implicit:
	// the frontend has to warn that the agent restarts, the way it already does
	// for restart-control.
	writeJSON(w, http.StatusCreated, map[string]any{
		"project":         toProjectResponse(p),
		"restart_pending": true,
	})
}

func (s *Server) handleProjectsPatch(w http.ResponseWriter, r *http.Request) {
	key, ok := s.resolveProjectCaller(w, r, true)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var req projectWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("body must be a JSON object"))
		return
	}
	if req.Name == nil && req.Instructions == nil {
		writeJSON(w, http.StatusBadRequest, errBody(`at least one of "name" or "instructions" is required`))
		return
	}

	var p projects.Project
	var err error
	if req.Name != nil {
		if p, err = s.Mgr.RenameProject(key, id, *req.Name); err != nil {
			writeProjectError(w, s, key, err)
			return
		}
	}
	if req.Instructions != nil {
		if p, err = s.Mgr.SetProjectInstructions(key, id, *req.Instructions); err != nil {
			writeProjectError(w, s, key, err)
			return
		}
	}
	// Neither a rename nor an instruction edit changes a mount, so no restart is
	// implied — the agent re-reads AGENT.md on its next turn.
	writeJSON(w, http.StatusOK, map[string]any{"project": toProjectResponse(p)})
}

func (s *Server) handleProjectsDelete(w http.ResponseWriter, r *http.Request) {
	key, ok := s.resolveProjectCaller(w, r, true)
	if !ok {
		return
	}
	if err := s.Mgr.DeleteProject(key, r.PathValue("id")); err != nil {
		writeProjectError(w, s, key, err)
		return
	}
	// Deleting removed the workspace, its files and its transcripts. The caller
	// is expected to have confirmed that; saying so here keeps the API honest
	// about what just happened.
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted":         true,
		"restart_pending": true,
	})
}

// writeProjectError maps the store's sentinels onto status codes. Anything
// unrecognized is a 500 — a store error is an infrastructure failure, not the
// caller's fault, and mapping it to 400 would send the frontend chasing the
// user's input.
func writeProjectError(w http.ResponseWriter, s *Server, key docker.WorkspaceKey, err error) {
	switch {
	case errors.Is(err, projects.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errBody(err.Error()))
	case errors.Is(err, projects.ErrDuplicate):
		writeJSON(w, http.StatusConflict, errBody(err.Error()))
	case errors.Is(err, projects.ErrEmptyName):
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
	default:
		s.logf("projects: operation failed user=%s: %v", key.UserAccID, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
	}
}

// workspaceSegmentFor resolves the optional `project` parameter into the
// workspace segment the request should read or write.
//
// An unknown project is refused, never silently downgraded to the main
// workspace. Falling through would answer as the default agent and write the
// conversation into the main workspace — a failure the user experiences as lost
// history days later, not as an error now.
//
// QUERY ONLY, deliberately. handleMediaPost is multipart and carries the project
// as a form field, so it uses checkProject instead; widening this helper to also
// read the form would make every GET and DELETE on it parse a body they do not
// have. The two are not interchangeable — reading the query on that one handler
// is what sent project uploads into the main workspace.
func (s *Server) workspaceSegmentFor(
	w http.ResponseWriter, r *http.Request, key docker.WorkspaceKey,
) (segment, projectID string, ok bool) {
	projectID = r.URL.Query().Get("project")
	if projectID == "" {
		return config.MainWorkspace, "", true
	}
	exists, err := s.Mgr.HasProject(key, projectID)
	if err != nil {
		s.logf("projects: lookup failed user=%s project=%s: %v", key.UserAccID, projectID, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return "", "", false
	}
	if !exists {
		writeJSON(w, http.StatusNotFound, errBody("unknown project: "+projectID))
		return "", "", false
	}
	return config.ProjectWorkspace(projectID), projectID, true
}

// projectSessionID applies the project prefix a dispatch rule matches on. Kept
// next to the segment resolution above so the two stay in step: a request that
// reads a project's transcripts must be the same one that wrote them.
func projectSessionID(projectID, sessionKey string) string {
	return identity.ProjectSessionID(projectID, sessionKey)
}

// checkProject refuses an unknown project id. Body-borne twin of
// workspaceSegmentFor, for handlers that take the project in JSON rather than a
// query parameter.
func (s *Server) checkProject(w http.ResponseWriter, key docker.WorkspaceKey, projectID string) bool {
	if projectID == "" {
		return true
	}
	exists, err := s.Mgr.HasProject(key, projectID)
	if err != nil {
		s.logf("projects: lookup failed user=%s project=%s: %v", key.UserAccID, projectID, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return false
	}
	if !exists {
		writeJSON(w, http.StatusNotFound, errBody("unknown project: "+projectID))
		return false
	}
	return true
}

// workspaceSegmentOf mirrors the proxy-side helper for the HTTP layer, which
// resolves a project id it already validated.
func workspaceSegmentOf(project string) string {
	if project == "" {
		return config.MainWorkspace
	}
	return config.ProjectWorkspace(project)
}
