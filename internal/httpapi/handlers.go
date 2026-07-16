// Package httpapi exposes the OpenAI-compatible HTTP surface, resolves the
// (agent, user) for each request, ensures the backing per-user picoclaw
// container is running, and runs the turn. Behaviour mirrors
// picoclaw-openai-proxy/server.js.
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sgelias/crab-shell-proxy/internal/config"
	"github.com/sgelias/crab-shell-proxy/internal/docker"
	"github.com/sgelias/crab-shell-proxy/internal/history"
	"github.com/sgelias/crab-shell-proxy/internal/identity"
)

// Orchestrator is the container-lifecycle + workspace-scaffold surface the
// handlers need (satisfied by *docker.Manager).
type Orchestrator interface {
	EnsureRunning(ctx context.Context, agent config.Agent, key docker.WorkspaceKey, ownerEmail string) (docker.Target, error)
	ArmIdle(agent config.Agent, key docker.WorkspaceKey)
	// ScaffoldSubscription idempotently creates the subscription root and
	// reports whether it was created now (true) or already existed (false).
	ScaffoldSubscription(tenantID, subsAccID string) (bool, error)
	// SubscriptionScaffolded reports whether the subscription root exists.
	SubscriptionScaffolded(tenantID, subsAccID string) bool
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
	mux.HandleFunc("POST /v1/accounts", s.handleAccounts)
	mux.HandleFunc("GET /v1/subscriptions", s.handleSubscriptions)
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
			"unknown or missing " + identity.ServiceNameHeader + " — reach this proxy through mycelium"
	}
	if r.Header.Get("Authorization") != "Bearer "+agent.ResolvedToken {
		return config.Agent{}, http.StatusUnauthorized, "invalid api key"
	}
	return agent, 0, ""
}

