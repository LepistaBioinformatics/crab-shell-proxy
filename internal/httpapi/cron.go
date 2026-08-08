package httpapi

// The member-facing read surface over picoclaw's scheduled tasks.
//
// Read-gated like GET /v1/restart rather than write-gated: a member who only holds
// read on the agent still has to be able to see what is scheduled and what it
// produced.
//
// Read-only on purpose. picoclaw owns the store and holds the live schedule in
// memory; whether it reloads an externally edited jobs.json is unverified, so a
// write here could silently disagree with the timers actually running.

import (
	"net/http"
	"sort"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/cron"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/history"
)

// cronTask is a job from the store plus the runs discovered for it. The embedded
// Job flattens into the response, so the client sees picoclaw's own field names.
type cronTask struct {
	cron.Job
	Runs []history.CronRun `json:"runs"`
}

// cronOrphanGroup holds runs whose job is no longer in the store. That is the
// normal end state of a one-shot job: deleteAfterRun removes the record and leaves
// the transcripts, so these are executions the user can still legitimately open —
// they just have no schedule left to describe.
type cronOrphanGroup struct {
	JobID string            `json:"jobId"`
	Runs  []history.CronRun `json:"runs"`
}

type cronTasksResponse struct {
	Tasks   []cronTask        `json:"tasks"`
	Orphans []cronOrphanGroup `json:"orphans"`
}

// handleCronTasks lists the caller's own scheduled tasks with their executions.
func (s *Server) handleCronTasks(w http.ResponseWriter, r *http.Request) {
	key, ok := s.restartCallerKey(w, r, false)
	if !ok {
		return
	}
	// agent-projects: scheduled jobs live in the workspace of the agent that owns
	// them, so a project has its own cron store.
	segment, _, ok := s.workspaceSegmentFor(w, r, key)
	if !ok {
		return
	}

	jobs, err := cron.Load(config.CronFile(s.Cfg.ContainerDataRoot,
		key.TenantID, key.SubsAccID, key.Role, key.UserAccID, segment))
	if err != nil {
		s.logf("cron: read store failed key=%+v: %v", key, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	runs, err := history.CronRuns(config.SessionsDir(s.Cfg.ContainerDataRoot,
		key.TenantID, key.SubsAccID, key.Role, key.UserAccID, segment))
	if err != nil {
		s.logf("cron: read runs failed key=%+v: %v", key, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}

	byJob := map[string][]history.CronRun{}
	for _, run := range runs {
		byJob[run.JobID] = append(byJob[run.JobID], run)
	}

	// Store order is preserved so the panel lists what `picoclaw cron list` lists.
	tasks := make([]cronTask, 0, len(jobs))
	for _, j := range jobs {
		owned := byJob[j.ID]
		if owned == nil {
			owned = []history.CronRun{} // an array the client can iterate, not null
		}
		tasks = append(tasks, cronTask{Job: j, Runs: owned})
		delete(byJob, j.ID)
	}

	orphans := make([]cronOrphanGroup, 0, len(byJob))
	for jobID, owned := range byJob {
		orphans = append(orphans, cronOrphanGroup{JobID: jobID, Runs: owned})
	}
	// Map iteration is random, so sort: newest execution first, tie-broken by job
	// id. Runs arrive newest-first from CronRuns, so runs[0] is the group's newest.
	sort.SliceStable(orphans, func(i, j int) bool {
		a, b := orphans[i], orphans[j]
		if a.Runs[0].StartedAt != b.Runs[0].StartedAt {
			return a.Runs[0].StartedAt > b.Runs[0].StartedAt
		}
		return a.JobID < b.JobID
	})

	writeJSON(w, http.StatusOK, cronTasksResponse{Tasks: tasks, Orphans: orphans})
}

// handleCronRun serves one execution's whole transcript, tool activity included.
//
// The "run" parameter names a file, so it is resolved against the runs actually
// discovered in the CALLER'S OWN sessions dir. Traversal and cross-workspace reads
// are impossible by construction rather than by sanitising the input.
func (s *Server) handleCronRun(w http.ResponseWriter, r *http.Request) {
	key, ok := s.restartCallerKey(w, r, false)
	if !ok {
		return
	}
	// agent-projects: scheduled jobs live in the workspace of the agent that owns
	// them, so a project has its own cron store.
	segment, _, ok := s.workspaceSegmentFor(w, r, key)
	if !ok {
		return
	}
	sessionsDir := config.SessionsDir(s.Cfg.ContainerDataRoot,
		key.TenantID, key.SubsAccID, key.Role, key.UserAccID, segment)

	runs, err := history.CronRuns(sessionsDir)
	if err != nil {
		s.logf("cron: read runs failed key=%+v: %v", key, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	requested := r.URL.Query().Get("run")
	known := false
	for _, run := range runs {
		if run.Basename == requested {
			known = true
			break
		}
	}
	if !known {
		writeJSON(w, http.StatusBadRequest,
			errBody(`"run" query parameter must name a known scheduled-task run`))
		return
	}

	entries, err := history.ReadCronRun(sessionsDir, requested)
	if err != nil {
		s.logf("cron: read transcript failed run=%s: %v", requested, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}
