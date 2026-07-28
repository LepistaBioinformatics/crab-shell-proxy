# admin-instance-config-editor — Specification

**Status:** Shipped (proxy slice). See "Reconciliation" at the end for what
changed during implementation.
**Size:** Large (new API surface + new admin UI + a privacy-invariant exception)
**Repos touched:**

| Repo | Role |
| --- | --- |
| `crab-shell-proxy` | `GET`/`PUT /v1/admin/users/config` — read and replace one workspace's `config.json` |
| `crab-exoskeleton-webapp` | Raw + tree JSON editor inside the Members panel (own `spec.md` in that repo) |
| `zombie-crab-project` (parent) | Submodule pointer bump only — `/v1/admin/*` is already a single gateway wildcard (restart-control FR-5.6) |

---

## Problem

A member's container is configured by one file: `config.json` in that member's
workspace (`<root>/tenants/<t>/subscriptions/<s>/agents/<role>/users/<u>/config.json`).
It is seeded once from the agent template and, for a returning user, **never
re-seeded** — `provisionUser` treats an existing `config.json` as "returning
user, leave as-is" (`internal/docker/provision.go:27`). Only five key paths are
ever rewritten afterwards (FR-3).

The consequence: once a workspace's `config.json` is wrong, it stays wrong
forever. It can be wrong for reasons entirely outside a member's control:

- it was seeded from a template that carried a bad value at the time,
- a picoclaw version bump changed a field's accepted shape,
- a tool or channel block was hand-edited during an incident and left broken,
- a value has the wrong JSON type (`"max_tokens": "32768"` as a string), which
  makes picoclaw refuse to boot with no member-visible explanation.

