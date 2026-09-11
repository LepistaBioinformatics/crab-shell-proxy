package httpapi

// The mutating half of /v1/cron/tasks: create, edit, delete.
//
// New, and ganglion-only (FR-B7, FR-B8). picoclaw is refused rather than served,
// because its schedule lives in timers this process cannot see — writing its
// jobs.json from here would produce a record the member can see and a timer that
// never changed. Its agent creates its own tasks from inside a conversation, which
// is the surface that still works there.
//
// The member is the author here, not the agent. The store is above the workspace
// bind, so nothing inside the container can reach it: a turn steered by untrusted
// text cannot schedule its own future turns.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/cron"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
)

// maxScheduledMessage caps the prompt a job fires with. Generous, and a limit all
// the same: this string is replayed as a turn on every occurrence, forever, with
// nobody reading the result.
const maxScheduledMessage = 8 << 10

// cronTaskRequest is the create/edit body.
//
// Enabled is a POINTER so PATCH can tell "leave it as it is" from "disable it" —
// pausing a task is the most ordinary edit there is, and a plain bool would make
// it unexpressible.
type cronTaskRequest struct {
	ID             string        `json:"id,omitempty"`
	Name           string        `json:"name"`
	Enabled        *bool         `json:"enabled,omitempty"`
	Schedule       cron.Schedule `json:"schedule"`
	Message        string        `json:"message"`
	DeleteAfterRun bool          `json:"deleteAfterRun"`
}

// cronWriteScope authorizes a mutation and resolves what it acts on.
//
// Write access, matching the surface around it: reading what is scheduled is a
// read, changing it is not. The project comes from the query parameter, as it does
// on the read routes, and is validated there — a task cannot be filed under a
// project that does not exist.
func (s *Server) cronWriteScope(
	w http.ResponseWriter, r *http.Request,
) (key docker.WorkspaceKey, agent config.Agent, projectID string, ok bool) {
	agent, status, msg := s.resolveAgent(r)
	if status != 0 {
		writeJSON(w, status, errBody(msg))
		return docker.WorkspaceKey{}, config.Agent{}, "", false
	}
	if agent.Harness != config.HarnessGanglion {
		writeJSON(w, http.StatusNotImplemented, errBody(
			"creating scheduled tasks over this API is not available on the "+
				harnessName(agent)+" harness (agent "+agent.Key+"): its agent creates them itself"))
		return docker.WorkspaceKey{}, config.Agent{}, "", false
	}
	key, ok = s.restartCallerKey(w, r, true)
	if !ok {
		return docker.WorkspaceKey{}, config.Agent{}, "", false
	}
	_, projectID, ok = s.workspaceSegmentFor(w, r, agent.Harness, key)
	if !ok {
		return docker.WorkspaceKey{}, config.Agent{}, "", false
	}
	if s.Schedules == nil {
		s.Schedules = cron.NewOwner()
	}
	return key, agent, projectID, true
}

func harnessName(agent config.Agent) string {
	if agent.Harness == "" {
		return config.HarnessPicoclaw
	}
	return agent.Harness
}

