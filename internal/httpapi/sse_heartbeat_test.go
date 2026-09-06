package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
)

// The heartbeat exists because picoclaw answers in ONE terminal frame: a measured
// tool-free turn emitted nothing for 51 seconds, and an idle SSE can be reclaimed by
// any hop between here and the browser. These tests pin the three properties that
// make it safe rather than merely present -- it is a comment, it stops when the
// client does, and it never lands between the turn's two terminal frames.

// slowTurner holds the turn open long enough for the ticker to fire, then emits.
type slowTurner struct {
	hold    time.Duration
	content string
	// started closes once RunTurn is entered, so a test can act mid-turn.
	started chan struct{}
	once    sync.Once
}

func (s *slowTurner) Cancel(_ context.Context, _ turn.Request) error { return nil }

func (s *slowTurner) RunTurn(_ context.Context, _ turn.Request, sink turn.Sink) (string, error) {
	s.once.Do(func() {
		if s.started != nil {
			close(s.started)
		}
	})
	time.Sleep(s.hold)
	if s.content != "" {
		sink.EmitContent(s.content)
	}
	return s.content, nil
}

// recorder is DELIBERATELY unsynchronized.
//
// The first version of this file locked its Write, on the reasoning that a failure
// would otherwise be ambiguous because httptest.ResponseRecorder is not thread-safe
// either. That reasoning was wrong and the mistake is worth recording: a lock here
// does not ISOLATE streamTurn's lock, it SUBSTITUTES for it. Every frame is written
// by exactly one Write call, so an atomic Write makes frames un-tearable no matter
// what streamTurn does -- and removing writeMu from the heartbeat then produced no
// race and no failure. The test was inert.
//
// A real http.ResponseWriter offers no such guarantee, so this one offers none
// either. With writeMu in place the concurrent writes are ordered by happens-before
// and -race is silent; remove it and -race reports the write/write race.
//
// blockOn widens a window that is otherwise microseconds long: Write sleeps when the
// payload contains it, so "nothing may be written between the terminal frames" can be
// asserted instead of merely hoped for.
type recorder struct {
	body    strings.Builder
	hdr     http.Header
	code    int
	blockOn string
	block   time.Duration
}

func newRecorder() *recorder {
	return &recorder{hdr: make(http.Header), code: http.StatusOK}
}
func (r *recorder) Header() http.Header { return r.hdr }
func (r *recorder) WriteHeader(c int)   { r.code = c }
func (r *recorder) Write(b []byte) (int, error) {
	if r.blockOn != "" && strings.Contains(string(b), r.blockOn) {
		time.Sleep(r.block)
	}
	return r.body.Write(b)
}
func (r *recorder) Flush()         {}
func (r *recorder) String() string { return r.body.String() }

func streamWith(t *testing.T, s *Server, tr Turner, req *http.Request) string {
	t.Helper()
	return streamInto(t, s, newRecorder(), req)
}

func streamInto(t *testing.T, s *Server, rec *recorder, req *http.Request) string {
	t.Helper()
	s.streamTurn(rec, req,
		config.Agent{Key: "alpha", Harness: config.HarnessPicoclaw},
		docker.WorkspaceKey{TenantID: "t", SubsAccID: "s", Role: "alpha", UserAccID: "u"},
		"owner@example.com", "sess", "hello", "picoclaw", "chatcmpl-test", "", false)
	return rec.String()
}

func heartbeatServer(t *testing.T, orch Orchestrator, tr Turner, every time.Duration) *Server {
	t.Helper()
	s := testServer(orch, tr)
	s.heartbeatEvery = every
	return s
}

func countPings(body string) int {
	return strings.Count(body, ": ping\n\n")
}

