package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
)

// Member-driven organisation of the uploads tree: create a folder, move a file or
// folder, delete a folder and its contents.
//
// Same authorization chain as the rest of media — resolveSecretCaller then
// authorizeSecret — because these write into the same directory `POST /v1/media`
// does. A read-only member cannot reorganise a workspace any more than they can
// upload to it.

// mediaFolderRequest is the body all three take. `to` is used only by the move.
type mediaFolderRequest struct {
	TenantID  string `json:"tenant_id"`
	SubsAccID string `json:"subs_acc_id"`
	Path      string `json:"path"`
	To        string `json:"to"`
	// agent-projects: empty means the agent's own workspace.
	Project string `json:"project,omitempty"`
}

// mediaFolderCaller decodes and authorizes, returning the workspace and the body.
func (s *Server) mediaFolderCaller(w http.ResponseWriter, r *http.Request) (docker.WorkspaceKey, string, mediaFolderRequest, bool) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return docker.WorkspaceKey{}, "", mediaFolderRequest{}, false
	}
	var req mediaFolderRequest
	// 64 KiB is far more than three short strings need; the cap exists so the body
	// cannot be used to make the proxy allocate.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return docker.WorkspaceKey{}, "", mediaFolderRequest{}, false
	}
	tenantID, err := uuid.Parse(req.TenantID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" is required and must be a UUID`))
		return docker.WorkspaceKey{}, "", mediaFolderRequest{}, false
	}
	subsAccID, err := uuid.Parse(req.SubsAccID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" is required and must be a UUID`))
		return docker.WorkspaceKey{}, "", mediaFolderRequest{}, false
	}
	if req.Path == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"path" is required`))
		return docker.WorkspaceKey{}, "", mediaFolderRequest{}, false
	}
	key, ok := s.authorizeSecret(w, agent, ident, tenantID, subsAccID)
	if !ok {
		return docker.WorkspaceKey{}, "", mediaFolderRequest{}, false
	}
	// agent-projects: folders belong to the workspace of the project they were
	// created in. The id travels in the BODY here, like every other field this
	// caller reads, rather than in the query string.
	if req.Project != "" {
		exists, err := s.Mgr.HasProject(key, req.Project)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
			return docker.WorkspaceKey{}, "", mediaFolderRequest{}, false
		}
		if !exists {
			writeJSON(w, http.StatusNotFound, errBody("unknown project: "+req.Project))
			return docker.WorkspaceKey{}, "", mediaFolderRequest{}, false
		}
	}
	return key, req.Project, req, true
}

// writeMediaFolderError maps the domain's refusals onto statuses a client can act on.
// They are all 4xx: every one is something the member asked for that cannot be done,
// not a failure of ours.
func (s *Server) writeMediaFolderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, docker.ErrMediaNotFound):
		writeJSON(w, http.StatusNotFound, errBody(err.Error()))
	case errors.Is(err, docker.ErrMediaExists):
		// 409, not 400: the request was well-formed, the world disagreed. The UI shows
		// "that name is taken" rather than "bad request".
		writeJSON(w, http.StatusConflict, errBody(err.Error()))
	case errors.Is(err, docker.ErrMediaReserved):
		// 403, not 400: the request is well-formed and the path exists — the member
		// simply does not own it. The interface says "managed by the system".
		writeJSON(w, http.StatusForbidden, errBody(err.Error()))
	case errors.Is(err, docker.ErrMediaName),
		errors.Is(err, docker.ErrMediaIntoSelf),
		errors.Is(err, docker.ErrMediaNotFolder),
		errors.Is(err, docker.ErrMediaRoot):
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
	default:
		s.logf("media folders: %v", err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
	}
}

// handleMediaFolderCreate is POST /v1/media/folder.
func (s *Server) handleMediaFolderCreate(w http.ResponseWriter, r *http.Request) {
	key, project, req, ok := s.mediaFolderCaller(w, r)
	if !ok {
		return
	}
	if err := s.Mgr.CreateFolder(key, project, mediaRelPath(req.Path)); err != nil {
		s.writeMediaFolderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "created", "path": req.Path})
}

// handleMediaMove is POST /v1/media/move. It covers renaming too: a move within the
// same parent is a rename, and it is the same call with the same failure modes.
func (s *Server) handleMediaMove(w http.ResponseWriter, r *http.Request) {
	key, project, req, ok := s.mediaFolderCaller(w, r)
	if !ok {
		return
	}
	if req.To == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"to" is required`))
		return
	}
	if err := s.Mgr.MoveMedia(key, project, mediaRelPath(req.Path), mediaRelPath(req.To)); err != nil {
		s.writeMediaFolderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "moved", "path": req.Path, "to": req.To})
}

// handleMediaFolderDelete is DELETE /v1/media/folder — recursive, and destructive.
//
// The response reports how many FILES went with it. The interface names that count in
// its confirmation BEFORE the call, from the tree it already has; the number here is
// what it reports afterwards, and the two disagreeing is how a member finds out the
// agent wrote something in between.
func (s *Server) handleMediaFolderDelete(w http.ResponseWriter, r *http.Request) {
	key, project, req, ok := s.mediaFolderCaller(w, r)
	if !ok {
		return
	}
	removed, err := s.Mgr.DeleteFolder(key, project, mediaRelPath(req.Path))
	if err != nil {
		s.writeMediaFolderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK,
		map[string]any{"status": "deleted", "path": req.Path, "removedFiles": removed})
}
