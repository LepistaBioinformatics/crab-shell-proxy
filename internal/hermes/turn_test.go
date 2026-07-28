package hermes

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
)

func TestRunTurnStreamsAndAccumulates(t *testing.T) {
	var gotAuth, gotSID, gotSKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSID = r.Header.Get("X-Hermes-Session-Id")
		gotSKey = r.Header.Get("X-Hermes-Session-Key")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		writeData := func(s string) {
			fmt.Fprintf(w, "data: %s\n\n", s)
			if fl != nil {
				fl.Flush()
			}
		}
		writeData(`{"choices":[{"delta":{"role":"assistant"}}]}`) // role-only: ignored
		writeData(`{"type":"hermes.tool.progress","tool":"x"}`)   // non-chunk: ignored
		writeData(`{"choices":[{"delta":{"content":"Hel"}}]}`)
		writeData(`{"choices":[{"delta":{"content":"lo"}}]}`)
		writeData("[DONE]")
	}))
	defer srv.Close()

	var deltas []string
	c := &Client{TurnTimeout: 5 * time.Second}
	got, err := c.RunTurn(context.Background(), turn.Request{
		Endpoint:   srv.URL,
		AuthToken:  "bearer-x",
		SessionID:  "sid-1",
		SessionKey: "user:role",
		Content:    "hi",
	}, turn.Sink{Content: func(d string) { deltas = append(deltas, d) }})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if got != "Hello" {
		t.Errorf("content = %q, want Hello", got)
	}
	if strings.Join(deltas, "|") != "Hel|lo" {
		t.Errorf("deltas = %v, want [Hel lo]", deltas)
	}
	if gotAuth != "Bearer bearer-x" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotSID != "sid-1" || gotSKey != "user:role" {
		t.Errorf("session headers: id=%q key=%q", gotSID, gotSKey)
	}
}

func TestRunTurnErrorsOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &Client{}
	if _, err := c.RunTurn(context.Background(), turn.Request{Endpoint: srv.URL, Content: "hi"}, turn.Sink{}); err == nil {
		t.Fatal("expected error on 401")
	}
}
