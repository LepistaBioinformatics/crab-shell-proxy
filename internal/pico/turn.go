// Package pico speaks picoclaw's Pico Protocol over a WebSocket and runs a
// single conversational turn, translating to/from the OpenAI-compatible shape.
//
// This is a direct port of picoclaw-openai-proxy/server.js's runTurn, including
// its empirically-tuned completion logic: picoclaw wraps EVERY outbound message
// (including inter-iteration "tool_calls" indicators) in its own
// typing.start/typing.stop pair, so typing.stop alone never means "turn over".
// We only finalize once real (non-thought / non-tool_calls) content has arrived
// AND typing has stopped, after a short grace window; any new typing.start
// cancels a pending finalize. The state machine (processor) is separated from
// the transport so the fiddly part is unit-testable without a live socket.
package pico

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
	"github.com/coder/websocket"
)

// graceWindow is how long after real content + typing.stop we wait before
// declaring the turn finished (matches server.js's 500ms).
const graceWindow = 500 * time.Millisecond

// Frame is one Pico Protocol message.
type Frame struct {
	Type    string  `json:"type"`
	Payload Payload `json:"payload"`
}

// Payload is the subset of a frame's payload this proxy inspects.
type Payload struct {
	MessageID   string `json:"message_id"`
	Content     string `json:"content"`
	Kind        string `json:"kind"`
	Placeholder bool   `json:"placeholder"`
	Message     string `json:"message"` // error frames carry human text here
}

// processor is the pure turn-completion state machine.
type processor struct {
	plain           map[string]string // message_id -> latest cumulative content
	lastPlainID     string
	hasPlainContent bool
	isTyping        bool
	onDelta         func(string)
}

func newProcessor(onDelta func(string)) *processor {
	return &processor{plain: map[string]string{}, onDelta: onDelta}
}

// signal tells the transport driver how to manage the finalize grace timer.
type signal struct {
	arm    bool   // (re)arm the finalize grace timer
	cancel bool   // cancel any pending finalize grace timer
	errMsg string // non-empty => picoclaw reported an error
}

// handle applies one frame and returns the timer action, mirroring server.js.
func (p *processor) handle(f Frame) signal {
	switch f.Type {
	case "message.create", "message.update":
		pl := f.Payload
		// Not the plain assistant answer: internal reasoning, tool-call
		// indicators, and placeholders are all skipped.
		if pl.Kind == "thought" || pl.Kind == "tool_calls" || pl.Placeholder {
			return signal{}
		}
		prev := p.plain[pl.MessageID]
		p.plain[pl.MessageID] = pl.Content
		p.lastPlainID = pl.MessageID
		p.hasPlainContent = true
		// Cumulative content: emit only the newly-appended suffix.
		if len(pl.Content) > len(prev) && p.onDelta != nil {
			p.onDelta(pl.Content[len(prev):])
		}
		return p.maybeArmGrace()
	case "typing.start":
		// The agent kept going (more tool calls, or a later message) — cancel
		// any pending finalize.
		p.isTyping = true
		return signal{cancel: true}
	case "typing.stop":
		p.isTyping = false
		return p.maybeArmGrace()
	case "error":
		msg := f.Payload.Message
		if msg == "" {
			msg = "picoclaw error"
		}
		return signal{errMsg: msg}
	}
	return signal{}
}

// maybeArmGrace arms the finalize timer only once real content has arrived and
// typing has stopped.
func (p *processor) maybeArmGrace() signal {
	if !p.hasPlainContent || p.isTyping {
		return signal{}
	}
	return signal{arm: true}
}

// finalContent is the assistant answer to return once the turn finishes.
func (p *processor) finalContent() string {
	if p.lastPlainID == "" {
		return ""
	}
	return p.plain[p.lastPlainID]
}

// Client runs turns against picoclaw Pico Protocol endpoints.
type Client struct {
	// TurnTimeout is the hard cap on a single turn.
	TurnTimeout time.Duration
}

// RunTurn opens a Pico Protocol WebSocket to req.Endpoint (e.g.
// ws://picoclaw-alpha-<hash>:18790/pico/ws), sends req.Content for req.SessionID,
// and returns the final assistant text. If onDelta is non-nil it is called with
// each newly-appended chunk as it streams in. req.SessionKey/Model are unused
// (picoclaw is pinned server-side).
func (c *Client) RunTurn(ctx context.Context, req turn.Request, onDelta func(string)) (string, error) {
	timeout := c.TurnTimeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	u, err := url.Parse(req.Endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid picoclaw ws url: %w", err)
	}
	q := u.Query()
	q.Set("session_id", req.SessionID)
	u.RawQuery = q.Encode()

	conn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		// Pico Protocol authenticates via the token.<token> subprotocol.
		Subprotocols: []string{"token." + req.AuthToken},
	})
	if err != nil {
		return "", fmt.Errorf("failed to connect to picoclaw pico channel (check token/url): %w", err)
	}
	// coder/websocket requires draining; CloseNow on all exits is safe.
	defer conn.CloseNow()
	// picoclaw answers can be large; lift the default read limit.
	conn.SetReadLimit(32 << 20)

	// Match server.js exactly: {type, session_id, payload:{content}} with no
	// extra payload fields.
	send, err := json.Marshal(map[string]any{
		"type":       "message.send",
		"session_id": req.SessionID,
		"payload":    map[string]any{"content": req.Content},
	})
	if err != nil {
		return "", err
	}
	if err := conn.Write(ctx, websocket.MessageText, send); err != nil {
		return "", fmt.Errorf("failed to send message to picoclaw: %w", err)
	}

	frames := make(chan Frame)
	readErr := make(chan error, 1)
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			var f Frame
			if err := json.Unmarshal(data, &f); err != nil {
				continue // ignore malformed frames, like server.js
			}
			select {
			case frames <- f:
			case <-ctx.Done():
				return
			}
		}
	}()

	proc := newProcessor(onDelta)
	grace := time.NewTimer(0)
	if !grace.Stop() {
		<-grace.C
	}

	for {
		select {
		case <-ctx.Done():
			return "", errors.New("timed out waiting for picoclaw response")
		case err := <-readErr:
			// Connection closed/failed before we finalized.
			return "", fmt.Errorf("picoclaw connection closed before response: %w", err)
		case <-grace.C:
			// Grace elapsed with no new activity — the turn is done.
			return proc.finalContent(), nil
		case f := <-frames:
			sig := proc.handle(f)
			if sig.errMsg != "" {
				return "", errors.New(sig.errMsg)
			}
			if sig.cancel || sig.arm {
				stopTimer(grace)
			}
			if sig.arm {
				grace.Reset(graceWindow)
			}
		}
	}
}

// stopTimer drains a timer that may or may not have fired, so a later Reset is
// clean.
func stopTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}
