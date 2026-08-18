package httpapi

import (
	"sync"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
)

func regScope(user string) memgraph.Scope {
	return memgraph.Scope{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: user}
}

func TestNoTurnMeansNoSource(t *testing.T) {
	t.Parallel()
	r := newTurnRegistry()
	if id, ok := r.Current(regScope("u1")); ok {
		t.Errorf("Current on an idle workspace returned %q; a cron or heartbeat write has no conversation", id)
	}
}

func TestOneTurnAttributes(t *testing.T) {
	t.Parallel()
	r := newTurnRegistry()
	sc := regScope("u1")
	end := r.Begin(sc, "conv-a")
	id, ok := r.Current(sc)
	if !ok || id != "conv-a" {
		t.Fatalf("Current = %q, %v; want conv-a, true", id, ok)
	}
	end()
	if _, ok := r.Current(sc); ok {
		t.Error("the session is still attributed after the turn ended")
	}
}

// The reason this is a counted set. Two tabs on two conversations of the same agent
// are concurrent — nothing serializes turns per workspace. A single-value map would
// overwrite and attribute a fact from one chat to the other, which is WORSE than no
// attribution: the member clicks through and reads a conversation that never said it.
func TestTwoConcurrentTurnsAttributeNothing(t *testing.T) {
	t.Parallel()
	r := newTurnRegistry()
	sc := regScope("u1")
	endA := r.Begin(sc, "conv-a")
	endB := r.Begin(sc, "conv-b")

	if id, ok := r.Current(sc); ok {
		t.Errorf("Current = %q with two conversations in flight; ambiguous must mean no source", id)
	}

	// Once one ends, the other is unambiguous again.
	endB()
	if id, ok := r.Current(sc); !ok || id != "conv-a" {
		t.Errorf("Current = %q, %v after the second turn ended; want conv-a", id, ok)
	}
	endA()
	if _, ok := r.Current(sc); ok {
		t.Error("still attributing after both turns ended")
	}
}

// The same conversation can legitimately have overlapping requests (a retry, a
// resolve alongside a turn). That must stay attributable, and only the LAST end
// clears it — which is why the entry is counted rather than a plain set.
func TestOverlappingRequestsOnOneConversationStayAttributable(t *testing.T) {
	t.Parallel()
	r := newTurnRegistry()
	sc := regScope("u1")
	end1 := r.Begin(sc, "conv-a")
	end2 := r.Begin(sc, "conv-a")

	if id, ok := r.Current(sc); !ok || id != "conv-a" {
		t.Fatalf("Current = %q, %v; two requests on ONE conversation are not ambiguous", id, ok)
	}
	end1()
	if id, ok := r.Current(sc); !ok || id != "conv-a" {
		t.Errorf("Current = %q, %v after one of two ended; the conversation is still running", id, ok)
	}
	end2()
	if _, ok := r.Current(sc); ok {
		t.Error("still attributing after the last request ended")
	}
}

// A double end must not drive the count negative and resurrect the entry, or leave the
// scope in a state where a later single turn reads as ambiguous.
func TestEndIsIdempotent(t *testing.T) {
	t.Parallel()
	r := newTurnRegistry()
	sc := regScope("u1")
	end := r.Begin(sc, "conv-a")
	end()
	end()
	end()

	if _, ok := r.Current(sc); ok {
		t.Error("Current attributes after the turn ended")
	}
	// And the registry is still usable.
	end2 := r.Begin(sc, "conv-b")
	if id, ok := r.Current(sc); !ok || id != "conv-b" {
		t.Errorf("Current = %q, %v; a repeated end corrupted the registry", id, ok)
	}
	end2()
}

func TestScopesDoNotInterfere(t *testing.T) {
	t.Parallel()
	r := newTurnRegistry()
	a, b := regScope("u1"), regScope("u2")
	endA := r.Begin(a, "conv-a")
	defer endA()
	endB := r.Begin(b, "conv-b")
	defer endB()

	if id, ok := r.Current(a); !ok || id != "conv-a" {
		t.Errorf("scope a -> %q, %v", id, ok)
	}
	if id, ok := r.Current(b); !ok || id != "conv-b" {
		t.Errorf("scope b -> %q, %v", id, ok)
	}
	// A different agent for the same member is a different graph, so a different scope.
	beta := memgraph.Scope{TenantID: "t1", SubsAccID: "s1", Role: "beta", UserAccID: "u1"}
	if _, ok := r.Current(beta); ok {
		t.Error("alpha's turn leaked into beta's scope")
	}
}