// design 1 -- a turn that outlasts the interval pings; one that does not, does not.
func TestHeartbeatFiresOnALongTurn(t *testing.T) {
	t.Parallel()
	s := heartbeatServer(t, &fakeOrch{}, &slowTurner{hold: 120 * time.Millisecond, content: "hi"}, 20*time.Millisecond)
	body := streamWith(t, s, nil, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	if got := countPings(body); got < 2 {
		t.Fatalf("pings = %d, want >= 2 on a turn spanning several intervals\n%q", got, body)
	}
}

func TestHeartbeatSilentOnAShortTurn(t *testing.T) {
	t.Parallel()
	s := heartbeatServer(t, &fakeOrch{}, &slowTurner{content: "hi"}, time.Hour)
	body := streamWith(t, s, nil, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	if got := countPings(body); got != 0 {
		t.Fatalf("pings = %d, want 0 when the turn finishes inside one interval", got)
	}
}

// design 1 (shape) -- the frame is an SSE COMMENT, never a data frame.
//
// This is the requirement most likely to be "simplified" into a bug. The webapp
// derives its quiet-for readout and its dock chips from the last EVENT's timestamp
// (background-turn-dock DEC-12); a heartbeat carrying `data:` would stamp it every
// interval and pin that readout at zero. The client skips non-"data:" lines before
// any bookkeeping, so a comment cannot -- but only while it stays a comment.
func TestHeartbeatIsACommentNotADataFrame(t *testing.T) {
	t.Parallel()
	s := heartbeatServer(t, &fakeOrch{}, &slowTurner{hold: 80 * time.Millisecond, content: "hi"}, 20*time.Millisecond)
	body := streamWith(t, s, nil, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	// Asserted by COUNTING data frames, not by looking for the word "ping".
	//
	// A shape-specific search fails with "nothing to assert on" if the heartbeat is
	// reshaped, which misidentifies the very mutation this test exists to catch. The
	// turn below produces exactly three data frames -- the role chunk, one content
	// delta, the finish_reason chunk -- plus [DONE]. Any heartbeat that carries
	// "data:" shows up as a fourth, whatever it is called.
	var comments, dataFrames int
	for _, block := range strings.Split(body, "\n\n") {
		line := strings.TrimSpace(block)
		switch {
		case line == "":
		case strings.HasPrefix(line, ":"):
			comments++
		case strings.HasPrefix(line, "data:"):
			if strings.TrimSpace(strings.TrimPrefix(line, "data:")) != "[DONE]" {
				dataFrames++
			}
		}
	}
	if comments < 2 {
		t.Fatalf("comment frames = %d, want >= 2 (the heartbeat must be a comment)\n%q", comments, body)
	}
	if dataFrames != 3 {
		t.Fatalf("data frames = %d, want exactly 3 (role, content, stop) -- "+
			"a heartbeat carrying data: would be an extra one\n%q", dataFrames, body)
	}
}

// design 2 -- no ping is written once the client is gone. The turn keeps draining;
// only the writing stops, which is the same guard every sink already applies.
func TestHeartbeatStopsWritingWhenTheClientIsGone(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	tr := &slowTurner{hold: 150 * time.Millisecond, content: "hi", started: started}
	s := heartbeatServer(t, &fakeOrch{}, tr, 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)

	go func() {
		<-started
		cancel() // the client walked away mid-turn
	}()

	body := streamWith(t, s, tr, req)

	// The role chunk is written before the client leaves, so the body is not empty;
	// what must not appear is a ping written after the cancel.
	if got := countPings(body); got != 0 {
		t.Fatalf("pings = %d after the client went away, want 0\n%q", got, body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("terminal frames written to a client that had gone\n%q", body)
	}
}

// design 3 -- nothing may be written between finish_reason "stop" and [DONE].
//
// The client treats a missing terminal marker as a cut turn, so the two frames are
// one signal. done() holds the write lock across both; stopHeartbeat() closes the
// window earlier still.
func TestNoHeartbeatBetweenTheTerminalFrames(t *testing.T) {
	t.Parallel()
	s := heartbeatServer(t, &fakeOrch{}, &slowTurner{hold: 40 * time.Millisecond, content: "hi"}, time.Millisecond)

	// HONEST LIMIT, recorded rather than papered over: this test cannot fail on the
	// mutation it is aimed at, and neither could two earlier attempts.
	//
	// Removing BOTH guards (stopHeartbeat before done, and done's single critical
	// section) still passed five runs out of five, because the window between the two
	// terminal writes is microseconds. Widening it with blockOn does not help either:
	// the sleep happens inside Write, which runs while writeMu is HELD, so the
	// heartbeat is blocked for exactly the duration being widened. Any mutation that
	// leaves each write taking the lock individually leaves a window too small to hit
	// from outside.
	//
	// What this test does prove: the ordinary path emits its terminal frames adjacent,
	// which is the observable requirement. What actually GUARANTEES it is structural
	// and visible in sse.go instead -- done() takes writeMu once and writes both
	// frames under it, and stopHeartbeat() runs before done() is called. Treat those
	// two lines as the invariant; this is a smoke test over them.
	rec := newRecorder()
	rec.blockOn = `"finish_reason":"stop"`
	rec.block = 40 * time.Millisecond
	body := streamInto(t, s, rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	stop := strings.LastIndex(body, `"finish_reason":"stop"`)
	if stop < 0 {
		t.Fatalf("no terminal chunk in body\n%q", body)
	}
	doneAt := strings.LastIndex(body, "data: [DONE]")
	if doneAt < 0 || doneAt < stop {
		t.Fatalf("no [DONE] after the terminal chunk\n%q", body)
	}
	if between := body[stop:doneAt]; strings.Contains(between, ": ping") {
		t.Fatalf("heartbeat landed between the terminal frames: %q", between)
	}
}

// design 4 -- concurrent writers do not interleave.
//
// Under -race this is the test that would have failed before the mutex existed. It
// asserts the OUTPUT too: a torn frame is the visible symptom, and a client
// mid-parse is what it breaks.
func TestConcurrentContentAndHeartbeatProduceWellFormedFrames(t *testing.T) {
	t.Parallel()
	s := heartbeatServer(t, &fakeOrch{}, &chattyTurner{bursts: 60, gap: 2 * time.Millisecond}, time.Millisecond)
	body := streamWith(t, s, nil, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	for _, block := range strings.Split(body, "\n\n") {
		line := strings.TrimSpace(block)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, ": "):
			if line != ": ping" {
				t.Fatalf("torn comment frame: %q", line)
			}
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				continue
			}
			if !strings.HasPrefix(payload, "{") || !strings.HasSuffix(payload, "}") {
				t.Fatalf("torn data frame: %q", payload)
			}
		default:
			t.Fatalf("frame is neither a comment nor a data line: %q", line)
		}
	}
}

// chattyTurner emits many small content deltas with gaps, so content writes and
// heartbeat writes genuinely contend.
type chattyTurner struct {
	bursts int
	gap    time.Duration
}

func (c *chattyTurner) Cancel(_ context.Context, _ turn.Request) error { return nil }

func (c *chattyTurner) RunTurn(_ context.Context, _ turn.Request, sink turn.Sink) (string, error) {
	for i := 0; i < c.bursts; i++ {
		sink.EmitContent("word ")
		time.Sleep(c.gap)
	}
	return strings.Repeat("word ", c.bursts), nil
}
