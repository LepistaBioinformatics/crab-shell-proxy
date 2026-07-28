package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/history"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
)

// turnTimeout bounds a picoclaw turn once it is decoupled from the client
// request. It must be generous enough for a long tool-using turn, but stops a
// stuck turn from running forever after the client has gone.
const turnTimeout = 10 * time.Minute

// streamTurn serves a streaming (SSE) chat completion.
//
// Ordering matters (design D9 / advisor note): the 200 headers and the initial
// role chunk are flushed BEFORE EnsureRunning, so the client connection stays
// open through a cold start and mycelium's gatewayTimeout isn't tripped waiting
// on Docker. Once headers are sent the HTTP status can no longer change, so a
// cold-start or turn failure is surfaced by closing the stream cleanly (a
// [DONE] with no content) and logging — same as server.js.
func (s *Server) streamTurn(w http.ResponseWriter, r *http.Request, agent config.Agent, key docker.WorkspaceKey, ownerEmail, sessionKey, userContent, model, id string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errBody("streaming unsupported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	created := time.Now().Unix()
	writeChunk := func(delta map[string]any, finish any) {
		payload := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// Progress rides as an extra top-level field on an otherwise-normal chunk
	// with an EMPTY delta. A client that knows nothing about it -- including a
	// generic OpenAI SDK -- reads choices[0].delta.content, finds nothing, and
	// skips the frame: the extension is ignored, never a parse error. A named
	// SSE event (`event: progress`) would instead be dropped wholesale by
	// data:-only parsers, so this shape is the compatible one.
	writeProgress := func(p turn.Progress) {
		payload := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": nil}},
			"x_crab_progress": map[string]any{
				"kind":  p.Kind,
				"text":  p.Text,
				"tool":  p.Tool,
				"state": p.State,
			},
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	done := func() {
		writeChunk(map[string]any{}, "stop")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}

	// Open the stream immediately so the connection survives the cold start.
	writeChunk(map[string]any{"role": "assistant", "content": ""}, nil)

	// The turn must complete even if the client disconnects (page reload /
	// navigation). Tying it to r.Context() would cancel the picoclaw WebSocket
	// mid-turn on disconnect, and picoclaw would persist a truncated transcript
	// (the "initial messages disappear after reload" bug). So run it on a
	// background context with its own bound; we only stop *writing* to the client
	// once it goes away, while the turn keeps draining to completion.
	turnCtx, cancel := context.WithTimeout(context.Background(), turnTimeout)
	defer cancel()
	clientCtx := r.Context()

	tgt, err := s.Mgr.EnsureRunning(turnCtx, agent, key, ownerEmail)
	if err != nil {
		s.logf("stream: ensure running failed: %v", err)
		if clientCtx.Err() == nil {
			done()
		}
		return
	}

	_, err = s.turnerFor(tgt.Harness).RunTurn(turnCtx, turn.Request{
		Endpoint:   tgt.Endpoint,
		AuthToken:  tgt.AuthToken,
		SessionID:  sessionKey,
		SessionKey: key.UserAccID + ":" + key.Role,
		Model:      turnModelFor(agent, model),
		Content:    userContent,
	}, turn.Sink{
		Content: func(delta string) {
			if clientCtx.Err() != nil {
				return // client gone — keep draining so the agent finishes its write
			}
			writeChunk(map[string]any{"content": delta}, nil)
		},
		Progress: func(p turn.Progress) {
			if clientCtx.Err() != nil {
				return
			}
			writeProgress(p)
		},
	})
	s.Mgr.ArmIdle(agent, key)
	if err != nil {
		s.logf("stream: turn failed: %v", err)
	}
	// Fold the just-written turn into the durable transcript now — while the live
	// file still holds it — so a later restart that rewrites the live file can't
	// erase the history.
	sessionsDir := config.SessionsDir(s.Cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if syncErr := history.SyncDurable(sessionsDir, sessionKey); syncErr != nil {
		s.logf("stream: sync durable history failed: %v", syncErr)
	}
	if clientCtx.Err() == nil {
		done()
	}
}