// chatRequest is the OpenAI-compatible request subset we read, plus the
// tenant/subscription the caller targets (required — the workspace is scoped to
// a subscription, and access is verified against the caller's profile).
type chatRequest struct {
	Messages  []message `json:"messages"`
	Model     string    `json:"model"`
	Stream    bool      `json:"stream"`
	SessionID string    `json:"session_id"`
	TenantID  string    `json:"tenant_id"`
	SubsAccID string    `json:"subs_acc_id"`
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
	tenantID, err := uuid.Parse(req.TenantID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" is required and must be a UUID`))
		return
	}
	subsAccID, err := uuid.Parse(req.SubsAccID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" is required and must be a UUID`))
		return
	}
	// Account-switching guard: a caller acting AS the subscription (not as an
	// individual member) has no per-user identity to isolate on.
	if ident.Profile.AccID == subsAccID {
		writeJSON(w, http.StatusForbidden,
			errBody("profile account id must differ from subs_acc_id (act as an individual member)"))
		return
	}
	// The profile filtering chain is the only authorization gate: the caller
	// must hold write access on this tenant, for a role named exactly like the
	// resolved agent, over this subscription account. Staff/manager short-circuit
	// to allow (intended elevated access). Note: `verified` is intentionally NOT
	// enforced (user decision) — unverified/pending-invite grants are accepted.
	if _, err := ident.Profile.
		WithWriteAccess().
		OnTenant(tenantID).
		WithRoles([]string{agent.Key}).
		OnAccount(subsAccID).
		GetRelatedAccountOrError(); err != nil {
		writeJSON(w, http.StatusForbidden,
			errBody("not licensed to use this subscription for this agent"))
		return
	}
	// Chat never creates the subscription root — only POST /v1/accounts does.
	if !s.Mgr.SubscriptionScaffolded(tenantID.String(), subsAccID.String()) {
		writeJSON(w, http.StatusConflict,
			errBody("subscription workspace has not been scaffolded yet"))
		return
	}

	key := docker.WorkspaceKey{
		TenantID:  tenantID.String(),
		SubsAccID: subsAccID.String(),
		Role:      agent.Key,
		UserAccID: ident.AccID,
	}
	userContent := lastUserContent(req.Messages)
	model := req.Model
	if model == "" {
		model = "picoclaw"
	}
	id := "chatcmpl-" + randomHex(12)

	if req.Stream {
		s.streamTurn(w, r, agent, key, ident.Email, sessionKey, userContent, model, id)
		return
	}

	tgt, err := s.Mgr.EnsureRunning(r.Context(), agent, key, ident.Email)
	if err != nil {
		s.logf("ensure running failed: %v", err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	content, err := s.Pico.RunTurn(r.Context(), tgt.WSEndpoint, tgt.PicoToken, sessionKey, userContent, nil)
	s.Mgr.ArmIdle(agent, key)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, completionResponse(id, model, content))
}

// accountWebhook is the two-field subset of the mycelium Account object the
// subscriptionAccount.created webhook posts: the account id and, nested in the
// externally-tagged accountType enum, the subscription's tenant id.
type accountWebhook struct {
	ID          string `json:"id"`
	AccountType struct {
		Subscription *struct {
			TenantID string `json:"tenantId"`
		} `json:"subscription"`
	} `json:"accountType"`
}

// handleAccounts receives the mycelium subscriptionAccount.created webhook and
// scaffolds the subscription root. It is authenticated by the shared webhook
// secret (not the per-agent service-name + bearer token) and is agent-agnostic.
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	secret := s.Cfg.ResolvedWebhookSecret
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if secret == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(secret)) != 1 {
		writeJSON(w, http.StatusUnauthorized, errBody("invalid webhook secret"))
		return
	}

	var acct accountWebhook
	if err := json.NewDecoder(r.Body).Decode(&acct); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	subsAccID, err := uuid.Parse(acct.ID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("account id is required and must be a UUID"))
		return
	}
	if acct.AccountType.Subscription == nil {
		writeJSON(w, http.StatusBadRequest, errBody("account is not a subscription account"))
		return
	}
	tenantID, err := uuid.Parse(acct.AccountType.Subscription.TenantID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("subscription tenantId is required and must be a UUID"))
		return
	}

	created, err := s.Mgr.ScaffoldSubscription(tenantID.String(), subsAccID.String())
	if err != nil {
		s.logf("scaffold subscription failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	status := http.StatusOK
	statusStr := "exists"
	if created {
		status = http.StatusCreated
		statusStr = "created"
	}
	writeJSON(w, status, map[string]any{
		"tenantId":  tenantID.String(),
		"subsAccId": subsAccID.String(),
		"status":    statusStr,
	})
}

// handleSubscriptions lists, from the caller's licensed_resources (the source
// of truth — leaf dirs are lazy), the (tenant, subscription, agent) tuples the
// caller may use. Agent-agnostic; no on-disk scan drives the list.
func (s *Server) handleSubscriptions(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.Resolver.Resolve(r.Header.Get(identity.ProfileHeader))
	if !ok {
		writeJSON(w, http.StatusUnauthorized,
			errBody("missing or invalid "+identity.ProfileHeader+" header"))
		return
	}
	subs := []map[string]any{}
	if lr := ident.Profile.LicensedResources; lr != nil {
		for _, res := range lr.ToLicensesVector() {
			subs = append(subs, map[string]any{
				"tenantId":   res.TenantID.String(),
				"subsAccId":  res.AccID.String(),
				"role":       res.Role,
				"perm":       res.Perm.String(),
				"verified":   res.Verified,
				"scaffolded": s.Mgr.SubscriptionScaffolded(res.TenantID.String(), res.AccID.String()),
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": subs})
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
	tenantID, err := uuid.Parse(r.URL.Query().Get("tenant_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" query parameter is required and must be a UUID`))
		return
	}
	subsAccID, err := uuid.Parse(r.URL.Query().Get("subs_acc_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" query parameter is required and must be a UUID`))
		return
	}
	// Account-switching guard (full authz filtering is deferred — CTX-TSW-09).
	if ident.Profile.AccID == subsAccID {
		writeJSON(w, http.StatusForbidden,
			errBody("profile account id must differ from subs_acc_id (act as an individual member)"))
		return
	}
	sessionsDir := config.SessionsDir(s.Cfg.ContainerDataRoot, tenantID.String(), subsAccID.String(), agent.Key, ident.AccID)
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
