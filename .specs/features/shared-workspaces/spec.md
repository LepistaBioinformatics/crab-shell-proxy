# shared-workspaces Specification

## Problem Statement

crab-shell-proxy isolates one picoclaw container per `(agent, user)`, keyed by
the caller's own account id and created on demand. That is exactly right for
private, personal agents — but there is no way to run a **shared** agent: a
single workspace (shared skills, memory, agent config) that a *group* of users —
everyone licensed into a given account — can all talk to. We want an explicit,
authorized way to provision such a shared workspace and let permitted users
chat with it, while keeping each member's own conversations private.

## Goals

- [ ] A `POST` endpoint that provisions a shared workspace for a given `accId`,
      authorized to callers with write/owner authority over that account.
- [ ] Permitted users (profile `licensedResources` grants access to that
      `accId`) can chat with the shared workspace; others are rejected.
- [ ] Shared workspaces are created **only** by the endpoint (not on demand from
      chat), unlike per-user workspaces.
- [ ] Inside a shared workspace, conversations are isolated per user; skills,
      agent, model, and memory are shared.
- [ ] Shared workspaces reuse the agent's existing lifecycle, provisioning,
      non-root, and OpenAI-surface behavior — no parallel machinery.

## Out of Scope

| Item | Reason |
| --- | --- |
| On-demand creation of shared workspaces from a chat | CTX-SW-04 / user: only the `POST` creates them |
| Changes to how mycelium builds/injects the profile or `licensedResources` | Consumed as-is |
| Cross-agent shared workspaces | Belongs to the agent the request routed to |
| A UI to manage shared workspaces | API only; admin UI is out of scope |
| Deleting/garbage-collecting shared workspaces | Separate concern (see Deferred) |

---

## User Stories

### P1: Create a shared workspace ⭐ MVP

**User Story**: As an account owner, I want to POST an `accId` and get a shared
workspace provisioned for it, so members of that account can later chat with a
shared agent.

**Why P1**: Nothing else works until a shared workspace can be created.

**Acceptance Criteria**:

1. WHEN `POST <create endpoint>` is called with body `{ "acc_id": "<uuid>" }` and
   the caller's profile has write/owner authority over `<uuid>` THEN the system
   SHALL provision and start the shared container `picoclaw-<agent>-<uuid>` for
   the agent resolved from `x-mycelium-service-name`, and respond `201`.
2. WHEN the shared workspace for that `(agent, acc_id)` already exists THEN the
   endpoint SHALL be idempotent (start if stopped, respond `200`), not error.
3. WHEN the caller lacks write/owner authority over `acc_id` THEN the system
   SHALL respond `403` and provision nothing.
4. WHEN the profile header is missing/undecodable, or `acc_id` is missing/not a
   valid id THEN the system SHALL respond `401` / `400` respectively.
5. WHEN the container is created THEN it SHALL be labeled as a shared workspace
   (distinct from personal ones) and record who created it.

**Independent Test**: With a profile granting `Write` on `acc-team`, POST
`{acc_id:"acc-team"}` to the alpha route → `picoclaw-alpha-acc-team` appears in
`docker ps` (non-root); a second POST returns `200`; a profile with only `Read`
gets `403`.

---

### P1: Chat with a shared workspace (authorized) ⭐ MVP

**User Story**: As a member of an account, I want to chat with its shared
workspace by naming it in my request, so I use the shared agent instead of my
private one.

**Why P1**: The shared workspace is useless if permitted members can't reach it.

**Acceptance Criteria**:

1. WHEN `POST /v1/chat/completions` includes `"workspace_acc_id": "<uuid>"` and
   the caller's profile `licensedResources` grants access (verified, ≥Read) to
   `<uuid>` THEN the system SHALL route the turn to `picoclaw-<agent>-<uuid>`.
2. WHEN the caller's profile does NOT grant access to `<uuid>` THEN the system
   SHALL respond `403` and SHALL NOT create or touch any container.
