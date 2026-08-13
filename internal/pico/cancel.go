package pico

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
	"github.com/coder/websocket"
)

// stopCommand is picoclaw's built-in command for aborting the turn running on a
// session. It is sent as an ordinary message.send because that is the ONLY
// client→server frame the Pico Protocol accepts (pkg/channels/pico/protocol.go
// at v0.3.1: message.send, media.send, ping).
//
// The command is not a convenience wrapper around a severed connection. It
// reaches AgentLoop.HardAbort, which cancels the provider and tool contexts,
// cascades cancellation to child sub-turns, and rolls the session history back
// to its length before the turn started. The half-written turn is REMOVED, not
// persisted — which is why this is a real stop and not a UI-only one.
const stopCommand = "/stop"

// Cancel aborts the turn running on req.SessionID, if there is one.
//
// It dials a SECOND WebSocket rather than reusing the turn's: that connection is
// owned by an in-flight RunTurn goroutine and is not safe to write from another.
// Both connections register against the same session, and picoclaw broadcasts to
// every connection registered for it (pkg/channels/pico/pico.go
// broadcastToSession), so the running turn's stream sees the stop reply and
// finalizes on its own.
//
// req.SessionID must be the SAME value the turn was started with. picoclaw
// derives its internal session key from it (an opaque hash, built the same way
// for a command as for a message), so a session id that differs by one byte
// aborts nothing and reports success.
//
// A session with no turn running is NOT an error: the member clicked while the
// turn was finishing, which is an ordinary race. picoclaw answers "No active
// task to stop." and the caller has nothing to do about it. Only failing to
// reach the agent at all is reported as an error.
func (c *Client) Cancel(ctx context.Context, req turn.Request) error {
	u, err := url.Parse(req.Endpoint)
	if err != nil {
		return fmt.Errorf("invalid picoclaw ws url: %w", err)
	}
	q := u.Query()
	q.Set("session_id", req.SessionID)
	u.RawQuery = q.Encode()

	conn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		Subprotocols: []string{"token." + req.AuthToken},
	})
	if err != nil {
		return fmt.Errorf("failed to connect to picoclaw pico channel (check token/url): %w", err)
	}
	defer conn.CloseNow()

	send, err := json.Marshal(map[string]any{
		"type":       "message.send",
		"session_id": req.SessionID,
		"payload":    map[string]any{"content": stopCommand},
	})
	if err != nil {
		return err
	}
	if err := conn.Write(ctx, websocket.MessageText, send); err != nil {
		return fmt.Errorf("failed to send stop to picoclaw: %w", err)
	}

	// Read one frame before closing. The write above only reaches picoclaw's
	// socket buffer; closing immediately can drop the frame before the read loop
	// picks it up, which would make Stop succeed silently and do nothing. Any
	// frame proves the command was consumed, so the content is not inspected —
	// picoclaw distinguishes "stopped" from "nothing to stop" in prose only, and
	// the caller acts identically either way.
	if _, _, err := conn.Read(ctx); err != nil {
		return fmt.Errorf("picoclaw did not acknowledge the stop: %w", err)
	}
	return nil
}
