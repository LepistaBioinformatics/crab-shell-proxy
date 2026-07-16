# tenant-scoped-workspaces Specification

Builds on `context.md` (CTX-TSW-01..09). Replaces crab-shell-proxy's flat
per-user data layout with a tenant→subscription→agent→user hierarchy, seeded by
a mycelium webhook and gated by mycelium-SDK profile filtering.

## Problem Statement

crab-shell-proxy keys one picoclaw container per `(agent, user)` on the caller's
own `accId`, created on demand, under `<dataRoot>/<agentKey>/<accId>/`. There is
no notion of *which subscription/tenant* a workspace belongs to, and access is
implicit (anyone the gateway authenticates gets a personal workspace). We want
workspaces to be **provisioned against a subscription account** (seeded by a
mycelium webhook) and **reachable only by users the profile proves are licensed**
into that subscription for that agent — while keeping each user's workspace
fully isolated from every other user's.

## Goals

- [ ] A `POST /v1/accounts` endpoint that receives the mycelium
      `subscriptionAccount.created` webhook and scaffolds a subscription
      workspace root, authenticated by the webhook's shared secret.
- [ ] A new on-disk layout keyed by
      `tenants/<tenant_id>/subscriptions/<subs_acc_id>/agents/<role_slug>/users/<user_acc_id>`,
      with total per-user isolation.
- [ ] `POST /v1/chat/completions` requires `tenant_id` + `subs_acc_id`, verifies
      the caller via the SDK filtering chain, and routes to that user's isolated
      workspace (created lazily on first chat).
- [ ] A discovery `GET` endpoint that lists, from the caller's
      `licensed_resources`, the `(tenant, subscription, agent)` tuples they may
      use — so the client knows what to pass to chat.
- [ ] Reuse the existing provisioning, non-root, lifecycle, and OpenAI-surface
      behavior — only the path/keying and the authorization gate change.

## Out of Scope

| Item | Reason |
| --- | --- |
| GC/deletion of dirs on `*.deleted` webhooks | CTX-TSW-09; consume only `.created` |
| `/v1/workspaces` (shared-workspaces) | Separate, deferred feature |
| Authz filtering on `/v1/models` | "demais endpoints depois" (CTX-TSW-09) |
| Authz filtering on `/v1/sessions/history` (path change is in-scope) | filtering "depois"; path must move now or it breaks |
| Changes to how mycelium builds the profile / fires the webhook / names roles | Consumed as-is |
| A personal (non-subscription) chat fallback | CTX-TSW-05: chat now requires a subscription |

---

## User Stories

### P1: Scaffold a subscription workspace from the webhook ⭐ MVP

**User Story**: As the mycelium gateway, when a subscription account is created I
POST its account object to `/v1/accounts`, so the proxy provisions a workspace
root for it.

**Why P1**: nothing downstream can route until the subscription root exists.

**Acceptance Criteria**:

1. WHEN `POST /v1/accounts` is called with a valid webhook secret and a body that
   is a mycelium `Account` with `accountType.subscription.tenantId` set THEN the
   system SHALL create `tenants/<tenant_id>/subscriptions/<subs_acc_id>/agents/`
   and respond `2xx`.
2. WHEN the same webhook fires again (retry) THEN the endpoint SHALL be
   idempotent (`mkdir -p` semantics; no error, `200`).
3. WHEN the secret is missing/incorrect THEN the system SHALL respond `401` and
   create nothing.
4. WHEN the body is not a subscription account (missing `id` or
   `accountType.subscription.tenantId`) THEN the system SHALL respond `400` and
   create nothing.
5. WHEN the endpoint runs THEN it SHALL NOT require `x-mycelium-service-name` or a
   per-agent bearer token (the webhook does not carry them) and SHALL be
   agent-agnostic (creates the `agents/` parent, not any specific `<role_slug>`).

**Independent Test**: POST a captured subscription `Account` JSON with the
configured secret → the scaffold dir tree appears; a second POST returns `200`;
a wrong secret returns `401`.

---

### P1: Chat into a subscription workspace (authorized, isolated) ⭐ MVP

**User Story**: As a licensed member, I chat by naming the `tenant_id` +
`subs_acc_id`, so I reach *my own* isolated workspace under that subscription's
agent.

**Why P1**: the workspace is useless if licensed members can't reach it, and
unsafe if unlicensed callers can.

**Acceptance Criteria**:

1. WHEN `POST /v1/chat/completions` includes `tenant_id` and `subs_acc_id` and the
   caller's profile passes
   `WithWriteAccess().OnTenant(tenant_id).WithRoles([agentName]).OnAccount(subs_acc_id).GetRelatedAccountOrError()`
   (agentName resolved from `x-mycelium-service-name`) THEN the system SHALL route
   the turn to
   `tenants/<tenant_id>/subscriptions/<subs_acc_id>/agents/<agentName>/users/<profile.accId>`,
   creating the `<agentName>/users/<accId>` leaves lazily on first use.
2. WHEN the filtering chain fails (no matching licensed resource: wrong tenant,
   wrong account, missing role, or read-only) THEN the system SHALL respond `403`
   and touch no container. NOTE: `verified` is intentionally NOT enforced (user
   decision 2026-07-16) — an unverified/pending-invite grant that otherwise
   matches IS accepted. The SDK chain does not filter on `verified` and the
   gateway injects unverified grants (`fetch_profile_from_email` → `was_verified=None`).
3. WHEN `tenant_id` or `subs_acc_id` is absent/malformed THEN the system SHALL
   respond `400`.
