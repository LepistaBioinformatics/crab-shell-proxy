package pico

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
	"github.com/coder/websocket"
)

// sent is what the harness saw on the wire: the whole frame the proxy wrote.
type sent struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Payload   struct {
		Content string `json:"content"`
	} `json:"payload"`
}

// cancelServer stands in for picoclaw's pico channel. It records the first frame
// it receives and answers with `reply` -- or, when reply is nil, stays silent the
// way a wedged agent would.
func cancelServer(t *testing.T, reply *Frame) (endpoint string, received chan sent) {
	t.Helper()
	received = make(chan sent, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{"token." + testToken},
		})
		if err != nil {
			return
		}
		defer conn.CloseNow()

		ctx := r.Context()
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var got sent
		if err := json.Unmarshal(data, &got); err != nil {
			return
		}
		received <- got

		if reply != nil {
			if err := writeFrame(ctx, conn, *reply); err != nil {
				return
			}
		}
		// Hold the connection open, like picoclaw, so the client's read is decided
		// by its own budget rather than by a server hang-up.
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	return "ws://" + strings.TrimPrefix(srv.URL, "http://"), received
}

// TestCancelSendsStopForTheSession is the test that matters: picoclaw looks an
// abort up by the session it derives from the id it is given, so a stop carrying
// the wrong session id -- or the wrong command -- aborts nothing while reporting
// success. That failure is invisible from this side, which is why it is asserted
// on the wire rather than through the return value.
func TestCancelSendsStopForTheSession(t *testing.T) {
	endpoint, received := cancelServer(t, &Frame{
		Type:    "message.create",
		Payload: Payload{Content: "Task stopped. \"go\" was canceled."},
	})

	c := &Client{}
	if err := c.Cancel(context.Background(), turn.Request{
		Endpoint:  endpoint,
		AuthToken: testToken,
		SessionID: "s1",
	}); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}

	got := <-received
	if got.Type != "message.send" {
		t.Errorf("type = %q, want message.send (the only client frame the protocol accepts)", got.Type)
	}
	if got.SessionID != "s1" {
		t.Errorf("session_id = %q, want %q -- a stop on another session aborts nothing", got.SessionID, "s1")
	}
	if got.Payload.Content != "/stop" {
		t.Errorf("content = %q, want %q", got.Payload.Content, "/stop")
	}
}

// TestCancelReportsNoActiveTurnAsSuccess: the member clicked while the turn was
// finishing. picoclaw says so in prose, and there is nothing the caller can do
// differently -- reporting it as a failure would make a working Stop look broken.
func TestCancelReportsNoActiveTurnAsSuccess(t *testing.T) {
	endpoint, _ := cancelServer(t, &Frame{
		Type:    "message.create",
		Payload: Payload{Content: "No active task to stop."},
	})

	c := &Client{}
	if err := c.Cancel(context.Background(), turn.Request{
		Endpoint:  endpoint,
		AuthToken: testToken,
		SessionID: "s1",
	}); err != nil {
		t.Fatalf("Cancel treated an ordinary race as an error: %v", err)
	}
}

// TestCancelFailsWhenUnacknowledged: an agent that never answers must not read as
// a successful stop. Returning nil here would leave the webapp clearing its bands
// for a turn that is still running.
func TestCancelFailsWhenUnacknowledged(t *testing.T) {
	endpoint, _ := cancelServer(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	c := &Client{}
	err := c.Cancel(ctx, turn.Request{
		Endpoint:  endpoint,
		AuthToken: testToken,
		SessionID: "s1",
	})
	if err == nil {
		t.Fatal("Cancel returned nil for a stop the agent never acknowledged")
	}
	if !strings.Contains(err.Error(), "acknowledge") {
		t.Errorf("error = %v, want it to say the stop went unacknowledged", err)
	}
}

func TestCancelFailsOnUnreachableAgent(t *testing.T) {
	// A server shut down before the dial: the port is closed on this host, so the
	// connection is refused immediately rather than timing out.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	srv.Close()

	c := &Client{}
	err := c.Cancel(context.Background(), turn.Request{
		Endpoint:  endpoint,
		AuthToken: testToken,
		SessionID: "s1",
	})
	if err == nil {
		t.Fatal("Cancel returned nil against an unreachable agent")
	}
}
