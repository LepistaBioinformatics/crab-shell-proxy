package httpapi

// The scheduler: the half of scheduled tasks that picoclaw keeps inside its own
// container and the ganglion does not have at all.
//
// WHY IT IS HERE AND NOT IN THE HARNESS. The ganglion runs scale-to-zero — that
// is the point of it — so a clock inside it sleeps through its own work: the
// container is stopped at the moment the job is due, and nothing inside a stopped
// container wakes it. The only participant that is always up is this proxy, which
// is also the only one that can start the container. So the schedule lives here,
// and firing a job is: wake the container, run a turn, write the run down.
//
// WHAT IT DOES NOT DO: deliver. A fired job answers nobody (FR-B4). Its run is
// stored and read back from the Tasks panel, exactly as a picoclaw run is — the
// whole read surface (history.CronRuns, /v1/cron/runs, the panel) is harness-blind
// and unchanged.
//
// picoclaw agents are skipped entirely. Two schedulers over one jobs.json would
// race, and picoclaw's timers are in its own memory where this process cannot see
// them.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/cron"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/history"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
)

// How long the scheduler sleeps between passes.
//
// Bounded rather than fixed: the pass computes the next due instant across every
// store it read and sleeps until then, clamped into this window. The ceiling keeps
// a store edited by a request from waiting a long time to be noticed; the floor
// keeps a badly-shaped schedule from spinning.
//
// A full re-scan each pass, with no write-triggers-rescan path, is deliberate:
// the state is on disk, so a proxy restart resumes from it with no recovery step,
// and the cost of a pass is a directory walk over the data root.
const (
	schedulerMinSleep = 1 * time.Second
	schedulerMaxSleep = 30 * time.Second
)

// scheduledTurnTimeout bounds one fired turn. Longer than a member's, because
// nobody is waiting on it and an unattended research task legitimately runs long;
// bounded all the same, because a turn that never returns holds its workspace's
// slot forever (see running).
const scheduledTurnTimeout = 30 * time.Minute

// scheduleRef is one user's store plus the identity needed to run a turn under it.
type scheduleRef struct {
	Agent config.Agent
	Key   docker.WorkspaceKey
	Email string
	Path  string
}

// StartScheduler runs the scheduler until ctx is done. Safe to call once.
//
// It returns immediately; the loop is a goroutine, because the caller is main and
// the proxy must listen before its first pass finishes.
func (s *Server) StartScheduler(ctx context.Context) {
	if s.Schedules == nil {
		s.Schedules = cron.NewOwner()
	}
	go func() {
		for {
			sleep := s.schedulerPass(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(sleep):
			}
		}
	}()
}

// schedulerPass fires everything due and returns how long to sleep.
func (s *Server) schedulerPass(ctx context.Context) time.Duration {
	now := time.Now()
	next := now.Add(schedulerMaxSleep)
	for _, ref := range s.scheduleRefs() {
		fire, due, err := s.claimDue(ref, now)
		if err != nil {
			s.logf("cron: pass failed user=%s: %v", ref.Key.UserAccID, err)
			continue
		}
		for _, job := range fire {
			go s.runScheduledJob(ctx, ref, job)
		}
		if !due.IsZero() && due.Before(next) {
			next = due
		}
	}
	sleep := time.Until(next)
	if sleep < schedulerMinSleep {
		sleep = schedulerMinSleep
	}
	if sleep > schedulerMaxSleep {
		sleep = schedulerMaxSleep
	}
	return sleep
}

// scheduleRefs enumerates the stores this scheduler owns.
//
// Walks the data root rather than keeping an index, for the reason the pass does:
// the disk is the state. A user with no store contributes a stat and nothing else.
func (s *Server) scheduleRefs() []scheduleRef {
	tenants, err := s.Mgr.ListTenants()
	if err != nil {
		s.logf("cron: list tenants failed: %v", err)
		return nil
	}
	var out []scheduleRef
	for _, tenantID := range tenants {
		subs, err := s.Mgr.ListTenantSubscriptions(tenantID)
		if err != nil {
			s.logf("cron: list subscriptions failed tenant=%s: %v", tenantID, err)
			continue
		}
		for _, subsAccID := range subs {
			users, err := s.Mgr.ListSubscriptionUsers(tenantID, subsAccID)
			if err != nil {
				s.logf("cron: list users failed tenant=%s subs=%s: %v", tenantID, subsAccID, err)
				continue
			}
			for _, u := range users {
				agent, ok := s.Cfg.Agents[u.Role]
				// An agent removed from the config leaves its workspaces on disk. Its
				// schedules stop firing, which is the same thing that happens to its
				// chats, and is why this is not logged per pass.
				if !ok || agent.Harness != config.HarnessGanglion {
					continue
				}
				out = append(out, scheduleRef{
					Agent: agent,
					Key: docker.WorkspaceKey{
						TenantID: tenantID, SubsAccID: subsAccID,
						Role: u.Role, UserAccID: u.AccID,
					},
					Email: u.Email,
					Path: config.SchedulesFile(s.Cfg.ContainerDataRoot,
						tenantID, subsAccID, u.Role, u.AccID),
				})
			}
		}
	}
	return out
}

