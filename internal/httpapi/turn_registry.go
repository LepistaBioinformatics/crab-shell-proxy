package httpapi

import (
	"sort"
	"sync"
	"time"

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
	// scope -> sessionID -> the in-flight requests claiming it.
	inFlight map[memgraph.Scope]map[string]turnEntry

	// Injected so List's timestamps are testable. A field rather than a package-level
	// var because the registry's tests run t.Parallel().
	now func() time.Time
}

// turnEntry is one conversation's in-flight state on one workspace.
//
// The count is what Active and Current have always read. `since` exists for the dock
// (background-turn-dock): a client that reloaded mid-turn has no local start time, and
// the store field it would otherwise reach for — recoveringSince — is stamped at resume
// time, so it would report a nine-minute turn as fresh.
type turnEntry struct {
	count int
	// FIRST seen, never updated. Begin is re-entrant by design (a retry, or a resolve
	// alongside a turn, increments the same key), and moving this on the second Begin
	// would be the same lie recoveringSince tells.
	since time.Time
}

// turnRecord is one listed conversation. Exported fields because the handler marshals it.
type turnRecord struct {
	SessionID string
	Since     time.Time
}

func newTurnRegistry() *turnRegistry {
	return &turnRegistry{
		inFlight: make(map[memgraph.Scope]map[string]turnEntry),
		now:      time.Now,
	}
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
		byScope = make(map[string]turnEntry, 1)
		r.inFlight[scope] = byScope
	}
	entry := byScope[sessionID]
	entry.count++
	// Only the FIRST request on this conversation sets the clock; see turnEntry.
	if entry.since.IsZero() {
		entry.since = r.now()
	}
	byScope[sessionID] = entry
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
			entry := byScope[sessionID]
			entry.count--
			if entry.count <= 0 {
				// Deleting drops `since` with the count, so a later turn on this
				// conversation does not inherit this one's age.
				delete(byScope, sessionID)
			} else {
				byScope[sessionID] = entry
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
	return r.inFlight[scope][sessionID].count > 0
}

// List reports every conversation with a turn in flight on scope, oldest first.
//
// Like Active, and for the same reason, it is NOT expressed in terms of Current. Current
// refuses to answer when two turns are in flight, because mislabelling a memory write is
// worse than not labelling it — and two concurrent conversations is exactly the case the
// dock exists for. Routing this through Current would report an idle workspace precisely
// when the member has the most work running.
//
// The sort is here rather than in the client so the segment order cannot change between a
// probe and a store update.
func (r *turnRegistry) List(scope memgraph.Scope) []turnRecord {
	r.mu.Lock()
	byScope := r.inFlight[scope]
	out := make([]turnRecord, 0, len(byScope))
	for sessionID, entry := range byScope {
		if entry.count > 0 {
			out = append(out, turnRecord{SessionID: sessionID, Since: entry.since})
		}
	}
	r.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Since.Equal(out[j].Since) {
			// Map iteration is randomised, so equal timestamps need a tiebreak or the
			// order flickers between probes.
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].Since.Before(out[j].Since)
	})
	return out
}
