# model-registry-source-of-truth — crab-shell-proxy scope

Proxy-side slice of the cross-cutting feature. The authoritative requirements,
user decisions and architecture live in the parent repo at
`.specs/features/model-registry-source-of-truth/{spec,context,design}.md`; this
file records what changes **here** and the invariants this repo must uphold.

## What this repo owns

The model inventory itself: storage, the single resolver, materialization into
workspaces, the boot migration, and the admin HTTP surface. Supersedes this
repo's `admin-model-override` feature (its cascade is absorbed) and deletes the
`registered-models` store.

## New package: `internal/registry`

The only code that knows the storage shape. Everything else calls it.

- **Store** over `go.etcd.io/bbolt` at `<containerDataRoot>/model-registry.db`,
  root-owned 0600, outside every per-workspace tree so no `chownTree` pass
  touches it. Buckets `models`, `assignments`, `scope_defaults`, `meta`.
- **`Resolve(WorkspaceKey) (primary *Model, chain []*Model, err error)`** — the
  single answer to "which model does this workspace use". Cascade: per-user
  assignment → subscription default → tenant default → agent default → global
  default → `ErrNoModelResolvable`. A `deprecated` result is followed to
  `replaced_by` **only** when the workspace has no materialized assignment,
  bounded at 8 hops with cycle detection.
- **Invariants inside one `bolt.Update`**: `model_name` unique; delete and
  `→ disabled` both blocked while referenced by any assignment, scope default, or
  another model's `replaced_by`, with the referrers named; `→ deprecated`
  requires a `replaced_by` naming an existing `active` model; acyclic deprecation
  chains; stale `version` rejected.
- **`APIKey` is `json:"-"`** on the wire type — leaking a key must require adding
  code, not forgetting it.

## Changes to existing code

| Location | Change |
|---|---|
| `internal/docker/model.go` | delete `resolveModel`, `reapplyModel`, `getModelOverride`, `setModelOverride`, `clearModelOverride`, `EffectiveModel`, `SetModelOverride`, `ClearModelOverride`, `ModelSel`. `ReapplyModelScope` / `ReapplyModelUser` survive, calling `registry.Resolve` + `materializeModels` |
| `internal/docker/registered_models.go` + test | deleted |
| `internal/docker/provision.go:103` | `applyModel` → `materializeModels(configPath, secPath, primary, chain)`; writes the full `model_list` **without `api_key`**, sets `agents.defaults.provider`/`model_name`/`model_fallbacks`, and the keys into `.security.yml` `model_list.<name>.api_keys` via read-modify-write |
| `internal/docker/provision.go` | refuse to provision on `ErrNoModelResolvable`, before creating anything |
| `internal/docker/defaulttemplate/picoclaw/config.json` | `"model_list": []`, empty `agents.defaults.provider`/`model_name` |
| `internal/docker/model-catalog.json` | new, `go:embed` — the 30 former template entries as a read-only suggestion catalog (`provider`, `model`, `api_base`; never a key) |
| `internal/docker/reconcile.go:19` | one-time migration guarded by `meta.schema_version`, plus a per-boot read-only drift check |
| `internal/docker/secrets.go:174` | `validateNativeSlot`'s `model_list.<model>.api_keys` family validates against the inventory instead of a template `.security.yml` |
| `internal/httpapi/admin.go` | `registered-models` and `model-override` handlers removed; new `internal/httpapi/admin_models.go` |
| `internal/httpapi/handlers.go:95-110` | the `Docker` interface loses `EffectiveModel`, `SetModelOverride`, `ClearModelOverride`; `ReapplyModelScope`/`ReapplyModelUser` keep their signatures |
| `internal/httpapi/openapi.json` | new routes documented, removed ones dropped |

### Routes removed or repurposed

Registered today at `internal/httpapi/handlers.go:211-219`:

| Existing route | Fate |
|---|---|
| `GET /v1/admin/models` | **repurposed** — was the `config.yaml` selectable list, becomes the inventory listing |
| `GET` `PUT` `DELETE` `/v1/admin/model` | removed — replaced by `/v1/admin/model-defaults` and `/v1/admin/model-assignments` |
| `GET /v1/admin/model/users` | removed — replaced by `/v1/admin/models/{name}/usage` |
| `GET` `POST` `DELETE` `/v1/admin/registered-models` | removed |
| `POST /v1/admin/registered-models/apply` | removed — replaced by `POST /v1/admin/model-assignments` |

