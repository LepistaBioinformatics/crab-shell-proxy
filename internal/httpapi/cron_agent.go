package httpapi

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/cron"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/mcpserver"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
)

// The agent's own half of scheduled tasks: the three MCP tools, and the bounds
// that exist because nothing bounded this before.
//
// WHAT IS THE SAME AS THE MEMBER'S PATH. The store, the file format, the
// scheduler, and cron.Validate. One validator, so the two authors cannot drift
// into disagreeing about what a schedule is.
//
// WHAT IS DIFFERENT, and why each one:
//
//	An approval is REQUIRED, and required HERE. The harness asks before it
//	invokes, but what makes it ask is an environment variable on the container.
//	A variable can be wrong; this check cannot be reconfigured from inside.
//
//	The minimum interval is higher. The member's 60-second floor exists because
//	every fire may cold-start a container that scale-to-zero has stopped — "a
//	request to keep an agent permanently warm by the back door", as the constant
//	itself puts it. That argument is stronger when the agent is the one asking.
//
//	There is a ceiling on how many tasks a workspace may hold, and a limit on
//	how fast they may be created. Neither exists for the member's route either;
//	they are added here because a turn can call a tool as fast as the model can
//	emit one, and each task is a standing instruction to wake a container.
//
//	There is no edit. A task a member approved must not be able to become a
//	different task without a second approval, so the surface has create, list
//	and remove and nothing else.

const (
	// maxSchedulesPerWorkspace bounds the standing instructions one workspace can
	// hold. Applies to BOTH authors: a ceiling only the agent feels would leave
	// the same disk, and the same scheduler, unbounded.
	maxSchedulesPerWorkspace = 20

	// agentMinEveryMs is the agent's floor for a recurring task, well above the
	// member's 60 seconds. Fifteen minutes is short enough for anything a person
	// would reasonably ask for on a timer and long enough that a schedule cannot
	// be used to hold a scale-to-zero container open.
	agentMinEveryMs = 15 * 60 * 1000

	// agentCreateWindow and agentCreateMax bound the rate. A member has to answer
	// for each one, which is already the strongest limiter there is; this is what
	// remains true if that ever stops being so.
	agentCreateWindow = time.Hour
	agentCreateMax    = 5
)

// errNoApproval is what the agent reads when it called the tool without a live
// grant. Phrased for the model, which will relay it: DEC-2 makes a refusal a
// Result it can explain rather than a failure.
var errNoApproval = errors.New(
	"the member has not approved this. Ask them in the conversation, and call this " +
		"again only after they agree")

// agentSchedules implements mcpserver.ScheduleStore against the proxy's own
// scheduled-task store.
type agentSchedules struct {
	srv *Server

	mu      sync.Mutex
	creates map[memgraph.Scope][]time.Time
	now     func() time.Time
}

func newAgentSchedules(s *Server) *agentSchedules {
	return &agentSchedules{srv: s, creates: map[memgraph.Scope][]time.Time{}, now: time.Now}
}

func (a *agentSchedules) path(sc memgraph.Scope) string {
	return config.SchedulesFile(a.srv.Cfg.ContainerDataRoot,
		sc.TenantID, sc.SubsAccID, sc.Role, sc.UserAccID)
}

// allowRate records this attempt and reports whether it fits the window.
func (a *agentSchedules) allowRate(sc memgraph.Scope) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cut := a.now().Add(-agentCreateWindow)
	kept := a.creates[sc][:0]
	for _, t := range a.creates[sc] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= agentCreateMax {
		a.creates[sc] = kept
		return false
	}
	a.creates[sc] = append(kept, a.now())
	return true
}

