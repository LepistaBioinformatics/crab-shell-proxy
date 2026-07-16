# agent-customization Specification

Builds on `context.md` (CTX-AC-01..04). Two proxy capabilities: **(A)** seed
custom agent template files into per-user workspaces, and **(B)** inject
per-`(user, agent)` secrets via endpoints, applied to every workspace of that
pair. **Frontend is out of scope** — endpoints + the `context.md` contract only.

## Problem Statement

Today a user's picoclaw workspace is seeded from `config.json` + `.security.yml`
only; the template's `AGENT.md`/`SOUL.md`/`skills/`/`memory/` never reach it, so
the agent always starts as picoclaw's native default — no way to customize its
persona/skills per agent. And there is no way for a user to give the agent a
secret (an API key a skill needs) except by typing it into the chat, which is
insecure and does not persist. We want (A) operator-configurable startup files
per agent, and (B) a secure, out-of-band secret injection that persists for the
user across future sessions of the same agent.

## Goals

- [ ] Provisioning seeds an allowlist of agent template workspace files
      (`AGENT.md`, `SOUL.md`, `USER.md`, `memory/`, `skills/`) from
      `data/templates/<agent>/workspace/`, never `sessions/` or runtime state.
- [ ] `POST /v1/secrets` injects a secret (structured → `.security.yml`;
      arbitrary → a generic workspace store), scoped to `(userAccId, role)`.
- [ ] The secret is applied to every workspace of that `(user, agent)` pair,
      now and in future sessions/subscriptions.
- [ ] Injecting restarts the caller's agent container so it takes effect
      immediately.
- [ ] `GET`/`DELETE /v1/secrets` let a management UI list (names only) and clear
      secrets, authorized by the same chain as chat.

## Out of Scope

| Item | Reason |
| --- | --- |
| Frontend / management UI | Built separately (user); we ship endpoints + contract |
| Editing agent templates via API/UI | CTX-AC-01: operator files, not user-editable |
| Encrypting the per-user secret store at rest | Deferred; same posture as today's `.security.yml` |
| Returning secret values over the API | Write-only; UI shows names/slots only |
| Changes to picoclaw or its `.security.yml`/skill parsing | Consumed as-is |

---

## User Stories

### P1: Custom agent startup files ⭐ MVP
**Story**: As an operator, I put `AGENT.md`/`SOUL.md`/skills in a template so the
agent starts customized for every user of that agent.

**Acceptance Criteria**:
1. WHEN a user workspace is first provisioned THEN the proxy SHALL copy the
   allowlisted template workspace files (`AGENT.md`, `SOUL.md`, `USER.md`,
   `memory/**`, `skills/**`) into the workspace, in addition to `config.json` +
   `.security.yml`.
2. WHEN seeding THEN it SHALL NEVER copy `sessions/`, `logs/`, or `.picoclaw.pid`
   (isolation + runtime-state invariant).
3. WHEN a template file is absent THEN seeding SHALL skip it without error
   (partial templates are valid).
4. WHEN the workspace already exists (returning user) THEN seeding SHALL NOT
   overwrite the user's evolved files (same "seed only on first provision" rule
   as today).

### P1: Inject a secret ⭐ MVP
**Story**: As a member, from the management UI I inject a secret into my agent's
workspace, out of band from chat; it persists for me on this agent.

**Acceptance Criteria**:
1. WHEN `POST /v1/secrets` is called with a valid profile, `tenant_id` +
   `subs_acc_id`, and passes `WithWriteAccess().OnTenant().WithRoles([agent]).OnAccount().GetRelatedAccountOrError()`
   THEN the system SHALL store the secret in the `(userAccId, role)` store and
   respond `200`; a failing chain ⇒ `403`; missing/invalid body ⇒ `400`; no
   profile ⇒ `401`.
2. WHEN the request selects `format` THEN the secret SHALL be written to that
   sink: `native` → the `.security.yml` slot (`web.<provider>` or
   `model_list.<model>.api_keys`; `channel_list` slots are rejected — protects the
   proxy↔picoclaw pico token, impl narrowing); `dotenv` → `.env`; `json` →
   `secrets.json`; `file` → `secrets/<name>`. Caller picks the format to match the
   consuming skill.
