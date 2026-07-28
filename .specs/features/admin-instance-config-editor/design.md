# admin-instance-config-editor — Design (crab-shell-proxy)

Reference: `spec.md` in this folder. This document covers the proxy slice only;
the editor UI is designed in `crab-exoskeleton-webapp/.specs/features/admin-instance-config-editor/design.md`.

---

## Shape of the change

Two handlers, one new `docker` file, one new `restart.Reason` value. Nothing
existing changes behaviour.

```
internal/docker/instance_config.go        (new)  read/write one workspace config.json
internal/docker/instance_config_test.go   (new)
internal/httpapi/admin_instance_config.go (new)  the two handlers
internal/httpapi/admin_instance_config_test.go (new)
internal/httpapi/handlers.go              (edit) 2 routes + 2 Orchestrator methods
internal/restart/restart.go               (edit) ReasonConfig
```

`internal/docker/materialize.go` gains **no** logic — the whole point of DEC-2 is
that the managed keys are re-established by calling the materialization that
already exists.

---

## Data model

```go
// internal/docker/instance_config.go

// InstanceConfig is one workspace's config.json as an admin sees it: the raw
// bytes, whether they parse, and the metadata a repair round-trip needs.
// The bytes are carried as a string and never re-marshalled from a parsed value:
// a config.json that does NOT parse is the primary thing this repairs, so the
// transport has to survive one (spec FR-1.2).
type InstanceConfig struct {
	Raw        string   `json:"raw"`
	Valid      bool     `json:"valid"`
	ParseError string   `json:"parseError,omitempty"`
	// Offset is the byte offset of a *json.SyntaxError, -1 when unknown.
	Offset       int64    `json:"offset,omitempty"`
	Size         int64    `json:"size"`
	ModifiedAt   time.Time `json:"modifiedAt"`
	Revision     string   `json:"revision"`
	ManagedPaths []string `json:"managedPaths"`
	RedactedPaths []string `json:"redactedPaths,omitempty"`
}
```

`ManagedPaths` is filled from one package-level constant (NFR-4):

```go
// ManagedConfigPaths are the config.json paths the PROXY owns. Dotted, with [*]
// for "every element". They are rewritten on every materialization
// (materializeModels) or provision (alignWorkspace), so an admin edit to one of
// them cannot survive and the editor renders them read-only (spec FR-3).
//
// Adding a writer to materializeModels or alignWorkspace WITHOUT adding its path
// here silently makes that key look admin-editable. TestManagedConfigPathsMatchWriters
// is the gate.
var ManagedConfigPaths = []string{
	"model_list",
	"agents.defaults.provider",
	"agents.defaults.model_name",
	"agents.defaults.model_fallbacks",
	"agents.defaults.workspace",
	"channel_list.pico.enabled",
}
```

## `docker` layer

```go
// ReadInstanceConfig returns one workspace's config.json. A file that does not
// parse comes back with Valid=false and no error: it is data an admin needs to
// see, not a failure (spec FR-1.3). ErrNotProvisioned when there is no file.
func (m *Manager) ReadInstanceConfig(key WorkspaceKey) (InstanceConfig, error)

// WriteInstanceConfig replaces one workspace's config.json with raw.
//
// revision, when non-empty, must match the current bytes or ErrStaleRevision is
// returned and nothing is written: this file has a second writer
// (materializeModels), so a blind write can discard a materialization that
// landed between the admin's read and their save (spec FR-2.5).
//
// After the write it runs the ordinary already-provisioned re-materialization,
// which is what makes the proxy-owned paths authoritative without a second copy
// of the merge rules (spec FR-3.2/FR-3.3). A materialization failure is
// REPORTED, not returned: the admin's write already landed and undoing it would
// throw away the repair.
func (m *Manager) WriteInstanceConfig(key WorkspaceKey, raw, revision string) (InstanceConfig, ReapplyResult, error)

type ReapplyResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}
```

