package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/cron"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/history"
)

// schedulerRef is the scope the other cron tests already use, as the scheduler
// would resolve it.
func schedulerRef(s *Server, root string) scheduleRef {
	return scheduleRef{
		Agent: s.Cfg.Agents["alpha"],
		Key: docker.WorkspaceKey{
			TenantID: tenantT, SubsAccID: subsX, Role: "alpha", UserAccID: accAlice,
		},
		Email: "alice@example.test",
		Path:  config.SchedulesFile(root, tenantT, subsX, "alpha", accAlice),
	}
}

func seedJobs(t *testing.T, s *Server, path string, jobs ...cron.Job) {
	t.Helper()
	if s.Schedules == nil {
		s.Schedules = cron.NewOwner()
	}
	if err := s.Schedules.Mutate(path, func([]cron.Job) ([]cron.Job, error) {
		return jobs, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func loadJobs(t *testing.T, s *Server, path string) map[string]cron.Job {
	t.Helper()
	jobs, err := s.Schedules.List(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]cron.Job{}
	for _, j := range jobs {
		out[j.ID] = j
	}
	return out
}

// The claim happens BEFORE the turn, so a proxy that dies mid-run loses that run
// instead of re-firing it on every boot forever.
func TestClaimDueAdvancesTheScheduleBeforeAnythingRuns(t *testing.T) {
	s, root := ganglionCronServer(t)
	ref := schedulerRef(s, root)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	seedJobs(t, s, ref.Path,
		cron.Job{ID: "due", Enabled: true,
			Schedule: cron.Schedule{Kind: cron.KindEvery, EveryMs: 3600_000},
			Payload:  cron.Payload{Kind: cron.PayloadAgentTurn, Message: "resuma"},
			State:    cron.State{NextRunAtMs: now.Add(-time.Minute).UnixMilli()}},
		cron.Job{ID: "later", Enabled: true,
			Schedule: cron.Schedule{Kind: cron.KindEvery, EveryMs: 3600_000},
			Payload:  cron.Payload{Kind: cron.PayloadAgentTurn, Message: "depois"},
			State:    cron.State{NextRunAtMs: now.Add(10 * time.Minute).UnixMilli()}},
	)

	fire, next, err := s.claimDue(ref, now)
	if err != nil {
		t.Fatalf("claimDue: %v", err)
	}
	if len(fire) != 1 || fire[0].ID != "due" {
		t.Fatalf("want only the due job claimed, got %+v", fire)
	}
	// The sleep is driven by the nearest future instant across the store: the
	// rescheduled job (+1h) and the untouched one (+10m).
	if want := now.Add(10 * time.Minute); !next.Equal(want) {
		t.Errorf("next = %s, want %s", next, want)
	}

	stored := loadJobs(t, s, ref.Path)
	if got := stored["due"].State.LastRunAtMs; got != now.UnixMilli() {
		t.Errorf("lastRunAtMs = %d, want the claim instant %d", got, now.UnixMilli())
	}
	if want := now.Add(time.Hour).UnixMilli(); stored["due"].State.NextRunAtMs != want {
		t.Errorf("nextRunAtMs = %d, want %d", stored["due"].State.NextRunAtMs, want)
	}
	if stored["later"].State.LastRunAtMs != 0 {
		t.Error("a job that was not due was claimed")
	}
}

// A schedule the proxy slept through fires ONCE and reschedules from now. An
// hourly job over a downed weekend must not deliver a hundred unattended turns.
func TestClaimDueDoesNotReplayMissedOccurrences(t *testing.T) {
	s, root := ganglionCronServer(t)
	ref := schedulerRef(s, root)
	now := time.Now()

	seedJobs(t, s, ref.Path, cron.Job{ID: "stale", Enabled: true,
		Schedule: cron.Schedule{Kind: cron.KindEvery, EveryMs: 3600_000},
		Payload:  cron.Payload{Kind: cron.PayloadAgentTurn, Message: "x"},
		State:    cron.State{NextRunAtMs: now.Add(-72 * time.Hour).UnixMilli()}})

	fire, _, err := s.claimDue(ref, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(fire) != 1 {
		t.Fatalf("want exactly one catch-up run, got %d", len(fire))
	}
	if again, _, _ := s.claimDue(ref, now); len(again) != 0 {
		t.Fatalf("the same overdue job fired twice: %+v", again)
	}
}

// A job stored with no next run has never been scheduled. It must be SCHEDULED,
// not treated as overdue -- otherwise the first pass over a store fires every
// task in it at once.
func TestClaimDueSchedulesAnUnscheduledJobInsteadOfFiringIt(t *testing.T) {
	s, root := ganglionCronServer(t)
	ref := schedulerRef(s, root)
	now := time.Now()

	seedJobs(t, s, ref.Path, cron.Job{ID: "fresh", Enabled: true,
		Schedule: cron.Schedule{Kind: cron.KindCron, Expr: "0 9 * * *"},
		Payload:  cron.Payload{Kind: cron.PayloadAgentTurn, Message: "x"}})

	fire, _, err := s.claimDue(ref, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(fire) != 0 {
		t.Fatalf("an unscheduled job was fired: %+v", fire)
	}
	if loadJobs(t, s, ref.Path)["fresh"].State.NextRunAtMs == 0 {
		t.Error("the job was left unscheduled, so it will never run")
	}
}

// A one-shot runs once. deleteAfterRun removes the record and leaves the
// transcript (which the read side renders as an orphan run); without it the job
// stays, disabled, so the member can still see that it ran.
func TestClaimDueRetiresAOneShot(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name           string
		deleteAfterRun bool
		wantPresent    bool
	}{
		{"kept and disabled", false, true},
		{"deleted", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, root := ganglionCronServer(t)
			ref := schedulerRef(s, root)
			seedJobs(t, s, ref.Path, cron.Job{ID: "once", Enabled: true,
				DeleteAfterRun: tc.deleteAfterRun,
				Schedule:       cron.Schedule{Kind: cron.KindAt, AtMs: now.Add(-time.Minute).UnixMilli()},
				Payload:        cron.Payload{Kind: cron.PayloadAgentTurn, Message: "x"},
				State:          cron.State{NextRunAtMs: now.Add(-time.Minute).UnixMilli()}})

			fire, _, err := s.claimDue(ref, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(fire) != 1 {
				t.Fatalf("the one-shot did not fire: %+v", fire)
			}
			stored := loadJobs(t, s, ref.Path)
			j, present := stored["once"]
			if present != tc.wantPresent {
				t.Fatalf("present = %v, want %v", present, tc.wantPresent)
			}
			if present && j.Enabled {
				t.Error("a one-shot that ran is still enabled, so it would fire again")
			}
		})
	}
}

// FR-B9: two scheduled turns never run at once in one container. They share a
// workspace and nobody is watching either.
func TestClaimDueLeavesAJobDueWhileTheWorkspaceIsBusy(t *testing.T) {
	s, root := ganglionCronServer(t)
	ref := schedulerRef(s, root)
	now := time.Now()

	seedJobs(t, s, ref.Path, cron.Job{ID: "queued", Enabled: true,
		Schedule: cron.Schedule{Kind: cron.KindEvery, EveryMs: 3600_000},
		Payload:  cron.Payload{Kind: cron.PayloadAgentTurn, Message: "x"},
		State:    cron.State{NextRunAtMs: now.Add(-time.Minute).UnixMilli()}})

	if !s.claim(ref.Key) {
		t.Fatal("claim refused on an idle workspace")
	}
	fire, next, err := s.claimDue(ref, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(fire) != 0 {
		t.Fatalf("a second turn was started in a busy container: %+v", fire)
	}
	if next.IsZero() || next.After(now.Add(schedulerMinSleep+time.Second)) {
		t.Errorf("next = %s: a queued job must be retried promptly, not forgotten", next)
	}
	// Still due, so it runs as soon as the container frees up.
	if loadJobs(t, s, ref.Path)["queued"].State.LastRunAtMs != 0 {
		t.Error("a queued job was marked as having run")
	}

	s.release(ref.Key)
	if fire, _, _ := s.claimDue(ref, now); len(fire) != 1 {
		t.Fatalf("the queued job did not run once the workspace freed up: %+v", fire)
	}
}

// The end of the round trip: firing a job writes a run the member's Tasks panel
// finds, in the project's own sessions directory.
func TestRunScheduledJobWritesARunThePanelFinds(t *testing.T) {
	s, root := ganglionCronServer(t)
	s.Pico = &fakeTurner{content: "pronto"}
	ref := schedulerRef(s, root)
	s.Schedules = cron.NewOwner()
	seedJobs(t, s, ref.Path, cron.Job{ID: "abcd", Enabled: true, Project: "seedtrial",
		Schedule: cron.Schedule{Kind: cron.KindEvery, EveryMs: 3600_000},
		Payload:  cron.Payload{Kind: cron.PayloadAgentTurn, Message: "resuma o ensaio"}})

	job := loadJobs(t, s, ref.Path)["abcd"]
	s.runScheduledJob(context.Background(), ref, job)

	// The project's own sessions dir, not the agent's. This is the part picoclaw
	// structurally cannot do: its one cron store fires into one workspace.
	sessionsDir := config.SessionsDir(root, tenantT, subsX, "alpha", accAlice,
		config.GanglionProjectWorkspace("seedtrial"))
	runs, err := history.CronRuns(sessionsDir)
	if err != nil {
		t.Fatalf("CronRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	if runs[0].JobID != "abcd" {
		t.Errorf("jobId = %q, want the job that fired", runs[0].JobID)
	}
	// Nothing wrote a transcript (the harness is a fake here), which the reader
	// reports rather than hiding.
	if !runs[0].TranscriptMissing {
		t.Error("TranscriptMissing = false with no transcript on disk")
	}
	if got := loadJobs(t, s, ref.Path)["abcd"].State.LastStatus; got != cron.StatusOK {
		t.Errorf("lastStatus = %q, want %q", got, cron.StatusOK)
	}
}

// FR-B6: a failure is recorded where the member can read it. picoclaw never
// wrote a value anyone observed; this is the one that does.
func TestRunScheduledJobRecordsAFailure(t *testing.T) {
	s, root := ganglionCronServer(t)
	s.Pico = &fakeTurner{err: http.ErrHandlerTimeout}
	ref := schedulerRef(s, root)
	s.Schedules = cron.NewOwner()
	seedJobs(t, s, ref.Path, cron.Job{ID: "abcd", Enabled: true,
		Schedule: cron.Schedule{Kind: cron.KindEvery, EveryMs: 3600_000},
		Payload:  cron.Payload{Kind: cron.PayloadAgentTurn, Message: "x"}})

	s.runScheduledJob(context.Background(), ref, loadJobs(t, s, ref.Path)["abcd"])

	stored := loadJobs(t, s, ref.Path)["abcd"]
	if stored.State.LastStatus != cron.StatusError || stored.State.LastError == "" {
		t.Fatalf("the failure was not recorded: %+v", stored.State)
	}
	// The run is still listed. "It failed" and "it never fired" are different
	// things, and only the meta can tell them apart.
	sessionsDir := config.SessionsDir(root, tenantT, subsX, "alpha", accAlice, config.MainWorkspace)
	if runs, _ := history.CronRuns(sessionsDir); len(runs) != 1 {
		t.Fatalf("a failed run was not written down: %+v", runs)
	}
}

// picoclaw's schedules are its own. Two schedulers over one jobs.json would race
// over timers this process cannot see.
func TestSchedulerRefsSkipPicoclawAgents(t *testing.T) {
	s, _ := cronServer(t) // agent alpha, picoclaw
	orch := s.Mgr.(*fakeOrch)
	orch.tenants = []string{tenantT}
	orch.tenantSubs = []string{subsX}
	orch.users = []docker.UserRef{{AccID: accAlice, Role: "alpha", Email: "alice@example.test"}}

	if refs := s.scheduleRefs(); len(refs) != 0 {
		t.Fatalf("a picoclaw workspace was taken over by the proxy scheduler: %+v", refs)
	}

	s2, _ := ganglionCronServer(t)
	o2 := s2.Mgr.(*fakeOrch)
	o2.tenants = []string{tenantT}
	o2.tenantSubs = []string{subsX}
	o2.users = []docker.UserRef{{AccID: accAlice, Role: "alpha", Email: "alice@example.test"}}
	refs := s2.scheduleRefs()
	if len(refs) != 1 || refs[0].Key.UserAccID != accAlice {
		t.Fatalf("the ganglion workspace was not picked up: %+v", refs)
	}
}
