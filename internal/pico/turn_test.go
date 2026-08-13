package pico

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
	"github.com/coder/websocket"
)

// drive replays a frame sequence through the processor, tracking the resulting
// grace-timer state (armed/cancelled) exactly as the transport driver would,
// and returns the streamed deltas plus whether grace was armed at the end.
func drive(frames []Frame) (deltas []string, armed bool, errMsg string, final string) {
	p := newProcessor(turn.Sink{Content: func(d string) { deltas = append(deltas, d) }}, "", "")
	for _, f := range frames {
		sig := p.handle(f)
		if sig.errMsg != "" {
			return deltas, armed, sig.errMsg, ""
		}
		if sig.cancel || sig.arm {
			armed = false
		}
		if sig.arm {
			armed = true
		}
	}
	return deltas, armed, "", p.finalContent()
}

func msg(id, content, kind string, placeholder bool) Frame {
	return Frame{Type: "message.update", Payload: Payload{
		MessageID: id, Content: content, Kind: kind, Placeholder: placeholder,
	}}
}

func TestPlainAnswerFinalizes(t *testing.T) {
	deltas, armed, errMsg, final := drive([]Frame{
		{Type: "typing.start"},
		msg("m1", "Hello", "", false),
		msg("m1", "Hello world", "", false),
		{Type: "typing.stop"},
	})
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if !armed {
		t.Error("grace should be armed after content + typing.stop")
	}
	if final != "Hello world" {
		t.Errorf("final = %q, want %q", final, "Hello world")
	}
	if got := strings.Join(deltas, "|"); got != "Hello|"+" world" {
		t.Errorf("deltas = %q, want %q", got, "Hello| world")
	}
}

func TestToolCallsDoNotFinalize(t *testing.T) {
	// A tool_calls indicator arrives and typing stops, but no real answer yet:
	// grace must NOT arm.
	_, armed, _, final := drive([]Frame{
		{Type: "typing.start"},
		msg("t1", "calling tool", "tool_calls", false),
		{Type: "typing.stop"},
	})
	if armed {
		t.Error("grace must not arm on tool_calls-only content")
	}
	if final != "" {
		t.Errorf("final = %q, want empty", final)
	}
}

func TestThoughtAndPlaceholderSkipped(t *testing.T) {
	deltas, armed, _, final := drive([]Frame{
		msg("th", "thinking...", "thought", false),
		msg("ph", "placeholder", "", true),
		{Type: "typing.stop"},
	})
	if armed {
		t.Error("thought/placeholder must not count as real content")
	}
	if len(deltas) != 0 {
		t.Errorf("no deltas expected, got %v", deltas)
	}
	if final != "" {
		t.Errorf("final = %q, want empty", final)
	}
}

func TestTypingStartCancelsPendingFinalize(t *testing.T) {
	// Real answer, typing stops (grace armed), then the agent resumes with
	// another tool call (typing.start) — grace must be cancelled, then re-armed
	// once the follow-up answer lands and typing stops again.
	deltas, armed, _, final := drive([]Frame{
		{Type: "typing.start"},
		msg("m1", "partial", "", false),
		{Type: "typing.stop"},  // arm
		{Type: "typing.start"}, // cancel
		msg("t1", "tool", "tool_calls", false),
		{Type: "typing.stop"}, // still armed? content is tool_calls, but hasPlainContent already true
	})
	if !armed {
		t.Error("grace should be re-armed after typing.stop with prior real content")
	}
	if final != "partial" {
		t.Errorf("final = %q, want %q", final, "partial")
	}
	if got := strings.Join(deltas, "|"); got != "partial" {
		t.Errorf("deltas = %q, want %q", got, "partial")
	}
}

func TestLaterMessageAfterToolCallWins(t *testing.T) {
	// The agent answers, calls a tool, then produces a NEW message that should
	// become the final content and stream its delta.
	deltas, armed, _, final := drive([]Frame{
		{Type: "typing.start"},
		msg("m1", "first", "", false),
		{Type: "typing.stop"},
		{Type: "typing.start"},
		msg("t1", "toolwork", "tool_calls", false),
		{Type: "typing.stop"},
		{Type: "typing.start"},
		msg("m2", "final answer", "", false),
		{Type: "typing.stop"},
	})
	if !armed {
		t.Error("grace should be armed after the final real message")
	}
	if final != "final answer" {
		t.Errorf("final = %q, want %q", final, "final answer")
	}
	if got := strings.Join(deltas, "|"); got != "first|final answer" {
		t.Errorf("deltas = %q, want %q", got, "first|final answer")
	}
}

func TestErrorFrame(t *testing.T) {
	_, _, errMsg, _ := drive([]Frame{
		{Type: "error", Payload: Payload{Message: "model exploded"}},
	})
	if errMsg != "model exploded" {
		t.Errorf("errMsg = %q, want %q", errMsg, "model exploded")
	}
}

