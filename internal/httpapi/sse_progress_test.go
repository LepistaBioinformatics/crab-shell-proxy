package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
)

// progressTurner emits a fixed script of progress events and content, so the
// exact bytes streamTurn puts on the wire can be asserted.
type progressTurner struct {
	progress []turn.Progress
	content  []string
}

func (p *progressTurner) RunTurn(_ context.Context, _ turn.Request, sink turn.Sink) (string, error) {
	for _, ev := range p.progress {
		sink.EmitProgress(ev)
	}
	for _, c := range p.content {
		sink.EmitContent(c)
	}
	return strings.Join(p.content, ""), nil
}

// frames splits an SSE body into its decoded `data:` payloads, skipping [DONE].
func frames(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, block := range strings.Split(body, "\n\n") {
		line := strings.TrimSpace(block)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("frame is not valid JSON (%v): %s", err, payload)
		}
		out = append(out, m)
	}
	return out
}

func runStream(t *testing.T, tr Turner) []map[string]any {
	t.Helper()
	s := testServer(&fakeOrch{}, tr)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	s.streamTurn(rec, req,
		config.Agent{Key: "alpha", Harness: config.HarnessPicoclaw},
		docker.WorkspaceKey{TenantID: "t", SubsAccID: "s", Role: "alpha", UserAccID: "u"},
		"owner@example.com", "sess", "hello", "picoclaw", "chatcmpl-test")
	return frames(t, rec.Body.String())
}

// The compatibility guarantee behind the whole wire format: a progress frame is
// a NORMAL chat.completion.chunk with an empty delta. A client that knows
// nothing about x_crab_progress reads choices[0].delta.content, finds nothing,
// and skips the frame -- it is ignored, never a parse error.
func TestProgressFrameIsAnOrdinaryChunkWithEmptyDelta(t *testing.T) {
	got := runStream(t, &progressTurner{
		progress: []turn.Progress{{Kind: "tool", Text: "Deixe-me buscar…", Tool: "web_fetch"}},
	})

	var progressFrames int
	for _, f := range got {
		if f["object"] != "chat.completion.chunk" {
			t.Errorf("every frame must stay a chat.completion.chunk, got %v", f["object"])
		}
		raw, ok := f["x_crab_progress"]
		if !ok {
			continue
		}
		progressFrames++

		choices, _ := f["choices"].([]any)
		if len(choices) != 1 {
			t.Fatalf("choices: %v", f["choices"])
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if len(delta) != 0 {
			t.Errorf("a progress frame must carry an EMPTY delta, got %v", delta)
		}
		if _, hasContent := delta["content"]; hasContent {
			t.Error("progress must never appear as assistant content")
		}

		ev, _ := raw.(map[string]any)
		if ev["kind"] != "tool" || ev["tool"] != "web_fetch" {
			t.Errorf("progress payload: %v", ev)
		}
		if ev["text"] != "Deixe-me buscar…" {
			t.Errorf("the agent's narration must survive verbatim, got %v", ev["text"])
		}
	}
	if progressFrames != 1 {
		t.Fatalf("progress frames: got %d, want 1", progressFrames)
	}
}

func TestProgressDoesNotPolluteTheAnswerOnTheWire(t *testing.T) {
	got := runStream(t, &progressTurner{
		progress: []turn.Progress{
			{Kind: "typing", State: "start"},
			{Kind: "thought", Text: "internal reasoning"},
		},
		content: []string{"the ", "answer"},
	})

	var assembled string
	for _, f := range got {
		choices, _ := f["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if c, ok := delta["content"].(string); ok {
			assembled += c
		}
	}
	// The role-opening chunk contributes "", the progress frames contribute
	// nothing, so an OpenAI-shaped client reconstructs exactly the answer.
	if assembled != "the answer" {
		t.Errorf("reconstructed answer: got %q, want %q", assembled, "the answer")
	}
}
