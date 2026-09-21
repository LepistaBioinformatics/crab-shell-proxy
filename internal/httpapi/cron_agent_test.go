package httpapi

import (
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/cron"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/mcpserver"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
)

func agentScheduleFixture(t *testing.T) (*agentSchedules, memgraph.Scope) {
	t.Helper()
	s, _ := approvalServer(t)
	s.Schedules = &cron.Owner{}
	return newAgentSchedules(s), callerScope()
}

// A well-formed weekly task, so each case below varies exactly one thing.
func weekly() mcpserver.ScheduleInput {
	return mcpserver.ScheduleInput{Kind: cron.KindCron, Expr: "0 7 * * 1", Message: "weekly report"}
}

// allow grants as the real endpoint does: with the WORKSPACE scope, never a
// project. The container's approval token is minted once per container and
// cannot name one, so a helper that granted per project would let these tests
// pass against a keying the proxy can never produce.
func allow(a *agentSchedules, sc memgraph.Scope) {
	ws := memgraph.Scope{TenantID: sc.TenantID, SubsAccID: sc.SubsAccID, Role: sc.Role, UserAccID: sc.UserAccID}
	a.srv.Approvals.recordGrant(ws, "schedule_create", accAlice)
}

// THE PROPERTY THIS WHOLE FEATURE RESTS ON. What makes the harness ask is an
// environment variable on the container, and a variable can be wrong. The
// enforcer has to be on the side that cannot be reconfigured from inside.
func TestAScheduleCannotBeCreatedWithoutAnApproval(t *testing.T) {
	a, sc := agentScheduleFixture(t)
	if _, err := a.CreateSchedule(sc, weekly()); err == nil {
		t.Fatal("an unapproved create succeeded")
	}
}

// An approval buys ONE task. Two need two answers, or one "yes" to a weekly
// report becomes a standing permission to schedule anything.
func TestAnApprovalIsSpentByOneCreate(t *testing.T) {
	a, sc := agentScheduleFixture(t)
	allow(a, sc)
	if _, err := a.CreateSchedule(sc, weekly()); err != nil {
		t.Fatalf("the approved create failed: %v", err)
	}
	if _, err := a.CreateSchedule(sc, weekly()); err == nil {
		t.Fatal("a second task was created on one approval")
	}
}

// The grant is scoped. An approval given in one workspace must not be spendable
// in another, which is the same rule the answer route enforces.
func TestAnApprovalDoesNotCrossWorkspaces(t *testing.T) {
	a, sc := agentScheduleFixture(t)
	other := memgraph.Scope{TenantID: sc.TenantID, SubsAccID: sc.SubsAccID, Role: sc.Role, UserAccID: "someone-else"}
	allow(a, other)
	if _, err := a.CreateSchedule(sc, weekly()); err == nil {
		t.Fatal("an approval from another workspace was spent here")
	}
}

// Provenance is established by the proxy from the grant, never asked for.
func TestAnAgentCreatedTaskRecordsWhoApprovedIt(t *testing.T) {
	a, sc := agentScheduleFixture(t)
	allow(a, sc)
	if _, err := a.CreateSchedule(sc, weekly()); err != nil {
		t.Fatal(err)
	}
	jobs, err := a.srv.Schedules.List(a.path(sc))
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %d, err = %v", len(jobs), err)
	}
	if jobs[0].CreatedBy != cron.CreatedByAgent {
		t.Fatalf("createdBy = %q, want %q", jobs[0].CreatedBy, cron.CreatedByAgent)
	}
	if jobs[0].ApprovedBy != accAlice {
		t.Fatalf("approvedBy = %q, want the answering member", jobs[0].ApprovedBy)
	}
}

// The member's 60-second floor exists because every fire may cold-start a
// stopped container. That argument is stronger when the agent is the one asking.
func TestTheAgentsIntervalFloorIsHigherThanTheMembers(t *testing.T) {
	a, sc := agentScheduleFixture(t)
	allow(a, sc)
	in := mcpserver.ScheduleInput{Kind: cron.KindEvery, EveryMs: 60000, Message: "poll"}
	_, err := a.CreateSchedule(sc, in)
	if err == nil {
		t.Fatal("a one-minute interval was accepted from the agent")
	}
	// The refusal names the limit, because the model relays it to the member.
	if !strings.Contains(err.Error(), "15 minutes") {
		t.Fatalf("the refusal does not say what the limit is: %v", err)
	}
}

func TestAWorkspaceCannotHoldMoreTasksThanTheCeiling(t *testing.T) {
	a, sc := agentScheduleFixture(t)
	// Past the ceiling, filed directly: this case is about the ceiling, not
	// about how many approvals it takes to reach it.
	if err := a.srv.Schedules.Mutate(a.path(sc), func(jobs []cron.Job) ([]cron.Job, error) {
		for i := 0; i < maxSchedulesPerWorkspace; i++ {
			jobs = append(jobs, cron.Job{ID: cron.NewID(), Enabled: true})
		}
		return jobs, nil
	}); err != nil {
		t.Fatal(err)
	}
	allow(a, sc)
	_, err := a.CreateSchedule(sc, weekly())
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("the ceiling did not hold: %v", err)
	}
}

func TestCreationIsRateLimitedWithinTheWindow(t *testing.T) {
	a, sc := agentScheduleFixture(t)
	for i := 0; i < agentCreateMax; i++ {
		allow(a, sc)
		if _, err := a.CreateSchedule(sc, weekly()); err != nil {
			t.Fatalf("create %d failed: %v", i, err)
		}
	}
	allow(a, sc)
	if _, err := a.CreateSchedule(sc, weekly()); err == nil {
		t.Fatalf("a %dth task was created inside the window", agentCreateMax+1)
	}

	// The window slides. An hour later the same workspace may create again --
	// this is a rate limit, not a lifetime quota.
	a.now = func() time.Time { return time.Now().Add(2 * agentCreateWindow) }
	allow(a, sc)
	if _, err := a.CreateSchedule(sc, weekly()); err != nil {
		t.Fatalf("the window did not slide: %v", err)
	}
}

