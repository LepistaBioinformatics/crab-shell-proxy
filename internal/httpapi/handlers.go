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
	"errors"
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
	// WriteSecret validates and persists one secret into the caller's
	// per-(user, agent) store, merging native secrets into the current workspace.
	WriteSecret(agent config.Agent, key docker.WorkspaceKey, format, name, value string) error
	// ListSecrets returns the set secret names per format (never values).
	ListSecrets(key docker.WorkspaceKey) (docker.SecretNames, error)
	// DeleteSecret removes one secret from the caller's store.
	DeleteSecret(key docker.WorkspaceKey, format, name string) error
	// RestartWorkspace restarts the caller's container so an injected secret
	// takes effect immediately.
	RestartWorkspace(key docker.WorkspaceKey) error
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
	mux.HandleFunc("POST /v1/secrets", s.handleSecretsPost)
	mux.HandleFunc("GET /v1/secrets", s.handleSecretsList)
	mux.HandleFunc("DELETE /v1/secrets", s.handleSecretsDelete)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return s.withLogging(mux)
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// statusRecorder captures the response status for access logging while
// preserving http.Flusher (the SSE streaming path type-asserts the writer to
// flush each chunk — a wrapper that drops Flusher would break streaming).
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.status = http.StatusOK
		r.wrote = true
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// withLogging emits one access-log line per request (method, path, status,
// duration, mycelium service-name). /healthz is skipped — the compose/gateway
// health pollers hit it every few seconds and would drown the log.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.logf("%s %s -> %d (%s) svc=%q", r.Method, r.URL.Path, rec.status,
			time.Since(start).Round(time.Millisecond), r.Header.Get(identity.ServiceNameHeader))
	})
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
		s.logf("chat: authz denied svc=%s tenant=%s subs=%s user=%s: %v",
			agent.Key, tenantID, subsAccID, ident.AccID, err)
		writeJSON(w, http.StatusForbidden,
			errBody("not licensed to use this subscription for this agent"))
		return
	}
	// Ensure the subscription root exists, creating it on demand. The filter
	// chain above already proved the caller is licensed for this
	// tenant+subscription+agent, so provisioning here is safe — a subscription
	// that predates the webhook (or was never POSTed to /v1/accounts) still
	// works. The /v1/accounts webhook is now an optional pre-warm, not a
	// precondition. Idempotent (no-op when the root already exists).
	if _, err := s.Mgr.ScaffoldSubscription(tenantID.String(), subsAccID.String()); err != nil {
		s.logf("chat: scaffold subscription failed tenant=%s subs=%s: %v", tenantID, subsAccID, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}

	key := docker.WorkspaceKey{
		TenantID:  tenantID.String(),
		SubsAccID: subsAccID.String(),
		Role:      agent.Key,
		UserAccID: ident.AccID,
	}
	s.logf("chat: authorized svc=%s tenant=%s subs=%s user=%s stream=%t",
		agent.Key, tenantID, subsAccID, ident.AccID, req.Stream)
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
		s.logf("chat: turn failed svc=%s user=%s: %v", agent.Key, ident.AccID, err)
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
				"accName":    res.AccName,
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

// secretRequest is the POST /v1/secrets body: the targeted subscription, the
// sink format, the name/slot, and the (write-only) value.
type secretRequest struct {
	TenantID  string `json:"tenant_id"`
	SubsAccID string `json:"subs_acc_id"`
	Format    string `json:"format"`
	Name      string `json:"name"`
	Value     string `json:"value"`
}

var knownSecretFormats = map[string]bool{
	docker.FormatDotenv: true, docker.FormatJSON: true,
	docker.FormatFile: true, docker.FormatNative: true,
}

// authorizeSecret runs the same authorization as chat over the given
// tenant/subscription (account-switching guard + the write-access chain) and, on
// success, returns the caller's WorkspaceKey. It writes the error response and
// returns ok=false on any failure.
func (s *Server) authorizeSecret(w http.ResponseWriter, agent config.Agent, ident identity.Identity, tenantID, subsAccID uuid.UUID) (docker.WorkspaceKey, bool) {
	if ident.Profile.AccID == subsAccID {
		writeJSON(w, http.StatusForbidden,
			errBody("profile account id must differ from subs_acc_id (act as an individual member)"))
		return docker.WorkspaceKey{}, false
	}
	if _, err := ident.Profile.
		WithWriteAccess().
		OnTenant(tenantID).
		WithRoles([]string{agent.Key}).
		OnAccount(subsAccID).
		GetRelatedAccountOrError(); err != nil {
		s.logf("secrets: authz denied svc=%s tenant=%s subs=%s user=%s: %v",
			agent.Key, tenantID, subsAccID, ident.AccID, err)
		writeJSON(w, http.StatusForbidden,
			errBody("not licensed to use this subscription for this agent"))
		return docker.WorkspaceKey{}, false
	}
	return docker.WorkspaceKey{
		TenantID:  tenantID.String(),
		SubsAccID: subsAccID.String(),
		Role:      agent.Key,
		UserAccID: ident.AccID,
	}, true
}

// resolveSecretCaller resolves the agent + profile shared by all /v1/secrets
// handlers, writing the error response and returning ok=false on failure.
func (s *Server) resolveSecretCaller(w http.ResponseWriter, r *http.Request) (config.Agent, identity.Identity, bool) {
	agent, status, msg := s.resolveAgent(r)
	if status != 0 {
		writeJSON(w, status, errBody(msg))
		return config.Agent{}, identity.Identity{}, false
	}
	ident, ok := s.Resolver.Resolve(r.Header.Get(identity.ProfileHeader))
	if !ok {
		writeJSON(w, http.StatusUnauthorized,
			errBody("missing or invalid "+identity.ProfileHeader+" header"))
		return config.Agent{}, identity.Identity{}, false
	}
	return agent, ident, true
}

// handleSecretsPost injects/updates one secret for the caller's (user, agent)
// pair and restarts the container so picoclaw re-reads it (AC-02, CTX-AC-04).
func (s *Server) handleSecretsPost(w http.ResponseWriter, r *http.Request) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	var req secretRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	if !knownSecretFormats[req.Format] {
		writeJSON(w, http.StatusBadRequest,
			errBody(`"format" must be one of dotenv, json, file, native`))
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
	key, ok := s.authorizeSecret(w, agent, ident, tenantID, subsAccID)
	if !ok {
		return
	}
	if err := s.Mgr.WriteSecret(agent, key, req.Format, req.Name, req.Value); err != nil {
		if errors.Is(err, docker.ErrInvalidSecretName) || errors.Is(err, docker.ErrUnknownNativeSlot) {
			writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
			return
		}
		s.logf("secrets: write failed svc=%s user=%s format=%s: %v", agent.Key, ident.AccID, req.Format, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	if err := s.Mgr.RestartWorkspace(key); err != nil {
		s.logf("secrets: restart failed svc=%s user=%s: %v", agent.Key, ident.AccID, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	s.logf("secrets: injected svc=%s tenant=%s subs=%s user=%s format=%s",
		agent.Key, tenantID, subsAccID, ident.AccID, req.Format)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "format": req.Format, "name": req.Name})
}

// handleSecretsList returns the set secret names grouped by format — never a
// value (write-only-over-API store, AC-08).
func (s *Server) handleSecretsList(w http.ResponseWriter, r *http.Request) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
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
	key, ok := s.authorizeSecret(w, agent, ident, tenantID, subsAccID)
	if !ok {
		return
	}
	names, err := s.Mgr.ListSecrets(key)
	if err != nil {
		s.logf("secrets: list failed svc=%s user=%s: %v", agent.Key, ident.AccID, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": names})
}

// handleSecretsDelete removes one secret and restarts the container (AC-08).
func (s *Server) handleSecretsDelete(w http.ResponseWriter, r *http.Request) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	format := r.URL.Query().Get("format")
	name := r.URL.Query().Get("name")
	if !knownSecretFormats[format] {
		writeJSON(w, http.StatusBadRequest,
			errBody(`"format" query parameter must be one of dotenv, json, file, native`))
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
	key, ok := s.authorizeSecret(w, agent, ident, tenantID, subsAccID)
	if !ok {
		return
	}
	if err := s.Mgr.DeleteSecret(key, format, name); err != nil {
		if errors.Is(err, docker.ErrInvalidSecretName) || errors.Is(err, docker.ErrUnknownNativeSlot) {
			writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
			return
		}
		s.logf("secrets: delete failed svc=%s user=%s format=%s: %v", agent.Key, ident.AccID, format, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	if err := s.Mgr.RestartWorkspace(key); err != nil {
		s.logf("secrets: restart after delete failed svc=%s user=%s: %v", agent.Key, ident.AccID, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "format": format, "name": name})
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
