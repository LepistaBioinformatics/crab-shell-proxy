package history

import "testing"

// What the member reads over the answer: not only what the agent SAID, but what
// it DID. The harness writes one entry per iteration carrying its events; this
// file is about serving them.

func readOne(t *testing.T, lines string) []Message {
	t.Helper()
	dir := t.TempDir()
	// The ganglion names a transcript after the conversation id, so no meta file
	// is involved -- Read finds it by name.
	writeFile(t, dir, "k.jsonl", lines)
	msgs, err := Read(dir, "k")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return msgs
}

// THE ENTRY THAT USED TO VANISH. A frame that asked for a tool and narrated
// nothing has no content and no reasoning, and the filter above dropped it — so
// a turn that ran fourteen tools showed ten steps, with no way to tell which
// work had produced the answer.
func TestRead_AnEventsOnlyEntryIsServedAsAStep(t *testing.T) {
	msgs := readOne(t, `{"role":"user","content":"oi"}
{"role":"assistant","content":"","events":[{"kind":"tool","name":"web_fetch","arguments":"{\"url\":\"https://x\"}","status":"ok"}]}
{"role":"assistant","content":"pronto"}
`)
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want the question, the events and the answer: %+v", len(msgs), msgs)
	}
	ev := msgs[1]
	if ev.Kind != KindStep {
		t.Errorf("kind = %q, want %q: an events entry is narration, never an answer", ev.Kind, KindStep)
	}
	if len(ev.Events) != 1 || ev.Events[0].Name != "web_fetch" || ev.Events[0].Status != "ok" {
		t.Errorf("events = %+v, want the call and how it ended", ev.Events)
	}
	if ev.Content != "" {
		t.Errorf("content = %q; the entry is the step's detail, not a second thing said", ev.Content)
	}
}

// And it must not become the answer. keepAnswerlessTurns promotes narration when
// a turn has nothing else to show, and an entry with no text would render as an
// empty band where the reply should be.
func TestRead_AnEventsOnlyEntryIsNeverPromotedToTheAnswer(t *testing.T) {
	msgs := readOne(t, `{"role":"user","content":"oi"}
{"role":"assistant","content":"","events":[{"kind":"tool","name":"sh","status":"failed"}]}
`)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages: %+v", len(msgs), msgs)
	}
	if msgs[1].Kind != KindStep {
		t.Errorf("kind = %q, want it to stay a step", msgs[1].Kind)
	}
}

// A narration frame names the tools it asked for, with no outcome — it is
// written before they run. This is also what keeps every transcript from before
// the harness recorded events naming its tools: nothing is backfilled.
func TestRead_ANarrationFrameNamesTheToolsItAskedFor(t *testing.T) {
	msgs := readOne(t, `{"role":"user","content":"oi"}
{"role":"assistant","content":"vou ver","tool_calls":[{"id":"1","name":"sh","function":{"name":"","arguments":"{\"cmd\":\"ls\"}"}}]}
{"role":"assistant","content":"pronto"}
`)
	step := msgs[1]
	if step.Kind != KindStep {
		t.Fatalf("kind = %q, want a step", step.Kind)
	}
	if len(step.Events) != 1 || step.Events[0].Name != "sh" {
		t.Fatalf("events = %+v, want the call it narrated", step.Events)
	}
	if step.Events[0].Status != "" {
		t.Errorf("status = %q; the frame is written before the call runs, so there is none", step.Events[0].Status)
	}
}

// picoclaw nests the name under `function`; the ganglion writes it flat. Two
// harnesses write this file, so both spellings are read.
func TestRead_ReadsEitherHarnessSpellingOfAToolName(t *testing.T) {
	msgs := readOne(t, `{"role":"user","content":"oi"}
{"role":"assistant","content":"vou ver","tool_calls":[{"function":{"name":"web_search","arguments":"{}"}}]}
{"role":"assistant","content":"pronto"}
`)
	if len(msgs[1].Events) != 1 || msgs[1].Events[0].Name != "web_search" {
		t.Errorf("events = %+v, want picoclaw's nested name read", msgs[1].Events)
	}
}

// A call with neither spelling names nothing. A row naming nothing is worse than
// no row, and the step marker does not depend on this.
func TestRead_ACallWithNoNameIsNotAnEvent(t *testing.T) {
	msgs := readOne(t, `{"role":"user","content":"oi"}
{"role":"assistant","content":"vou ver","tool_calls":[{"id":"1"}]}
{"role":"assistant","content":"pronto"}
`)
	if msgs[1].Kind != KindStep {
		t.Errorf("kind = %q; the presence of a call is still what marks a step", msgs[1].Kind)
	}
	if len(msgs[1].Events) != 0 {
		t.Errorf("events = %+v, want none", msgs[1].Events)
	}
}

// A transcript with no events anywhere is served exactly as it was. Every
// picoclaw conversation is one, and so is every ganglion conversation older than
// this feature.
func TestRead_ATranscriptWithNoEventsIsUnchanged(t *testing.T) {
	msgs := readOne(t, `{"role":"user","content":"oi"}
{"role":"assistant","content":"pronto"}
`)
	if len(msgs) != 2 || msgs[1].Kind != "" || len(msgs[1].Events) != 0 {
		t.Errorf("msgs = %+v, want an ordinary question and answer", msgs)
	}
}
