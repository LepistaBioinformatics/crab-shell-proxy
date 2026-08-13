package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
)

// errorTurner covers both ways a turn can fail: picoclaw publishing the failure as
// an ordinary assistant message (which internal/pico classifies and reports through
// Sink.Error, while still streaming it as content), and RunTurn itself returning an
// error — picoclaw's own `error` frame, or a transport failure.
type errorTurner struct {
	content  []string
	reported string // emitted through Sink.Error
	runErr   error  // returned by RunTurn
}

func (e *errorTurner) Cancel(_ context.Context, _ turn.Request) error { return nil }

func (e *errorTurner) RunTurn(_ context.Context, _ turn.Request, sink turn.Sink) (string, error) {
	for _, c := range e.content {
		sink.EmitContent(c)
	}
	if e.reported != "" {
		sink.EmitError(e.reported)
	}
	if e.runErr != nil {
		return "", e.runErr
	}
	return strings.Join(e.content, ""), nil
}

// errorFrames returns the x_crab_error payloads on the wire.
func errorFrames(t *testing.T, got []map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, f := range got {
		raw, ok := f["x_crab_error"]
		if !ok {
			continue
		}
		ev, _ := raw.(map[string]any)
		out = append(out, ev)

		// Same compatibility contract progress carries: an ordinary chunk with an
		// EMPTY delta, so a client that knows nothing about the field finds no content
		// and skips the frame rather than failing to parse it.
		if f["object"] != "chat.completion.chunk" {
			t.Errorf("an error frame must stay a chat.completion.chunk, got %v", f["object"])
		}
		choices, _ := f["choices"].([]any)
		if len(choices) != 1 {
			t.Fatalf("choices: %v", f["choices"])
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if len(delta) != 0 {
			t.Errorf("an error frame must carry an EMPTY delta, got %v", delta)
		}
	}
	return out
}

const visionErr = `Error processing message: selected vision model "glm-4.7-flash" ` +
	`does not support image input; update agents.defaults.image_model to a multimodal model`

// The reported bug. The failure text streams as content AND is reported as an error:
// the content is what a generic OpenAI client reads, and the signal is what lets the
// member's interface say "this broke" — necessary because the text is never
// persisted, so any client showing it as a reply loses it to the next reconcile
// against the durable transcript.
func TestFailureIsReportedAndStillStreamed(t *testing.T) {
	got := runStream(t, &errorTurner{content: []string{visionErr}, reported: visionErr})

	errs := errorFrames(t, got)
	if len(errs) != 1 {
		t.Fatalf("error frames: got %d, want 1", len(errs))
	}
	if errs[0]["message"] != visionErr {
		t.Errorf("message = %v, want the harness sentence verbatim", errs[0]["message"])
	}

	// The actionable half must survive on the content channel too.
	var streamed string
	for _, f := range got {
		choices, _ := f["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if c, ok := delta["content"].(string); ok {
			streamed += c
		}
	}
	if !strings.Contains(streamed, "image_model") {
		t.Errorf("streamed content lost the actionable text: %q", streamed)
	}
}

// A RunTurn error used to be logged and nothing else, so the client received a
// well-formed finish_reason "stop" with no content and no reason — a broken turn was
// byte-indistinguishable from one that answered nothing.
func TestRunTurnErrorReachesTheClient(t *testing.T) {
	got := runStream(t, &errorTurner{runErr: errors.New("picoclaw connection closed")})

	errs := errorFrames(t, got)
	if len(errs) != 1 {
		t.Fatalf("error frames: got %d, want 1", len(errs))
	}
	if msg, _ := errs[0]["message"].(string); !strings.Contains(msg, "connection closed") {
		t.Errorf("message = %q, want the failure named", msg)
	}
}

// An ordinary turn must not grow an error frame.
func TestSuccessfulTurnCarriesNoErrorFrame(t *testing.T) {
	got := runStream(t, &errorTurner{content: []string{"Here is your answer."}})
	if errs := errorFrames(t, got); len(errs) != 0 {
		t.Errorf("a successful turn emitted %d error frame(s): %v", len(errs), errs)
	}
}