func TestContentWhileTypingDoesNotArm(t *testing.T) {
	// Real content arrives but typing has not stopped yet: no finalize.
	_, armed, _, _ := drive([]Frame{
		{Type: "typing.start"},
		msg("m1", "streaming", "", false),
	})
	if armed {
		t.Error("must not arm while still typing")
	}
}

// The attachment fixture below is copied VERBATIM from upstream picoclaw's own
// test (pkg/channels/pico/client_test.go, ForwardsTextWithDownloadAttachment), so
// this fails if the frame shape was misread rather than passing against a shape
// invented here. Upstream builds the same map in pkg/channels/pico/pico.go's
// SendMedia.
const upstreamAttachmentFrame = `{
  "type": "message.create",
  "payload": {
    "content": "see attached",
    "attachments": [
      {
        "type": "image",
        "url": "/pico/media/abc",
        "filename": "image.png",
        "content_type": "image/png"
      }
    ]
  }
}`

func TestAttachmentFrameDecodesUpstreamShape(t *testing.T) {
	var f Frame
	if err := json.Unmarshal([]byte(upstreamAttachmentFrame), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(f.Payload.Attachments) != 1 {
		t.Fatalf("attachments = %#v, want exactly one", f.Payload.Attachments)
	}
	got := f.Payload.Attachments[0]
	want := Attachment{Type: "image", URL: "/pico/media/abc", Filename: "image.png", ContentType: "image/png"}
	if got != want {
		t.Errorf("attachment = %+v, want %+v", got, want)
	}
}

// The delivered file has to come out ABSOLUTE and with the bearer the media route
// requires (upstream pico_test.go sets "Authorization: Bearer test-token" on
// GET /pico/media/<id>), because whoever fetches it only has what the sink gave.
func TestAttachmentIsEmittedAbsoluteWithToken(t *testing.T) {
	var f Frame
	if err := json.Unmarshal([]byte(upstreamAttachmentFrame), &f); err != nil {
		t.Fatal(err)
	}
	var got []turn.Attachment
	var deltas []string
	p := newProcessor(turn.Sink{
		Content:    func(d string) { deltas = append(deltas, d) },
		Attachment: func(a turn.Attachment) { got = append(got, a) },
	}, "http://picoclaw-alpha-abc:18790", "pico-token")

	sig := p.handle(f)

	if len(got) != 1 {
		t.Fatalf("attachments emitted = %d, want 1", len(got))
	}
	if got[0].URL != "http://picoclaw-alpha-abc:18790/pico/media/abc" {
		t.Errorf("url = %q, want the harness origin + the relative path", got[0].URL)
	}
	if got[0].AuthToken != "pico-token" {
		t.Errorf("token = %q, want the turn's own bearer", got[0].AuthToken)
	}
	if got[0].Filename != "image.png" || got[0].ContentType != "image/png" {
		t.Errorf("metadata lost: %+v", got[0])
	}
	// A caption is the agent talking, so it still reaches the reader.
	if len(deltas) != 1 || deltas[0] != "see attached" {
		t.Errorf("deltas = %v, want the caption forwarded as content", deltas)
	}
	// And it must not end the turn on its own.
	if sig.arm || sig.cancel {
		t.Errorf("signal = %+v, want no timer change from a delivery", sig)
	}
}

// The ordering that would look exactly like the reported bug. picoclaw sends the
// file with an EMPTY caption, and the plain-content branch would have made that
// frame the "last plain message" — so finalContent() returned "" and the answer
// vanished on a non-streaming request, or the turn finalized before the reply.
func TestAttachmentFrameNeverErasesOrEndsTheAnswer(t *testing.T) {
	answer := Frame{Type: "message.create", Payload: Payload{
		MessageID: "m1", Content: "Requested output delivered via tool attachment.",
	}}
	delivery := Frame{Type: "message.create", Payload: Payload{
		MessageID: "m2", Content: "", // upstream sends the caption empty
		Attachments: []Attachment{{Type: "file", URL: "/pico/media/abc", Filename: "report.pdf"}},
	}}

	for _, tc := range []struct {
		name   string
		frames []Frame
	}{
		{"delivery after the answer", []Frame{answer, delivery, {Type: "typing.stop"}}},
		{"delivery before the answer", []Frame{delivery, answer, {Type: "typing.stop"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var files []turn.Attachment
			p := newProcessor(turn.Sink{Attachment: func(a turn.Attachment) { files = append(files, a) }},
				"http://h:18790", "tok")
			var armed bool
			for _, f := range tc.frames {
				sig := p.handle(f)
				if sig.arm {
					armed = true
				}
				if sig.cancel {
					armed = false
				}
			}
			if len(files) != 1 || files[0].Filename != "report.pdf" {
				t.Errorf("delivered files = %+v, want exactly report.pdf", files)
			}
			if got := p.finalContent(); got != "Requested output delivered via tool attachment." {
				t.Errorf("finalContent = %q, want the answer text intact", got)
			}
			// typing.stop is what legitimately arms; the delivery must not have
			// prevented it either.
			if !armed {
				t.Error("grace never armed: the turn would hang after a delivery")
			}
		})
	}
}

// A FAILED turn arrives as an ordinary assistant message — picoclaw formats it in
// pkg/agent/error_format.go and publishes it through the same call an answer takes,
// with no frame type, kind or severity to separate the two. The prefix is the only
// discriminator on the wire, and without acting on it the member cannot tell "it
// answered nothing" from "it broke": the text is not persisted either, so whatever
// showed it loses it to the next reconcile against the durable transcript.
func TestProcessingErrorIsReportedAsWellAsStreamed(t *testing.T) {
	const errText = `Error processing message: selected vision model "glm-4.7-flash" ` +
		`does not support image input; update agents.defaults.image_model to a multimodal model`

	cases := []struct {
		name     string
		frames   []Frame
		wantErrs []string
		wantText string
	}{
		{
			// The reported shape: one frame, whole error.
			name: "error in a single frame",
			frames: []Frame{
				{Type: "typing.start"},
				msg("m1", errText, "", false),
				{Type: "typing.stop"},
			},
			wantErrs: []string{errText},
			wantText: errText,
		},
		{
			// Content is CUMULATIVE and only the suffix is streamed, so a prefix test
			// against the emitted delta would miss this entirely.
			name: "error arriving as an update after a partial",
			frames: []Frame{
				msg("m1", "Error proc", "", false),
				msg("m1", errText, "", false),
			},
			wantErrs: []string{errText},
			wantText: errText,
		},
		{
			// Once per message, not once per update: a banner that re-fires on every
			// frame of a long error is noise.
			name: "further updates of an errored message report once",
			frames: []Frame{
				msg("m1", errText, "", false),
				msg("m1", errText+"\n\nOriginal error:\nboom", "", false),
			},
			wantErrs: []string{errText},
			wantText: errText + "\n\nOriginal error:\nboom",
		},
		{
			name: "an ordinary answer reports nothing",
			frames: []Frame{
				{Type: "typing.start"},
				msg("m1", "Here is your answer.", "", false),
				{Type: "typing.stop"},
			},
			wantErrs: nil,
			wantText: "Here is your answer.",
		},
		{
			// The word appearing mid-sentence is the agent talking about errors, not
			// picoclaw reporting one.
			name: "the prefix must be at the start",
			frames: []Frame{
				msg("m1", "I hit an Error processing message: check the log", "", false),
			},
			wantErrs: nil,
			wantText: "I hit an Error processing message: check the log",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var errs []string
			var streamed string
			p := newProcessor(turn.Sink{
				Content: func(d string) { streamed += d },
				Error:   func(m string) { errs = append(errs, m) },
			}, "", "")
			for _, f := range c.frames {
				p.handle(f)
			}

			if len(errs) != len(c.wantErrs) {
				t.Fatalf("reported %d error(s) %q, want %d %q", len(errs), errs, len(c.wantErrs), c.wantErrs)
			}
			for i := range errs {
				if errs[i] != c.wantErrs[i] {
					t.Errorf("error[%d] = %q, want %q", i, errs[i], c.wantErrs[i])
				}
			}
			// The text is ALSO delivered as content. Suppressing it would leave a
			// generic OpenAI client with an empty answer and no error at all, and would
			// empty the non-streaming path's body, which finalContent derives from the
			// same plain-content bookkeeping.
			if streamed != c.wantText {
				t.Errorf("streamed = %q, want %q", streamed, c.wantText)
			}
			if final := p.finalContent(); final != c.wantText {
				t.Errorf("finalContent = %q, want %q (the non-streaming body)", final, c.wantText)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Transport-level tests.
//
// Everything above drives the processor directly. These drive RunTurn against a
// real Pico Protocol server, because the bug they cover lives in the transport's
// timer handling and is invisible to the state machine.
// ----------------------------------------------------------------------------

const testToken = "test-token"

// picoServer starts a WebSocket server that speaks enough of the Pico Protocol
// to drive one turn: it waits for the client's message.send, then hands the
// connection to script, which writes the frames the test wants to exercise.
func picoServer(t *testing.T, script func(ctx context.Context, conn *websocket.Conn)) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{"token." + testToken},
		})
		if err != nil {
			return
		}
		defer conn.CloseNow()

		ctx := r.Context()
		// The turn does not start until the client has asked for it.
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		script(ctx, conn)

		// Hold the connection open until the client hangs up, the way picoclaw
		// does. Closing as soon as the script runs out of frames would race the
		// client's grace window and surface as a spurious EOF.
		//
		// Blocking on a read rather than on ctx.Done(): this connection is
		// hijacked, so the request context is not reliably cancelled when the peer
		// goes away, and a handler parked on it would outlive the test.
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	return "ws://" + strings.TrimPrefix(srv.URL, "http://")
}

func writeFrame(ctx context.Context, conn *websocket.Conn, f Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}

// TestLongTurnSurvivesWhileFramesKeepArriving is the regression test for the
// "long tasks freeze, then the answer shows up on reload" bug.
//
// The turn runs LONGER than the client's timeout while never going quiet for
// more than a fraction of it — a long tool-using turn narrating its work. That
// must complete, because the timeout is meant to catch a DEAD connection, not to
// cap how long legitimate work may take. The outer bound belongs to the caller
// (httpapi's turnCtx), which is where it already is.
func TestLongTurnSurvivesWhileFramesKeepArriving(t *testing.T) {
	const (
		idle    = 200 * time.Millisecond
		spacing = 60 * time.Millisecond
		frames  = 8 // 480ms of activity: well past idle, never idle for long
	)

	endpoint := picoServer(t, func(ctx context.Context, conn *websocket.Conn) {
		if err := writeFrame(ctx, conn, Frame{Type: "typing.start"}); err != nil {
			return
		}
		// Thought frames: they keep the connection busy without finalizing, which
		// is exactly the shape of a turn that is working rather than answering.
		for i := 0; i < frames; i++ {
			time.Sleep(spacing)
			if err := writeFrame(ctx, conn, msg("m1", "working", "thought", false)); err != nil {
				return
			}
		}
		if err := writeFrame(ctx, conn, msg("m2", "Done at last", "", false)); err != nil {
			return
		}
		_ = writeFrame(ctx, conn, Frame{Type: "typing.stop"})
	})

	c := &Client{IdleTimeout: idle}
	got, err := c.RunTurn(context.Background(), turn.Request{
		Endpoint:  endpoint,
		AuthToken: testToken,
		SessionID: "s1",
		Content:   "go",
	}, turn.Sink{})
	if err != nil {
		t.Fatalf("RunTurn failed on a turn that never went idle: %v", err)
	}
	if got != "Done at last" {
		t.Errorf("final = %q, want %q", got, "Done at last")
	}
}

// TestSilentConnectionTimesOut is the other half: the timeout must still fire
// when picoclaw genuinely stops talking, or a wedged turn would hold the request
// open until the caller's own (much longer) bound expires.
func TestSilentConnectionTimesOut(t *testing.T) {
	const idle = 150 * time.Millisecond

	endpoint := picoServer(t, func(ctx context.Context, conn *websocket.Conn) {
		_ = writeFrame(ctx, conn, Frame{Type: "typing.start"})
		// ...and then never speak again. picoServer holds the connection open.
	})

	c := &Client{IdleTimeout: idle}
	start := time.Now()
	_, err := c.RunTurn(context.Background(), turn.Request{
		Endpoint:  endpoint,
		AuthToken: testToken,
		SessionID: "s1",
		Content:   "go",
	}, turn.Sink{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("RunTurn returned nil error on a connection that went silent")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want it to mention a timeout", err)
	}
	// It must fire on the IDLE window, not on some longer bound.
	if elapsed > 2*time.Second {
		t.Errorf("took %v to give up, want ~%v", elapsed, idle)
	}
}

// TestIdleTimeoutIsNotATotalCap states the distinction directly: a turn that is
// silent for less than the window, repeatedly, for a total far exceeding it,
// must not be cut off.
func TestIdleTimeoutIsNotATotalCap(t *testing.T) {
	const idle = 120 * time.Millisecond

	endpoint := picoServer(t, func(ctx context.Context, conn *websocket.Conn) {
		_ = writeFrame(ctx, conn, Frame{Type: "typing.start"})
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			time.Sleep(40 * time.Millisecond)
			if err := writeFrame(ctx, conn, msg("m1", "still working", "thought", false)); err != nil {
				return
			}
		}
		_ = writeFrame(ctx, conn, msg("m2", "ok", "", false))
		_ = writeFrame(ctx, conn, Frame{Type: "typing.stop"})
	})

	c := &Client{IdleTimeout: idle}
	got, err := c.RunTurn(context.Background(), turn.Request{
		Endpoint:  endpoint,
		AuthToken: testToken,
		SessionID: "s1",
		Content:   "go",
	}, turn.Sink{})
	if err != nil {
		t.Fatalf("RunTurn failed after %v of steady activity with a %v idle window: %v",
			500*time.Millisecond, idle, err)
	}
	if got != "ok" {
		t.Errorf("final = %q, want %q", got, "ok")
	}
}
