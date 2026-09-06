package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
)

// blockingTurner holds a turn open until it is released, so a second request can
// arrive while the first is genuinely in flight -- which is the only state this
// file is about.
// Only the FIRST turn blocks: the second must be able to return, or the test that
// reads its stream would wait on the turn it is asking about.
type blockingTurner struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (b *blockingTurner) Cancel(_ context.Context, _ turn.Request) error { return nil }

func (b *blockingTurner) RunTurn(_ context.Context, _ turn.Request, _ turn.Sink) (string, error) {
	if b.calls.Add(1) > 1 {
		return "", nil
	}
	b.started <- struct{}{}
	<-b.release
	return "ok", nil
}

const streamingBody = `{"messages":[{"role":"user","content":"hi"}],"session_id":"s","stream":true,` +
	`"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX + `"}`

// A message sent while the conversation already has a turn in flight is NOT a
// second turn. picoclaw claims the session key and folds the message into the
// running turn (`enqueueSteeringMessage`, upstream pkg/agent/agent.go), telling
// nobody -- and because the pico channel fans frames out to every connection on
// the session (`broadcastToSession`), this stream then shows the FIRST turn's
// answer, minutes later.
//
// The member reads that as "the chat is taking forever to send my message". The
// proxy is the only layer that knows what actually happened, so it says so.
//
// See .specs/features/steering-messages/investigation.md §6-§7 (project repo).
func TestSecondPostOnALiveConversationIsAnnouncedAsSteering(t *testing.T) {
	bt := &blockingTurner{started: make(chan struct{}, 1), release: make(chan struct{})}
	s := testServer(scaffoldedOrch(), bt)
	defer close(bt.release)

	first := httptest.NewRecorder()
	go s.Handler().ServeHTTP(first, chatReq(t, streamingBody, goodHeaders(t)))
	<-bt.started // the first turn is registered and running

	second := httptest.NewRecorder()
	s.Handler().ServeHTTP(second, chatReq(t, streamingBody, goodHeaders(t)))

	var announced int
	for _, f := range frames(t, second.Body.String()) {
		raw, ok := f["x_crab_steering"]
		if !ok {
			continue
		}
		announced++
		// Same compatibility shape as x_crab_progress: an ordinary chunk with an
		// EMPTY delta, so a client that knows nothing about it skips the frame
		// instead of rendering an announcement as the assistant's words.
		choices, _ := f["choices"].([]any)
		if len(choices) != 1 {
			t.Fatalf("choices: %v", f["choices"])
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if len(delta) != 0 {
			t.Errorf("a steering frame must carry an EMPTY delta, got %v", delta)
		}
		if ev, _ := raw.(map[string]any); ev["folded"] != true {
			t.Errorf("x_crab_steering payload = %v, want folded:true", raw)
		}
	}
	if announced != 1 {
		t.Fatalf("x_crab_steering frames = %d, want exactly 1", announced)
	}
}

// The ordinary case must stay byte-identical: nothing running, nothing announced.
func TestFirstPostCarriesNoSteeringFrame(t *testing.T) {
	s := testServer(scaffoldedOrch(), &fakeTurner{content: "ok"})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, streamingBody, goodHeaders(t)))

	if strings.Contains(w.Body.String(), "x_crab_steering") {
		t.Errorf("a lone turn must not be announced as steering:\n%s", w.Body.String())
	}
}