3. WHEN `workspace_acc_id` names a shared workspace that was never created THEN
   the system SHALL respond `409` (or `404`) — shared workspaces are not
   created on demand.
4. WHEN a request omits `workspace_acc_id` THEN behavior SHALL be unchanged
   (private per-user workspace keyed by the caller's own account).
5. WHEN two different members chat with the same shared workspace THEN each
   member's conversation history SHALL be isolated
   (`sessionKey = sha256(<acc_id>::<ownerId>::<session_id>)`), while both share
   the same skills / agent / memory.

**Independent Test**: Two profiles both licensed into `acc-team` chat with
`workspace_acc_id:"acc-team"`; both hit `picoclaw-alpha-acc-team`; each sees only
their own history via `/v1/sessions/history`; a third profile not licensed into
`acc-team` gets `403`.

---

### P2: History for a shared workspace

**User Story**: As a member, I want `/v1/sessions/history` to return *my* history
within a shared workspace, so the client renders my past conversations there too.

**Acceptance Criteria**:

1. WHEN `GET /v1/sessions/history?session_id=...&workspace_acc_id=<uuid>` is
   called with access to `<uuid>` THEN the system SHALL return that member's
   history for the shared workspace (scoped by owner id), not other members'.
2. WHEN the caller lacks access to `<uuid>` THEN the system SHALL respond `403`.

---

### P3: Observability of shared workspaces

**User Story**: As an operator, I want to tell shared workspaces apart from
personal ones and see who created each, for auditing.

**Acceptance Criteria**:

1. WHEN a shared container is created THEN it SHALL carry a label marking it
   shared and a per-dir marker recording the creator (email/accId) and kind.

---

## Edge Cases

- WHEN concurrent `POST`s (or first chats) target the same shared workspace THEN
  the container SHALL be created exactly once (single-flight).
- WHEN a chat names `workspace_acc_id` equal to the caller's OWN personal
  account THEN it SHALL be treated as a normal request (no privilege change).
- WHEN the profile lists the `accId` but with `verified=false` or an insufficient
  permission THEN access SHALL be denied.
- WHEN the shared container is mid-stop (scale-to-zero) and a member chats THEN
  the request SHALL serialize and cleanly restart it (existing per-container
  lock).
- WHEN `acc_id` in the body contains characters invalid for a container name
  THEN it SHALL be sanitized identically to the per-user path.

---

## Requirement Traceability

| ID | Story | Phase | Status |
| --- | --- | --- | --- |
| SW-01 | P1 create: provision by accId + agent | Design | Pending |
| SW-02 | P1 create: write/owner authorization | Design | Pending |
| SW-03 | P1 create: idempotent | Design | Pending |
| SW-04 | P1 create: 401/400/403 handling + shared label/marker | Design | Pending |
| SW-05 | P1 chat: route to shared by workspace_acc_id | Design | Pending |
| SW-06 | P1 chat: authorize via licensedResources (≥Read, verified) | Design | Pending |
| SW-07 | P1 chat: not-created ⇒ 409 (no on-demand shared create) | Design | Pending |
| SW-08 | P1 chat: omitted ⇒ unchanged per-user behavior | Design | Pending |
| SW-09 | P1 chat: per-user session isolation inside shared workspace | Design | Pending |
| SW-10 | P2 history scoped to member within shared workspace | Design | Pending |
| SW-11 | P3 shared label + creator marker | Design | Pending |
| SW-12 | Edge: single-flight; own-account; verified/perm; stop race; sanitize | Design | Pending |

**Status values:** Pending → In Design → In Tasks → Implementing → Verified

---

## Success Criteria

- [ ] An owner can POST an `accId` and get a running, non-root shared container.
- [ ] Two licensed members chat the same shared workspace, share skills/memory,
      and each keeps a private conversation history.
- [ ] An unlicensed account is refused (`403`) at both create and chat.
- [ ] A chat to an uncreated shared workspace is refused (`409`), proving shared
      workspaces are never created on demand.
- [ ] Omitting `workspace_acc_id` leaves today's per-user flow byte-for-byte
      unchanged.
