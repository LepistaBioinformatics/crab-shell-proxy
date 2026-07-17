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
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/history"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
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
	// StoreMedia writes an uploaded file into the caller's workspace uploads
	// dir and returns its workspace-relative path.
	StoreMedia(key docker.WorkspaceKey, rawName string, r io.Reader) (docker.StoredMedia, error)
	// ListMedia returns the files in the caller's workspace uploads dir.
	ListMedia(key docker.WorkspaceKey) ([]docker.StoredMedia, error)
	// DeleteMedia removes one uploaded file (by its stored filename).
	DeleteMedia(key docker.WorkspaceKey, storedName string) error
	// OpenMedia opens one uploaded file for download (reader + display name).
	OpenMedia(key docker.WorkspaceKey, storedName string) (io.ReadCloser, string, error)
	// ReadMemory returns the caller's workspace MEMORY_CUSTOM.md (empty if unset).
	ReadMemory(key docker.WorkspaceKey) (string, error)
	// WriteMemory replaces the caller's workspace MEMORY_CUSTOM.md.
	WriteMemory(key docker.WorkspaceKey, content string) error

	// --- admin-shared-content (authority-over-target; gated in internal/authz) ---

	// ListSharedFiles returns the metadata (never bytes) of a scope's shared files.
	ListSharedFiles(scope docker.Scope) ([]docker.FileMeta, error)
	// WriteSharedFile stores an uploaded shared file at a scope (latest-write-wins).
	WriteSharedFile(scope docker.Scope, rawName string, r io.Reader) (docker.StoredMedia, error)
	// ReadSharedFile opens a scope's shared file for download plus its metadata.
	ReadSharedFile(scope docker.Scope, name string) (io.ReadCloser, docker.FileMeta, error)
	// DeleteSharedFile removes one shared file from a scope.
	DeleteSharedFile(scope docker.Scope, name string) error
	// WriteSharedSecret upserts one shared secret at a scope (dotenv/json only).
	WriteSharedSecret(scope docker.Scope, format, name, value string) error
	// ListSharedSecrets returns a scope's shared-secret names per format (never values).
	ListSharedSecrets(scope docker.Scope) (docker.SecretNames, error)
	// DeleteSharedSecret removes one shared secret from a scope.
	DeleteSharedSecret(scope docker.Scope, format, name string) error
	// ListTenants returns the tenant ids present on disk (scope discovery, Instance).
	ListTenants() ([]string, error)
	// ListTenantSubscriptions returns the subscription ids under a tenant on disk.
	ListTenantSubscriptions(tenantID string) ([]string, error)
	// ListSubscriptionUsers enumerates the end users under a subscription.
	ListSubscriptionUsers(tenantID, subsAccID string) ([]docker.UserRef, error)
	// ListUserFiles returns a user's private-file metadata only (no bytes — FR-7).
	ListUserFiles(key docker.WorkspaceKey) ([]docker.FileMeta, error)
	// DeleteUserFile removes one of a user's private files (never reads it — FR-7).
	DeleteUserFile(key docker.WorkspaceKey, name string) error
	// RestartScope best-effort recreates running containers under a scope (NFR-4).
	RestartScope(scope docker.Scope) error
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
	mux.HandleFunc("GET /v1/sessions/resolve", s.handleSessionsResolve)
	mux.HandleFunc("POST /v1/secrets", s.handleSecretsPost)
	mux.HandleFunc("GET /v1/secrets", s.handleSecretsList)
	mux.HandleFunc("DELETE /v1/secrets", s.handleSecretsDelete)
	mux.HandleFunc("POST /v1/media", s.handleMediaPost)
	mux.HandleFunc("GET /v1/media", s.handleMediaList)
	mux.HandleFunc("DELETE /v1/media", s.handleMediaDelete)
	mux.HandleFunc("GET /v1/memory", s.handleMemoryGet)
	mux.HandleFunc("PUT /v1/memory", s.handleMemoryPut)
	// admin-shared-content: authority-over-target ops, gated in-proxy via
	// internal/authz. There is deliberately NO users/files/content route and no
	// user-file write route (FR-7 privacy invariant).
	mux.HandleFunc("GET /v1/admin/scopes", s.handleAdminScopes)
	mux.HandleFunc("GET /v1/admin/shared", s.handleAdminSharedList)
	mux.HandleFunc("POST /v1/admin/shared", s.handleAdminSharedPost)
	mux.HandleFunc("GET /v1/admin/shared/content", s.handleAdminSharedContent)
	mux.HandleFunc("DELETE /v1/admin/shared", s.handleAdminSharedDelete)
	mux.HandleFunc("POST /v1/admin/shared-secrets", s.handleAdminSharedSecretsPost)
	mux.HandleFunc("GET /v1/admin/shared-secrets", s.handleAdminSharedSecretsList)
	mux.HandleFunc("DELETE /v1/admin/shared-secrets", s.handleAdminSharedSecretsDelete)
	mux.HandleFunc("GET /v1/admin/users", s.handleAdminUsersList)
	mux.HandleFunc("GET /v1/admin/users/files", s.handleAdminUserFilesList)
	mux.HandleFunc("DELETE /v1/admin/users/files", s.handleAdminUserFilesDelete)
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

	// Decoupled from r.Context() so a client disconnect can't cut the picoclaw
	// turn mid-write and leave a truncated transcript (see streamTurn).
	turnCtx, cancel := context.WithTimeout(context.Background(), turnTimeout)
	defer cancel()
	tgt, err := s.Mgr.EnsureRunning(turnCtx, agent, key, ident.Email)
	if err != nil {
		s.logf("ensure running failed: %v", err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	content, err := s.Pico.RunTurn(turnCtx, tgt.WSEndpoint, tgt.PicoToken, sessionKey, userContent, nil)
	s.Mgr.ArmIdle(agent, key)
	if err != nil {
		s.logf("chat: turn failed svc=%s user=%s: %v", agent.Key, ident.AccID, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	sessionsDir := config.SessionsDir(s.Cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if syncErr := history.SyncDurable(sessionsDir, sessionKey); syncErr != nil {
		s.logf("chat: sync durable history failed: %v", syncErr)
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
	// Fold in any just-completed turn before reading, then serve the durable
	// transcript (survives picoclaw's live-file rewrites across restarts).
	if syncErr := history.SyncDurable(sessionsDir, sessionKey); syncErr != nil {
		s.logf("history: sync durable failed: %v", syncErr)
	}
	messages, err := history.Read(sessionsDir, sessionKey)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": messages})
}

// handleSessionsResolve resolves the session identifiers behind a conversation:
// the deterministic sessionKey (known immediately) and picoclaw's on-disk file
// basename (sessionFile, "" until picoclaw persists the transcript). Same
// resolveAgent + profile + account-switching guard + params as
// handleSessionsHistory; read-only metadata, never a transcript.
func (s *Server) handleSessionsResolve(w http.ResponseWriter, r *http.Request) {
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
	sessionFile := history.FindSessionFile(sessionsDir, sessionKey)
	writeJSON(w, http.StatusOK, map[string]any{"sessionKey": sessionKey, "sessionFile": sessionFile})
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

// handleMediaPost stores an uploaded file in the caller's agent-readable
// workspace (media-upload). Authorized with the same chat write chain as
// secrets. The whole request body is capped by MaxBytesReader so an oversized
// upload is rejected (413) without buffering it all; the extension allowlist
// guards the type (400); the manager sanitizes the name and keeps the file
// inside the workspace. The turn later references the returned `path`.
func (s *Server) handleMediaPost(w http.ResponseWriter, r *http.Request) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}

	// Cap the body; ParseMultipartForm fails once MaxBytesReader trips -> 413.
	r.Body = http.MaxBytesReader(w, r.Body, s.Cfg.MediaMaxBytes+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge,
			errBody(fmt.Sprintf("file exceeds the %d-byte limit", s.Cfg.MediaMaxBytes)))
		return
	}

	tenantID, err := uuid.Parse(r.FormValue("tenant_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" is required and must be a UUID`))
		return
	}
	subsAccID, err := uuid.Parse(r.FormValue("subs_acc_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" is required and must be a UUID`))
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("a `file` part is required"))
		return
	}
	defer file.Close()

	if !s.mediaExtAllowed(header.Filename) {
		writeJSON(w, http.StatusBadRequest,
			errBody("file type not allowed (allowed: "+strings.Join(s.Cfg.MediaAllowedExts, ", ")+")"))
		return
	}

	key, ok := s.authorizeSecret(w, agent, ident, tenantID, subsAccID)
	if !ok {
		return
	}

	stored, err := s.Mgr.StoreMedia(key, header.Filename, file)
	if err != nil {
		if errors.Is(err, docker.ErrMediaName) {
			writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
			return
		}
		s.logf("media: store failed svc=%s user=%s: %v", agent.Key, ident.AccID, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	s.logf("media: stored svc=%s tenant=%s subs=%s user=%s path=%s size=%d",
		agent.Key, tenantID, subsAccID, ident.AccID, stored.Path, stored.Size)
	writeJSON(w, http.StatusOK, stored)
}

// handleMediaList returns the files in the caller's workspace uploads dir
// (names + sizes, never contents), authorized like the upload.
func (s *Server) handleMediaList(w http.ResponseWriter, r *http.Request) {
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
	// GET /v1/media?path=uploads/<file> downloads that file; without `path` it
	// lists the uploads dir.
	if path := r.URL.Query().Get("path"); path != "" {
		s.serveMediaFile(w, key, agent, ident, path)
		return
	}
	files, err := s.Mgr.ListMedia(key)
	if err != nil {
		s.logf("media: list failed svc=%s user=%s: %v", agent.Key, ident.AccID, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// serveMediaFile streams one uploaded file back as a download attachment.
func (s *Server) serveMediaFile(w http.ResponseWriter, key docker.WorkspaceKey, agent config.Agent, ident identity.Identity, path string) {
	rc, display, err := s.Mgr.OpenMedia(key, filepath.Base(path))
	if err != nil {
		switch {
		case errors.Is(err, docker.ErrMediaName):
			writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		case errors.Is(err, docker.ErrMediaNotFound):
			writeJSON(w, http.StatusNotFound, errBody("file not found"))
		default:
			s.logf("media: open failed svc=%s user=%s: %v", agent.Key, ident.AccID, err)
			writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		}
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+display+`"`)
	_, _ = io.Copy(w, rc)
}

// handleMediaDelete removes one uploaded file from the caller's workspace,
// identified by its `path` (the "uploads/<file>" from the list). Authorized
// like the upload/list.
func (s *Server) handleMediaDelete(w http.ResponseWriter, r *http.Request) {
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
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, http.StatusBadRequest, errBody(`"path" query parameter is required`))
		return
	}
	key, ok := s.authorizeSecret(w, agent, ident, tenantID, subsAccID)
	if !ok {
		return
	}
	// The stored filename is the last path segment ("uploads/<uid>-<name>").
	if err := s.Mgr.DeleteMedia(key, filepath.Base(path)); err != nil {
		if errors.Is(err, docker.ErrMediaName) {
			writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
			return
		}
		s.logf("media: delete failed svc=%s user=%s: %v", agent.Key, ident.AccID, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "path": path})
}

// memoryMaxBytes bounds the user-editable workspace memory document so a single
// PUT can't write an unbounded file into the workspace.
const memoryMaxBytes = 256 << 10 // 256 KiB

// handleMemoryGet returns the caller's workspace MEMORY_CUSTOM.md, authorized
// with the same write chain as media (a read-only member can't edit memory, so
// the editor is gated the same as the files panel). An unset file reads as "".
func (s *Server) handleMemoryGet(w http.ResponseWriter, r *http.Request) {
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
	content, err := s.Mgr.ReadMemory(key)
	if err != nil {
		s.logf("memory: read failed svc=%s user=%s: %v", agent.Key, ident.AccID, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": content})
}

// handleMemoryPut replaces the caller's workspace MEMORY_CUSTOM.md. No restart:
// the agent reads the file at turn time, so the new content takes effect on the
// next message without re-provisioning the container.
func (s *Server) handleMemoryPut(w http.ResponseWriter, r *http.Request) {
	agent, ident, ok := s.resolveSecretCaller(w, r)
	if !ok {
		return
	}
	var req struct {
		TenantID  string `json:"tenant_id"`
		SubsAccID string `json:"subs_acc_id"`
		Content   string `json:"content"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, memoryMaxBytes+(1<<10))).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body (or content exceeds the size limit)"))
		return
	}
	if len(req.Content) > memoryMaxBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge,
			errBody(fmt.Sprintf("content exceeds the %d-byte limit", memoryMaxBytes)))
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
	if err := s.Mgr.WriteMemory(key, req.Content); err != nil {
		s.logf("memory: write failed svc=%s user=%s: %v", agent.Key, ident.AccID, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	s.logf("memory: wrote svc=%s tenant=%s subs=%s user=%s bytes=%d",
		agent.Key, tenantID, subsAccID, ident.AccID, len(req.Content))
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// mediaExtAllowed reports whether a filename's (lowercased) extension is in the
// configured upload allowlist.
func (s *Server) mediaExtAllowed(name string) bool {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	for _, allowed := range s.Cfg.MediaAllowedExts {
		if allowed == ext {
			return true
		}
	}
	return false
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