// An empty session id records nothing: there is nothing to attribute to, and counting
// it would make a legitimate single turn look ambiguous.
func TestEmptySessionIsNotRecorded(t *testing.T) {
	t.Parallel()
	r := newTurnRegistry()
	sc := regScope("u1")
	endEmpty := r.Begin(sc, "")
	defer endEmpty()
	end := r.Begin(sc, "conv-a")
	defer end()

	if id, ok := r.Current(sc); !ok || id != "conv-a" {
		t.Errorf("Current = %q, %v; an unrecorded empty session must not create ambiguity", id, ok)
	}
}

// The registry must not accumulate one empty map per workspace over a long process
// life. Verified through the exported behaviour plus the internal map, since a leak
// here is invisible to any assertion about Current.
func TestEndedTurnsLeaveNothingBehind(t *testing.T) {
	t.Parallel()
	r := newTurnRegistry()
	for i := 0; i < 50; i++ {
		sc := regScope(string(rune('a' + i%26)))
		end := r.Begin(sc, "conv")
		end()
	}
	r.mu.Lock()
	n := len(r.inFlight)
	r.mu.Unlock()
	if n != 0 {
		t.Errorf("inFlight holds %d scopes after every turn ended; the map leaks", n)
	}
}

func TestConcurrentBeginEndIsRaceFree(t *testing.T) {
	t.Parallel()
	r := newTurnRegistry()
	sc := regScope("u1")
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			end := r.Begin(sc, "conv-a")
			_, _ = r.Current(sc)
			end()
		}(i)
	}
	wg.Wait()
	if _, ok := r.Current(sc); ok {
		t.Error("attribution survived every turn ending")
	}
}

// Active answers a different question from Current, and the difference is the
// reason it exists.
//
// Current is for MCP attribution: it refuses to answer when two conversations are
// in flight on one workspace, because guessing which one a memory write came from
// is worse than not labelling it. Active is asked BY a named conversation ABOUT
// itself -- "am I still running?" -- so concurrency on the workspace is irrelevant
// and answering false there would tell a member their turn had vanished.
func TestActiveReportsThisConversationOnly(t *testing.T) {
	t.Parallel()
	r := newTurnRegistry()
	sc := regScope("u1")

	if r.Active(sc, "conv-a") {
		t.Error("Active on an idle workspace must be false")
	}

	end := r.Begin(sc, "conv-a")
	if !r.Active(sc, "conv-a") {
		t.Error("the running conversation reports itself inactive")
	}
	if r.Active(sc, "conv-b") {
		t.Error("a conversation that is NOT running reported itself active")
	}

	end()
	if r.Active(sc, "conv-a") {
		t.Error("still active after the turn ended")
	}
}

// The case Current deliberately gets "wrong": two tabs, two conversations, one
// workspace. Current returns false for both; Active must return true for both, or
// a reload during either one would report the turn lost.
func TestActiveSurvivesConcurrentConversations(t *testing.T) {
	t.Parallel()
	r := newTurnRegistry()
	sc := regScope("u1")
	endA := r.Begin(sc, "conv-a")
	endB := r.Begin(sc, "conv-b")
	defer endA()
	defer endB()

	if _, ok := r.Current(sc); ok {
		t.Error("precondition: Current should decline to attribute with two in flight")
	}
	for _, id := range []string{"conv-a", "conv-b"} {
		if !r.Active(sc, id) {
			t.Errorf("Active(%q) = false; both concurrent conversations are running", id)
		}
	}
}

// Overlapping requests on ONE conversation (a retry alongside a turn) decrement to
// zero rather than dropping out on the first end.
func TestActiveHonoursTheCount(t *testing.T) {
	t.Parallel()
	r := newTurnRegistry()
	sc := regScope("u1")
	end1 := r.Begin(sc, "conv-a")
	end2 := r.Begin(sc, "conv-a")

	end1()
	if !r.Active(sc, "conv-a") {
		t.Error("one of two overlapping requests ended and the conversation went inactive")
	}
	end2()
	if r.Active(sc, "conv-a") {
		t.Error("both ended and it is still active")
	}
}

func TestActiveIsScopedToTheWorkspace(t *testing.T) {
	t.Parallel()
	r := newTurnRegistry()
	end := r.Begin(regScope("u1"), "conv-a")
	defer end()
	if r.Active(regScope("u2"), "conv-a") {
		t.Error("another member's workspace reported this conversation active")
	}
}

// ---------------------------------------------------------------------------
// background-turn-dock: List, and the first-seen timestamp it reports
// ---------------------------------------------------------------------------