// handleCronTaskCreate stores a new scheduled task.
func (s *Server) handleCronTaskCreate(w http.ResponseWriter, r *http.Request) {
	key, agent, projectID, ok := s.cronWriteScope(w, r)
	if !ok {
		return
	}
	var req cronTaskRequest
	if !decodeCronTask(w, r, &req) {
		return
	}

	now := time.Now()
	job := cron.Job{
		ID:      cron.NewID(),
		Name:    strings.TrimSpace(req.Name),
		Enabled: req.Enabled == nil || *req.Enabled,
		// Trusted verbatim from the member, refused only for shape. The schedule is
		// theirs to state; what it means is cron.Validate's business.
		Schedule: req.Schedule,
		Payload: cron.Payload{
			Kind:    cron.PayloadAgentTurn,
			Message: strings.TrimSpace(req.Message),
		},
		CreatedAtMs:    now.UnixMilli(),
		UpdatedAtMs:    now.UnixMilli(),
		DeleteAfterRun: req.DeleteAfterRun,
		Project:        projectID,
	}
	if job.Name == "" {
		job.Name = firstLine(job.Payload.Message)
	}
	if err := validateCronTask(job); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	}
	// Scheduled here rather than left to the scheduler's first pass, so the record
	// the member gets back already says when it runs. The pass repairs a zero
	// anyway; this is what makes the answer truthful immediately.
	if at, scheduled := cron.NextRun(job.Schedule, now); scheduled {
		job.State.NextRunAtMs = at.UnixMilli()
	} else if job.Schedule.Kind == cron.KindAt {
		writeJSON(w, http.StatusBadRequest, errBody("schedule.atMs is in the past"))
		return
	}

	path := cronStore(s.Cfg, agent.Harness, key)
	err := s.Schedules.Mutate(path, func(jobs []cron.Job) ([]cron.Job, error) {
		return append(jobs, job), nil
	})
	if err != nil {
		s.logf("cron: create failed key=%+v: %v", key, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

// handleCronTaskUpdate edits one task in place.
//
// Every field is optional and an absent one is left alone, so pausing a task and
// rewording it are the same request shape. The id is the only thing required.
func (s *Server) handleCronTaskUpdate(w http.ResponseWriter, r *http.Request) {
	key, agent, projectID, ok := s.cronWriteScope(w, r)
	if !ok {
		return
	}
	var req cronTaskRequest
	if !decodeCronTask(w, r, &req) {
		return
	}
	id := req.ID
	if id == "" {
		id = r.URL.Query().Get("id")
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"id" is required`))
		return
	}

	now := time.Now()
	var updated cron.Job
	err := s.Schedules.Mutate(cronStore(s.Cfg, agent.Harness, key), func(jobs []cron.Job) ([]cron.Job, error) {
		for i := range jobs {
			if jobs[i].ID != id {
				continue
			}
			// A task belongs to the scope it was filed under. Editing it from a
			// different one would silently move it — and its runs are already written
			// under the old project's sessions dir.
			if jobs[i].Project != projectID {
				return nil, cron.ErrNotFound
			}
			j := jobs[i]
			if req.Name != "" {
				j.Name = strings.TrimSpace(req.Name)
			}
			if req.Enabled != nil {
				j.Enabled = *req.Enabled
			}
			if req.Message != "" {
				j.Payload.Message = strings.TrimSpace(req.Message)
			}
			if req.Schedule.Kind != "" {
				j.Schedule = req.Schedule
				// The old next-run belongs to the old schedule. Cleared rather than
				// recomputed here so there is exactly one place that answers "when
				// next" — the create path above reschedules, and so does the pass.
				j.State.NextRunAtMs = 0
				if at, scheduled := cron.NextRun(j.Schedule, now); scheduled {
					j.State.NextRunAtMs = at.UnixMilli()
				}
			}
			j.UpdatedAtMs = now.UnixMilli()
			if err := validateCronTask(j); err != nil {
				return nil, err
			}
			jobs[i] = j
			updated = j
			return jobs, nil
		}
		return nil, cron.ErrNotFound
	})
	switch {
	case err == cron.ErrNotFound:
		writeJSON(w, http.StatusNotFound, errBody("unknown scheduled task: "+id))
		return
	case err != nil && isCronValidation(err):
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	case err != nil:
		s.logf("cron: update failed key=%+v: %v", key, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// handleCronTaskDelete removes one task.
//
// Its RUNS are left on disk. That is the same end state a one-shot job reaches
// through deleteAfterRun, which the read side already renders as an orphan group —
// work the agent actually did is not something a member deletes by tidying up
// their schedule.
func (s *Server) handleCronTaskDelete(w http.ResponseWriter, r *http.Request) {
	key, agent, projectID, ok := s.cronWriteScope(w, r)
	if !ok {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"id" query parameter is required`))
		return
	}
	err := s.Schedules.Mutate(cronStore(s.Cfg, agent.Harness, key), func(jobs []cron.Job) ([]cron.Job, error) {
		for i := range jobs {
			if jobs[i].ID != id || jobs[i].Project != projectID {
				continue
			}
			return append(jobs[:i:i], jobs[i+1:]...), nil
		}
		return nil, cron.ErrNotFound
	})
	switch {
	case err == cron.ErrNotFound:
		writeJSON(w, http.StatusNotFound, errBody("unknown scheduled task: "+id))
	case err != nil:
		s.logf("cron: delete failed key=%+v: %v", key, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func decodeCronTask(w http.ResponseWriter, r *http.Request, req *cronTaskRequest) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body: "+err.Error()))
		return false
	}
	return true
}

// cronValidationError marks a refusal the member can act on, so the mutation
// helpers can tell it apart from a store that failed to be read.
type cronValidationError struct{ error }

func isCronValidation(err error) bool {
	_, ok := err.(cronValidationError)
	return ok
}

func validateCronTask(j cron.Job) error {
	if len(j.Payload.Message) > maxScheduledMessage {
		return cronValidationError{errors.New("payload.message is too long")}
	}
	if err := cron.Validate(j); err != nil {
		return cronValidationError{err}
	}
	return nil
}

// firstLine is the fallback name for a task the member did not name: the opening
// of what it will say. Better than "Untitled" in a list whose whole job is to let
// someone recognise their own task.
func firstLine(message string) string {
	line := message
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	const max = 60
	if len(line) > max {
		// Cut on a rune boundary; a name is displayed, and half a character is not.
		cut := max
		for cut > 0 && !utf8ValidCut(line, cut) {
			cut--
		}
		line = strings.TrimSpace(line[:cut]) + "…"
	}
	if line == "" {
		return "Scheduled task"
	}
	return line
}

func utf8ValidCut(s string, i int) bool {
	return i <= len(s) && (i == len(s) || s[i]&0xC0 != 0x80)
}