// claimDue claims every job of one store that is due and returns them, together
// with the earliest instant this store next needs attention (zero when none).
//
// It does not run anything: the caller launches the turns. Keeping the
// bookkeeping separate from the firing is what makes the bookkeeping — which is
// all of the reasoning below — testable without a container.
//
// CLAIM THEN RUN, not run then record. The next run and the last-run stamp are
// written before the turn starts, so a proxy that dies mid-turn loses that run
// rather than re-firing it on every boot forever. At-most-once is the right
// trade for unattended work: a missed daily summary is a gap, a re-fired one is
// an agent doing real work twice with no one watching.
func (s *Server) claimDue(ref scheduleRef, now time.Time) ([]cron.Job, time.Time, error) {
	var fire []cron.Job
	var next time.Time
	advance := func(at time.Time) {
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}

	err := s.Schedules.Mutate(ref.Path, func(jobs []cron.Job) ([]cron.Job, error) {
		out := make([]cron.Job, 0, len(jobs))
		for _, j := range jobs {
			if !j.Enabled {
				out = append(out, j)
				continue
			}
			// A job stored with no next run has never been scheduled (or its store
			// predates this field). Compute it; do NOT treat zero as "overdue", which
			// would fire every task in the store the first time it is read.
			if j.State.NextRunAtMs == 0 {
				if at, ok := cron.NextRun(j.Schedule, now); ok {
					j.State.NextRunAtMs = at.UnixMilli()
					j.UpdatedAtMs = now.UnixMilli()
					advance(at)
				} else {
					j.Enabled = false
				}
				out = append(out, j)
				continue
			}
			due := time.UnixMilli(j.State.NextRunAtMs)
			if due.After(now) {
				advance(due)
				out = append(out, j)
				continue
			}
			// Due, possibly long ago — the proxy may have been down. It fires ONCE
			// and reschedules from now rather than replaying every occurrence it
			// slept through, which for an hourly job over a weekend would be a
			// hundred unattended turns arriving at once.
			if s.running(ref.Key) {
				// FR-B9: a run already in flight for this workspace. Left due, so the
				// next pass picks it up — queued, never two turns at once in one
				// container.
				advance(now.Add(schedulerMinSleep))
				out = append(out, j)
				continue
			}
			fire = append(fire, j)
			j.State.LastRunAtMs = now.UnixMilli()
			j.UpdatedAtMs = now.UnixMilli()
			at, ok := cron.NextRun(j.Schedule, now)
			if ok {
				j.State.NextRunAtMs = at.UnixMilli()
				advance(at)
			} else {
				// A one-shot that has run. deleteAfterRun removes the record and
				// leaves the transcript, which is what makes an orphan run a normal
				// end state rather than a lost one (see cronOrphanGroup).
				j.State.NextRunAtMs = 0
				if j.DeleteAfterRun {
					continue
				}
				j.Enabled = false
			}
			out = append(out, j)
		}
		return out, nil
	})
	if err != nil {
		return nil, time.Time{}, err
	}
	return fire, next, nil
}

// runScheduledJob wakes the container, runs the turn, and writes the run down.
//
// Detached from the pass's context deadline but NOT from its cancellation: a
// shutdown stops a scheduled turn the same way it stops a member's.
func (s *Server) runScheduledJob(ctx context.Context, ref scheduleRef, job cron.Job) {
	if !s.claim(ref.Key) {
		return
	}
	defer s.release(ref.Key)

	runID := randomHex(6)
	sessionID := history.CronSessionID(job.ID, runID)
	startedAt := time.Now()

	turnCtx, cancel := context.WithTimeout(ctx, scheduledTurnTimeout)
	defer cancel()

	status, message := s.runScheduledTurn(turnCtx, ref, job, sessionID)

	// The meta is written whether or not the turn succeeded. A failed run that
	// left no trace is indistinguishable from a schedule that never fired, and
	// the member has no other way to find out which happened.
	segment := workspaceSegmentOf(ref.Agent.Harness, job.Project)
	sessionsDir := config.SessionsDir(s.Cfg.ContainerDataRoot,
		ref.Key.TenantID, ref.Key.SubsAccID, ref.Key.Role, ref.Key.UserAccID, segment)
	if err := history.WriteCronMeta(sessionsDir, sessionID, "", startedAt, time.Now()); err != nil {
		s.logf("cron: write run meta failed job=%s run=%s: %v", job.ID, runID, err)
	}

	if status == cron.StatusError {
		s.logf("cron: job %s run %s failed: %s", job.ID, runID, message)
	}
	s.recordOutcome(ref, job.ID, status, message)
}

