# admin-model-override — HTTP endpoints + reapply-to-scope report

## Status
Complete. All gates green; new tests pass; no regressions beyond pre-existing
sandbox-only chown failures (unrelated, present on the base commit too).

## What was built

### 1. Reapply-to-scope (`internal/docker/model.go`)
- `ModelTarget{Kind, TenantID, SubsAccID, Role, UserAccID}` + `modelOverridePath`
  unify where a model override read/write/clear applies (tenant/subscription
  file, or a per-user file when `UserAccID` is set).
- `SetModelOverride`, `ClearModelOverride`, `EffectiveModel` (exported wrappers
  around the existing unexported `setModelOverride`/`getModelOverride`):
  `EffectiveModel` walks the same user>subscription>tenant>default precedence
  as `resolveModel`, but only from the target's own level downward, and
  reports which level set it.
- `ReapplyModelScope(scope Scope)`: enumerates workspace keys under the scope
  (`ListSubscriptionUsers` for a subscription; a
  `tenants/<t>/subscriptions/*/agents/*/users/*` glob — mirroring
  `reconcile.go`'s `existingWorkspaces` pattern — for a whole tenant), looks up
  each workspace's agent by role, skips unprovisioned workspaces (no
  `config.json` yet) and unknown roles (logged), calls `reapplyModel` +
  `chownTree` on each provisioned one, then calls `RestartScope(scope)`.
- `ReapplyModelUser(key, agent)`: same reapply+chown for one workspace, then
  `RestartWorkspace(key)` (already a no-op when not running).

### 2. HTTP handlers (`internal/httpapi/admin.go` + `handlers.go`)
- `GET /v1/admin/models` — caller agent's `SelectableModels()` as
  `{provider,name}` only.
- `GET/PUT/DELETE /v1/admin/model` — shared `resolveModelTarget` helper parses
  + authorizes a target from either query params or the PUT JSON body: a
  per-user target (`user_acc_id` set) is authorized like
  `adminUserFileKey`/`AuthorizeUserManagement`; a scope target is authorized
  like the existing shared-content handlers (`AuthorizeSharedScope`). PUT
  validates `{provider,name}` against `agent.FindModel` (400, nothing written,
  if absent) before authorizing/writing. Both PUT and DELETE re-apply (via
  `ReapplyModelScope`/`ReapplyModelUser`) and restart after the write/clear.
- `GET /v1/admin/model/users` — mirrors `handleAdminUsersList`, adding each
  user's effective `{provider,name,level}` (resolved against *that user's own*
  agent by role, since a subscription can host users under several agents).
- Added the 5 methods to the `Orchestrator` interface and registered all 5
  routes in `handlers.go` next to the existing `/v1/admin/*` block.

### 3. Tests
- `internal/docker/model_test.go`: `TestReapplyModelScopeSubscription` (two
  provisioned workspaces, subscription override, asserts config.json +
  security.yml updated while each workspace's own pico token survives),
  `TestReapplyModelScopeTenantWide` (tenant-wide glob across two
  subscriptions), `TestReapplyModelScopeSkipsUnprovisionedWorkspace`.
- `internal/httpapi/admin_model_test.go`: no-key-leak assertion on
  `/v1/admin/models` and `/v1/admin/model/users` (grepping the response body
  for the fake keys/`apiKeyEnv`/`api_key`), PUT-forbidden-no-write (403),
  PUT-unknown-model-no-write (400), and a real-`docker.Manager`-backed
  PUT→GET→DELETE round trip proving the override file lands on disk and GET
  reflects it (+ level), falling back to "default" after DELETE.

## How the caller's agent was resolved
Reused the existing `s.resolveSecretCaller(w, r)` helper (already used by
`/v1/secrets`, `/v1/media`, `/v1/memory`) — it runs `resolveAgent` (service-name
header + bearer token) then resolves the mycelium profile, returning
`(config.Agent, identity.Identity, bool)`. No new resolution helper was needed.

## No-key-leak confirmation
`TestAdminModelsListSelectableNoKeyLeak` and
`TestAdminModelUsersListIncludesEffectiveModel` build an agent whose default
and selectable models carry distinct fake api keys (and an `apiKeyEnv` name),
then assert the JSON response body contains none of
`sk-default-secret`/`sk-openai-should-not-leak`/`OPENAI_KEY`/`apiKeyEnv`/
`api_key`/`apiKey`/`APIKey`. Both pass.

## Tenant-wide enumeration
`scopeWorkspaceKeys` globs `tenants/<t>/subscriptions/*/agents/*/users/*`
(tenant id sanitized via `identity.SanitizeID`, matching the on-disk layout
builders), parsing each match's `Rel` path into
`{tenantID, subsAccID, role, userAccID}` — the same 8-segment-path approach
`reconcile.go`'s `existingWorkspaces` already uses for a fixed role, extended
to a wildcard role.

## Test summary
- `internal/docker`: new model tests all pass (`TestReapplyModelScope*`, plus
  the pre-existing `TestModelOverrideRoundTrip`/`TestResolveModel*`/
  `TestReapplyModel*` still green).
- `internal/httpapi`: all new `TestAdminModel*` tests pass; full package `go
  test` is green.
- Pre-existing `TestEnsureRunning*`/`TestRestartWorkspaceRestartsAndRearms`/
  `TestReconcileEnsuresContinuousWorkspaces` chown failures are present
  identically on the base commit (verified via `git stash`) — sandbox-only
  (non-root, can't chown to uid 1000), unrelated to this change.

## Gate results
- `gofmt -l` on touched files: clean.
- `go build ./...`: clean.
- `go vet ./internal/...`: clean.
- `go test ./internal/config/... ./internal/docker/... ./internal/httpapi/...`:
  green except the pre-existing sandbox chown failures noted above.

## Concerns / follow-ups
- Gateway TOML + webapp are out of scope here (per instructions) — the routes
  exist in the proxy but aren't yet exposed through the gateway or consumed by
  the webapp.
- PUT/DELETE reapply/restart failures are logged, not surfaced in the HTTP
  response (the override write itself already succeeded) — matches the
  existing shared-secrets handler's error-handling style in this codebase.
