// Package hermes drives a hermes-agent container over its OpenAI-compatible HTTP
// API server (POST /v1/chat/completions), running one conversational turn and
// streaming the reply. Unlike picoclaw's bespoke Pico Protocol, this is a near
// passthrough: the proxy already speaks the OpenAI shape.
package hermes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
)

// Client runs turns against a hermes-agent API server.
type Client struct {
	// TurnTimeout is the hard cap on a single turn.
	TurnTimeout time.Duration
	// HTTPClient is optional; when nil a default client bounded by the turn
	// context is used.
	HTTPClient *http.Client
}

// chunk is the subset of an OpenAI chat.completion.chunk this proxy reads.
type chunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

// RunTurn POSTs req.Content to <req.Endpoint>/v1/chat/completions on the hermes
// API server, authenticating with req.AuthToken and scoping the transcript
// (X-Hermes-Session-Id) and long-term memory (X-Hermes-Session-Key). It streams
// the SSE chat.completion.chunk deltas: onDelta (when non-nil) receives each new
// content fragment as it arrives; the full assistant text is returned.
// Non-content events (hermes.tool.progress, role-only deltas) are ignored.
func (c *Client) RunTurn(ctx context.Context, req turn.Request, onDelta func(string)) (string, error) {
	timeout := c.TurnTimeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := json.Marshal(map[string]any{
		"model":    req.Model,
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": req.Content}},
	})
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(req.Endpoint, "/") + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if req.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.AuthToken)
	}
	if req.SessionID != "" {
		httpReq.Header.Set("X-Hermes-Session-Id", req.SessionID)
	}
	if req.SessionKey != "" {
		httpReq.Header.Set("X-Hermes-Session-Key", req.SessionKey)
	}

	client := c.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("hermes request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("hermes API status %d", resp.StatusCode)
	}

	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	// Assistant answers can be large; lift the default token size.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue // ignore SSE "event:"/"id:"/comment lines
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var ch chunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			continue // non-chunk event (e.g. hermes.tool.progress)
		}
		for _, choice := range ch.Choices {
			if choice.Delta.Content == "" {
				continue
			}
			sb.WriteString(choice.Delta.Content)
			if onDelta != nil {
				onDelta(choice.Delta.Content)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return sb.String(), fmt.Errorf("hermes stream read: %w", err)
	}
	return sb.String(), nil
}