// A one-shot in the past would never fire, so it is refused rather than filed.
func TestAOneShotInThePastIsRefused(t *testing.T) {
	a, sc := agentScheduleFixture(t)
	allow(a, sc)
	in := mcpserver.ScheduleInput{Kind: cron.KindAt, AtMs: 1, Message: "too late"}
	if _, err := a.CreateSchedule(sc, in); err == nil {
		t.Fatal("a moment that has already passed was accepted")
	}
}

// The same validator the member's route uses, so the two authors cannot drift
// into disagreeing about what a schedule is.
func TestAnUnschedulableTaskIsRefusedByTheSharedValidator(t *testing.T) {
	a, sc := agentScheduleFixture(t)
	allow(a, sc)
	in := mcpserver.ScheduleInput{Kind: cron.KindCron, Expr: "not a cron expression", Message: "x"}
	if _, err := a.CreateSchedule(sc, in); err == nil {
		t.Fatal("an invalid cron expression was filed")
	}
}

func TestListAndDeleteAreScopedToTheProject(t *testing.T) {
	a, sc := agentScheduleFixture(t)
	inProject := sc
	inProject.Project = "p1"

	allow(a, sc)
	if _, err := a.CreateSchedule(sc, weekly()); err != nil {
		t.Fatal(err)
	}
	allow(a, inProject)
	if _, err := a.CreateSchedule(inProject, weekly()); err != nil {
		t.Fatal(err)
	}

	jobs, _ := a.srv.Schedules.List(a.path(sc))
	if len(jobs) != 2 {
		t.Fatalf("the store holds %d, want both", len(jobs))
	}

	// Each scope sees only its own.
	out, err := a.ListSchedules(sc)
	if err != nil {
		t.Fatal(err)
	}
	listed := out.(map[string]any)["tasks"].([]map[string]any)
	if len(listed) != 1 {
		t.Fatalf("the main workspace listed %d tasks, want only its own", len(listed))
	}

	// And a delete cannot reach across. The id is real; the scope is not its own.
	var projectID string
	for _, j := range jobs {
		if j.Project == "p1" {
			projectID = j.ID
		}
	}
	if _, err := a.DeleteSchedule(sc, projectID); err == nil {
		t.Fatal("a task was deleted from a scope it was not filed under")
	}
	if _, err := a.DeleteSchedule(inProject, projectID); err != nil {
		t.Fatalf("the project could not delete its own task: %v", err)
	}
}

// A refusal the AGENT can fix — a too-short interval, a bad expression — must not
// spend the member's answer. Otherwise the agent has to ask again for a mistake
// it can see, and the member answers twice for one task.
func TestALimitRefusalHandsTheApprovalBack(t *testing.T) {
	a, sc := agentScheduleFixture(t)
	allow(a, sc)

	tooOften := mcpserver.ScheduleInput{Kind: cron.KindEvery, EveryMs: 60000, Message: "poll"}
	if _, err := a.CreateSchedule(sc, tooOften); err == nil {
		t.Fatal("a one-minute interval was accepted")
	}
	// The same approval still buys the corrected request.
	if _, err := a.CreateSchedule(sc, weekly()); err != nil {
		t.Fatalf("the approval was spent on the refused call: %v", err)
	}
}

// Failing on purpose must not extend the window. The grant comes back with the
// expiry it had, so a caller cannot hold an approval open by retrying.
func TestHandingAnApprovalBackDoesNotExtendIt(t *testing.T) {
	a, sc := agentScheduleFixture(t)
	allow(a, sc)

	bad := mcpserver.ScheduleInput{Kind: cron.KindCron, Expr: "nonsense", Message: "x"}
	if _, err := a.CreateSchedule(sc, bad); err == nil {
		t.Fatal("an invalid expression was accepted")
	}
	// Past the original two-minute window, the restored grant is dead like any
	// other.
	a.srv.Approvals.now = func() time.Time { return time.Now().Add(2 * grantTTL) }
	if _, err := a.CreateSchedule(sc, weekly()); err == nil {
		t.Fatal("a restored approval outlived its window")
	}
}

// A refusal that is NOT the agent's to fix still spends it: the member said yes
// to this task, and the workspace being full is a fact about the workspace. The
// distinction is deliberate -- see restoreGrant.
func TestTheCeilingRefusalAlsoHandsItBack(t *testing.T) {
	a, sc := agentScheduleFixture(t)
	if err := a.srv.Schedules.Mutate(a.path(sc), func(jobs []cron.Job) ([]cron.Job, error) {
		for i := 0; i < maxSchedulesPerWorkspace; i++ {
			jobs = append(jobs, cron.Job{ID: cron.NewID(), Enabled: true})
		}
		return jobs, nil
	}); err != nil {
		t.Fatal(err)
	}
	allow(a, sc)
	if _, err := a.CreateSchedule(sc, weekly()); err == nil {
		t.Fatal("the ceiling did not hold")
	}
	// Freeing a slot lets the same approval through -- the agent can act on this
	// one too, by removing a task.
	if err := a.srv.Schedules.Mutate(a.path(sc), func(jobs []cron.Job) ([]cron.Job, error) {
		return jobs[:len(jobs)-1], nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateSchedule(sc, weekly()); err != nil {
		t.Fatalf("the approval was spent on the refused call: %v", err)
	}
}
