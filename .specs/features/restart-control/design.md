# restart-control — Design

Implements [spec.md](spec.md) under the decisions in [context.md](context.md).

---

## 1. The core idea

Today a restart is a **side effect buried in a handler**. This design turns it
into two separable things:

```
admin action  ─┬─► propagate (always)   effective secrets rebuilt, native slots merged
               └─► bounce   (policy)    now │ schedule(at) │ notice-only
```

and makes "does this workspace still need a bounce?" a **derived** question
rather than stored per-user state:

```
pending(workspace) ⇔ lastRestartAt(workspace) < noticeAt(nearest scope notice)
```

Nothing is fanned out at admin-action time, so a workspace that is scaled to
zero, or that does not exist yet, is handled for free: it has no marker (or an
old one) until it starts, and container create stamps the marker, which is
exactly right — a container created after the change already has the change.

## 2. State on disk

A new area **outside** the tenant tree, because `UserWorkspace` is bind-mounted
whole into the agent container (NFR-2).

```
<root>/restart/
├── scopes/
│   └── <tenantID>/
│       ├── _tenant.json                 # tenant-scope notices
│       └── <subsAccID>.json             # subscription-scope notices
└── workspaces/
    └── <tenantID>/<subsAccID>/<role>/<userAccID>.json
```

`internal/config/config.go` gains three helpers next to the existing ones:

```go
func RestartScopeFile(root, tenantID, subsAccID string) string  // subsAccID=="" -> _tenant.json
func RestartWorkspaceFile(root, tenantID, subsAccID, role, userAccID string) string
func RestartRoot(root string) string
```

### Scope record

One file per (tenant) or (tenant, subscription); agent-scoping is a key inside,
mirroring how `Scope.AgentKey == ""` already means "all agents":

```json
{
  "agents": {
    "*":     { "noticeAt": "2026-07-26T18:04:11Z", "reason": "shared-secret", "note": "", "by": "admin@x" },
    "alpha": { "noticeAt": "2026-07-26T19:00:00Z", "scheduledAt": "2026-07-27T02:00:00Z", "reason": "model", "note": "nightly window" }
  }
}
```

`"*"` is the all-agents entry. Writes are whole-file, guarded by a package-level
mutex — these files are tiny and written at human frequency.

### Workspace marker

```json
{ "lastRestartAt": "2026-07-26T18:20:03Z" }
```

## 3. New package: `internal/restart`

Pure state + policy, no Docker. Keeps `internal/docker` from growing another
concern and makes the resolution rules unit-testable without a daemon.

```go
package restart

type Reason string // shared-secret | shared-skills | shared-files | model | own-secret | admin-request

type Notice struct {
    NoticeAt    time.Time  `json:"noticeAt"`
    ScheduledAt *time.Time `json:"scheduledAt,omitempty"`
    Reason      Reason     `json:"reason"`
    Note        string     `json:"note,omitempty"`
    By          string     `json:"by,omitempty"`
}

type Store struct{ root string }

func NewStore(root string) *Store

// Raise writes/replaces the notice for a scope+agent ("" agent -> "*").
func (s *Store) Raise(tenantID, subsAccID, agentKey string, n Notice) error
func (s *Store) Withdraw(tenantID, subsAccID, agentKey string) error
func (s *Store) Get(tenantID, subsAccID, agentKey string) (Notice, bool, error)

// Resolve walks the cascade agent-in-subscription -> all-agents-in-subscription
// -> agent-in-tenant -> all-agents-in-tenant and returns the newest notice.
func (s *Store) Resolve(tenantID, subsAccID, agentKey string) (Notice, bool, error)

// Stamp / LastRestart manage the per-workspace marker.
func (s *Store) Stamp(tenantID, subsAccID, role, userAccID string, at time.Time) error
func (s *Store) LastRestart(tenantID, subsAccID, role, userAccID string) (time.Time, error)

// Status is the derived answer (FR-3.3).
func (s *Store) Status(tenantID, subsAccID, role, userAccID string) (Status, error)

type Status struct {
    Pending       bool
    Reason        Reason
    Note          string
    NoticeAt      time.Time
    ScheduledAt   *time.Time
    LastRestartAt time.Time
}

// Schedules enumerates every scope record carrying a future or elapsed
// ScheduledAt — the input to Reconcile's re-arm (FR-6.2).
func (s *Store) Schedules() ([]ScheduleRef, error)
```

