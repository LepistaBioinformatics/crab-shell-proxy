package pico

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
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
