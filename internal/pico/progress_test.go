package pico

import (
	"encoding/json"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
)

// collect drives frames through a processor and returns every progress event it
// emitted, plus the processor so completion state can be asserted.
func collect(frames []Frame) ([]turn.Progress, *processor) {
	var got []turn.Progress
	p := newProcessor(turn.Sink{Progress: func(ev turn.Progress) { got = append(got, ev) }}, "", "")
	for _, f := range frames {
		p.handle(f)
	}
	return got, p
}

// The exact frame captured off the wire from picoclaw (deepseek-chat), recorded
// in .specs/features/chat-progress-events/spec.md. `content` is empty and
// everything useful lives in tool_calls -- which this proxy used to discard.
const realToolFrame = `{
  "type": "message.create",
  "session_id": "50f6a0d6ea9ad59b4f0cc471dcf2b0ba",
  "payload": {
    "content": "",
    "kind": "tool_calls",
    "message_id": "8f9d4e24-0679-4b80-bf5f-e490ac7a9db0",
    "model_name": "deepseek-chat",
    "tool_calls": [{
      "id": "call_00_sUd4PTjpXM9bUcnfi25z5619",
      "type": "function",
      "function": {
        "name": "web_fetch",
        "arguments": "{\"maxChars\": 15000, \"url\": \"https://github.com/x\"}"
      },
      "extra_content": {
        "tool_feedback_explanation": "Com certeza! Deixe-me buscar novamente as informações do projeto."
      }
    }]
  }
}`

func TestDecodesRealToolCallFrame(t *testing.T) {
	var f Frame
	if err := json.Unmarshal([]byte(realToolFrame), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(f.Payload.ToolCalls) != 1 {
		t.Fatalf("tool_calls: got %d, want 1", len(f.Payload.ToolCalls))
	}
	tc := f.Payload.ToolCalls[0]
	if tc.Function.Name != "web_fetch" {
		t.Errorf("function name: got %q", tc.Function.Name)
	}
	want := "Com certeza! Deixe-me buscar novamente as informações do projeto."
	if tc.ExtraContent.Explanation != want {
		t.Errorf("explanation: got %q", tc.ExtraContent.Explanation)
	}
}

func TestRealToolFrameBecomesNarratedProgress(t *testing.T) {
	var f Frame
	if err := json.Unmarshal([]byte(realToolFrame), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, _ := collect([]Frame{f})
	if len(got) != 1 {
		t.Fatalf("progress events: got %d, want 1", len(got))
	}
	if got[0].Kind != "tool" || got[0].Tool != "web_fetch" {
		t.Errorf("kind/tool: got %q/%q", got[0].Kind, got[0].Tool)
	}
	if got[0].Text == "" {
		t.Error("the agent's own narration must be forwarded, not dropped")
	}
}

func TestToolCallWithoutNarrationStillNamesTheTool(t *testing.T) {
	// tool_feedback_explanation is model-generated and sometimes absent; the
	// function name is the fallback, and it must survive.
	f := Frame{Type: "message.create", Payload: Payload{
		Kind:      "tool_calls",
		ToolCalls: []ToolCall{{}},
	}}
	f.Payload.ToolCalls[0].Function.Name = "read_file"
	got, _ := collect([]Frame{f})
	if len(got) != 1 || got[0].Tool != "read_file" {
		t.Fatalf("got %+v", got)
	}
}

func TestThoughtAndPlaceholderBecomeProgress(t *testing.T) {
	got, _ := collect([]Frame{
		{Type: "message.create", Payload: Payload{Kind: "thought", Content: "thinking...", MessageID: "a"}},
		{Type: "message.create", Payload: Payload{Placeholder: true, Content: "Processing...", MessageID: "b"}},
	})
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Kind != "thought" || got[0].Text != "thinking..." {
		t.Errorf("thought: %+v", got[0])
	}
	if got[1].Kind != "placeholder" || got[1].Text != "Processing..." {
		t.Errorf("placeholder: %+v", got[1])
	}
}

func TestTypingBecomesProgress(t *testing.T) {
	got, _ := collect([]Frame{{Type: "typing.start"}, {Type: "typing.stop"}})
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Kind != "typing" || got[0].State != "start" {
		t.Errorf("start: %+v", got[0])
	}
	if got[1].State != "stop" {
		t.Errorf("stop: %+v", got[1])
	}
}

func TestIdenticalProgressIsDeduped(t *testing.T) {
	// Placeholder content is cumulative like plain content, so an unchanged
	// re-send would otherwise flood the stream.
	same := Frame{Type: "message.update", Payload: Payload{Placeholder: true, Content: "Processing...", MessageID: "b"}}
	got, _ := collect([]Frame{same, same, same})
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1 (deduped)", len(got))
	}
}

// The load-bearing guarantee: forwarding progress must not perturb the
// turn-completion state machine by even one field. turn_test.go covers the
// behaviour; this pins the internals a progress emit could plausibly touch.
func TestProgressDoesNotTouchCompletionState(t *testing.T) {
	_, p := collect([]Frame{
		{Type: "message.create", Payload: Payload{Kind: "tool_calls", Content: "", MessageID: "t"}},
		{Type: "message.create", Payload: Payload{Kind: "thought", Content: "hmm", MessageID: "h"}},
		{Type: "message.create", Payload: Payload{Placeholder: true, Content: "wait", MessageID: "p"}},
	})
	if p.hasPlainContent {
		t.Error("skipped frames must never count as real content")
	}
	if p.lastPlainID != "" {
		t.Errorf("lastPlainID: got %q, want empty", p.lastPlainID)
	}
	if len(p.plain) != 0 {
		t.Errorf("plain map: got %d entries, want 0", len(p.plain))
	}
	if p.finalContent() != "" {
		t.Errorf("finalContent: got %q, want empty", p.finalContent())
	}
}

func TestProgressNeverContributesToTheAnswer(t *testing.T) {
	var content string
	p := newProcessor(turn.Sink{Content: func(d string) { content += d }}, "", "")
	p.handle(Frame{Type: "message.create", Payload: Payload{Kind: "tool_calls", Content: "", MessageID: "t"}})
	p.handle(Frame{Type: "message.create", Payload: Payload{Kind: "thought", Content: "internal", MessageID: "h"}})
	p.handle(Frame{Type: "message.create", Payload: Payload{Content: "the answer", MessageID: "m"}})
	if content != "the answer" {
		t.Errorf("content: got %q, want %q", content, "the answer")
	}
	if p.finalContent() != "the answer" {
		t.Errorf("finalContent: got %q", p.finalContent())
	}
}

func TestNilSinkIsSafe(t *testing.T) {
	// The zero Sink is the old `nil` callback: no panics, same completion.
	p := newProcessor(turn.Sink{}, "", "")
	p.handle(Frame{Type: "typing.start"})
	p.handle(Frame{Type: "message.create", Payload: Payload{Kind: "tool_calls", MessageID: "t"}})
	p.handle(Frame{Type: "message.create", Payload: Payload{Content: "hi", MessageID: "m"}})
	if p.finalContent() != "hi" {
		t.Errorf("finalContent: got %q", p.finalContent())
	}
}
