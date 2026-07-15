# shared-workspaces Design

Builds on `context.md` (CTX-SW-01..04) and the existing crab-shell-proxy
internals (`identity`, `docker.Manager`, `httpapi`, `config`). All IDs refer to
`spec.md`. **No code here — architecture and contracts only.**

---

## 1. Identity: the profile carries more than an email now

Today `identity.Resolver` returns `{AccID, Email}` from the mycelium profile.
Shared workspaces need two more facts from the same profile:

- **Principal owner id** — `profile.owners[principal].id` (a stable per-*human*
  UUID). Used to isolate each member's sessions inside a shared workspace
  (SW-09). Distinct from `accId` (the account).
- **Licensed resources** — `profile.licensedResources[]`, each entry (camelCase):
  `{ accId, tenantId, role, roleId, perm, verified, permitFlags, denyFlags }`.
  This is how "the profile has permission to access account X" is expressed
  (SW-06), and — with `perm`/ownership — who may create (SW-02).

**Extend the resolver** to expose:

```
Identity {
  AccID     string            // current/personal account (unchanged)
  OwnerID   string            // owners[principal].id  (NEW)
  Email     string            // traceability only (unchanged)
  IsStaff   bool              // profile.isStaff / isManager (NEW, elevated path)
  Licensed  []LicensedGrant   // {AccID, Perm, Verified} (NEW)
}
```

Plus authorization predicates (pure, table-testable):

- `CanReadWorkspace(id, accId)` → `IsStaff` OR a `Licensed` entry with
  `AccID==accId && Verified && Perm>=Read`.
- `CanCreateWorkspace(id, accId)` → `IsStaff` OR a `Licensed` entry with
  `AccID==accId && Verified && Perm==Write` (tenant ownership also qualifies if
  surfaced in the profile).

**Seam / SDK note (ties back to the earlier "do we need the mycelium SDK?"
question):** parsing `licensedResources` + `owners[].id` is the point where the
fallback decoder stops being trivial. Two options, decided at Tasks time:
(a) extend `FallbackResolver` to decode these fields (still no external dep), or
(b) adopt the parallel Go mycelium SDK behind the existing `Resolver` interface.
The exact `perm` enum values (`Read`/`Write`/…) and the `LicensedResources`
JSON envelope (it is an enum in mycelium core) MUST be confirmed against the
real injected profile before implementing — do not guess.

---

## 2. New endpoint: create a shared workspace (SW-01..04)

```
POST /v1/workspaces          (routed through mycelium like the other paths)
Body: { "acc_id": "<uuid>" }
```

Flow:
1. Resolve agent from `x-mycelium-service-name` + validate the injected bearer
   token (same as every route today).
2. Resolve identity from the profile; `401` if undecodable.
3. Validate `acc_id` present + well-formed; `400` otherwise.
4. `CanCreateWorkspace(id, acc_id)` → `403` if not.
5. `EnsureRunning(agent, key=SanitizeID(acc_id), kind=shared)` — reuses the
   existing provision → create → health-wait path, adding the shared label +
   marker. Idempotent (start-if-stopped), so a repeat returns `200`, first
   creation `201`.
6. Respond with the workspace identity (name + acc_id + agent + status).

Naming is the same scheme as per-user: `picoclaw-<agent>-<sanitized-accId>`.
accIds are globally unique UUIDs, so a shared account's container never collides
with a personal one. What distinguishes them is a **label**, not the name.

---

## 3. Chat & history routing (SW-05..10)

`/v1/chat/completions` and `/v1/sessions/history` gain an optional
`workspace_acc_id` (body field for chat; query param for history):

```
if workspace_acc_id is set:
    if not CanReadWorkspace(id, workspace_acc_id): 403
    key      = SanitizeID(workspace_acc_id)
    sessionK = sha256(workspace_acc_id :: id.OwnerID :: session_id)   # per-user
    require the shared container to already EXIST:
        if inspect(picoclaw-<agent>-<key>) missing: 409  (no on-demand shared create)
    route to picoclaw-<agent>-<key>
else:                                   # unchanged, private per-user path
    key      = SanitizeID(id.AccID)
    sessionK = sha256(id.AccID :: session_id)
    on-demand create allowed (today's behavior)
```

