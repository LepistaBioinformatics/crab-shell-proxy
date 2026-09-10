// Package ganglion runs a turn against crab-ganglion-harness, this project's
// own agent runtime.
//
// It is the second implementation of httpapi.Turner, selected by
// config.HarnessGanglion. internal/pico stays live and untouched: ganglion is
// an ALTERNATIVE to picoclaw, not a replacement, and picoclaw remains the
// default until the exit criteria in the spec are met.
//
// This runner is markedly simpler than internal/pico, and the reason is worth
// stating: there is no protocol translation. The harness serves SSE natively
// and finishes a turn by closing the stream, so there is no analogue of
// pico/turn.go's 500ms graceWindow -- the heuristic that guesses when a turn
// ended because picoclaw wraps every outbound message in its own typing pair.
package ganglion

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
)

// maxLine is the SSE scanner's buffer. bufio.Scanner's 64KiB default truncates
// real assistant answers -- the Hermes runner hit exactly this and had to lift
// its own scanner to 8MiB.
const maxLine = 8 << 20

// Client runs turns against a ganglion container.
type Client struct {
	HTTP *http.Client
	Logf func(string, ...any)

	mu     sync.Mutex
	active map[string]context.CancelFunc // sessionID -> cancel
}

func New(hc *http.Client) *Client {
	if hc == nil {
		// No global timeout: a turn legitimately runs for minutes, and a
		// Client.Timeout would cut it mid-answer. The per-turn context is what
		// bounds this.
		hc = &http.Client{}
	}
	return &Client{HTTP: hc, active: map[string]context.CancelFunc{}}
}

// RunTurn satisfies httpapi.Turner.
func (c *Client) RunTurn(ctx context.Context, req turn.Request, sink turn.Sink) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	c.track(req.SessionID, cancel)
	defer c.untrack(req.SessionID)
	defer cancel()

	body, err := json.Marshal(request{
		Model:    req.Model,
		Stream:   true,
		Messages: []wireMessage{{Role: "user", Content: req.Content}},
	})
	if err != nil {
		return "", fmt.Errorf("marshal turn: %w", err)
	}

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(req.Endpoint, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	if req.AuthToken != "" {
		hreq.Header.Set("Authorization", "Bearer "+req.AuthToken)
	}
	// FR-17: SessionID and SessionKey are populated on turn.Request and were
	// unread by every runner until now. The harness never derives them -- the
	// proxy owns the preimage, and computing it twice is how two components
	// silently disagree.
	hreq.Header.Set("X-Ganglion-Session-Id", req.SessionID)
	hreq.Header.Set("X-Ganglion-Session-Key", req.SessionKey)

	resp, err := c.HTTP.Do(hreq)
	if err != nil {
		return "", fmt.Errorf("ganglion request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("ganglion %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	return c.consume(resp.Body, sink)
}

// consume reads the SSE stream and fans it out to the sink.
func (c *Client) consume(r io.Reader, sink turn.Sink) (string, error) {
	var answer strings.Builder
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		// Only data: lines carry payload. A comment (": ping") is the
		// heartbeat -- it MUST be dropped here rather than forwarded, because
		// downstream the webapp derives "quiet for" from the last event it saw,
		// and a heartbeat that counted as an event would pin that at zero.
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var ch chunk
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			// A line that is not a chunk is skipped, never treated as content:
			// forwarding it would inject raw JSON into the member's answer.
			continue
		}
		if len(ch.Choices) == 0 {
			continue
		}
		d := ch.Choices[0].Delta
		if d.Content != "" {
			answer.WriteString(d.Content)
			sink.EmitContent(d.Content)
		}
		if d.Progress != nil {
			sink.EmitProgress(turn.Progress{
				Kind:  d.Progress.Kind,
				Text:  d.Progress.Text,
				Tool:  d.Progress.Tool,
				State: d.Progress.State,
			})
		}
		if d.Error != "" {
			// Reported as a signal AND left out of the answer. The proxy's sink
			// has a separate Error channel precisely so a failure is not prose
			// the client shows once and then loses to the durable transcript.
			sink.EmitError(d.Error)
		}
	}
	if err := sc.Err(); err != nil {
		return answer.String(), fmt.Errorf("read ganglion stream: %w", err)
	}
	return answer.String(), nil
}

// Cancel stops the turn running on req.SessionID, if there is one. A session
// with no active turn is not an error.
func (c *Client) Cancel(_ context.Context, req turn.Request) error {
	c.mu.Lock()
	cancel := c.active[req.SessionID]
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (c *Client) track(id string, cancel context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil {
		c.active = map[string]context.CancelFunc{}
	}
	c.active[id] = cancel
}

func (c *Client) untrack(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.active, id)
}

// --- wire ------------------------------------------------------------------

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type request struct {
	Model    string        `json:"model"`
	Stream   bool          `json:"stream"`
	Messages []wireMessage `json:"messages"`
}

// chunk is the harness's OpenAI-compatible frame plus this stack's two
// extensions, which crab-shell-proxy's own sse.go already emits downstream
// under the same names.
type chunk struct {
	Choices []struct {
		Delta struct {
			Content  string `json:"content"`
			Progress *struct {
				Kind  string `json:"kind"`
				Text  string `json:"text"`
				Tool  string `json:"tool"`
				State string `json:"state"`
			} `json:"x_crab_progress"`
			Error string `json:"x_crab_error"`
		} `json:"delta"`
	} `json:"choices"`
}
