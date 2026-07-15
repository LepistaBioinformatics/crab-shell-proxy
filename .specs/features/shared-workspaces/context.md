# shared-workspaces — Discussion Context (gray-area decisions)

Captured during Specify. These are the user's explicit answers to the design
forks; spec/design/tasks build directly on them.

Background: today crab-shell-proxy isolates **one container per `(agent, user)`**,
keyed by the caller's own account id (`accId` from the mycelium profile), created
**on demand** on the user's first chat. This feature adds an opt-in **shared
workspace**: one container keyed by a *given* account id, usable by *any* user
whose profile grants access to that account.

---

## CTX-SW-01: Chat targets the shared workspace via a request field; access is authorized from `licensedResources`

**Decision:** A chat request selects a shared workspace by carrying the shared
account id in the request body (a `workspace_acc_id` field) — or absent it, the
request keeps today's per-user behavior. The proxy authorizes access by checking
the mycelium-injected profile's `licensedResources` for an entry whose `accId`
equals `workspace_acc_id` (verified, sufficient permission). If granted, the
request is routed to `picoclaw-<agent>-<workspace_acc_id>` (the shared
container); if not, `403`.

**Why:** matches the user's framing ("usuários que tiverem no profile permissão
para acessar esse workspace"). The profile is server-built and unforgeable, so a
caller cannot reach a shared workspace they aren't licensed into by just passing
an arbitrary id.

---

## CTX-SW-02: Only callers with write/owner permission over the `accId` may CREATE

**Decision:** `POST` (create shared workspace) is authorized only when the
caller's profile has write/owner authority over the target `accId` — a
`licensedResources` entry with `accId == body.accId` and `perm = Write`, or
tenant ownership of that account's tenant (staff/manager also allowed as an
elevated path).

**Why:** self-service but safe — the account's owner provisions its own shared
workspace; a licensed *reader* cannot.

---

## CTX-SW-03: Per-user sessions inside a shared workspace; skills/agents/memory shared

**Decision:** Within one shared workspace, **conversation sessions are isolated
per user** (each member has their own chat history), while **skills, agent
config, model, and the workspace/memory files are shared** (it's one container,
one volume). The per-user session boundary uses the caller's principal **owner
id** (a per-human id, distinct from the shared account id):
`sessionKey = sha256(<sharedAccId>::<ownerId>::<session_id>)`.

**Why:** the shared workspace exists to share *context* (skills, memory, agent),
not to expose each member's private conversations to the others.

---

## CTX-SW-04: Lifecycle follows the agent's existing config

**Decision:** A shared workspace's container obeys the **same `mode` /
`idleTimeout`** declared for its agent in `config.yaml` (scale-to-zero or
continuous). No separate lifecycle policy for shared workspaces. For a
scale-to-zero agent, any member's request re-arms the idle timer (the existing
per-container timer already behaves this way).

**Why:** one place to configure lifecycle; shared workspaces are just another
container of the same agent.

---

## Out of scope / confirmed

- Creating shared workspaces **on demand** from a chat request — explicitly NOT
  done: a shared workspace exists only after the `POST` endpoint creates it
  (contrast with per-user workspaces, which are on-demand).
- Any change to how the mycelium gateway builds/injects the profile or
  `licensedResources` — consumed as-is.
- Cross-agent shared workspaces — a shared workspace belongs to the agent whose
  route the request hit (resolved from `x-mycelium-service-name`), same as chat.
