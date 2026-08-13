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

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/history"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/mcpserver"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/projects"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/restart"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/turn"
	"github.com/google/uuid"
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
	// Projects: a member carving their own agent into Claude-style projects, each
	// a picoclaw agent of its own. HasProject is separate and deliberately cheap —
	// it gates every project-scoped request before any container work, so an
	// unknown project cannot be answered by the default agent.
	ListProjects(key docker.WorkspaceKey) ([]projects.Project, error)
	CreateProject(key docker.WorkspaceKey, name, instructions string) (projects.Project, error)
	RenameProject(key docker.WorkspaceKey, id, name string) (projects.Project, error)
	SetProjectInstructions(key docker.WorkspaceKey, id, instructions string) (projects.Project, error)
	DeleteProject(key docker.WorkspaceKey, id string) error
	HasProject(key docker.WorkspaceKey, id string) (bool, error)

	// per-(user, agent) store, merging native secrets into the current workspace.
	WriteSecret(agent config.Agent, key docker.WorkspaceKey, format, name, value string) error
	// ListSecrets returns the set secret names per format (never values).
	ListSecrets(key docker.WorkspaceKey) (docker.SecretNames, error)
	// DeleteSecret removes one secret from the caller's store.
	DeleteSecret(key docker.WorkspaceKey, format, name string) error
	// RestartWorkspace restarts the caller's container so an injected secret
	// takes effect immediately.
	RestartWorkspace(key docker.WorkspaceKey) error

	// --- restart-control (policy-driven bounces + member self-restart) ---

	// RestartStatus reports whether one workspace still needs a bounce, why, and
	// whether a restart is already scheduled for it.
	RestartStatus(key docker.WorkspaceKey) (docker.RestartStatus, error)
	// RaiseWorkspaceRestartNotice records a notice concerning only this member (their
	// (a member’s own secret write, or a targeted model re-apply), never their peers.
	RaiseWorkspaceRestartNotice(key docker.WorkspaceKey, reason restart.Reason) error
	// RaiseRestartNotice / RestartNotice / WithdrawRestartNotice manage a scope's
	// pending-restart record.
	RaiseRestartNotice(scope docker.Scope, n restart.Notice) error
	RestartNotice(scope docker.Scope) (restart.Notice, bool, error)
	WithdrawRestartNotice(scope docker.Scope) error
	// PropagateScope puts a change on disk for every workspace in scope (running
	// or not); BounceScope stops+starts only the running ones. Split so the
	// bounce can be deferred while propagation never is (FR-4.1).
	PropagateScope(scope docker.Scope) error
	BounceScope(scope docker.Scope) error
	// ArmScheduledBounce schedules a scope bounce, replacing any pending timer.
	ArmScheduledBounce(scope docker.Scope, at time.Time)
	// StoreMedia writes an uploaded file into the caller's workspace uploads
	// dir and returns its workspace-relative path.
	StoreMedia(key docker.WorkspaceKey, project, rawName string, r io.Reader) (docker.StoredMedia, error)
	// StoreAgentAttachment saves a file the HARNESS delivered out-of-band (the
	// "Requested output delivered via tool attachment." path) into
	// uploads/attachments/, which is what makes it reachable: the media list and
	// download route already handle nested paths, so it appears in the uploads
	// sidebar like any file the user uploaded.
	StoreAgentAttachment(key docker.WorkspaceKey, project, rawName string, r io.Reader) (docker.StoredMedia, error)
	// ListMedia returns the files in the caller's workspace uploads dir.
	ListMedia(key docker.WorkspaceKey, project string) ([]docker.StoredMedia, error)
	// DeleteMedia removes one uploaded file (by its stored filename).
	DeleteMedia(key docker.WorkspaceKey, project, storedName string) error
	// Member-driven organisation of the uploads tree. MoveMedia covers renaming;
	// DeleteFolder is recursive and reports how many files it removed.
	CreateFolder(key docker.WorkspaceKey, project, rel string) error
	MoveMedia(key docker.WorkspaceKey, project, fromRel, toRel string) error
	DeleteFolder(key docker.WorkspaceKey, project, rel string) (int, error)
	// OpenMedia opens one uploaded file for download (reader + display name).
	OpenMedia(key docker.WorkspaceKey, project, storedName string) (io.ReadCloser, string, error)
	// ReadMemory returns the caller's workspace MEMORY_CUSTOM.md (empty if unset).
	ReadMemory(key docker.WorkspaceKey, project string) (string, error)
	// WriteMemory replaces the caller's workspace MEMORY_CUSTOM.md.
	WriteMemory(key docker.WorkspaceKey, project, content string) error

	// --- admin-shared-content (authority-over-target; gated in internal/authz) ---

	// ListSharedFiles returns the metadata (never bytes) of a scope's shared files.
	ListSharedFiles(scope docker.Scope) ([]docker.FileMeta, error)
	// WriteSharedFile stores an uploaded shared file at a scope (latest-write-wins).
	WriteSharedFile(scope docker.Scope, rawName string, r io.Reader) (docker.StoredMedia, error)
	// ReadSharedFile opens a scope's shared file for download plus its metadata.
	ReadSharedFile(scope docker.Scope, name string) (io.ReadCloser, docker.FileMeta, error)
	// DeleteSharedFile removes one shared file from a scope.
	DeleteSharedFile(scope docker.Scope, name string) error
	// WriteSharedSecret upserts one shared secret at a scope (dotenv/json/native).
	WriteSharedSecret(scope docker.Scope, format, name, value string) error
	// ListSharedSecrets returns a scope's shared-secret names per format (never values).
	ListSharedSecrets(scope docker.Scope) (docker.SecretNames, error)
	// DeleteSharedSecret removes one shared secret from a scope.
	DeleteSharedSecret(scope docker.Scope, format, name string) error
	// UnsetNativeSlotForScope clears a deleted native slot from the .security.yml
	// of every workspace the scope covers (the merge on restart only sets slots).
	UnsetNativeSlotForScope(scope docker.Scope, slot string)
	// ListTenants returns the tenant ids present on disk (scope discovery, Instance).
	ListTenants() ([]string, error)
	// ListTenantSubscriptions returns the subscription ids under a tenant on disk.
	ListTenantSubscriptions(tenantID string) ([]string, error)
	// ListSubscriptionUsers enumerates the end users under a subscription.
	ListSubscriptionUsers(tenantID, subsAccID string) ([]docker.UserRef, error)
	// ListUserFiles returns a user's private-file metadata only (no bytes — FR-7).
	ListUserFiles(key docker.WorkspaceKey, project string) ([]docker.FileMeta, error)
	// DeleteUserFile removes one of a user's private files (never reads it — FR-7).
	DeleteUserFile(key docker.WorkspaceKey, project, name string) error

	// --- admin-instance-config-editor ---

	// ReadInstanceConfig returns one workspace's config.json, INCLUDING one that
	// does not parse — a broken config is the thing an admin needs to see.
	// This is not the private-file content FR-7 forbids: config.json is
	// proxy-materialized provisioning state at the workspace root, not one of the
	// uploads ListUserFiles enumerates.
	ReadInstanceConfig(key docker.WorkspaceKey) (docker.InstanceConfig, error)
	// WriteInstanceConfig replaces one workspace's config.json and re-runs the
	// ordinary materialization, which is what keeps docker.ManagedConfigPaths
	// proxy-owned. The ReapplyResult reports that pass; its failure does not
	// undo the write.
	WriteInstanceConfig(key docker.WorkspaceKey, raw, revision string) (docker.InstanceConfig, docker.ReapplyResult, error)

	// --- admin-bulk-instance-config (one key across a subscription) ---

	// InspectScopeConfigKey groups every instance of one agent in one subscription
	// by what it holds at one dotted config.json key, so an admin can see the drift
	// before changing it. It also supplies the per-instance revisions the apply
	// needs.
	InspectScopeConfigKey(scope docker.Scope, key string) (docker.ScopeConfigInspection, error)
	// ApplyScopeConfigKey sets that key on every instance whose value DIFFERS and
	// reports a per-instance outcome. The batch never fails wholesale: one member
	// with a corrupt config.json must not block a policy change for the rest.
	ApplyScopeConfigKey(scope docker.Scope, ch docker.ScopeConfigChange) (docker.ScopeConfigResult, error)
	// TemplateConfigKeys flattens an agent TEMPLATE's config.json into dotted leaf
	// paths, which is what the key picker offers. Takes a template NAME, not an
	// agent key — config.yaml declares them separately and two agents may share one.
	TemplateConfigKeys(template string) (docker.TemplateCatalog, error)
	// ApplyTemplateConfigKey optionally writes the same key into the template, so
	// members provisioned LATER inherit it: a workspace's config.json is seeded from
	// the template once and never re-seeded.
	ApplyTemplateConfigKey(template, key string, value any, revision, by string, at time.Time) (docker.TemplateResult, error)
	// ApplyOverlayConfigKey scopes one key to ONE subscription's future members, via
	// a seed overlay applied when a new workspace is provisioned. It is the
	// alternative to writing the agent template, whose reach is every subscription
	// running that agent.
	ApplyOverlayConfigKey(scope docker.Scope, key string, value json.RawMessage, by string, at time.Time) (docker.OverlayResult, error)

	// --- model re-apply (internal/registry is the resolver; no keys transit here) ---

	// Each re-apply takes `bounce`: true restarts the affected workspaces now,
	// false leaves a per-workspace restart notice instead (restart-control FR-4).
	// The notice is per workspace, not per scope, because these passes compute an
	// exact affected set — pinned workspaces are skipped, and a model edit spans
	// tenants — so a scope notice would banner members whose instance is unchanged.

	// ReapplyModelScope re-applies the resolved model to every established
	// workspace under a scope.
	ReapplyModelScope(scope docker.Scope, bounce bool) error
	// ReapplyModelUser re-applies the resolved model to one user's established
	// workspace.
	ReapplyModelUser(key docker.WorkspaceKey, bounce bool) error
	// ReapplyModelForModel re-materializes every workspace whose materialized set
	// contains the model — primaries AND chain holders.
	ReapplyModelForModel(modelName string, bounce bool) error
	// SetModelAssignment pins one workspace to a model; ClearModelAssignment drops
	// the pin so the scope default applies again.
	SetModelAssignment(key docker.WorkspaceKey, modelName string, bounce bool) error
	ClearModelAssignment(key docker.WorkspaceKey, bounce bool) error

	// --- admin-shared-skills (per-scope skill dirs; keys never involved) ---

	ListSharedSkills(scope docker.Scope) ([]docker.SkillMeta, error)
	ReadSharedSkillDoc(scope docker.Scope, name string) (string, docker.SkillMeta, error)
	WriteSharedSkillDoc(scope docker.Scope, name, body string) error
	WriteSharedSkillZip(scope docker.Scope, name string, r io.Reader) error
	ArchiveSharedSkill(scope docker.Scope, name string, w io.Writer) error
	DeleteSharedSkill(scope docker.Scope, name string) error
	// SyncEffectiveSkillsForScope rebuilds the merged effective-skills dir(s) a
	// scope change affects, so the RO mount reflects it on the next stop/start.
	SyncEffectiveSkillsForScope(scope docker.Scope) error

	// --- persona (the read-only identity files at the workspace root) ---

	ListPersona(scope docker.Scope) ([]docker.PersonaEntry, error)
	// ReadPersona resolves the scope's read cascade (this scope → the layer below
	// → the agent template) and reports which layer answered, so the admin screen
	// can preload the identity an agent actually runs without passing one layer's
	// content off as another's.
	ReadPersona(scope docker.Scope, name string) (content string, source string, err error)
	WritePersona(scope docker.Scope, name, body string) error
	DeletePersona(scope docker.Scope, name string) error
	// SyncEffectivePersonaForScope re-resolves the cascade for every workspace a
	// scope write reaches. EnsureRunning does it too, on every request — but the
	// admin handler bounces the scope right after the write, so without this the
	// container boots on the previous effective file.
	SyncEffectivePersonaForScope(scope docker.Scope) error
}