**Cascade order** deliberately mirrors `sharedSecretsCascade` in
`internal/docker/shared.go`: narrowest first, and among candidates the **newest
`noticeAt` wins** (a tenant-wide notice raised after a subscription one must not
be masked by the stale narrower record).

## 4. Splitting `RestartScope`

`internal/docker/shared.go` currently interleaves propagation and bouncing in
one loop over *running* containers. Two problems: a stopped workspace never gets
`applyNativeToWorkspace`, and the bounce cannot be deferred.

```go
// PropagateScope rebuilds the effective secret view and merges native slots for
// EVERY workspace in scope (running or not). Always runs, whatever the policy.
func (m *Manager) PropagateScope(scope Scope) error

// BounceScope stops+starts the RUNNING containers in scope. Best-effort, as
// RestartScope is today: per-container failures are logged, not returned.
func (m *Manager) BounceScope(scope Scope) error

// RestartScope is kept as PropagateScope + BounceScope so no existing caller
// changes meaning while sites migrate.
func (m *Manager) RestartScope(scope Scope) error
```

`workspacesInScope` (already present, used by `UnsetNativeSlotForScope`) gives
`PropagateScope` its iteration set, so this is a re-grouping of existing code,
not new logic.

## 5. Policy application

One helper, used by every migrated site:

```go
// internal/httpapi/restart_policy.go
type Policy struct {
    Mode        string     // "now" | "notice" | "schedule"; "" == "now"
    ScheduledAt *time.Time
    Note        string
}

// Apply propagates, then either bounces now, arms a schedule, or only raises the
// notice. It always raises the scope notice EXCEPT for mode "now", where the
// bounce itself is the notification.
func (s *Server) applyRestartPolicy(scope docker.Scope, reason restart.Reason, p Policy, by string) error
```

The `now` branch skips the notice on purpose: raising a notice and immediately
bouncing would leave a `noticeAt` newer than the markers of any container that
was *not* running, which would show those members a stale banner. Cold start
stamps their marker anyway, so a notice adds nothing.

**Reading the policy from a request.** Admin endpoints accept it as query
parameters so the multipart upload handlers (skills, shared files) need no body
change:

```
?restart=now|notice|schedule&restart_at=<RFC3339>&restart_note=<text>
```

Absent `restart` ⇒ `now` (FR-4.2).

## 6. Scheduler

Lives on `Manager` beside the idle timer, reusing its shape:

```go
type scheduleState struct {
    mu     sync.Mutex
    timers map[string]*time.Timer // key: tenant|subs|agent
}

func (m *Manager) ArmScheduledBounce(scope Scope, at time.Time)  // replaces any pending timer for the key
func (m *Manager) CancelScheduledBounce(scope Scope)
```

On fire: `BounceScope(scope)`, then clear `ScheduledAt` from the record (leaving
`noticeAt`, per FR-6.1) and drop the timer.

`Reconcile` gains a final step: `store.Schedules()`, and for each, arm — an
already-elapsed time arms with a zero duration so it fires immediately (FR-6.2).

## 7. Stamping the marker

Two places, both narrow:

- `Manager.RestartWorkspace` — after `waitHealthy` succeeds, `store.Stamp(...)`.
- `Manager.EnsureRunning` — after a **create** (not on the hot path where the
  container is already running), `store.Stamp(...)`.

`RestartWorkspace`'s no-op branch (container absent/stopped) also stamps: the
member pressed the button, and the next cold start will apply everything, so
their notice is genuinely resolved (FR-1.4 + FR-3.4).

## 8. HTTP surface

### Member (new gateway routes required)

```
GET  /v1/restart   -> 200 {pending, reason, note, noticeAt, scheduledAt, lastRestartAt, running}
POST /v1/restart   -> 200 {status: "restarted"|"noop", lastRestartAt}
                      500 {error:{message}} on a real Docker failure
```

