package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/sgelias/crab-shell-proxy/internal/config"
)

// streamTurn serves a streaming (SSE) chat completion.
//
// Ordering matters (design D9 / advisor note): the 200 headers and the initial
// role chunk are flushed BEFORE EnsureRunning, so the client connection stays
// open through a cold start and mycelium's gatewayTimeout isn't tripped waiting
// on Docker. Once headers are sent the HTTP status can no longer change, so a
// cold-start or turn failure is surfaced by closing the stream cleanly (a
// [DONE] with no content) and logging — same as server.js.
func (s *Server) streamTurn(w http.ResponseWriter, r *http.Request, agent config.Agent, userHash, sessionKey, userContent, model, id string) {
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
	done := func() {
		writeChunk(map[string]any{}, "stop")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}

	// Open the stream immediately so the connection survives the cold start.
	writeChunk(map[string]any{"role": "assistant", "content": ""}, nil)

	tgt, err := s.Mgr.EnsureRunning(r.Context(), agent, userHash)
	if err != nil {
		s.logf("stream: ensure running failed: %v", err)
		done()
		return
	}

	_, err = s.Pico.RunTurn(r.Context(), tgt.WSEndpoint, tgt.PicoToken, sessionKey, userContent,
		func(delta string) { writeChunk(map[string]any{"content": delta}, nil) })
	s.Mgr.ArmIdle(agent, userHash)
	if err != nil {
		s.logf("stream: turn failed: %v", err)
	}
	done()
}