// Turner runs one conversational turn (satisfied by *pico.Client).
type Turner interface {
	RunTurn(ctx context.Context, req turn.Request, sink turn.Sink) (string, error)
	// Cancel stops the turn running on req.SessionID, if there is one. A session
	// with no active turn is not an error.
	Cancel(ctx context.Context, req turn.Request) error
}

// Server holds the handler dependencies.
type Server struct {
	Cfg      *config.Config
	Resolver identity.Resolver
	Mgr      Orchestrator
	// Pico runs picoclaw turns.
	Pico Turner
	Logf func(string, ...any)
	// Reg is the model inventory. Handlers read and write it directly; Mgr is
	// used only to make a change take effect on disk.
	Reg *registry.Registry
	// MemoryGraph owns the per-workspace knowledge graphs. It backs both the
	// read-only /v1/memory-graph* routes and the MCP endpoint the agent uses, so
	// the interface and the bot read the same bytes.
	MemoryGraph *memgraph.Store

	// turns tracks which conversation each workspace is mid-turn on, so a
	// memory-graph write arriving over MCP can be attributed to the chat it came out
	// of. Built by Handler, shared with the MCP endpoint through Deps.SourceFor.
	turns *turnRegistry

	// probes throttles the connectivity test in user_models.go. Per account, so
	// one member cannot spend the instance's outbound budget while another waits.
	probes *probeLimiter
}

