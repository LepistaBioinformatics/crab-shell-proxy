package httpapi

import (
	"sync"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
)

// turnRegistry answers one question for the MCP endpoint: when a memory-graph write
// arrives for a workspace, which conversation is it coming out of?
//
// The endpoint cannot be told. It is stateless, the bearer token carries only the
// workspace (a per-conversation token is impossible — it lives in config.json and is
// deterministic), and asking the agent to pass its session id would put a
// caller-supplied scope input on a route reachable from the container network. So
// the proxy correlates instead: it is already proxying the turn, so it knows which
// conversation a workspace is mid-turn on.
//
// A COUNTED SET, not map[Scope]string, and that is the whole design:
//
//   - Nothing serializes turns per workspace. Two tabs on two conversations of the
//     same agent are concurrent, and a single-value map would silently overwrite —
//     attributing a fact from chat A to chat B. That is WORSE than no attribution,
//     because the member clicks through and reads a conversation that never said it.
//   - So Current returns a session only when exactly ONE is in flight. Zero and
//     two-or-more both yield "no source", and the fact is stored without one.
//
// Counts rather than a plain set because the same conversation can legitimately have
// overlapping requests (a retry, a resolve alongside a turn); decrementing to zero is
// what removes it.
type turnRegistry struct {
	mu sync.Mutex
	// scope -> sessionID -> how many in-flight turns claim it.
	inFlight map[memgraph.Scope]map[string]int
}

func newTurnRegistry() *turnRegistry {
	return &turnRegistry{inFlight: make(map[memgraph.Scope]map[string]int)}
}

// Begin records that a turn for sessionID is running against scope, and returns the
// function that ends it.
//
// ALWAYS call the returned function, with defer. A turn that panics, or whose context
// dies without deregistering, leaves an entry behind — and a leaked entry does not
// merely lose attribution, it mis-attributes every later tool call for that workspace
// to a conversation that is no longer running.
//
// An empty sessionID is not recorded: there is nothing to attribute to, and counting
// it would make a legitimate single turn look ambiguous.
func (r *turnRegistry) Begin(scope memgraph.Scope, sessionID string) func() {
	if sessionID == "" {
		return func() {}
	}
	r.mu.Lock()
	byScope := r.inFlight[scope]
	if byScope == nil {
		byScope = make(map[string]int, 1)
		r.inFlight[scope] = byScope
	}
	byScope[sessionID]++
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			byScope := r.inFlight[scope]
			if byScope == nil {
				return
			}
			byScope[sessionID]--
			if byScope[sessionID] <= 0 {
				delete(byScope, sessionID)
			}
			// Drop the scope's map when it empties, so a proxy serving many members
			// over a long life does not accumulate one empty map per workspace.
			if len(byScope) == 0 {
				delete(r.inFlight, scope)
			}
		})
	}
}

// Current returns the conversation to attribute a write to, and whether it is
// unambiguous. False means "store no source" — which is a normal outcome, not an
// error:
//
//   - zero turns: a cron job, the heartbeat, or post-turn evolution wrote it
//   - two or more: concurrent conversations on the same workspace
func (r *turnRegistry) Current(scope memgraph.Scope) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	byScope := r.inFlight[scope]
	if len(byScope) != 1 {
		return "", false
	}
	for sessionID := range byScope {
		return sessionID, true
	}
	return "", false
}

// Active reports whether a named conversation has a turn in flight on scope.
//
// Deliberately NOT expressed in terms of Current, and the difference is the point.
// Current answers "which single conversation may I attribute this workspace's
// memory write to?", and refuses when two are running, because mislabelling is
// worse than not labelling. Active is asked BY a conversation ABOUT itself, so
// what else runs on the workspace is irrelevant — routing it through Current
// would tell a member with two tabs open that their turn had vanished.
func (r *turnRegistry) Active(scope memgraph.Scope, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inFlight[scope][sessionID] > 0
}