Today the only remedies are `docker exec`/host filesystem access, or destroying
the workspace (which loses the member's transcripts, uploads and skills). There
is **no** administrative surface for it: the admin screen can list and delete a
member's uploads and nothing else.

## Goal

Give an admin who manages a subscription a way to **read and repair one
instance's `config.json`**, in place, without host access and without destroying
the member's data.

"Instance" here is the per-member container, keyed by
`WorkspaceKey{TenantID, SubsAccID, Role, UserAccID}` — the same sense the proxy
already uses for the word (`internal/docker/model.go:111`). One member with a
grant on two agents has two instances and two `config.json` files.

## Non-goals

- **Not** a general file-content endpoint for member workspaces. The route reads
  and writes exactly `config.json`, takes no file-name parameter, and cannot
  address any other path (FR-1.4). See the FR-7 discussion below.
- **Not** an editor for the agent template (`templates/<agent>/config.json`).
  Fixing the seed for future members is a separate, wider-blast-radius feature. — DEC-5
- **Not** a schema validator for picoclaw config. The proxy validates that the
  document is JSON and is an object; whether `tools.exec.timeout_seconds` is a
  sensible number is the admin's judgement (FR-2.3).
- **Not** a secrets surface. Credentials live in `.security.yml`, which this
  feature never reads or writes.
- **Not** a config history / rollback store. Single-slot backups and diffs are
  deferred (DEFER-1).

---

## The FR-7 question (privacy invariant), answered explicitly

`admin-shared-content` **FR-7** states: *"No admin endpoint ever returns the
bytes of an end user's private file, and no admin endpoint permits editing one.
… This holds regardless of caller tier (including Instance)."* The existing code
carries that as a standing instruction in three places — the handler comment at
`internal/httpapi/admin.go:530`, the BFF route comment in
`app/api/admin/users/files/route.ts`, and the panel comment at
`app/admin/members-panel.tsx:38` ("Do not add a link, download icon, or row
click handler to the file rows").

This feature is **not** an exception to that invariant, because `config.json` is
not one of the files FR-7 protects:

1. **Different surface.** FR-7's subject is the set enumerated by
   `Manager.ListUserFiles`, which reads **only** the uploads directory
   (`internal/docker/shared.go:381` → `config.UploadsDir`). `config.json` lives
   at the workspace root and never appears in that listing.
2. **Different authorship.** FR-7 protects *member-authored* content — what a
   person uploaded or wrote. `config.json` is **proxy-materialized provisioning
   state**: the proxy seeds it, the proxy rewrites five of its paths on every
   materialization, and it exists only because the proxy created it.
3. **Narrow by construction.** The endpoint takes no `name` parameter. There is
   no path traversal to defend against because there is no caller-supplied path.

**Requirement:** the new handler, the new BFF route and the new UI each carry a
comment stating this distinction and pointing back at FR-7, so a future reader
does not mistake the new route for a violation, and so the existing "do not add
a content route here" instructions stay correct in their own files. — FR-6.3

---

## Requirements

### FR-1 — Read one instance's config

- **FR-1.1** `GET /v1/admin/users/config?tenant_id=&subs_acc_id=&user_acc_id=&agent=`
  returns that workspace's `config.json`.
- **FR-1.2** The response returns the **raw bytes as a string**, never a parsed
  object:

  ```json
  {
    "raw": "{\n  \"session\": { … }\n}",
    "valid": true,
    "parseError": null,
    "size": 11834,
    "modifiedAt": "2026-07-28T12:04:11Z",
    "revision": "sha256:9f2c…",
    "managedPaths": ["model_list", "agents.defaults.provider", "…"]
  }
  ```

  Returning a parsed object would make the feature unable to open the exact
  files it exists for: a `config.json` that does not parse is the primary
  failure this repairs. A broken file must still load.
- **FR-1.3** `valid` reports whether the bytes parse as a JSON **object**, and
  `parseError` carries `encoding/json`'s message (with byte offset when the
  error is a `*json.SyntaxError`) so the UI can point at the break. A file that
  fails to parse is **still returned** with `valid: false` and HTTP **200** — it
  is data, not an error.
- **FR-1.4** The addressed file is always `<userDir>/config.json`. There is no
  `name`, `path` or `file` parameter, and none may be added. — DEC-4
- **FR-1.5** `404` when the workspace directory has no `config.json`: that member
  has never been provisioned for that agent, so there is nothing to repair. The
  body distinguishes it (`{"error":"not_provisioned"}`) from a permission denial.
- **FR-1.6** `agent` is an **explicit** query parameter naming the target
  agent, validated against the configured agent set. It is deliberately **not**
  taken from the routing vehicle the way `adminUserFileKey` does
  (`internal/httpapi/admin.go:502` uses `agent.Key`, and the webapp's
  `ADMIN_BASE` hardcodes `/alpha/v1/admin`, so those endpoints always address
  the *alpha* workspace whatever the member actually uses). Repairing the wrong
  agent's config would be worse than useless. The vehicle stays the bearer
  guard; the target comes from the parameter. — DEC-1
- **FR-1.7** `revision` is `sha256:<hex>` of the bytes as read. It is the
  concurrency token for FR-2.5.

### FR-2 — Replace one instance's config

- **FR-2.1** `PUT /v1/admin/users/config?tenant_id=&subs_acc_id=&user_acc_id=&agent=`
  with body `{"raw": "<full document text>", "revision": "sha256:…"}` replaces
  the file wholesale. There is no patch/merge-by-key API: the editor round-trips
  the whole document, and a partial write on a broken file has no well-defined
  base.
- **FR-2.2** `raw` must parse as JSON and its top level must be an **object**;
  otherwise `400 {"error":"invalid_json","detail":"…","offset":N}`. This is the
  one syntactic gate: a document that does not parse cannot help any instance and
  would only re-break the one being repaired.
- **FR-2.3** Beyond FR-2.2 the content is **not** validated. Semantically odd
  values are the admin's call — the proxy has no picoclaw config schema and
  inventing a partial one would reject valid config from a newer picoclaw.
- **FR-2.4** Body size cap: `1 MiB`. The seeded document is ~12 KiB; the cap
  exists so the endpoint cannot be used to plant a large file in a workspace.
- **FR-2.5** `revision` must match the current on-disk bytes; otherwise
  `409 {"error":"stale_revision"}` and the caller re-reads. The file has two
  concurrent writers — this endpoint and `materializeModels` (FR-3) — so a
  blind write could silently discard a materialization that landed between the
  admin's read and save.
- **FR-2.6** The write is atomic: temp file in the same directory, `0o600`,
  `rename(2)` over the target, then the workspace is re-chowned to
  `PicoclawUser` (reusing `chownTree`'s `Lchown` semantics). A torn write here
  bricks the instance being repaired.
- **FR-2.7** On success the response carries the **post-write** state — `raw`,
  `revision`, `valid`, `managedPaths`, plus `reapplied` (FR-3.3) — so the editor
  shows what actually landed rather than what was sent.

### FR-3 — Proxy-owned keys stay proxy-owned

The proxy rewrites exactly these paths in `config.json` after seeding:

| Path | Writer | Why |
| --- | --- | --- |
| `model_list` | `materializeModels` (`internal/docker/materialize.go:54`) | Fully replaced from the model registry resolution |
| `agents.defaults.provider` | `materializeModels:62` | Resolved primary model |
| `agents.defaults.model_name` | `materializeModels:63` | Resolved primary model |
| `agents.defaults.model_fallbacks` | `materializeModels:69` / deleted at `:73` | Resolved fallback chain |
| `channel_list.pico.enabled` | `materializeModels:45` (forced `true`) | The only channel the proxy reaches picoclaw through |
| `agents.defaults.workspace` | `alignWorkspace` (`internal/docker/provision.go:138`) | Must match the bind-mount path |

- **FR-3.1** The `GET` response lists them in `managedPaths` (dotted paths) so
  the UI can render them read-only with an explanation. The list is a single
  exported constant in the proxy — the UI must not hardcode its own copy.
- **FR-3.2** An admin edit is **never** authoritative for a managed path.
  Rather than diffing and rejecting, the proxy re-establishes them by running
  the **existing** materialization after the write (FR-3.3): the keys become
  correct by construction, and no new merge logic can drift from
  `materializeModels`. — DEC-2
- **FR-3.3** After a successful write the handler calls the existing
  already-provisioned re-materialization for that workspace (the
  `reapplyWorkspace` path, which is a no-op when `config.json` is absent).
  - It runs on **every** successful write, including one whose submitted
    document changed no managed path — that is what makes "the proxy owns these"
    true rather than conditional.
  - A materialization failure (e.g. the registry resolves no active model) does
    **not** fail the request: the admin's write already landed and reverting it
    would throw away the repair. The response reports
    `reapplied: {"ok": false, "detail": "…"}` and the failure is logged. This
    mirrors `PropagateScope`'s "per-workspace failures are logged, not returned".
  - When the pre-write file did **not** parse, materialization would fail on
    `parse config.json` before the write and succeed after it — which is exactly
    the repair working. No special case is needed.
- **FR-3.4** Because materialization runs last, the response's `raw` (FR-2.7) is
  read back **after** it, so an admin who edited a managed path in raw mode sees
  it revert immediately instead of believing the edit stuck.

### FR-4 — Authorization

- **FR-4.1** Both verbs authorize with `authz.AuthorizeUserManagement(profile,
  tenantID, subsAccID)` — the same authority the existing
  `/v1/admin/users/files` endpoints require. A caller strictly above the member
  in that member's subscription branch (Subscription tier of `S`, Tenant tier of
  `T`, or Instance) qualifies. — DEC-3
- **FR-4.2** `AuthorizeUserManagement` failure → `403`, with the same message
  shape the sibling endpoints use.
- **FR-4.3** Every id parameter is parsed as a UUID before use, and `agent` is
  matched against the configured agent set, so nothing caller-supplied reaches a
  path join unvalidated (`identity.SanitizeID` remains the only path builder).
- **FR-4.4** The security consequence is stated, not hidden: `config.json`
  carries `agents.defaults.restrict_to_workspace`,
  `allow_read_outside_workspace`, `tools.exec.enable_deny_patterns`,
  `tools.allow_read_paths` / `allow_write_paths`. Write access to this file is
  write access to the agent's sandbox boundary. DEC-3 accepts that at the
  tenant/subscription-manager tier on the grounds that the same tier can already
  publish shared skills and native secrets into every container below it, which
  is comparable authority. It is the deliberate choice, and the reason FR-5
  requires an audit log line.

### FR-5 — Observability

- **FR-5.1** Every successful `PUT` logs one line naming the caller (profile
  email or account id), the full workspace key, the byte size before and after,
  and whether the pre-write document parsed. The write is a
  sandbox-boundary-capable change to someone else's container; it must be
  reconstructable from logs.
- **FR-5.2** A rejected `PUT` (403, 409, 400) logs at the same site, so a
  failed repair attempt is as visible as a successful one.
- **FR-5.3** No log line ever contains the document body: `config.json` may hold
  residual credentials in legacy layouts (FR-6.2).

### FR-6 — Interaction with existing invariants

- **FR-6.1 (restart)** `gateway.hot_reload` is `false` in the seeded config, so
  a saved change reaches picoclaw only on a container bounce. The write does not
  bounce on its own: it goes through the established restart policy
  (`restart-control` FR-4) with a new reason enum value `config`, honouring
  `restart=now|notice|schedule` from the query string exactly as the other
  admin mutations do. Default `now`, matching every sibling endpoint.
  - The target is **one workspace**, not a scope, so the handler reuses the
    existing per-workspace reduction `bounceNow` + `Manager.bounceOrNotify`
    (`internal/httpapi/restart_policy.go`, `internal/docker/model.go:112`):
    `now` → `RestartWorkspace(key)`, anything else → a **workspace** notice via
    `RaiseWorkspaceRestartNotice(key, ReasonConfig)`.
  - `schedule` therefore behaves as `notice`, which is `bounceNow`'s documented
    behaviour at every per-workspace site already (the scheduler arms per scope,
    and a scope schedule would bounce every member to apply one member's
    config). Matching it beats inventing a rejection this endpoint alone would
    have. The admin can still arm a window via `POST /v1/admin/restart`. — DEC-6
- **FR-6.2 (residual credentials)** A workspace that has not been materialized
  since the model-registry migration can still carry
  `model_list[*].api_keys` inside `config.json` (`migrate_models.go:542`
  documents `.security.yml` as the sink the current code writes; the legacy
  layout kept keys in `config.json`). The `GET` response **redacts** those
  values to `"***"`. Redaction is safe against round-tripping because
  the **write** restores it: before the bytes are written, any masked `api_keys`
  whose counterpart still exists on disk is replaced with the stored value.
  Redaction is reported in `redactedPaths: ["model_list[0].api_keys", …]` so the
  UI can say why a value is masked.
  - The restore does **not** rely on FR-3.3's materialization, and an earlier
    draft of this requirement did. Materialization replaces the whole
    `model_list`, but it is best-effort by design — and a registry that resolves
    nothing is exactly the broken-instance case this feature exists for. A save
    would then have written `"***"` over the workspace's only copy of the key.
    `TestWriteInstanceConfigNeverStoresTheMask` is the gate.
  - A mask with no counterpart on disk is written as given: it is a literal the
    admin typed, and restoring "from nowhere" would resurrect a key they removed.
  - Only a document that actually carried a mask is re-marshalled. One without is
    written byte-for-byte, so the common case keeps the admin's own formatting.
- **FR-6.3 (comments as instructions)** The new handler, BFF route and UI each
  state why they are not an FR-7 violation, and the three existing
  "no content route here" comments are left intact and unweakened.

### NFR

- **NFR-1** No new Go dependency: `encoding/json`, `crypto/sha256`, `os`.
- **NFR-2** The read is a single `os.ReadFile` + `os.Stat`, no Docker call. It
  runs from an admin screen, not a hot path, but it must not wake a container.
- **NFR-3** The endpoint is additive. No existing handler, response shape or
  authorization decision changes. `/v1/admin/*` is already a gateway wildcard,
  so no parent-repo route work.
- **NFR-4** `managedPaths` is derived from one exported constant that
  `materializeModels` and `alignWorkspace` are the only writers behind. A test
  asserts the constant matches the paths those two functions actually write, so
  a future writer added to `materializeModels` cannot silently become
  admin-editable.

---

## Decisions

| ID | Decision | Rationale |
| --- | --- | --- |
| DEC-1 | Target agent is an explicit `agent` parameter, not the routing vehicle | The webapp pins `/alpha/v1/admin`; inheriting the vehicle would edit alpha's config while the admin believes they are fixing beta's |
| DEC-2 | Managed paths are re-established by running the existing materialization after the write, not by diff-and-reject | Correct by construction; no second copy of the merge rules to drift; a broken-config repair is not blocked by a spurious conflict |
| DEC-3 | `AuthorizeUserManagement` (tenant/subscription manager), not instance-admin only | Chosen by the product owner; consistent with the sibling `/v1/admin/users/*` endpoints. Accepted with FR-4.4 stated and FR-5.1 audit logging required |
| DEC-4 | No file-name parameter, ever | Keeps the route provably incapable of becoming the general content endpoint FR-7 forbids |
| DEC-5 | Workspace config only; template config out of scope | Chosen by the product owner. A bad template breaks every future member, a different risk class deserving its own spec |
| DEC-6 | Restart delivery reuses `bounceNow`, so `schedule` degrades to `notice` | Identical to every existing per-workspace site; the scheduler arms per scope and would bounce the whole scope for one member's config |

## Deferred

| ID | Idea | Why not now |
| --- | --- | --- |
| DEFER-1 | Single-slot backup (`config.json.bak`) + restore, or a small revision history | Real value for repair work, but it is a storage-lifecycle feature (the workspace root is bind-mounted into the container) and belongs in its own spec |
| DEFER-2 | Extend the editor to `templates/<agent>/config.json` | DEC-5 |
| DEFER-3 | Fix `adminUserFileKey`/`ADMIN_BASE` so the existing user-files endpoints stop always addressing `alpha` | Pre-existing bug this feature routes around (FR-1.6) rather than fixes; changing those endpoints' key derivation is its own change with its own blast radius |
| DEFER-4 | A picoclaw config schema, so the editor can validate semantically and offer completion | Needs a schema source of truth picoclaw does not publish |

---

## Traceability

| ID | Verified by |
| --- | --- |
| FR-1.2 | Unit: response `raw` is byte-identical to the file on disk |
| FR-1.3 | Unit: a syntactically broken `config.json` returns 200, `valid:false`, and an offset |
| FR-1.4 | Grep gate: no `name`/`path`/`file` parameter read in the handler |
| FR-1.5 | Unit: workspace dir with no `config.json` → 404 `not_provisioned` |
| FR-1.6 | Unit: `agent=beta` reads beta's file while the request is routed through alpha |
| FR-2.2 | Unit: unparseable body → 400 `invalid_json` with offset; array top level → 400 |
| FR-2.4 | Unit: body over 1 MiB → 413/400, nothing written |
| FR-2.5 | Unit: PUT with a stale `revision` → 409, file unchanged |
| FR-2.6 | Unit: write is atomic and the result is mode `0o600` |
| FR-2.7 | Unit: response `raw` equals the file after the reapply, not the submitted bytes |
| FR-3.1 | Unit: `managedPaths` in the response equals the exported constant |
| FR-3.3 | Unit: a PUT that changes `agents.defaults.model_name` lands, then the reapply restores the registry's value; response reports it |
| FR-3.3 (failure) | Unit: registry resolution error → 200 with `reapplied.ok:false`, write still on disk |
| FR-3.3 (repair) | Unit: pre-write file unparseable → write succeeds and the reapply then succeeds |
| FR-4.1 | Unit: a subscriptions-manager of another subscription → 403 on both verbs |
| FR-4.3 | Unit: non-UUID ids → 400; unknown `agent` → 400 |
| FR-5.1 | Unit: a successful PUT emits one log line with caller + key + sizes and **no** body |
| FR-6.1 | Unit: `restart=notice` raises a workspace notice with reason `config` and does not bounce; `restart=schedule` behaves as `notice` |
| FR-6.2 | Unit: legacy `model_list[0].api_keys` is `"***"` in the response and listed in `redactedPaths` |
| FR-6.2 (restore) | Unit: a masked round-trip keeps the stored key even when the reapply fails; object layout too; a mask with no stored key is written as typed; an unmasked document is not reformatted |
| NFR-4 | Unit: constant-vs-writers assertion over `materializeModels` + `alignWorkspace` |

---

## Reconciliation (what shipped)

Every FR above is implemented. Four things differ from the draft or are worth
recording because a later reader would otherwise have to rediscover them.

**FR-2.2 grew a case the draft missed.** `json.Unmarshal` of `null` into a
`map[string]any` **succeeds** and leaves the map nil, so a `config.json`
containing `null` would have read as a valid document and been accepted as one.
`parseConfigObject` now parses into `any` and asserts the map, which makes
`null`, `[]` and `42` all `ErrConfigNotObject`. The read path reports them as
`valid: false` rather than as an error, same as a syntax failure.
`TestReadInstanceConfigRejectsNonObject` and the `null` case in
`TestWriteInstanceConfigRejectsInvalidJSON` are the gates.

**FR-2.4 is enforced twice, at different sizes.** The document cap is 1 MiB in
the `docker` layer (authoritative). The handler additionally caps the request
**envelope** at 4 MiB via `io.LimitReader`, because JSON string escaping inside
`raw` inflates the same document and the envelope has to be bounded before it is
buffered. Both map to `413 too_large`.

**FR-6.2's safety argument was wrong, and a test caught it.** The draft claimed a
`"***"` could never reach disk because materialization replaces `model_list`
after every write. But that pass is best-effort — `TestWriteInstanceConfigSurvivesReapplyFailure`
exists precisely because it can fail — and an unresolvable registry is the
broken-instance case this feature targets. A legacy workspace round-tripping its
own redacted document therefore wrote the mask over its only copy of the key.
`unmaskAgainst` now restores masked values from the file BEFORE writing, so the
guarantee no longer depends on the reapply, and
`TestWriteInstanceConfigNeverStoresTheMask` fails if that regresses.

**FR-5.2 was only half-implemented.** `adminInstanceKey` answers a 403 or a
bad-parameter 400 and returns, so the refusal never reached
`logInstanceConfigWrite` — only 409/413 and success were audited. Since FR-4.4
leans on FR-5.1/5.2 to justify DEC-3's authz tier, the unlogged case was the one
that mattered most. The handler now audits from its own refusal branches
(`logInstanceConfigRefusal`, recording the targeted workspace from the raw
parameters since the key does not exist yet), and
`TestInstanceConfigRequiresUserManagement` asserts the line.

**FR-6.1's `schedule` handling was inverted from the draft (DEC-6).** The first
draft rejected `restart=schedule` with a 400. It now degrades to `notice`, which
is what `bounceNow` already does at every per-workspace site
(`internal/httpapi/restart_policy.go`). Matching the existing behaviour beat
inventing a rejection this one endpoint would have had.

**FR-1.4 / FR-6.3 gates, run:**

- `grep -nE 'Get\("(name|path|file)"\)' internal/httpapi/admin_instance_config.go`
  → no match. The handler reads no file-name parameter, so there is nothing that
  could redirect which file it opens.
- `git diff <base>..HEAD -- internal/httpapi/admin.go` → empty. The
  "no content route here" instruction at `admin.go:530` is untouched, as are the
  webapp's two.

**NFR-3, verified rather than cited.** The claim that `/v1/admin/*` needs no
gateway declaration came from restart-control's spec. Confirmed against the actual
configs: `deploy/dokploy/config.base.toml`, `deploy/prod/config.base.toml` and
`deploy/standalone/config.standalone.toml` each declare `path = "/v1/admin/*"`
with `methods = ["GET", "POST", "PUT", "DELETE"]` for every agent, so the `PUT`
this feature adds is already routed.

**Test status.** `go build ./... && go vet ./...` clean. `go test ./...`: the
`internal/docker` package still fails the same **8** `Lchown`-permission tests it
failed before this branch (`TestEnsureRunning*`, `TestCreateAddsReadOnlySecretsBind`,
`TestRestartWorkspaceRestartsAndRearms`, `TestScaleToZeroIdleStop`,
`TestContinuousDoesNotArmIdle`, `TestReconcileEnsuresContinuousWorkspaces`) —
sandbox noise per STATE.md L-001, identical to the recorded baseline. Every other
package is green, including the 18 new `docker` tests and the 13 new `httpapi`
tests.