// Handler returns the routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Built here so the chat handler and the MCP endpoint share one instance: the
	// chat side registers turns, the MCP side reads them.
	if s.turns == nil {
		s.turns = newTurnRegistry()
	}
	if s.probes == nil {
		s.probes = newProbeLimiter()
	}
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("POST /v1/chat/cancel", s.handleChatCancel)
	mux.HandleFunc("POST /v1/accounts", s.handleAccounts)
	mux.HandleFunc("GET /v1/subscriptions", s.handleSubscriptions)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /v1/sessions/history", s.handleSessionsHistory)
	mux.HandleFunc("GET /v1/sessions/resolve", s.handleSessionsResolve)
	// user-owned-models: a member's own model, and which one they run. Under
	// /v1/models/ rather than /v1/secrets/ because it is a model, not a
	// credential — the key it carries is one field of a definition.
	mux.HandleFunc("GET /v1/models/mine", s.handleUserModelsList)
	mux.HandleFunc("POST /v1/models/mine", s.handleUserModelCreate)
	mux.HandleFunc("PUT /v1/models/mine", s.handleUserModelUpdate)
	mux.HandleFunc("DELETE /v1/models/mine", s.handleUserModelDelete)
	mux.HandleFunc("POST /v1/models/mine/test", s.handleUserModelTest)
	mux.HandleFunc("PUT /v1/models/mine/selection", s.handleUserModelSelect)
	mux.HandleFunc("DELETE /v1/models/mine/selection", s.handleUserModelDeselect)
	mux.HandleFunc("POST /v1/secrets", s.handleSecretsPost)
	mux.HandleFunc("GET /v1/secrets", s.handleSecretsList)
	mux.HandleFunc("DELETE /v1/secrets", s.handleSecretsDelete)
	mux.HandleFunc("POST /v1/media", s.handleMediaPost)
	mux.HandleFunc("GET /v1/media", s.handleMediaList)
	mux.HandleFunc("DELETE /v1/media", s.handleMediaDelete)
	// Member-driven organisation of the uploads tree. Same write gate as the upload
	// itself; the move covers renaming, since a move within one parent IS a rename.
	mux.HandleFunc("POST /v1/media/folder", s.handleMediaFolderCreate)
	mux.HandleFunc("DELETE /v1/media/folder", s.handleMediaFolderDelete)
	mux.HandleFunc("POST /v1/media/move", s.handleMediaMove)
	// Scheduled tasks are picoclaw's; these routes only read its store and the
	// per-run transcripts it leaves in the sessions dir.
	mux.HandleFunc("GET /v1/cron/tasks", s.handleCronTasks)
	mux.HandleFunc("GET /v1/cron/runs", s.handleCronRun)
	mux.HandleFunc("GET /v1/restart", s.handleRestartStatus)
	mux.HandleFunc("POST /v1/restart", s.handleRestartPost)
	mux.HandleFunc("GET /v1/projects", s.handleProjectsList)
	mux.HandleFunc("POST /v1/projects", s.handleProjectsPost)
	mux.HandleFunc("PATCH /v1/projects/{id}", s.handleProjectsPatch)
	mux.HandleFunc("DELETE /v1/projects/{id}", s.handleProjectsDelete)
	mux.HandleFunc("GET /v1/memory", s.handleMemoryGet)
	mux.HandleFunc("PUT /v1/memory", s.handleMemoryPut)
	// memory-graph-mcp: the knowledge graph. READ ONLY — the bot writes through
	// /v1/mcp below, and there is deliberately no write route here (FR-6.5).
	// Distinct paths from /v1/memory on purpose: that is MEMORY_CUSTOM.md, a
	// different thing, and the two surfaces are not unified.
	if s.MemoryGraph != nil {
		mux.HandleFunc("GET /v1/memory-graph", s.handleMemoryGraphGet)
		mux.HandleFunc("GET /v1/memory-graph/nodes", s.handleMemoryGraphNodes)
		mux.HandleFunc("GET /v1/memory-graph/search", s.handleMemoryGraphSearch)
		mux.HandleFunc("GET /v1/memory-graph/recent", s.handleMemoryGraphRecent)
	}
	// The native MCP endpoint, mounted ONLY when a signing secret is configured
	// (FR-4.5). Unlike every other route here it is reachable by any container on
	// the proxy's network without passing through mycelium, so an unconfigured
	// deployment must get no endpoint rather than an unauthenticated one.
	//
	// No method pattern: the streamable-HTTP transport uses POST, GET and DELETE
	// (picoclaw's client uses POST and DELETE — measured, context.md E-9), and the
	// SDK handler routes them itself.
	if s.MemoryGraph != nil && s.Cfg != nil && s.Cfg.ResolvedMCPTokenSecret != "" {
		mux.Handle("/v1/mcp", mcpserver.NewHandler(mcpserver.Deps{
			Store:  s.MemoryGraph,
			Secret: s.Cfg.ResolvedMCPTokenSecret,
			Logf:   s.Logf,
			// Provenance. Passed as a function so mcpserver stays free of any
			// dependency on this package — same shape as Store.
			SourceFor: s.turns.Current,
		}))
	}
	// admin-shared-content: authority-over-target ops, gated in-proxy via
	// internal/authz. There is deliberately NO users/files/content route and no
	// user-file write route (FR-7 privacy invariant).
	mux.HandleFunc("GET /v1/admin/restart", s.handleAdminRestartGet)
	mux.HandleFunc("POST /v1/admin/restart", s.handleAdminRestartPost)
	mux.HandleFunc("DELETE /v1/admin/restart", s.handleAdminRestartDelete)
	mux.HandleFunc("GET /v1/admin/scopes", s.handleAdminScopes)
	mux.HandleFunc("GET /v1/admin/agents", s.handleAdminAgents)
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
	mux.HandleFunc("GET /v1/admin/users/config", s.handleAdminInstanceConfigGet)
	mux.HandleFunc("PUT /v1/admin/users/config", s.handleAdminInstanceConfigPut)
	mux.HandleFunc("GET /v1/admin/scope/config/keys", s.handleAdminScopeConfigKeys)
	mux.HandleFunc("GET /v1/admin/scope/config/inspect", s.handleAdminScopeConfigInspect)
	mux.HandleFunc("PUT /v1/admin/scope/config", s.handleAdminScopeConfigPut)
	mux.HandleFunc("POST /v1/admin/users/restart", s.handleAdminInstanceRestart)
	mux.HandleFunc("GET /v1/admin/skills", s.handleAdminSkillsList)
	mux.HandleFunc("GET /v1/admin/skills/doc", s.handleAdminSkillsDoc)
	mux.HandleFunc("GET /v1/admin/skills/archive", s.handleAdminSkillsArchive)
	mux.HandleFunc("POST /v1/admin/skills", s.handleAdminSkillsPost)
	mux.HandleFunc("DELETE /v1/admin/skills", s.handleAdminSkillsDelete)
	mux.HandleFunc("GET /v1/admin/persona", s.handleAdminPersonaList)
	mux.HandleFunc("GET /v1/admin/persona/doc", s.handleAdminPersonaDoc)
	mux.HandleFunc("POST /v1/admin/persona", s.handleAdminPersonaPost)
	mux.HandleFunc("DELETE /v1/admin/persona", s.handleAdminPersonaDelete)
	// model inventory: proxy-admin gated (internal/registry is the source of
	// truth). /order is registered before /{name} — Go's mux prefers the more
	// specific literal pattern regardless, but keeping them adjacent and ordered
	// makes the intent obvious to the next reader.
	mux.HandleFunc("GET /v1/admin/models", s.handleAdminModelsList)
	mux.HandleFunc("POST /v1/admin/models", s.handleAdminModelCreate)
	mux.HandleFunc("PUT /v1/admin/models/order", s.handleAdminModelsReorder)
	mux.HandleFunc("PUT /v1/admin/models/{name}", s.handleAdminModelUpdate)
	mux.HandleFunc("DELETE /v1/admin/models/{name}", s.handleAdminModelDelete)
	mux.HandleFunc("PUT /v1/admin/models/{name}/status", s.handleAdminModelStatus)
	mux.HandleFunc("POST /v1/admin/models/{name}/deprecate", s.handleAdminModelDeprecate)
	mux.HandleFunc("GET /v1/admin/models/{name}/usage", s.handleAdminModelUsage)
	mux.HandleFunc("GET /v1/admin/model-catalog", s.handleAdminModelCatalog)
	mux.HandleFunc("GET /v1/admin/model-defaults", s.handleAdminModelDefaultGet)
	mux.HandleFunc("PUT /v1/admin/model-defaults", s.handleAdminModelDefaultSet)
	mux.HandleFunc("DELETE /v1/admin/model-defaults", s.handleAdminModelDefaultClear)
	// user-owned-models, admin half: see what members registered, switch one off,
	// and lock a scope against personal models entirely.
	mux.HandleFunc("GET /v1/admin/user-models", s.handleAdminUserModelsList)
	mux.HandleFunc("PUT /v1/admin/user-models/status", s.handleAdminUserModelStatus)
	mux.HandleFunc("GET /v1/admin/model-policy", s.handleAdminModelPolicyGet)
	mux.HandleFunc("PUT /v1/admin/model-policy", s.handleAdminModelPolicySet)
	mux.HandleFunc("DELETE /v1/admin/model-policy", s.handleAdminModelPolicyClear)
	mux.HandleFunc("GET /v1/admin/model-assignments", s.handleAdminModelAssignmentList)
	mux.HandleFunc("POST /v1/admin/model-assignments", s.handleAdminModelAssignmentSet)
	mux.HandleFunc("DELETE /v1/admin/model-assignments", s.handleAdminModelAssignmentClear)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	// Unauthenticated OpenAPI document for mycelium tool discovery (fetched
	// directly from the service host via openapiPath, not through the gateway).
	mux.HandleFunc("GET /doc/openapi.json", s.handleOpenAPI)
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
	// Project scopes the conversation to one of the caller's projects: it selects
	// the picoclaw agent (through the session id prefix a dispatch rule matches)
	// and therefore the workspace. Empty means the main agent — today's behavior.
	Project   string `json:"project,omitempty"`
	TenantID  string `json:"tenant_id"`
	SubsAccID string `json:"subs_acc_id"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// chatScope is one authorized conversation: which container answers, and which
// picoclaw session the turn belongs to.
//
// It exists so running a turn and stopping one resolve their target through the
// SAME code. picoclaw looks an abort up by the session it derives from the id it
// was given, so a stop that computed its session id even slightly differently
// would abort nothing and report success — the failure would look like a dead
// button, not like a bug here.
type chatScope struct {
	Agent config.Agent
	Ident identity.Identity
	Key   docker.WorkspaceKey
	// SessionID is the value picoclaw is addressed with: the per-account session
	// key, prefixed when the conversation belongs to a project.
	SessionID string
}

// resolveChatScope decodes a chat request, authorizes it and resolves its
// target. It writes the error response itself and reports false when the caller
// must stop.
//
// It owns the decode so that WHO is asking is settled before WHAT they sent:
// an unknown agent must answer 404 rather than complaining about the body of a
// request it was never going to serve.
func (s *Server) resolveChatScope(w http.ResponseWriter, r *http.Request) (chatScope, chatRequest, bool) {
	agent, status, msg := s.resolveAgent(r)
	if status != 0 {
		writeJSON(w, status, errBody(msg))
		return chatScope{}, chatRequest{}, false
	}
	ident, ok := s.Resolver.Resolve(r.Header.Get(identity.ProfileHeader))
	if !ok {
		writeJSON(w, http.StatusUnauthorized,
			errBody("missing or invalid "+identity.ProfileHeader+" header"))
		return chatScope{}, chatRequest{}, false
	}

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return chatScope{}, chatRequest{}, false
	}
	sessionKey := identity.SessionKey(ident.AccID, req.SessionID)
	if sessionKey == "" {
		writeJSON(w, http.StatusBadRequest,
			errBody(`"session_id" is required to isolate conversations for this account`))
		return chatScope{}, chatRequest{}, false
	}
	tenantID, err := uuid.Parse(req.TenantID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"tenant_id" is required and must be a UUID`))
		return chatScope{}, chatRequest{}, false
	}
	subsAccID, err := uuid.Parse(req.SubsAccID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(`"subs_acc_id" is required and must be a UUID`))
		return chatScope{}, chatRequest{}, false
	}
	// Account-switching guard: a caller acting AS the subscription (not as an
	// individual member) has no per-user identity to isolate on.
	if ident.Profile.AccID == subsAccID {
		writeJSON(w, http.StatusForbidden,
			errBody("profile account id must differ from subs_acc_id (act as an individual member)"))
		return chatScope{}, chatRequest{}, false
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
		return chatScope{}, chatRequest{}, false
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
		return chatScope{}, chatRequest{}, false
	}

	key := docker.WorkspaceKey{
		TenantID:  tenantID.String(),
		SubsAccID: subsAccID.String(),
		Role:      agent.Key,
		UserAccID: ident.AccID,
	}
	// The project prefix is what routes this turn. picoclaw sees the prefixed
	// value as its chat id, and the dispatch rule the projection wrote matches it
	// with a wildcard — so every conversation in a project reaches the project's
	// agent without a rule of its own.
	//
	// An unknown project is refused here, BEFORE any container work. Falling
	// through would answer as the main agent and write the turn into the main
	// workspace; the user would meet that days later as missing history, not now
	// as an error.
	if ok := s.checkProject(w, key, req.Project); !ok {
		return chatScope{}, chatRequest{}, false
	}
	sessionKey = projectSessionID(req.Project, sessionKey)
	s.logf("chat: authorized svc=%s tenant=%s subs=%s user=%s project=%q",
		agent.Key, tenantID, subsAccID, ident.AccID, req.Project)
	return chatScope{Agent: agent, Ident: ident, Key: key, SessionID: sessionKey}, req, true
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	scope, req, ok := s.resolveChatScope(w, r)
	if !ok {
		return
	}
	if len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, errBody(`"messages" is required`))
		return
	}
	agent, ident, key, sessionKey := scope.Agent, scope.Ident, scope.Key, scope.SessionID
	// Memory-graph provenance: mark this workspace as mid-turn on this conversation,
	// so an MCP write during the turn can be attributed to it. Placed here — after
	// authorization, before either the streaming or the synchronous branch — so one
	// registration covers both.
	//
	// `defer f()()` is deliberate: Begin runs NOW, its returned closure runs on
	// return. Deferring matters more than it looks — a leaked entry does not merely
	// lose attribution, it mis-attributes every later write for this workspace to a
	// conversation that already ended. The RAW session id is recorded, not the
	// sessionKey hash, because it is what the webapp navigates by.
	if s.turns != nil {
		defer s.turns.Begin(scopeOf(key), req.SessionID)()
	}
	userContent := lastUserContent(req.Messages)
	model := req.Model
	if model == "" {
		model = "picoclaw"
	}
	id := "chatcmpl-" + randomHex(12)

	if req.Stream {
		s.streamTurn(w, r, agent, key, ident.Email, sessionKey, userContent, model, id, req.Project)
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
	// The non-streaming path needs the same attachment handling as the streaming
	// one: a delivery dropped here is exactly the reported bug, only without a
	// stream to notice it in. Notices are collected and appended to the answer
	// below, since there is no incremental channel to write them to.
	var notices []string
	content, err := s.Pico.RunTurn(turnCtx, turn.Request{
		Endpoint:   tgt.Endpoint,
		AuthToken:  tgt.AuthToken,
		SessionID:  sessionKey,
		SessionKey: key.UserAccID + ":" + key.Role,
		Model:      model,
		Content:    userContent,
	}, turn.Sink{
		Attachment: func(a turn.Attachment) {
			stored, storeErr := s.storeTurnAttachment(turnCtx, key, req.Project, a)
			if storeErr != nil {
				s.logf("chat: attachment %q not stored: %v", a.Filename, storeErr)
				return
			}
			s.logf("chat: attachment stored at %s (%d bytes)", stored.Path, stored.Size)
			notices = append(notices, attachmentNotice(stored.Path, a.Filename))
		},
	})
	s.Mgr.ArmIdle(agent, key)
	if err != nil {
		s.logf("chat: turn failed svc=%s user=%s: %v", agent.Key, ident.AccID, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	sessionsDir := config.SessionsDir(s.Cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID, workspaceSegmentOf(req.Project))
	if syncErr := history.SyncDurable(sessionsDir, sessionKey); syncErr != nil {
		s.logf("chat: sync durable history failed: %v", syncErr)
	}
	// Appended AFTER the answer: unlike the stream, the order here is ours to pick,
	// and a file notice reads as a footnote to the reply rather than an interruption.
	writeJSON(w, http.StatusOK, completionResponse(id, model, content+strings.Join(notices, "")))
}

// cancelTimeout bounds a stop request. It is short on purpose: a stop that has
// to wait is useless to the member who pressed it, and there is nothing here
// that legitimately takes long — one WebSocket dial, one frame out, one back.
const cancelTimeout = 20 * time.Second

// handleChatCancel stops the turn running on a conversation.
//
// It reuses resolveChatScope, so the session it addresses is the one the turn
// was started with. The request body is a chatRequest like the completions call,
// minus "messages": stopping a turn needs the same scope, not its content.
//
// Answers 204 whether or not a turn was actually running. The turn finishing
// while the member clicks is an ordinary race, and picoclaw reports "nothing to
// stop" in prose the caller cannot act on differently — reporting it as failure
// would only make a working Stop look broken.
func (s *Server) handleChatCancel(w http.ResponseWriter, r *http.Request) {
	scope, req, ok := s.resolveChatScope(w, r)
	if !ok {
		return
	}

	// Not decoupled from the request context, unlike a turn: nothing here writes
	// a transcript, so a caller that walks away has nothing to leave truncated.
	ctx, cancel := context.WithTimeout(r.Context(), cancelTimeout)
	defer cancel()
	tgt, err := s.Mgr.EnsureRunning(ctx, scope.Agent, scope.Key, scope.Ident.Email)
	if err != nil {
		s.logf("cancel: ensure running failed: %v", err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	if err := s.Pico.Cancel(ctx, turn.Request{
		Endpoint:  tgt.Endpoint,
		AuthToken: tgt.AuthToken,
		SessionID: scope.SessionID,
	}); err != nil {
		s.logf("cancel: stop failed svc=%s user=%s: %v", scope.Agent.Key, scope.Ident.AccID, err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	s.logf("cancel: stop sent svc=%s user=%s project=%q", scope.Agent.Key, scope.Ident.AccID, req.Project)
	w.WriteHeader(http.StatusNoContent)
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
	// A project's transcripts live under its OWN workspace, because each picoclaw
	// agent keeps sessions beside itself. Reading the main workspace for a project
	// chat would return an empty history rather than an error.
	key := docker.WorkspaceKey{
		TenantID: tenantID.String(), SubsAccID: subsAccID.String(),
		Role: agent.Key, UserAccID: ident.AccID,
	}
	segment, projectID, ok := s.workspaceSegmentFor(w, r, key)
	if !ok {
		return
	}
	sessionKey = projectSessionID(projectID, sessionKey)
	sessionsDir := config.SessionsDir(s.Cfg.ContainerDataRoot, tenantID.String(), subsAccID.String(), agent.Key, ident.AccID, segment)
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
	// agent-projects: both halves of the answer are project-scoped, exactly as in
	// handleSessionsHistory above. The prefixed key is what picoclaw was handed, and
	// the file that holds it lives under the project's own workspace — resolving the
	// main workspace returned the wrong sessionKey AND looked for it in a directory
	// that never held it.
	key := docker.WorkspaceKey{
		TenantID: tenantID.String(), SubsAccID: subsAccID.String(),
		Role: agent.Key, UserAccID: ident.AccID,
	}
	segment, projectID, ok := s.workspaceSegmentFor(w, r, key)
	if !ok {
		return
	}
	sessionKey = projectSessionID(projectID, sessionKey)
	sessionsDir := config.SessionsDir(s.Cfg.ContainerDataRoot, tenantID.String(), subsAccID.String(), agent.Key, ident.AccID, segment)
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

// rejectNativeForUser writes a 403 and returns true when a per-user secret WRITE
// names the native format. Native slots (picoclaw's own .security.yml —
// search-provider and model keys) are an administrative surface: they are
// published at tenant/subscription scope via /v1/admin/shared-secrets and reach
// workspaces through the cascade (native-secrets-admin-only FR-1). The gate lives
// here, in the proxy, and not only in the webapp BFF — the reverted first attempt
// gated the BFF alone and left this layer open.
//
// It gates WRITES only. Listing still reports a user's pre-gate native NAMES
// (never a value), and deleting one is still allowed: those entries are the
// user's own data, and removing the only way to clean them up would strand them
// permanently. Deletion cannot inject a credential, so it is outside what this
// feature moves to admins.
func rejectNativeForUser(w http.ResponseWriter, format string) bool {
	if format != docker.FormatNative {
		return false
	}
	writeJSON(w, http.StatusForbidden, errBody(
		"native secrets are administered at tenant/subscription scope; ask a scope administrator to set this credential"))
	return true
}

// handleSecretsPost injects/updates one secret for the caller's (user, agent)
// pair (AC-02). The container is no longer bounced here: the member gets a
// restart notice and decides when to apply it (restart-control DEC-3).
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
	if rejectNativeForUser(w, req.Format) {
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
	// The secret is stored and the effective view rebuilt, but the container is
	// NOT bounced here (restart-control DEC-3): a forced restart mid-conversation
	// is exactly the disruption this feature removes. The member gets a notice on
	// their own marker — never their peers' — and presses the button when ready.
	if err := s.Mgr.RaiseWorkspaceRestartNotice(key, restart.ReasonOwnSecret); err != nil {
		s.logf("secrets: raise restart notice failed svc=%s user=%s: %v", agent.Key, ident.AccID, err)
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
	if err := s.Mgr.RaiseWorkspaceRestartNotice(key, restart.ReasonOwnSecret); err != nil {
		s.logf("secrets: raise restart notice after delete failed svc=%s user=%s: %v", agent.Key, ident.AccID, err)
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
	// agent-projects: the project arrives as a FORM FIELD here, like tenant_id and
	// subs_acc_id above — this is the one multipart route on the media surface, so
	// workspaceSegmentFor (query-only) cannot see it and silently answered "main
	// workspace" for every project upload. The file then landed where the project's
	// agent could not open it, and the turn's `[anexo: uploads/...]` reference
	// pointed at nothing.
	project := r.FormValue("project")
	if !s.checkProject(w, key, project) {
		return
	}

	stored, err := s.Mgr.StoreMedia(key, project, header.Filename, file)
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
	_, project, ok := s.workspaceSegmentFor(w, r, key)
	if !ok {
		return
	}
	// GET /v1/media?path=uploads/<file> downloads that file; without `path` it
	// lists the uploads dir.
	if path := r.URL.Query().Get("path"); path != "" {
		s.serveMediaFile(w, key, project, agent, ident, path)
		return
	}
	files, err := s.Mgr.ListMedia(key, project)
	if err != nil {
		s.logf("media: list failed svc=%s user=%s: %v", agent.Key, ident.AccID, err)
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// mediaRelPath turns the client-supplied "uploads/<...>" reference into the path
// relative to the uploads dir. It must NOT collapse to the base name: the agent
// organizes files into folders, and doing so made "uploads/reports/q1.pdf"
// resolve to a non-existent "uploads/q1.pdf". Traversal is rejected downstream
// by safeStoredPath + resolveWithin, which is where the boundary belongs.
func mediaRelPath(p string) string {
	return strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(p)), "uploads/")
}

// serveMediaFile streams one uploaded file back as a download attachment.
func (s *Server) serveMediaFile(w http.ResponseWriter, key docker.WorkspaceKey, project string, agent config.Agent, ident identity.Identity, path string) {
	rc, display, err := s.Mgr.OpenMedia(key, project, mediaRelPath(path))
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
	_, project, ok := s.workspaceSegmentFor(w, r, key)
	if !ok {
		return
	}
	if err := s.Mgr.DeleteMedia(key, project, mediaRelPath(path)); err != nil {
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
	_, project, ok := s.workspaceSegmentFor(w, r, key)
	if !ok {
		return
	}
	content, err := s.Mgr.ReadMemory(key, project)
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
		// agent-projects: empty means the agent's own workspace. In the BODY, like
		// every other field of this request — the GET beside it takes the project as a
		// query parameter because it has no body to put it in, and reading the query
		// HERE meant a note edited inside a project silently overwrote the main
		// workspace's document instead.
		Project string `json:"project,omitempty"`
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
	if !s.checkProject(w, key, req.Project) {
		return
	}
	if err := s.Mgr.WriteMemory(key, req.Project, req.Content); err != nil {
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