Sentinels: `ErrNotProvisioned`, `ErrStaleRevision`, `ErrConfigNotObject`,
`ErrConfigTooLarge` (mapped to statuses in the handler, following
`skillErrStatus`'s pattern in `admin.go`).

### Write sequence

1. `raw` length ≤ `maxInstanceConfigBytes` (1 MiB) → else `ErrConfigTooLarge`.
2. `json.Unmarshal(raw, &map[string]any{})` → syntax error becomes
   `*json.SyntaxError`-aware; a non-object top level becomes
   `ErrConfigNotObject`.
3. Read current bytes; if `revision != ""` and
   `revision != revisionOf(current)` → `ErrStaleRevision`.
4. Write `config.json.tmp-<pid>` in the **same** directory, `0o600`, `rename(2)`
   over `config.json`. Same dir so the rename is atomic on the same filesystem.
5. `chownTree(userDir, m.cfg.PicoclawUser)` — the workspace is bind-mounted and
   picoclaw must own what it reads. `chownTree` already handles the
   symlink/`Lchown` hazard.
6. `m.reapplyWorkspace(key)` → `ReapplyResult`. Never fatal.
7. Re-read and return the resulting `InstanceConfig` (spec FR-2.7/FR-3.4).

`revisionOf(b []byte) string` → `"sha256:" + hex(sha256.Sum256(b))`.

### Redaction (FR-6.2)

`redactModelKeys` walks `model_list` (array or, in a legacy layout, object) and
replaces each entry's `api_keys` **values** with `"***"`, collecting the dotted
paths it touched. It runs on the READ path only, and it re-marshals — so a
redacted response's `raw` is reformatted relative to disk.

That is a real consequence: `revision` must be computed over the **on-disk
bytes**, not over the redacted output, or the admin's very first save would 409.
The redaction path therefore keeps the disk revision and, because `model_list`
is managed, the `"***"` cannot reach disk (step 6 replaces the whole array).

Redaction only happens when a key is actually present, so the overwhelmingly
common case (a post-migration workspace) returns bytes verbatim.

## `httpapi` layer

```go
// GET  /v1/admin/users/config?tenant_id=&subs_acc_id=&user_acc_id=&agent=
// PUT  /v1/admin/users/config?…&restart=now|notice
func (s *Server) handleAdminInstanceConfigGet(w http.ResponseWriter, r *http.Request)
func (s *Server) handleAdminInstanceConfigPut(w http.ResponseWriter, r *http.Request)
```

Both start with `s.resolveSecretCaller` (bearer + profile) and then build the key
with a **new** helper — not `adminUserFileKey`:

```go
// adminInstanceKey resolves the (tenant, subscription, agent, user) key an
// instance-config op targets, gating it on user-management authority.
//
// Unlike adminUserFileKey, the agent comes from an EXPLICIT `agent` parameter
// rather than the addressed agent (the auth vehicle). The webapp pins
// /alpha/v1/admin for every admin call, so inheriting the vehicle would read and
// repair alpha's config while the admin believes they are fixing beta's
// (spec FR-1.6 / DEC-1). `agent` is required here — "all" and "" are rejected,
// because a workspace has exactly one role.
func (s *Server) adminInstanceKey(w http.ResponseWriter, r *http.Request, ident identity.Identity) (docker.WorkspaceKey, bool)
```

It reuses `resolveAgentTarget` for the "is this a configured agent key" check and
adds the required-and-not-`all` rule on top.

### The FR-7 comment (mandatory)

`admin_instance_config.go` opens with the exception argument from `spec.md`, in
the file, because the three sibling files carry standing "do not add a content
route here" instructions and a reader hitting this file next needs to know why it
is not that route:

```go
// Instance config administration: read and replace ONE workspace's config.json.
//
// This is NOT the content endpoint admin-shared-content FR-7 forbids, and the
// "no content route here" instructions in admin.go (user files), the webapp BFF
// route and members-panel.tsx all stay in force. The distinction:
//
//   - FR-7's subject is the set ListUserFiles enumerates, which is the UPLOADS
//     dir alone (shared.go → config.UploadsDir). config.json is not in it.
//   - FR-7 protects MEMBER-AUTHORED content. config.json is proxy-materialized
//     provisioning state: the proxy seeds it and rewrites six of its paths.
//   - This route takes no name/path/file parameter. It addresses exactly
//     <userDir>/config.json and cannot be pointed anywhere else. Do not add one.
```

### Status mapping

| Condition | Status | Body |
| --- | --- | --- |
| bad/absent uuid, bad `agent` | 400 | `errBody(msg)` |
| not user-manager for the target | 403 | `errBody("not authorized to manage this user's configuration")` |
| `ErrNotProvisioned` | 404 | `{"error":"not_provisioned"}` |
| `ErrConfigTooLarge` | 413 | `{"error":"too_large"}` |
| syntax error / non-object | 400 | `{"error":"invalid_json","detail":…,"offset":…}` |
| `ErrStaleRevision` | 409 | `{"error":"stale_revision"}` |
| other | 500 | `errBody(err.Error())` |

Existing responses in this package are hand-built maps via `writeJSON`; these
follow that, not a new envelope type.

### Restart delivery

`PUT` reads the policy with the existing `s.bounceNow(w, r)` and, after a
successful write, calls `s.Mgr` the way `bounceOrNotify` does — `RestartWorkspace`
for `now`, `RaiseWorkspaceRestartNotice(key, restart.ReasonConfig)` otherwise.
The policy is parsed **before** the write (a malformed policy must not 400 a
write that already happened — `parseRestartPolicy`'s documented reason).

`restart.ReasonConfig = "config"` joins the enum; the webapp must add copy for
it (its `restart-notice` reason switch is exhaustive over the enum).

### Audit log (FR-5)

One `s.logf` at the end of `PUT`, success or failure:

```
admin: instance config write by=<caller> key=<t>/<s>/<role>/<u> before=<n>B valid=<bool> after=<n>B result=<ok|409|403|…> reapplied=<bool>
```

`by` comes from `ident.Profile` (email when present, else account id — the same
`by` value `applyRestartPolicy` already threads). The body is never logged
(FR-5.3).

---

## Orchestrator interface

Two methods appended under a new `// --- admin-instance-config-editor ---`
heading, mirroring how `ReadMemory`/`WriteMemory` are grouped:

```go
ReadInstanceConfig(key docker.WorkspaceKey) (docker.InstanceConfig, error)
WriteInstanceConfig(key docker.WorkspaceKey, raw, revision string) (docker.InstanceConfig, docker.ReapplyResult, error)
```

`internal/httpapi/handlers_test.go`'s fake orchestrator gains both.

## Routes

Beside the existing user-file routes so the grouping reads as one surface:

```go
mux.HandleFunc("GET /v1/admin/users/config", s.handleAdminInstanceConfigGet)
mux.HandleFunc("PUT /v1/admin/users/config", s.handleAdminInstanceConfigPut)
```

`/v1/admin/*` is a single gateway wildcard, so no parent-repo route change
(spec NFR-3). `openapi.json` describes only the gateway-discoverable chat surface
and carries no `/v1/admin` path today — nothing to add there.

---

## Test plan

`internal/docker/instance_config_test.go` (real filesystem, `t.TempDir`, the
pattern `provision_test.go` uses):

| Test | Asserts |
| --- | --- |
| `TestReadInstanceConfigReturnsBytesVerbatim` | FR-1.2 — `Raw` is byte-identical to disk |
| `TestReadInstanceConfigOnBrokenJSON` | FR-1.3 — `Valid:false`, message, offset, no error |
| `TestReadInstanceConfigNotProvisioned` | FR-1.5 — `ErrNotProvisioned` |
| `TestReadInstanceConfigRedactsLegacyModelKeys` | FR-6.2 — `"***"` + `RedactedPaths`, revision still matches disk |
| `TestWriteInstanceConfigRejectsInvalidJSON` | FR-2.2 — syntax error and array top level, file untouched |
| `TestWriteInstanceConfigRejectsStaleRevision` | FR-2.5 — 409 path, file untouched |
| `TestWriteInstanceConfigRejectsOversize` | FR-2.4 |
| `TestWriteInstanceConfigIsAtomicAnd0600` | FR-2.6 — mode, no leftover temp file |
| `TestWriteInstanceConfigReapplyRestoresManagedPaths` | FR-3.3 — a submitted `agents.defaults.model_name` reverts to the registry's |
| `TestWriteInstanceConfigSurvivesReapplyFailure` | FR-3.3 — `ReapplyResult{OK:false}`, bytes still on disk |
| `TestWriteInstanceConfigRepairsUnparseableFile` | FR-3.3 — broken before, valid after, reapply then succeeds |
| `TestManagedConfigPathsMatchWriters` | NFR-4 — see below |

`TestManagedConfigPathsMatchWriters` is the anti-drift gate. It cannot read Go
source meaningfully, so it works behaviourally: seed a config whose every
`ManagedConfigPaths` entry holds a sentinel value, run
`materializeModels` + `alignWorkspace`, and assert that **exactly** the listed
paths changed — a new writer touching an unlisted path fails the test, and a
listed path no longer written fails it too.

`internal/httpapi/admin_instance_config_test.go` (fake orchestrator, the pattern
`admin_test.go` uses):

| Test | Asserts |
| --- | --- |
| `TestInstanceConfigGetRequiresUserManagement` | FR-4.1 — foreign subscriptions-manager → 403, both verbs |
| `TestInstanceConfigRejectsBadIDs` | FR-4.3 — non-uuid → 400 |
| `TestInstanceConfigRequiresExplicitAgent` | FR-1.6 — missing `agent` → 400; `agent=all` → 400 |
| `TestInstanceConfigTargetsTheNamedAgentNotTheVehicle` | FR-1.6/DEC-1 — request routed as alpha with `agent=beta` builds a beta key |
| `TestInstanceConfigGetIncludesManagedPaths` | FR-3.1 — equals `docker.ManagedConfigPaths` |
| `TestInstanceConfigPutRestartNotice` | FR-6.1 — `restart=notice` raises a workspace notice with `ReasonConfig`, no bounce |
| `TestInstanceConfigPutRestartNow` | FR-6.1 — default bounces the workspace |
| `TestInstanceConfigPutSchedulesAsNotice` | FR-6.1 — `restart=schedule` behaves as notice |
| `TestInstanceConfigPutLogsWithoutBody` | FR-5.1/FR-5.3 — one line, caller + key + sizes, and no document text |
| `TestInstanceConfigPutReturnsPostReapplyState` | FR-2.7 |
| `TestInstanceConfigNoFileNameParameter` | FR-1.4 — a `name=`/`path=` parameter changes nothing about which file is read |

Gate: `go build ./... && go test ./...` and `go vet ./...`.
