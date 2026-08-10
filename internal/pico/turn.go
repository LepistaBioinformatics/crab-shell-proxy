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
	"strings"
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
	// tool_calls frames always arrive with an EMPTY Content -- everything useful
	// is in here, and this proxy used to drop it on the floor.
	ToolCalls []ToolCall `json:"tool_calls"`
	// A file the agent produced. picoclaw's Pico channel sends these on a
	// message.create of its own (upstream pkg/channels/pico/pico.go SendMedia),
	// with the caption in Content -- usually empty. Dropping this array is why
	// "Requested output delivered via tool attachment." arrived with no file.
	Attachments []Attachment `json:"attachments"`
}

// Attachment is one file picoclaw delivered through the channel. Field names are
// upstream's (pico.go's SendMedia builds this map literal); `URL` is RELATIVE to
// the harness endpoint, e.g. "/pico/media/<refID>", and needs the same bearer
// token the WebSocket uses.
type Attachment struct {
	Type        string `json:"type"` // file | image | audio | video
	URL         string `json:"url"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
}

// ToolCall is one entry of a tool_calls frame.
type ToolCall struct {
	Function struct {
		Name string `json:"name"`
		// `arguments` is deliberately NOT decoded: it is unbounded, untrusted
		// model output that can carry paths, URLs or secrets, and it has no
		// display role once Explanation exists.
	} `json:"function"`
	ExtraContent struct {
		// Explanation is a first-person sentence the agent wrote itself, in the
		// user's own language ("Deixe-me buscar as informações do projeto."). It
		// is the best progress text available anywhere in this pipeline. Being
		// model-generated it is sometimes absent, hence the Name fallback.
		Explanation string `json:"tool_feedback_explanation"`
	} `json:"extra_content"`
}

// processor is the pure turn-completion state machine.
type processor struct {
	plain           map[string]string // message_id -> latest cumulative content
	lastPlainID     string
	hasPlainContent bool
	isTyping        bool
	sink            turn.Sink
	lastProgress    turn.Progress
	// mediaBase is the harness origin ("http://name:18790") that an attachment's
	// relative URL hangs off, and mediaToken the bearer it needs. They live here
	// rather than in httpapi because resolving them means knowing that the
	// endpoint is a ws:// URL of this channel's own shape -- which is this
	// package's business, not its caller's.
	mediaBase  string
	mediaToken string
}

// newProcessor takes the sink by value: its zero value (all callbacks nil) is
// a valid no-op, so `newProcessor(turn.Sink{}, "", "")` is exactly the old
// `newProcessor(nil)`.
func newProcessor(sink turn.Sink, mediaBase, mediaToken string) *processor {
	return &processor{plain: map[string]string{}, sink: sink, mediaBase: mediaBase, mediaToken: mediaToken}
}

// resolveAttachment turns the frame's harness-relative URL into something a
// caller can fetch. An already-absolute URL is passed through: upstream builds a
// relative one today, and inventing a base for a value that already has one would
// corrupt it.
func (p *processor) resolveAttachment(a Attachment) turn.Attachment {
	out := turn.Attachment{
		Type: a.Type, URL: a.URL, Filename: a.Filename,
		ContentType: a.ContentType, AuthToken: p.mediaToken,
	}
	if strings.HasPrefix(a.URL, "/") && p.mediaBase != "" {
		out.URL = p.mediaBase + a.URL
	}
	return out
}

// emitProgress forwards a non-content signal, suppressing an exact repeat.
// Placeholder/thought content is cumulative like plain content, so an unchanged
// re-send would otherwise flood the stream.
//
// It touches NO processor state that the completion machine reads, which is
// what keeps the turn-completion behaviour bit-identical.
func (p *processor) emitProgress(kind, text, tool, state string) {
	next := turn.Progress{Kind: kind, Text: text, Tool: tool, State: state}
	if next == p.lastProgress {
		return
	}
	p.lastProgress = next
	p.sink.EmitProgress(next)
}

// progressFor maps a skipped frame to its progress event. tool_calls carry the
// agent's own narration; everything else falls back to the frame's content.
func progressFor(pl Payload) (kind, text, tool string) {
	switch {
	case pl.Kind == "tool_calls":
		if len(pl.ToolCalls) > 0 {
			// A frame may carry several calls; the first is the one the agent
			// narrated.
			tc := pl.ToolCalls[0]
			return "tool", tc.ExtraContent.Explanation, tc.Function.Name
		}
		return "tool", pl.Content, ""
	case pl.Kind == "thought":
		return "thought", pl.Content, ""
	default:
		return "placeholder", pl.Content, ""
	}
}

// processingErrorPrefix is what picoclaw puts in front of a turn it could not
// complete — `fmt.Sprintf("Error processing message: %v", err)` in
// pkg/agent/error_format.go, published through PublishResponseIfNeeded like any
// other assistant message.
//
// A string match is unsatisfying and it is what upstream offers: the frame carries
// no type, kind or severity that separates a failure from an answer. If picoclaw
// ever grows a real marker, this is the one place that has to change.
const processingErrorPrefix = "Error processing message:"

func isProcessingError(content string) bool {
	return strings.HasPrefix(content, processingErrorPrefix)
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
		// indicators, and placeholders are all skipped -- but they are the only
		// sign of life during a turn, so they are forwarded as progress first.
		// Nothing below the emit changes: the skip, and every field the
		// completion machine reads, are exactly as they were.
		if pl.Kind == "thought" || pl.Kind == "tool_calls" || pl.Placeholder {
			kind, text, tool := progressFor(pl)
			p.emitProgress(kind, text, tool, "")
			return signal{}
		}
		// A frame carrying files is a DELIVERY, not the assistant's answer, and it
		// is handled before the plain-content branch on purpose. That branch would
		// set lastPlainID to this frame's id, and picoclaw sends these with an
		// empty caption -- so finalContent() would return "" and ERASE the answer
		// on a non-streaming request. It must not arm the finalize grace either: the
		// frame arrives inside picoclaw's own typing pair, and letting a delivery
		// end a turn would cut the reply that follows it.
		//
		// A non-empty caption is still the agent talking, so it goes out as content.
		if len(pl.Attachments) > 0 {
			for _, a := range pl.Attachments {
				p.sink.EmitAttachment(p.resolveAttachment(a))
			}
			if pl.Content != "" {
				p.sink.EmitContent(pl.Content)
			}
			return signal{}
		}
		prev := p.plain[pl.MessageID]
		p.plain[pl.MessageID] = pl.Content
		p.lastPlainID = pl.MessageID
		p.hasPlainContent = true
		// Cumulative content: emit only the newly-appended suffix.
		if len(pl.Content) > len(prev) {
			p.sink.EmitContent(pl.Content[len(prev):])
		}
		// A FAILED turn arrives here, as an ordinary assistant message. picoclaw
		// formats it in pkg/agent/error_format.go and publishes it through the same
		// call an answer takes — no distinct frame type, no kind, no severity — so
		// this prefix is the only discriminator on the wire.
		//
		// Reported as a signal AS WELL AS content, never instead of it: a generic
		// OpenAI client reads delta.content and nothing else, and the non-streaming
		// path derives its whole body from this same plain-content bookkeeping.
		//
		// Tested against the CUMULATIVE content, not the suffix just emitted: the
		// prefix is only reliably at the start of the whole message, so a failure
		// whose text arrives as an update after a partial would otherwise slip
		// through. Comparing against `prev` is what keeps it to one report per
		// message rather than one per update.
		if isProcessingError(pl.Content) && !isProcessingError(prev) {
			p.sink.EmitError(pl.Content)
		}
		return p.maybeArmGrace()
	case "typing.start":
		// The agent kept going (more tool calls, or a later message) — cancel
		// any pending finalize.
		p.isTyping = true
		p.emitProgress("typing", "", "", "start")
		return signal{cancel: true}
	case "typing.stop":
		p.isTyping = false
		p.emitProgress("typing", "", "", "stop")
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

// mediaBaseFrom turns the ws endpoint into the http origin that serves
// /pico/media/<id> -- the same host and port, since the Pico channel's WebSocket
// and its media route are one HTTP server (upstream serves both from the channel's
// ServeHTTP). An unparseable endpoint yields "", which leaves an attachment URL
// relative and unfetchable rather than guessing an origin.
func mediaBaseFrom(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := "http"
	if u.Scheme == "wss" || u.Scheme == "https" {
		scheme = "https"
	}
	return scheme + "://" + u.Host
}

// Client runs turns against picoclaw Pico Protocol endpoints.
type Client struct {
	// TurnTimeout is the hard cap on a single turn.
	TurnTimeout time.Duration
}

// RunTurn opens a Pico Protocol WebSocket to req.Endpoint (e.g.
// ws://picoclaw-alpha-<hash>:18790/pico/ws), sends req.Content for req.SessionID,
// and returns the final assistant text. sink.Content receives each newly-appended
// chunk; sink.Progress receives the non-content signals (tool narration, thoughts,
// typing) that would otherwise be invisible during the wait. req.SessionKey/Model
// are unused (picoclaw is pinned server-side).
func (c *Client) RunTurn(ctx context.Context, req turn.Request, sink turn.Sink) (string, error) {
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

	proc := newProcessor(sink, mediaBaseFrom(req.Endpoint), req.AuthToken)
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
