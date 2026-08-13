package httpapi

import (
	"sync"
	"testing"

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
