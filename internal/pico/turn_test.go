package pico

import (
	"strings"
	"testing"
)

// drive replays a frame sequence through the processor, tracking the resulting
// grace-timer state (armed/cancelled) exactly as the transport driver would,
// and returns the streamed deltas plus whether grace was armed at the end.
func drive(frames []Frame) (deltas []string, armed bool, errMsg string, final string) {
	p := newProcessor(func(d string) { deltas = append(deltas, d) })
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
		{Type: "typing.stop"}, // arm
		{Type: "typing.start"}, // cancel
		msg("t1", "tool", "tool_calls", false),
		{Type: "typing.stop"},  // still armed? content is tool_calls, but hasPlainContent already true
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