The repurposing of `GET /v1/admin/models` is a breaking response-shape change on
an existing path. No client calls it today (the webapp never shipped the
`admin-model-override` UI its FR-6 specified), so no coordinated cutover is
needed — but the gateway allowlist and `openapi.json` must both be updated.

## HTTP surface

| Method | Route | Gate |
|---|---|---|
| `GET` `POST` | `/v1/admin/models` | `HasAdminPrivileges` |
| `PUT` `DELETE` | `/v1/admin/models/{name}` | `HasAdminPrivileges` |
| `POST` | `/v1/admin/models/{name}/deprecate` | `HasAdminPrivileges` |
| `PUT` | `/v1/admin/models/order` | `HasAdminPrivileges` |
| `GET` | `/v1/admin/models/{name}/usage` | `HasAdminPrivileges` |
| `GET` | `/v1/admin/model-catalog` | `HasAdminPrivileges` |
| `GET` `PUT` `DELETE` | `/v1/admin/model-defaults?scope=global\|agent` | `HasAdminPrivileges` |
| `GET` `PUT` `DELETE` | `/v1/admin/model-defaults?scope=tenant\|subscription` | `authz.AuthorizeSharedScope` |
| `POST` `DELETE` | `/v1/admin/model-assignments` | `authz.AuthorizeUserManagement` |

Inventory mutations take the proxy-admin gate because the caller supplies API
keys with instance-wide blast radius; the `global` and `agent` defaults take the
same gate, since `AuthorizeSharedScope` has no level above tenant to express.
`PUT` omitting `api_key` keeps the stored one; there is no read-back. Status
codes: 400 invalid input or a bad deprecation; 403 gate; 404 unknown model; 409
duplicate name, in-use rejection, version conflict, or `ErrNoModelResolvable`.

## Re-materialization

Scope-default changes, per-user assignment changes and model definition/key edits
re-materialize eagerly (stop/start the affected workspaces, never recreate).
Reordering the active set is applied lazily on each workspace's next
materialization, plus an explicit "apply now" — a drag must not restart the fleet.
Deprecation triggers nothing: existing users keeping the model is the point.

## Migration (in `Reconcile`, once)

1. `config.yaml` `agent.Model` + `agent.Models` → model records (keys from
   `apiKeyEnv`); `agent.Model` → `scope_defaults[agent/<key>]`.
2. `registered-models/<agent>.json` → model records (they carry real keys).
3. `shared/model.json` → scope defaults; `.crab-model.json` → explicit
   assignments.
4. Every existing workspace: read `config.json`'s `agents.defaults.model_name`
   and record the assignment (`inherited` unless step 3 gave it an explicit one).
   A model named by a workspace but absent from the inventory is imported from
   that workspace's own `model_list` entry + `.security.yml` key, flagged
   `ImportedOrphan` and logged.
5. Normalize every `<dataRoot>/templates/<agent>/config.json` to an empty
   `model_list`, backing the original up to `config.json.pre-registry` — the only
   destructive write, ordered last so an earlier failure leaves it undone.
6. Write `meta.schema_version`; log that the superseded files are now ignored.

Later sources win on `model_name` collision: a `registered-models` entry or a
live workspace holds a key an admin actually entered, whereas the `config.yaml`
seed may name an environment variable that is no longer set.

Step 4 is load-bearing. Without it every existing user reads as unassigned and
the first scope-default change re-resolves them — the orphaning this feature
exists to remove.

## Out of scope here

Hermes agents keep reading `config.yaml`'s `agent.Model`; their key is injected
as a container environment variable (`internal/docker/provision_hermes.go:177`),
a different mechanism that needs its own decision. The inventory governs picoclaw
agents only this cycle.

## Verification gate

`go vet ./...` and `go test ./...` clean (the Dockerfile build *is* the gate,
`Dockerfile:22-23`). New tests: store invariants each rejected with nothing
written; all five cascade levels plus explicit-beats-inherited; the deprecation
hop firing only for unmaterialized workspaces; materialization asserting no
`api_key` in `config.json` and the key present in `.security.yml` with the pico
token intact; provision refusal with no container created; a fixture data root
exercising the whole migration and asserting no workspace's active model changed,
then that a second run is a no-op; each HTTP gate returning 403 for the wrong
tier.

## Status

Spec written 2026-07-25. Parent design approved. Tasks pending.