4. WHEN `profile.accId == subs_acc_id` (caller acting AS the subscription, not as
   an individual member) THEN the system SHALL respond `403` (per-user isolation
   requires an individual identity — the account-switching guard, CTX-TSW-02).
5. WHEN two different licensed members chat the same `(tenant, subscription,
   agent)` THEN each SHALL get a distinct container + dir keyed by their own
   `profile.accId`; neither can read the other's history or workspace.
6. WHEN the subscription scaffold for `(tenant, subs_acc_id)` does not exist THEN
   the system SHALL respond `409`/`404` (workspaces are not created on demand by
   chat — only the `/v1/accounts` webhook creates the subscription root).

**Independent Test**: two profiles licensed (write, verified, role=`alpha`) into
`subs-X` under `tenant-T` chat with `{tenant_id:T, subs_acc_id:X}` → each hits
`.../subscriptions/X/agents/alpha/users/<own-accId>`; histories are separate; a
read-only / wrong-tenant / unlicensed profile gets `403`; a profile with
`accId==X` gets `403`.

---

### P1: Discover accessible subscriptions ⭐ MVP

**User Story**: As a member, I GET the list of `(tenant, subscription, agent)` I
may use, so my client knows what `tenant_id`/`subs_acc_id` to send to chat.

**Acceptance Criteria**:

1. WHEN the discovery `GET` (proposed `GET /v1/subscriptions`) is called with a
   valid profile THEN the system SHALL return, from `licensed_resources`, the
   tuples `{tenant_id, subs_acc_id, role, verified, perm}` the caller holds,
   optionally annotated with whether the on-disk subscription scaffold exists.
2. WHEN the profile is missing/undecodable THEN the system SHALL respond `401`.
3. The list SHALL be computed from `licensed_resources` (source of truth), NOT
   from an on-disk scan (leaf dirs are lazy — CTX-TSW-06).

**Independent Test**: a profile licensed into two subscriptions returns both
tuples; an unlicensed profile returns an empty list; no leaf dirs need to exist.

---

### P2: History follows the new layout

**User Story**: As a member, `/v1/sessions/history` returns my history from my
isolated workspace under the new layout.

**Acceptance Criteria**:

1. WHEN `GET /v1/sessions/history?session_id=...&tenant_id=...&subs_acc_id=...` is
   called THEN the system SHALL read from
   `.../subscriptions/<subs_acc_id>/agents/<agentName>/users/<profile.accId>/workspace/sessions`.
2. History's authorization filtering (the full SDK chain) MAY be deferred with the
   other endpoints, but its **path** SHALL move to the new layout in this feature
   (CTX-TSW-09) so it does not read a stale location.

---

## Edge Cases

- WHEN concurrent first-chats race for the same user leaf dir THEN provisioning
  SHALL create it exactly once (existing per-container single-flight).
- WHEN the webhook body's `accountType` is a non-subscription variant (user,
  manager, …) THEN `/v1/accounts` SHALL `400` (no `subscription.tenantId`).
- WHEN a `subs_acc_id`/`tenant_id`/`accId` contains characters invalid for a path
  or container name THEN it SHALL be sanitized identically to today's path.
- WHEN a licensed member's role does not match any configured agent
  (`licensed_resources.role` ∉ agent keys) THEN the chain's `WithRoles` yields no
  grant ⇒ `403` (CTX-TSW-07 contract not satisfied).

---

## Requirement Traceability

| ID | Story | Phase | Status |
| --- | --- | --- | --- |
| TSW-01 | P1 webhook: scaffold subscription root from `Account` payload | Design | Pending |
| TSW-02 | P1 webhook: shared-secret auth (header/query), no agent-token | Design | Pending |
| TSW-03 | P1 webhook: idempotent; 400 on non-subscription; agent-agnostic | Design | Pending |
| TSW-04 | New layout `tenants/<t>/subscriptions/<s>/agents/<role>/users/<u>` | Design | Pending |
| TSW-05 | Chat: require tenant_id + subs_acc_id; SDK filtering chain (write+tenant+role+account) | Design | Pending |
| TSW-06 | Chat: route to per-user isolated workspace; lazy leaf creation | Design | Pending |
| TSW-07 | Chat: account-switching guard (accId==subs ⇒ 403) | Design | Pending |
| TSW-08 | Chat: not-scaffolded ⇒ 409/404 (no on-demand subscription create) | Design | Pending |
| TSW-09 | Discovery GET from licensed_resources | Design | Pending |
| TSW-10 | Session key 2-part sha256(accId::session_id); supersedes SW-09 | Design | Pending |
| TSW-11 | History path moved to new layout (filtering deferred) | Design | Pending |
| TSW-12 | Role identity contract (agent key == role_slug == licensed_resources.role) | Design | Pending |
| TSW-13 | Replace personal on-demand flow; reconcile updated to new layout | Design | Pending |

**Status values:** Pending → In Design → In Tasks → Implementing → Verified

---

## Success Criteria

- [ ] A `subscriptionAccount.created` webhook scaffolds the subscription root
      (non-root, correct tree); a retry is a clean `200`.
- [ ] A licensed member (write, verified, role=agent) chats with
      `tenant_id`+`subs_acc_id` and reaches their own isolated container/dir.
- [ ] A second member chatting the same subscription gets a separate dir/history;
      neither can read the other's.
- [ ] Unlicensed / read-only / wrong-tenant / `accId==subs` callers get `403`.
- [ ] Chatting a never-scaffolded subscription gets `409`/`404`.
- [ ] The discovery GET lists a caller's subscriptions from the profile alone.
- [ ] `docker build --network=host .` passes `go vet` + `go test ./...`.
