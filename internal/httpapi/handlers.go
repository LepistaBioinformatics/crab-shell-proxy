// Package httpapi exposes the OpenAI-compatible HTTP surface, resolves the
// (agent, user) for each request, ensures the backing per-user picoclaw
// container is running, and runs the turn. Behaviour mirrors
// picoclaw-openai-proxy/server.js.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/sgelias/crab-shell-proxy/internal/config"
	"github.com/sgelias/crab-shell-proxy/internal/docker"
	"github.com/sgelias/crab-shell-proxy/internal/history"
	"github.com/sgelias/crab-shell-proxy/internal/identity"
)

// Orchestrator is the container-lifecycle surface the handlers need
// (satisfied by *docker.Manager).
type Orchestrator interface {
	EnsureRunning(ctx context.Context, agent config.Agent, userKey, ownerEmail string) (docker.Target, error)
	ArmIdle(agent config.Agent, userKey string)
}

// Turner runs one conversational turn (satisfied by *pico.Client).
type Turner interface {
	RunTurn(ctx context.Context, wsURL, picoToken, sessionID, userContent string, onDelta func(string)) (string, error)
}

// Server holds the handler dependencies.
type Server struct {
	Cfg      *config.Config
	Resolver identity.Resolver
	Mgr      Orchestrator
	Pico     Turner
	Logf     func(string, ...any)
}

// Handler returns the routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /v1/sessions/history", s.handleSessionsHistory)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func writeJSON(w http.ResponseWriter, status int, obj any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(obj)
}

func errBody(msg string) map[string]any {
	return map[string]any{"error": map[string]any{"message": msg}}
}

// resolveAgent reads the mycelium-injected service-name header, matches it to a
// configured agent, and verifies the injected bearer token — so a request that
// bypassed mycelium (no/incorrect token) is rejected.
func (s *Server) resolveAgent(r *http.Request) (config.Agent, int, string) {
	serviceName := r.Header.Get(identity.ServiceNameHeader)
	agent, ok := s.Cfg.AgentByServiceName(serviceName)
	if !ok {
		return config.Agent{}, http.StatusNotFound,
			"unknown or missing "+identity.ServiceNameHeader+" — reach this proxy through mycelium"
	}
	if r.Header.Get("Authorization") != "Bearer "+agent.ResolvedToken {
		return config.Agent{}, http.StatusUnauthorized, "invalid api key"
	}
	return agent, 0, ""
}

// chatRequest is the OpenAI-compatible request subset we read.
type chatRequest struct {
	Messages  []message `json:"messages"`
	Model     string    `json:"model"`
	Stream    bool      `json:"stream"`
	SessionID string    `json:"session_id"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	agent, status, msg := s.resolveAgent(r)
	if status != 0 {
		writeJSON(w, status, errBody(msg))
		return
	}
	ident, ok := s.Resolver.Resolve(r.Header.Get(identity.ProfileHeader))
	if !ok {
		writeJSON(w, http.StatusUnauthorized,
			errBody("missing or invalid "+identity.ProfileHeader+" header"))
		return
	}

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	if len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, errBody(`"messages" is required`))
		return
	}
	sessionKey := identity.SessionKey(ident.AccID, req.SessionID)
	if sessionKey == "" {
		writeJSON(w, http.StatusBadRequest,
			errBody(`"session_id" is required to isolate conversations for this account`))
		return
	}
	userKey := identity.SanitizeID(ident.AccID)
	userContent := lastUserContent(req.Messages)
	model := req.Model
	if model == "" {
		model = "picoclaw"
	}
	id := "chatcmpl-" + randomHex(12)

	if req.Stream {
		s.streamTurn(w, r, agent, userKey, ident.Email, sessionKey, userContent, model, id)
		return
	}

	tgt, err := s.Mgr.EnsureRunning(r.Context(), agent, userKey, ident.Email)
	if err != nil {
		s.logf("ensure running failed: %v", err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	content, err := s.Pico.RunTurn(r.Context(), tgt.WSEndpoint, tgt.PicoToken, sessionKey, userContent, nil)
	s.Mgr.ArmIdle(agent, userKey)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, completionResponse(id, model, content))
}

// randomHex returns n random bytes as a hex string (2n chars).
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, status, msg := s.resolveAgent(r); status != 0 {
		writeJSON(w, status, errBody(msg))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": "picoclaw", "object": "model", "created": 0, "owned_by": "picoclaw"},
		},
	})
}

func (s *Server) handleSessionsHistory(w http.ResponseWriter, r *http.Request) {
	agent, status, msg := s.resolveAgent(r)
	if status != 0 {
		writeJSON(w, status, errBody(msg))
		return
	}
	ident, ok := s.Resolver.Resolve(r.Header.Get(identity.ProfileHeader))
	if !ok {
		writeJSON(w, http.StatusUnauthorized,
			errBody("missing or invalid "+identity.ProfileHeader+" header"))
		return
	}
	sessionKey := identity.SessionKey(ident.AccID, r.URL.Query().Get("session_id"))
	if sessionKey == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"session_id" query parameter is required`))
		return
	}
	sessionsDir := s.Cfg.SessionsDir(agent.Key, identity.SanitizeID(ident.AccID))
	messages, err := history.Read(sessionsDir, sessionKey)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": messages})
}

// handleHealthz is unauthenticated (mycelium's health dispatcher issues a plain
// GET) and reports this proxy's own liveness. It deliberately does not touch any
// per-user container.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"proxy": "ok", "time": time.Now().UTC().Format(time.RFC3339)})
}

func completionResponse(id, model, content string) map[string]any {
	return map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	}
}

// lastUserContent extracts the text of the last user message (or the last
// message if no user turn), mirroring server.js.
func lastUserContent(messages []message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return extractText(messages[i].Content)
		}
	}
	return extractText(messages[len(messages)-1].Content)
}

// extractText handles both a plain string content and the OpenAI array-of-parts
// content shape.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return ""
}
