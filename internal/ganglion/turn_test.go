package ganglion

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
)

type collected struct {
	mu       sync.Mutex
	content  []string
	progress []turn.Progress
	errs     []string
}

func (c *collected) sink() turn.Sink {
	return turn.Sink{
		Content:  func(s string) { c.mu.Lock(); c.content = append(c.content, s); c.mu.Unlock() },
		Progress: func(p turn.Progress) { c.mu.Lock(); c.progress = append(c.progress, p); c.mu.Unlock() },
		Error:    func(s string) { c.mu.Lock(); c.errs = append(c.errs, s); c.mu.Unlock() },
	}
}

func sse(t *testing.T, lines ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			w.Write([]byte(l + "\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
}

func req(endpoint string) turn.Request {
	return turn.Request{
		Endpoint:   endpoint,
		AuthToken:  "tok",
		SessionID:  "conv-1",
		SessionKey: "sk-1",
		Model:      "deepseek-chat",
		Content:    "oi",
	}
}

func TestRunTurn_ForwardsDeltasAndReturnsTheWholeAnswer(t *testing.T) {
	srv := sse(t,
		`data: {"choices":[{"delta":{"content":"Oi"}}]}`,
		`data: {"choices":[{"delta":{"content":", tudo bem?"}}]}`,
		`data: [DONE]`,
	)
	defer srv.Close()

	c := &collected{}
	got, err := New(srv.Client()).RunTurn(context.Background(), req(srv.URL), c.sink())
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if got != "Oi, tudo bem?" {
		t.Errorf("answer = %q", got)
	}
	if len(c.content) != 2 {
		t.Errorf("expected 2 deltas forwarded, got %d -- the runner buffered", len(c.content))
	}
}

// The heartbeat is an SSE comment. Forwarding it would stamp the webapp's
// last-event clock and pin "quiet for" at zero, destroying exactly the
// staleness detection it exists to protect.
func TestRunTurn_HeartbeatCommentsAreDroppedNotForwarded(t *testing.T) {
	srv := sse(t,
		`: ping`,
		`data: {"choices":[{"delta":{"content":"ok"}}]}`,
		`: ping`,
		`data: [DONE]`,
	)
	defer srv.Close()

	c := &collected{}
	if _, err := New(srv.Client()).RunTurn(context.Background(), req(srv.URL), c.sink()); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(c.content) != 1 || len(c.progress) != 0 {
		t.Errorf("a heartbeat leaked into the sink: content=%v progress=%v", c.content, c.progress)
	}
}

// FR-17: both headers must go out. Until this runner, SessionID and SessionKey
// were populated on turn.Request and read by nobody.
func TestRunTurn_SendsBothSessionHeaders(t *testing.T) {
	var gotID, gotKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = r.Header.Get("X-Ganglion-Session-Id")
		gotKey = r.Header.Get("X-Ganglion-Session-Key")
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	if _, err := New(srv.Client()).RunTurn(context.Background(), req(srv.URL), turn.Sink{}); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if gotID != "conv-1" || gotKey != "sk-1" {
		t.Errorf("session headers = %q/%q, want conv-1/sk-1", gotID, gotKey)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

func TestRunTurn_ProgressAndErrorReachTheirOwnChannels(t *testing.T) {
	srv := sse(t,
		`data: {"choices":[{"delta":{"x_crab_progress":{"kind":"tool","tool":"shell"}}}]}`,
		`data: {"choices":[{"delta":{"x_crab_error":"provider exploded"}}]}`,
		`data: [DONE]`,
	)
	defer srv.Close()

	c := &collected{}
	got, err := New(srv.Client()).RunTurn(context.Background(), req(srv.URL), c.sink())
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(c.progress) != 1 || c.progress[0].Tool != "shell" {
		t.Errorf("progress = %+v", c.progress)
	}
	if len(c.errs) != 1 {
		t.Errorf("errors = %v", c.errs)
	}
	// An error is a signal; it must not become part of the answer.
	if strings.Contains(got, "exploded") {
		t.Errorf("the error leaked into the answer: %q", got)
	}
}

func TestRunTurn_NonOKStatusNamesTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("harness not ready"))
	}))
	defer srv.Close()

	_, err := New(srv.Client()).RunTurn(context.Background(), req(srv.URL), turn.Sink{})
	if err == nil || !strings.Contains(err.Error(), "harness not ready") {
		t.Errorf("err = %v, want it to name the harness's own message", err)
	}
}

// A line that is not a chunk must be skipped, never rendered. Hermes' own
// progress event was exactly this, and treating it as content injected raw
// JSON into the member's answer.
func TestRunTurn_UnparseableLinesAreSkipped(t *testing.T) {
	srv := sse(t, `data: not json`, `data: {"choices":[{"delta":{"content":"ok"}}]}`, `data: [DONE]`)
	defer srv.Close()

	c := &collected{}
	got, err := New(srv.Client()).RunTurn(context.Background(), req(srv.URL), c.sink())
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if got != "ok" {
		t.Errorf("answer = %q", got)
	}
}

func TestCancel_StopsARunningTurnAndIsSafeWhenThereIsNone(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	c := New(srv.Client())
	if err := c.Cancel(context.Background(), turn.Request{SessionID: "nothing-running"}); err != nil {
		t.Errorf("cancelling an idle session must not error: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.RunTurn(context.Background(), req(srv.URL), turn.Sink{})
	}()
	time.Sleep(50 * time.Millisecond)
	if err := c.Cancel(context.Background(), turn.Request{SessionID: "conv-1"}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Cancel did not stop the running turn")
	}
}

// The proxy owns the preimage of every scope the harness runs a turn in, and
// the project is now one of them (ganglion-projects D-2). The harness never
// derives it: a project reaches picoclaw through the "p.<id>." session-id
// prefix and a dispatch rule, and making the ganglion parse that string would
// make it depend on a routing convention owned somewhere else.
func TestAProjectTurnCarriesTheProjectHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Ganglion-Project")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	tr := req(srv.URL)
	tr.Project = "seedtrial"
	if _, err := New(srv.Client()).RunTurn(context.Background(), tr, turn.Sink{}); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if got != "seedtrial" {
		t.Errorf("X-Ganglion-Project = %q, want %q — the turn would run in the main workspace", got, "seedtrial")
	}
}

// A turn with no project must send the request it sends today, header for
// header. This is the regression bar for every deployment that has no projects
// at all (NFR-1): an empty header is a value the harness would have to decide
// what to do with, and there is nothing for it to mean.
func TestATurnWithNoProjectSendsNoProjectHeader(t *testing.T) {
	present := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["X-Ganglion-Project"]
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	if _, err := New(srv.Client()).RunTurn(context.Background(), req(srv.URL), turn.Sink{}); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if present {
		t.Error("a turn with no project sent X-Ganglion-Project; the unscoped request must be unchanged")
	}
}