// The reason List cannot be built on Current. Current refuses when two turns are in
// flight, because mislabelling a memory write is worse than not labelling it. The dock
// is asked exactly in that case — the member left two conversations running — so a List
// routed through Current would answer "nothing is running" precisely when something is.
func TestListReportsEveryConcurrentConversation(t *testing.T) {
	t.Parallel()
	r := newTurnRegistry()
	sc := regScope("u1")
	endA := r.Begin(sc, "conv-a")
	defer endA()
	endB := r.Begin(sc, "conv-b")
	defer endB()

	if _, ok := r.Current(sc); ok {
		t.Fatal("Current attributed a workspace with two turns; this test's premise is gone")
	}

	got := r.List(sc)
	if len(got) != 2 {
		t.Fatalf("List returned %d entries; want both concurrent conversations", len(got))
	}
	seen := map[string]bool{}
	for _, e := range got {
		seen[e.SessionID] = true
	}
	for _, id := range []string{"conv-a", "conv-b"} {
		if !seen[id] {
			t.Errorf("List omitted %q", id)
		}
	}
}

func TestListOnAnIdleScopeIsEmpty(t *testing.T) {
	t.Parallel()
	r := newTurnRegistry()
	if got := r.List(regScope("u1")); len(got) != 0 {
		t.Errorf("List on an idle workspace returned %d entries; want none", len(got))
	}
}

func TestListIsScopedToTheWorkspace(t *testing.T) {
	t.Parallel()
	r := newTurnRegistry()
	end := r.Begin(regScope("u1"), "conv-a")
	defer end()
	if got := r.List(regScope("u2")); len(got) != 0 {
		t.Errorf("another member's workspace listed %d entries", len(got))
	}
}

// Oldest first, so the dock's segments do not reshuffle between a probe and a store
// update, and the conversation most at risk of being forgotten is the one that survives
// the segment cap.
func TestListIsSortedOldestFirst(t *testing.T) {
	t.Parallel()
	r := newTurnRegistry()
	base := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	tick := base
	r.now = func() time.Time { return tick }
	sc := regScope("u1")

	tick = base.Add(2 * time.Minute)
	endLate := r.Begin(sc, "conv-late")
	defer endLate()
	tick = base
	endEarly := r.Begin(sc, "conv-early")
	defer endEarly()

	got := r.List(sc)
	if len(got) != 2 {
		t.Fatalf("List returned %d entries; want 2", len(got))
	}
	if got[0].SessionID != "conv-early" || got[1].SessionID != "conv-late" {
		t.Errorf("List order = %q, %q; want conv-early first", got[0].SessionID, got[1].SessionID)
	}
}

// FIRST-seen, not last-seen. Begin is re-entrant by design (a retry, or a resolve
// alongside a turn, increments the same key), so resetting on the second Begin would
// report a nine-minute turn as fresh — the exact lie the timestamp exists to remove.
func TestListSinceIsFirstSeenAndDoesNotReset(t *testing.T) {
	t.Parallel()
	r := newTurnRegistry()
	start := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	tick := start
	r.now = func() time.Time { return tick }
	sc := regScope("u1")

	end1 := r.Begin(sc, "conv-a")
	defer end1()
	tick = start.Add(9 * time.Minute)
	end2 := r.Begin(sc, "conv-a")
	defer end2()

	got := r.List(sc)
	if len(got) != 1 {
		t.Fatalf("List returned %d entries; two overlapping requests are ONE conversation", len(got))
	}
	if !got[0].Since.Equal(start) {
		t.Errorf("Since = %v; want %v — the second Begin moved the clock forward", got[0].Since, start)
	}
}

// The timestamp goes when the entry goes: a later turn on the same conversation is a
// new turn, and must not inherit the previous one's age.
func TestListSinceIsDroppedWithTheEntry(t *testing.T) {
	t.Parallel()
	r := newTurnRegistry()
	start := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	tick := start
	r.now = func() time.Time { return tick }
	sc := regScope("u1")

	r.Begin(sc, "conv-a")()
	if got := r.List(sc); len(got) != 0 {
		t.Fatalf("List returned %d entries after the turn ended", len(got))
	}

	tick = start.Add(time.Hour)
	end := r.Begin(sc, "conv-a")
	defer end()
	got := r.List(sc)
	if len(got) != 1 {
		t.Fatalf("List returned %d entries; want the new turn", len(got))
	}
	if !got[0].Since.Equal(tick) {
		t.Errorf("Since = %v; want %v — the new turn inherited the old entry's age", got[0].Since, tick)
	}
}