3. WHEN `format=native` names a slot/model absent from the config THEN it SHALL
   be rejected `400`; other formats accept any safe-charset `name`.
4. WHEN a secret is injected THEN the system SHALL apply it to the caller's
   current workspace AND persist it to the `(userAccId, role)` store, and SHALL
   restart the caller's agent container so picoclaw re-reads it (CTX-AC-04).
5. WHEN the same user later chats the same agent in ANY subscription THEN that
   workspace SHALL receive the stored secret at provision/ensure (CTX-AC-03).
6. WHEN secrets are exposed to the agent THEN the sink files SHALL be
   **read-only from the agent's (picoclaw, non-root) perspective** — picoclaw can
   read them but cannot modify, delete, or replace them (only the proxy, via the
   endpoints, may). Enforced at the mount/permission layer, not by convention.

### P2: List / clear secrets
**Story**: As a member, the UI shows which secrets are set and lets me clear one.

**Acceptance Criteria**:
1. WHEN `GET /v1/secrets?tenant_id&subs_acc_id` is called (authorized) THEN it
   SHALL return the set secret **names only, never values**, grouped by format
   (`{dotenv:[…], json:[…], native:[…], file:[…]}`), parsed server-side from each
   sink (left-of-`=` for dotenv, JSON keys, set `.security.yml` slots, filenames).
2. WHEN `DELETE /v1/secrets/<name>` is called (authorized) THEN it SHALL remove
   that secret from the `(userAccId, role)` store + current workspace and restart
   the container.
3. WHEN the caller is not authorized for `(tenant, subs, agent)` THEN `GET`/
   `DELETE` SHALL respond `403`.

---

## Edge Cases
- Concurrent injects for the same `(user, agent)` SHALL not corrupt the store
  (serialize writes; reuse the per-container lock for the restart).
- A `structured` secret naming a slot/model that isn't in the config SHALL be
  rejected `400` (don't write unknown slots blindly).
- A `generic` secret name SHALL be validated (safe key charset) before it becomes
  a store key / file entry.
- Restart-on-inject SHALL disarm/re-arm the scale-to-zero idle timer like a turn,
  so it can't race a stop.
- Injecting when the container isn't running (scaled to zero) SHALL just write
  the store (next chat cold-starts with it) — no spurious start required.

---

## Requirement Traceability

| ID | Story | Component | Status |
| --- | --- | --- | --- |
| AC-01 | Seed allowlisted template workspace files (never sessions/) | `internal/docker/provision.go` (+ `config`) | Pending |
| AC-02 | `POST /v1/secrets` inject (structured + generic), authz | `internal/httpapi` | Pending |
| AC-03 | Per-`(userAccId, role)` secret store outside the tenant tree | `internal/docker` (+ `config` path) | Pending |
| AC-04 | Apply stored secrets to a workspace at provision/ensure | `internal/docker/provision.go` | Pending |
| AC-05 | `native` sink: merge into `.security.yml` slots (reject unknown slot) | `internal/docker/provision.go` | Pending |
| AC-06 | Multi-format sinks: `dotenv` (.env), `json` (secrets.json), `file` (secrets/<name>) | `internal/docker` | Pending |
| AC-07 | Restart the caller's container on inject/delete | `internal/docker/manager.go` (+ handler) | Pending |
| AC-08 | `GET` (names per format, no values) + `DELETE` secrets | `internal/httpapi` | Pending |
| AC-09 | mycelium routes for `/v1/secrets` (GET/POST/DELETE) | `mycelium/config.standalone.toml` (parent) | Pending |
| AC-10 | Secret sinks read-only to the agent (RO mount / root-owned perms) | `internal/docker/manager.go` (+ provision) | Pending |

**Status values:** Pending → In Design → In Tasks → Implementing → Verified

---

## Success Criteria
- [ ] A template with a custom `AGENT.md` + a skill makes a freshly provisioned
      agent start with that persona/skill (visible in the workspace + behavior).
- [ ] An authorized user injects a secret; the agent container restarts and a
      skill can read it; an unauthorized caller gets `403`.
- [ ] The same user chatting the same agent in a second subscription gets the
      secret applied automatically.
- [ ] `GET /v1/secrets` lists names without values; `DELETE` removes one.
- [ ] `docker build --network=host .` passes `go vet` + `go test ./...`.