Both resolve the workspace key exactly like `handleSecretsPost` does — via
`resolveSecretCaller` (profile `accId` + routed agent) — so FR-1.1 is satisfied
by construction: there is no code path that reads a user id from the request.

### Admin (no gateway change; `/v1/admin/*` is a wildcard)

```
GET    /v1/admin/restart?tenant_id=&subs_acc_id=&agent_key=
POST   /v1/admin/restart   {tenant_id, subs_acc_id?, agent_key?, mode, at?, reason?, note?}
DELETE /v1/admin/restart?tenant_id=&subs_acc_id=&agent_key=
```

Authorization: `authz.AuthorizeSharedScope(profile, kind, tenantID, subsAccID)`
where `kind` is `"subscription"` when `subs_acc_id` is present, else `"tenant"`
— identical to the shared-content endpoints.

### Gateway declarations

`/v1/restart` needs a `[[<agent>.path]]` block per agent in **three** files:
`deploy/standalone/config.standalone.toml`, `deploy/prod/config.base.toml`,
`deploy/dokploy/config.base.toml`. **Verified during implementation:** the gateway matches routes by **path alone**
(`adapters/mem_db/src/repositories/routes_read.rs` — `WildMatch::new(route.path)`
against the request path, then an explicit error when more than one route
matches: *"Multiple routes found for the specified path"*). Two blocks differing
only in `methods` would therefore break the route entirely, not gate it.

So `/v1/restart` is **one** block per agent covering both methods, gated on the
role name (read), with the write requirement for POST enforced in-proxy by the
same profile chain `/v1/secrets` uses. This is design §8's documented fallback,
now the primary approach:

```toml
[[alpha.path]]
group = { protectedByRoles = [{ name = "alpha" }] }
path = "/v1/restart"
secretName = "alpha-authorization-header"
acceptInsecureRouting = true
methods = ["GET", "POST"]
```

Added for every agent in all three deploy configs (`alpha`, `beta`,
`hermes-glm`; dokploy has no hermes-glm).

`openapi.json` is updated for both routes so the gateway's discovery stays
accurate.

## 9. Webapp

### BFF routes (`app/api/restart/route.ts`)

`GET` and `POST` proxy to the agent-routed proxy path via the existing
`fetchMycelium` + session-token pattern used by `app/api/secrets`.
`app/api/admin/restart/route.ts` mirrors it for the admin trio, through
`lib/adminProxy.ts`.

### Member UI

- `lib/restart.ts` — the status type, the reason→phrase map (i18n keys, per
  DEC-2), and the fetch helpers.
- `app/chat/restart-banner.tsx` — rendered by `chat-shell.tsx` above the
  conversation. Three states: hidden, scheduled (informational), actionable
  (button). Button disabled while in flight (DEC-4).
- Polling: reuse the chat screen's existing interval; on mount and after any
  secret write, re-fetch.
- `app/chat/secrets-drawer.tsx` — after a successful write, show the inline
  restart affordance instead of relying on a silent restart (FR-7.7).

### Admin UI

- `app/admin/restart-policy-select.tsx` — a small three-way control plus a
  datetime input, held in the admin screen's state so the choice persists across
  panel actions in one session (FR-8.2).
- `shared-secrets-panel.tsx`, `shared-skills-panel.tsx`, `model-*-panel.tsx`
  append the chosen policy to their mutation calls.
- A pending notice for the selected scope is shown with a **Withdraw** action.

## 10. What could go wrong

| Risk | Handling |
| --- | --- |
| Two path blocks per route rejected by mycelium | Documented fallback in §8 — in-proxy permission check |
| Clock skew between proxy and browser makes a scheduled time look wrong | The API returns RFC3339 UTC; the browser formats to local. No relative arithmetic server-side |
| A notice raised while a scheduled bounce is pending | Same record; re-scheduling replaces the timer (FR-6.3) |
| Marker file write fails (disk full) | Log and continue — a missing marker means "pending", which is the safe direction (a spurious banner, never a silently skipped restart) |
| `RestartScope` callers outside the migrated set | Kept as `Propagate + Bounce`, so their behaviour is byte-identical |