// runScheduledTurn is the turn itself, with the sink that listens to nothing.
func (s *Server) runScheduledTurn(
	ctx context.Context, ref scheduleRef, job cron.Job, sessionID string,
) (status, message string) {
	tgt, err := s.Mgr.EnsureRunning(ctx, ref.Agent, ref.Key, ref.Email)
	if err != nil {
		return cron.StatusError, err.Error()
	}
	// Re-armed even on failure: the container was woken for this job and has no
	// member watching it, so leaving the idle timer disarmed would keep a
	// scale-to-zero agent up until its next chat.
	defer s.Mgr.ArmIdle(ref.Agent, ref.Key)

	// The harness-reported failure, which arrives as a signal rather than as an
	// error: a turn can fail and still return content.
	var reported string
	_, err = s.turnerFor(tgt.Harness).RunTurn(ctx, turn.Request{
		Endpoint:  tgt.Endpoint,
		AuthToken: tgt.AuthToken,
		// The run's OWN conversation. It is what keeps a scheduled turn from
		// appending to a member's window: the harness's single-flight and its
		// transcript are both per conversation.
		SessionID:  sessionID,
		SessionKey: ref.Key.UserAccID + ":" + ref.Key.Role,
		// No model. The harness resolves its own chain, which is exactly what a
		// member's turn gets when they have chosen none.
		Project: job.Project,
		Content: job.Payload.Message,
	}, turn.Sink{
		Error: func(msg string) { reported = msg },
	})
	if err != nil {
		return cron.StatusError, err.Error()
	}
	if reported != "" {
		return cron.StatusError, reported
	}
	return cron.StatusOK, ""
}

// recordOutcome writes lastStatus/lastError back onto the job.
//
// FR-B6: the panel has always rendered these two fields and picoclaw never wrote
// a value anyone observed. A job deleted or edited while its run was in flight is
// not an error — the outcome simply has nowhere to go.
func (s *Server) recordOutcome(ref scheduleRef, jobID, status, message string) {
	err := s.Schedules.Mutate(ref.Path, func(jobs []cron.Job) ([]cron.Job, error) {
		for i := range jobs {
			if jobs[i].ID != jobID {
				continue
			}
			jobs[i].State.LastStatus = status
			jobs[i].State.LastError = message
			jobs[i].UpdatedAtMs = time.Now().UnixMilli()
		}
		return jobs, nil
	})
	if err != nil && !errors.Is(err, cron.ErrNotFound) {
		s.logf("cron: record outcome failed job=%s: %v", jobID, err)
	}
}

// --- one run at a time per workspace (FR-B9) -------------------------------

// claim reserves the workspace for a scheduled run, or reports that one is
// already in flight.
//
// Scheduled runs are serialised per WORKSPACE, not globally and not with the
// member's own turns. Globally would let one slow task delay every other member's
// schedules; with the member's turns would mean a scheduled job silently waiting
// on a conversation that may run for ten minutes. Two SCHEDULED turns in one
// container is the case worth excluding: they share a workspace, and nobody is
// watching either of them.
func (s *Server) claim(key docker.WorkspaceKey) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.scheduledRuns == nil {
		s.scheduledRuns = map[string]bool{}
	}
	id := scopeKey(key)
	if s.scheduledRuns[id] {
		return false
	}
	s.scheduledRuns[id] = true
	return true
}

func (s *Server) release(key docker.WorkspaceKey) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	delete(s.scheduledRuns, scopeKey(key))
}

func (s *Server) running(key docker.WorkspaceKey) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return s.scheduledRuns[scopeKey(key)]
}

func scopeKey(key docker.WorkspaceKey) string {
	return strings.Join([]string{key.TenantID, key.SubsAccID, key.Role, key.UserAccID}, "|")
}