func (a *agentSchedules) CreateSchedule(sc memgraph.Scope, in mcpserver.ScheduleInput) (any, error) {
	// THE APPROVAL IS CHECKED BEFORE ANYTHING ELSE, including validation. A
	// refusal here must not depend on whether the request was well formed —
	// otherwise the shape of the error tells an unapproved caller which of its
	// arguments the proxy liked.
	// THE GRANT IS THE WORKSPACE'S, not the project's. The approval endpoint's
	// token carries no project -- one container serves them all -- so a grant is
	// recorded with an empty one and has to be looked up the same way. The task
	// itself still lands in the project this call names, below.
	ws := memgraph.Scope{TenantID: sc.TenantID, SubsAccID: sc.SubsAccID, Role: sc.Role, UserAccID: sc.UserAccID}
	by, grantedUntil, ok := a.srv.Approvals.consumeGrant(ws, "schedule_create")
	if !ok {
		return nil, errNoApproval
	}
	// A refusal from HERE down is the proxy's own, about a request the member
	// already answered for. Handing the approval back means the agent can fix a
	// too-short interval or free a slot without asking the member again for the
	// same thing. The expiry is unchanged, so failing on purpose buys no time.
	refuse := func(err error) (any, error) {
		a.srv.Approvals.restoreGrant(ws, "schedule_create", by, grantedUntil)
		return nil, err
	}

	if in.Kind == cron.KindEvery && in.EveryMs < agentMinEveryMs {
		return refuse(fmt.Errorf(
			"the shortest interval you may schedule is %d minutes; ask the member to set "+
				"a shorter one themselves if they need it", agentMinEveryMs/60000))
	}
	if !a.allowRate(sc) {
		return refuse(fmt.Errorf(
			"you have created %d scheduled tasks in the last hour, which is the limit. "+
				"Remove one you no longer need, or wait", agentCreateMax))
	}

	now := a.now()
	job := cron.Job{
		ID:      cron.NewID(),
		Name:    in.Name,
		Enabled: true,
		Schedule: cron.Schedule{
			Kind: in.Kind, Expr: in.Expr, EveryMs: in.EveryMs, AtMs: in.AtMs, Tz: in.TZ,
		},
		Payload:        cron.Payload{Kind: cron.PayloadAgentTurn, Message: in.Message},
		CreatedAtMs:    now.UnixMilli(),
		UpdatedAtMs:    now.UnixMilli(),
		DeleteAfterRun: in.DeleteAfterRun,
		Project:        sc.Project,
		// PROVENANCE IS THE PROXY'S. The agent asked for a schedule and a
		// message; who authored the result is established here, from the grant.
		CreatedBy:  cron.CreatedByAgent,
		ApprovedBy: by,
	}
	if job.Name == "" {
		job.Name = firstLine(in.Message)
	}
	if err := cron.Validate(job); err != nil {
		return refuse(err)
	}
	if job.Schedule.Kind == cron.KindAt && job.Schedule.AtMs <= now.UnixMilli() {
		return refuse(errors.New("that moment has already passed"))
	}
	if next, ok := cron.NextRun(job.Schedule, now); ok {
		job.State.NextRunAtMs = next.UnixMilli()
	}

	err := a.srv.Schedules.Mutate(a.path(sc), func(jobs []cron.Job) ([]cron.Job, error) {
		if len(jobs) >= maxSchedulesPerWorkspace {
			return nil, fmt.Errorf(
				"this workspace already holds %d scheduled tasks, which is the limit. "+
					"Remove one before adding another", maxSchedulesPerWorkspace)
		}
		return append(jobs, job), nil
	})
	if err != nil {
		return refuse(err)
	}
	return agentJobView(job), nil
}

func (a *agentSchedules) ListSchedules(sc memgraph.Scope) (any, error) {
	jobs, err := a.srv.Schedules.List(a.path(sc))
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, j := range jobs {
		// Scoped to the project the token and header resolved to, the same rule
		// the member's listing follows: a project's tasks are its own.
		if j.Project != sc.Project {
			continue
		}
		out = append(out, agentJobView(j))
	}
	return map[string]any{"tasks": out}, nil
}

func (a *agentSchedules) DeleteSchedule(sc memgraph.Scope, id string) (any, error) {
	if id == "" {
		return nil, errors.New("which task? Call schedule_list for the ids")
	}
	found := false
	err := a.srv.Schedules.Mutate(a.path(sc), func(jobs []cron.Job) ([]cron.Job, error) {
		kept := jobs[:0]
		for _, j := range jobs {
			// The project check is not cosmetic: without it a task could be
			// removed from a scope it was not filed under, which is the rule the
			// member's route enforces too.
			if j.ID == id && j.Project == sc.Project {
				found = true
				continue
			}
			kept = append(kept, j)
		}
		return kept, nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("no scheduled task here has the id %q", id)
	}
	// The run transcripts stay on disk. A task that ran and was then removed is
	// an ordinary end state, not a gap.
	return map[string]any{"deleted": id}, nil
}

// agentJobView is what the model sees. Deliberately not the whole record:
// internal bookkeeping would be noise it cannot act on.
func agentJobView(j cron.Job) map[string]any {
	view := map[string]any{
		"id":       j.ID,
		"name":     j.Name,
		"enabled":  j.Enabled,
		"schedule": j.Schedule,
		"message":  j.Payload.Message,
	}
	if j.State.NextRunAtMs != 0 {
		view["nextRunAtMs"] = j.State.NextRunAtMs
	}
	if j.State.LastStatus != "" {
		view["lastStatus"] = j.State.LastStatus
	}
	if j.State.LastError != "" {
		view["lastError"] = j.State.LastError
	}
	if j.DeleteAfterRun {
		view["deleteAfterRun"] = true
	}
	return view
}

// agentScheduleStore returns the schedule surface for the MCP server, or nil
// when this deployment has nothing to write into.
//
// Nil rather than a store that errors: the tools are then absent from the
// model's tool list entirely, which is the honest shape. A tool that exists and
// always fails is one the model will keep reaching for.
func agentScheduleStore(s *Server) mcpserver.ScheduleStore {
	if s.Schedules == nil || s.Cfg == nil || s.Cfg.ContainerDataRoot == "" {
		return nil
	}
	return newAgentSchedules(s)
}
