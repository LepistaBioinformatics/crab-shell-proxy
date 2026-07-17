# tenant-scoped-workspaces — Discussion Context (gray-area decisions)

Captured during Specify (discuss). These are the user's explicit answers to the
design forks; spec/design/tasks build directly on them. Grounded in the mycelium
webhook/profile source read at the **deployed ref** `d471dc7a` (9.0.0-rc.5) and
the published `mycelium-sdk-go@v0.1.0` already wired into `internal/identity`.

Background: today crab-shell-proxy keys one container per `(agent, user)` on the
caller's own `accId`, created **on demand** on first chat, under a flat
`<dataRoot>/<agentKey>/<accId>/` layout. This feature replaces that with a
tenant→subscription→agent→user hierarchy, seeded by a mycelium webhook and gated
by profile filtering.

---

## CTX-TSW-01: New data layout — tenant/subscription/agent/user, total per-user isolation

**Decision:** The per-user workspace path becomes:

```
<dataRoot>/tenants/<tenant_id>/subscriptions/<subs_acc_id>/agents/<role_slug>/users/<user_acc_id>/
```

`accounts` (the user's original wording) is renamed to `subscriptions` to avoid
confusion, because a *user* account id appears later in the same path under
`users/`. Isolation is by the **whole chain** — there are **no shared
workspaces**; one user never reaches another user's dir.

**Why:** the user rejected the shared-container model — "o isolamento deve ser
total por usuário". Each `(tenant, subscription, agent, user)` tuple is a
distinct workspace/container.

---

## CTX-TSW-02: `users/<id>` segment = `profile.accId` (with an account-switching guard)

**Decision:** the `<user_acc_id>` segment is the caller's `profile.accId`
(top-level current-account id). The user chose this over `owners[principal].id`
despite the flagged risk.

**Known risk + mitigation:** mycelium's `Profile.accId` doc-comment says that for
subscription accounts the id "should be propagated along the application flow"
(account-switching) — so `profile.accId` *could* equal the subscription account
id, collapsing every switched member into one `users/` dir (cross-user leak). To
close this without changing the chosen id, the chat/history path SHALL reject a
request where `profile.accId == subs_acc_id` (caller acting AS the subscription,
not as an individual member).

**User confirmation (2026-07-16):** the user asserts this is *unreachable in
practice* — "o profile sempre será de um usuário, não uma conta de subscrição",
so `profile.accId` is never a subscription account id even though both are UUIDs.
The `403` guard is kept anyway as belt-and-suspenders (expected-never-hit).
Alternative on record if this ever proves brittle: switch the segment to
`owners[principal].id`.

---

## CTX-TSW-03: `POST /v1/accounts` (webhook) creates the scaffold only; `<role_slug>`/`users` are lazy

**Decision:** a new `POST /v1/accounts` endpoint receives the mycelium
`subscriptionAccount.created` webhook and creates only the subscription scaffold:

```
<dataRoot>/tenants/<tenant_id>/subscriptions/<subs_acc_id>/agents/
```

The `<role_slug>` and `users/<user_acc_id>` leaves are created **lazily on the
user's first chat** (existing on-demand provisioning, relocated to the new path).

**Why (source-forced):** the webhook payload is the bare `Account` object and
carries **no role/guest-role info** (only `id` = subs_acc_id and, nested,
`accountType.subscription.tenantId`). There is also **no** guest-role/invite
webhook trigger in mycelium (only 6 account-lifecycle triggers). So the webhook
cannot know `<role_slug>` — the scaffold is the most it can create. Idempotent
(retries re-fire `.created`): a repeat is a no-op `200` (`mkdir -p` semantics).

**Reversed 2026-07-16 — chat also creates the scaffold on demand:** the webhook
only fires for subscriptions created *after* it is registered (mycelium
`skip`s the `.created` event otherwise), so pre-existing subscriptions were never
scaffolded and 409'd forever. Since the chat authorization chain already proves
the caller is licensed for the tenant+subscription+agent, `/v1/chat/completions`
now creates the subscription root on demand (idempotent `ScaffoldSubscription`)
instead of returning 409. The webhook becomes an **optional pre-warm**, no longer
the sole creator. (Supersedes the old TSW-08 "no on-demand shared create".)

## CTX-TSW-04: `/v1/accounts` auth = configured webhook shared secret (not agent-token)

**Decision:** `/v1/accounts` is authenticated by a shared secret the proxy
validates against a configured value (env), matching mycelium's webhook
`HttpSecret` mechanism — either an `Authorization`-style header
(`header_name`/`prefix`/`token`) or a query parameter. It does **not** use the
`x-mycelium-service-name` + per-agent bearer token that the chat routes use.

**Why (source-forced):** mycelium webhooks POST directly to the registered URL
with only the configured secret attached (header or query param) — **no HMAC
signature, no service-name header, no per-agent token**. The webhook is
registered via mycelium's `POST system-manager/webhooks` API with
`{name, url, trigger, method, secret}`; we control that secret. `/v1/accounts`
is agent-agnostic (it scaffolds a subscription usable by any agent).

---

## CTX-TSW-05: Chat requires `tenant_id` + `subs_acc_id`; profile filtering; replaces the personal flow

**Decision:** `POST /v1/chat/completions` now **requires** the caller to pass the
target `tenant_id` and subscription `acc_id` in the request body. The proxy runs
the mycelium SDK filtering chain to verify the grant, then routes to that user's
workspace under the new layout. This **replaces** today's on-demand personal
workspace flow (keyed on the caller's own accId) — there is no personal
fallback.

Filtering chain (agent name populated dynamically from the resolved agent):

```go
related, err := profile.
    WithWriteAccess().
    OnTenant(tenantID).
    WithRoles([]string{agentName}).
    OnAccount(subsAccID).
    GetRelatedAccountOrError()
```

**Why:** the user chose "substitui: chat exige subscription". Write access is
required (read-only members cannot chat) — the user's explicit chain.

---

## CTX-TSW-06: Discovery GET is driven by `licensed_resources`, not by disk

**Decision:** a new GET endpoint (proposed `GET /v1/subscriptions`) lets a user
list the `(tenant_id, subs_acc_id, role/agent)` tuples they may use, computed
from their profile's `licensed_resources`. The user then sends the chosen
`tenant_id` + `subs_acc_id` to `/v1/chat/completions`. The same discovery
strategy is reused for history and other endpoints later.

**Why (correctness):** the `<role_slug>`/`users` leaf dirs are lazy and don't
exist before a user's first chat, so intersecting with disk would return nothing
and defeat the endpoint's purpose (telling the user what to pass *before*
chatting). The source of truth is `licensed_resources`; the on-disk subscription
scaffold is at most an annotation ("has the webhook fired yet?"), never required.

---

## CTX-TSW-07: Role identity contract + operational dependency

**Decision:** one identity is shared across three places, and they MUST be equal:

```
config agent key  ==  <role_slug> path segment  ==  licensed_resources.role  (fed to WithRoles)
```

e.g. `alpha` / `beta`.

**Operational dependency (cannot be enforced in code):** mycelium guest-roles
must be **named exactly after the agents** (`alpha`, `beta`), because
`licensed_resources.role` is the guest-role name. This lands in the same manual
role-assignment territory STATE.md already flags (AD-002).

---

## CTX-TSW-08: Session key reverts to 2 parts; supersedes shared-workspaces SW-09

**Decision:** `sessionKey = sha256(<profile.accId>::<session_id>)[:32]` — the
existing 2-part construction. With total per-user directory + container
isolation, the 3-part `sha256(accId::ownerId::session_id)` from the
shared-workspaces design (SW-09) solves a shared-volume problem that no longer
exists here. This supersedes SW-09 **for this feature** (shared-workspaces stays
deferred).

---

## CTX-TSW-09: Staff/manager elevated short-circuit (intentional)

**Decision:** `GetRelatedAccountOrError()` returns allow for `is_staff` /
`is_manager` **before** the `OnAccount`/`WithRoles` filters apply — so a
staff/manager profile passes the chat chain without literally holding the
account+role grant. Accepted as intended elevated access (matches
shared-workspaces R1). The `users/<profile.accId>` segment still isolates their
workspace.

---

## Out of scope / confirmed

- **GC / deletion** of dirs on `subscriptionAccount.deleted` / `userAccount.deleted`
  — the triggers exist but this feature consumes only `.created`. Deferred.
- **`/v1/workspaces`** (shared-workspaces) — explicitly deferred, unchanged.
- **Authz filtering on `/v1/models` and `/v1/sessions/history`** — "os demais
  endpoints seguirão a mesma lógica depois". `/v1/sessions/history`'s *path* must
  change mechanically with the new layout (or it breaks); its filtering lands
  later. `/v1/models` is unchanged for now.
- Any change to how mycelium builds the profile, fires the webhook, or names
  guest-roles — consumed as-is.
