package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"github.com/google/uuid"
)

// The member's own skills.
//
// A separate surface from /v1/admin/skills, and it has to be: handlers.go's
// member block records that there is deliberately NO admin route that reads or
// writes member content (FR-7 of the admin feature). A member's skills are
// member content, so they are reached with the member's own licence chain and
// a workspace key whose UserAccID comes from the mycelium profile rather than
// from the request.
//
// NO PROJECT PARAMETER, on any of the four. The harness's skill loader is fixed
// on the main workspace at boot, so a skill written into workspace-<id>/skills
// is read by nothing; accepting the parameter would let a member write inert
// files. See the feature spec's DEC-5.
//
// No restart policy either, unlike the admin routes: the write lands in the
// live workspace directory and the ganglion re-reads its index from disk each
// turn, so the change is in the next message's system prompt.

// memberSkillMaxBytes caps a SKILL.md. Generous, because only the frontmatter
// reaches the prompt -- the body is read with the shell when the agent decides
// the skill applies, so a long one costs nothing until it is opened.
const memberSkillMaxBytes = 256 << 10

func (s *Server) handleSkillsList(w http.ResponseWriter, r *http.Request) {
	agent, ident, key, ok := s.resolveSkillCaller(w, r, r.URL.Query().Get)
	if !ok {
		return
	}
	skills, err := s.Mgr.ListMemberSkills(key, agent.Harness)
	if err != nil {
		s.logf("skills: list failed svc=%s user=%s: %v", agent.Key, ident.AccID, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": skills})
}

// handleSkillsDoc serves ONE FILE of a skill. `path` is relative to the skill's
// own directory and defaults to SKILL.md, so the route keeps answering its
// original shape for callers that do not know about supporting files.
//
// It serves all three layers. A member may read the administrator's and the
// operator's skills, including their templates and scripts: that is the point of
// listing them at all -- they explain why the agent behaves as it does, and a
// skill whose referenced template is invisible cannot be understood.
func (s *Server) handleSkillsDoc(w http.ResponseWriter, r *http.Request) {
	agent, ident, key, ok := s.resolveSkillCaller(w, r, r.URL.Query().Get)
	if !ok {
		return
	}
	q := r.URL.Query()
	content, file, origin, err := s.Mgr.ReadMemberSkillFile(key, agent.Harness, q.Get("name"), q.Get("path"))
	if err != nil {
		s.writeSkillError(w, "read", agent.Key, ident.AccID, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": q.Get("name"), "path": file.Path, "content": content, "file": file, "origin": origin,
	})
}

// handleSkillsFiles lists everything inside one skill. A skill is a DIRECTORY,
// and one that carries a template or a script is common enough that hiding them
// behind a "has files" badge left the member unable to see what they had.
func (s *Server) handleSkillsFiles(w http.ResponseWriter, r *http.Request) {
	agent, ident, key, ok := s.resolveSkillCaller(w, r, r.URL.Query().Get)
	if !ok {
		return
	}
	files, origin, err := s.Mgr.ListMemberSkillFiles(key, agent.Harness, r.URL.Query().Get("name"))
	if err != nil {
		s.writeSkillError(w, "files", agent.Key, ident.AccID, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files, "origin": origin})
}

func (s *Server) handleSkillsPut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TenantID  string `json:"tenant_id"`
		SubsAccID string `json:"subs_acc_id"`
		Name      string `json:"name"`
		// Path is relative to the skill's own directory; empty means SKILL.md.
		Path    string `json:"path,omitempty"`
		Content string `json:"content"`
		// ModifiedAt is the version the caller read. Empty means "create".
		// The harness's evolution writes this same directory in apply mode, so an
		// unconditional write can drop a skill the agent learned with no trace.
		ModifiedAt string `json:"modifiedAt,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, memberSkillMaxBytes+(1<<10))).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body (or content exceeds the size limit)"))
		return
	}
	if len(req.Content) > memberSkillMaxBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge,
			errBody(fmt.Sprintf("content exceeds the %d-byte limit", memberSkillMaxBytes)))
		return
	}
	agent, ident, key, ok := s.resolveSkillCaller(w, r, func(k string) string {
		switch k {
		case "tenant_id":
			return req.TenantID
		case "subs_acc_id":
			return req.SubsAccID
		}
		return ""
	})
	if !ok {
		return
	}
	file, err := s.Mgr.WriteMemberSkillFile(key, agent.Harness, req.Name, req.Path, req.Content, req.ModifiedAt)
	if err != nil {
		s.writeSkillError(w, "write", agent.Key, ident.AccID, err)
		return
	}
	s.logf("skills: wrote svc=%s user=%s name=%s path=%s bytes=%d",
		agent.Key, ident.AccID, req.Name, file.Path, len(req.Content))
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "name": req.Name, "path": file.Path, "file": file,
	})
}

func (s *Server) handleSkillsDelete(w http.ResponseWriter, r *http.Request) {
	agent, ident, key, ok := s.resolveSkillCaller(w, r, r.URL.Query().Get)
	if !ok {
		return
	}
	name := r.URL.Query().Get("name")
	if err := s.Mgr.DeleteMemberSkill(key, agent.Harness, name); err != nil {
		s.writeSkillError(w, "delete", agent.Key, ident.AccID, err)
		return
	}
	s.logf("skills: deleted svc=%s user=%s name=%s", agent.Key, ident.AccID, name)
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "name": name})
}

// resolveSkillCaller runs the member chain and returns the caller's own
// workspace key. `get` is parameterised so the same function serves the query
// string and the PUT body, the way adminScope already does.
//
// authorizeSecret -- the WRITE chain -- on the reads too, matching /v1/memory
// beside it. Stricter than the route needs, and the same licence the member
// needs to say anything to this agent at all.
func (s *Server) resolveSkillCaller(
	w http.ResponseWriter, r *http.Request, get func(string) string,
) (config.Agent, identity.Identity, docker.WorkspaceKey, bool) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return config.Agent{}, identity.Identity{}, docker.WorkspaceKey{}, false
	}
	tenantID, err := uuid.Parse(get("tenant_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" is required and must be a UUID`))
		return config.Agent{}, identity.Identity{}, docker.WorkspaceKey{}, false
	}
	subsAccID, err := uuid.Parse(get("subs_acc_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" is required and must be a UUID`))
		return config.Agent{}, identity.Identity{}, docker.WorkspaceKey{}, false
	}
	key, ok := s.authorizeSecret(w, agent, ident, tenantID, subsAccID)
	if !ok {
		return config.Agent{}, identity.Identity{}, docker.WorkspaceKey{}, false
	}
	return agent, ident, key, true
}

// writeSkillError maps the store's vocabulary onto status codes.
//
// A read-only layer is 403 and not 404: the name is real and the member can see
// it in the list, so pretending it does not exist would be a worse answer than
// saying they may not have it. Same reasoning as ErrMediaReserved.
func (s *Server) writeSkillError(w http.ResponseWriter, op, svc, user string, err error) {
	switch {
	case errors.Is(err, docker.ErrSkillReadOnly):
		writeJSON(w, http.StatusForbidden, errBody(err.Error()))
	case errors.Is(err, docker.ErrSkillConflict), errors.Is(err, docker.ErrSkillExists):
		writeJSON(w, http.StatusConflict, errBody(err.Error()))
	case errors.Is(err, docker.ErrInvalidSkillName),
		errors.Is(err, docker.ErrReservedSkillName),
		errors.Is(err, docker.ErrSkillMetadata),
		errors.Is(err, docker.ErrSkillNameMismatch),
		errors.Is(err, docker.ErrSkillFilePath),
		errors.Is(err, docker.ErrSkillFileBinary),
		errors.Is(err, docker.ErrMediaName):
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
	case errors.Is(err, docker.ErrMediaNotFound), os.IsNotExist(err):
		writeJSON(w, http.StatusNotFound, errBody("no skill by that name"))
	default:
		s.logf("skills: %s failed svc=%s user=%s: %v", op, svc, user, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
	}
}