- **Per-user sessions inside a shared workspace (SW-09):** the session key mixes
  the member's `OwnerID`, so picoclaw records/serves each member's conversations
  separately in the shared volume's `workspace/sessions/`, while skills, agent
  config, model, and memory (shared parts of the volume) are common.
- **No on-demand shared create (SW-07):** the private path may cold-start a
  container; the shared path must NOT — it requires a prior `POST`. The manager
  needs an "ensure-running only if it already exists" mode (a flag on
  `EnsureRunning`, or a separate `EnsureExistingRunning`) so a chat can start a
  *stopped* shared container (scale-to-zero) but never *create* one.
- **History (SW-10):** `SessionsDir(agent, key)` + the same `.meta.json`
  scope-marker scan; the marker now embeds the owner-scoped session key, so a
  member only sees their own transcripts.

---

## 4. Lifecycle, provisioning, labels (SW-04, SW-11)

- **Lifecycle:** inherits the agent's `mode`/`idleTimeout` from `config.yaml`
  (CTX-SW-04). No new fields. For scale-to-zero, any member's chat re-arms the
  container's idle timer via the existing per-container mechanism; the shared
  container is stopped only after `idleTimeout` with no member activity.
- **Provisioning:** identical to per-user (template clone → workspace align →
  model injection from env → non-root → chown). A shared workspace is just a
  container of the same agent with a different key.
- **Labels + marker:** add `crab-shell.kind=shared` (vs `personal`) on the
  container, and extend `.crab-owner.json` for shared dirs to record
  `{ kind: "shared", accId, createdByEmail, createdByAccId }` for audit (SW-11).

---

## 5. External wiring (mycelium)

- **`mycelium/config.standalone.toml`** (in the monorepo) gains a new
  `[[picoclaw-*.path]]` block for `/v1/workspaces` (method `POST`), same
  `protectedByRoles`/secret as the other paths, for each agent. Without it the
  gateway won't forward the new route. (Documented here; lives in the parent
  repo, not this submodule.)
- No other gateway change: the profile already carries `licensedResources`; the
  proxy just starts reading them.

---

## 6. Component / file map (where things land — for planning)

| Concern | Location |
| --- | --- |
| Identity: OwnerID, IsStaff, Licensed[], Can*Workspace predicates | `internal/identity` |
| "ensure only if exists" + `kind` label + shared marker | `internal/docker` (`manager.go`, `provision.go`) |
| `POST /v1/workspaces` handler + authz | `internal/httpapi` |
| `workspace_acc_id` on chat/history + shared routing + session key | `internal/httpapi` (+ `identity.SessionKey` variant) |
| Config: none (lifecycle inherited) | — |

---

## 7. Risks & security

- **R1 — deliberate de-isolation, so authz must be airtight.** A shared
  workspace intentionally shares one container across members; the ONLY thing
  keeping a non-member out is the `licensedResources` check on the unforgeable
  profile. That check must be server-side, require `verified=true` and the right
  `perm`, and never trust a client-supplied field beyond `workspace_acc_id`
  (which is only ever used *after* authorization). Get this wrong and it's a
  cross-account data breach.
- **R2 — profile parsing fidelity.** `licensedResources` is richer/enum-shaped
  in mycelium; decode it against a real profile (or via the SDK), not a guess
  (see §1 note). A silent mis-parse that yields an empty grant list fails safe
  (everything 403), but the inverse (over-permissive parse) must be tested.
- **R3 — session-key migration.** Private sessions use
  `sha256(accId::session_id)`; shared use `sha256(accId::ownerId::session_id)`.
  These are different namespaces — a workspace is only ever one or the other, so
  no collision, but tests must confirm a member's shared history and private
  history never bleed into each other.
- **R4 — own-account edge.** If `workspace_acc_id` equals the caller's personal
  `accId`, treat as a normal request (the "shared" framing adds nothing); the
  authz check still passes (they're licensed into their own account) and
  behavior is the private path.
