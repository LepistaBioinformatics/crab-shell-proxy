# tenant-scoped-workspaces Design

Builds on `context.md` (CTX-TSW-01..09) and `spec.md` (TSW-01..13). Reuses the
existing crab-shell-proxy internals (`config`, `docker.Manager`, `provision`,
`httpapi`, `identity`) and the now-wired `mycelium-sdk-go@v0.1.0`. **Architecture
and contracts only — no code here.** All IDs refer to `spec.md`.

---

## 0. Data-root shape (D0) — RESOLVED: Option A (user, 2026-07-16)

Root becomes `/data`; the per-user tree is `/data/tenants/…`; templates relocate
to `/data/templates/…`. Compose bind changes `./data/agents:/data/agents` →
`./data:/data` (one infra edit + a template move; clean start, dev stack). The
rest of the design already assumes this.

(Rejected: Option B — keep `/data/agents` root, tree `/data/agents/tenants/…/agents/…`
with "agents" appearing twice, which the rename was meant to avoid.)

---

## 1. The new layout & path builder (TSW-04)

Single source of truth for the layout, in `internal/config` (replacing the flat
`<dataRoot>/<agentKey>/<userKey>` scheme):

```
Root(dataRoot)                       = <dataRoot>
TemplatesDir(template)               = <dataRoot>/templates/<template>
SubscriptionRoot(t, s)               = <dataRoot>/tenants/<t>/subscriptions/<s>/agents        (webhook scaffold)
UserWorkspace(t, s, role, u)         = <dataRoot>/tenants/<t>/subscriptions/<s>/agents/<role>/users/<u>
SessionsDir(t, s, role, u)           = UserWorkspace(...)/workspace/sessions
```

Every dynamic segment (`t`, `s`, `role`, `u`) passes through the existing
`identity.SanitizeID` before it reaches the filesystem or a container name.
`role` is `agent.Key` (already Docker/path safe — CTX-TSW-07). Both the
container-side (`containerDataRoot`) and host-side (`hostDataRoot`) roots build
the same relative tree; only the prefix differs (unchanged split from today).

**Provisioning** (`provision.go`) is unchanged in *behavior* (template clone →
workspace align → model injection → chown non-root) — only the destination dir
comes from `UserWorkspace(...)` instead of `<agentKey>/<userKey>`. The template
source is `TemplatesDir(agent.Template)`.

`writeOwnerFile` (the `.crab-owner.json` marker) is extended to record the full
tuple for audit: `{ tenantId, subsAccId, role, userAccId, email }` (replacing the
single `accId`).

---

## 2. Container identity & labels (TSW-06)

- **Name:** `picoclaw-<role>-<subsAccId>-<userAccId>` (via `ContainerPrefix`).
  `subsAccId` is a globally-unique mycelium account UUID, so `(role, subsAccId,
  userAccId)` is unique without needing `tenantId` in the name. ~90 chars, within
  Docker limits and debuggable. (Fallback if names prove unwieldy: a short
  sha256 of the full tuple — noted, not chosen.)
- **Labels** (extend the current `crab-shell.*` set): `LabelAgent` (=role,
  unchanged), plus new `LabelTenant`, `LabelSubscription`, `LabelUser`
  (=userAccId), `LabelMode`, `LabelManaged`. These let `Reconcile` rebuild the
  full `EnsureRunning` context from a running container without re-deriving it
  from the name.

---

## 3. `POST /v1/accounts` — webhook receiver (TSW-01,02,03)

```
POST /v1/accounts
Auth:  Authorization: Bearer <webhookSecret>       (see §7)
Body:  the bare mycelium Account object (camelCase), e.g.
       { "id":"<subs_acc_id>", "accountType": { "subscription": { "tenantId":"<t>" } }, ... }
```

Flow:
1. Validate the webhook secret (constant-time compare); `401` on mismatch.
2. Decode the body as an `Account` subset: `id` (→ subsAccId) and
   `accountType.subscription.tenantId` (→ tenantId). A body whose `accountType`
   is any non-`subscription` variant, or missing `id`/`tenantId`, ⇒ `400`.
3. `MkdirAll(SubscriptionRoot(tenantId, subsAccId))` (0700, then chown to
   `picoclawUser` so lazy per-user provisioning under it can write) — idempotent,
   so a retry is a no-op `200`; a first create is `201`.
4. Respond with `{ tenantId, subsAccId, status }`.

Agent-agnostic: it creates the `…/agents` parent only; no `<role>` subdir (the
payload has no role — CTX-TSW-03). Not routed by `x-mycelium-service-name`; the
handler does NOT call `resolveAgent`.

**Account payload parsing** — a small local struct in `httpapi` (or an
`identity`/`webhook` helper) mirroring only the two fields we read; the
`accountType` tag is the externally-tagged enum `{"subscription":{"tenantId":…}}`.
(We deliberately do NOT depend on the SDK for the Account DTO — the SDK models the
Profile, not Account; this is a 2-field read.)

---

## 4. `GET /v1/subscriptions` — discovery (TSW-09)

```
GET /v1/subscriptions
Auth:  profile (x-mycelium-profile) -> 401 if undecodable
Resp:  { "subscriptions": [ { tenantId, subsAccId, role, perm, verified, scaffolded }, ... ] }
```

Computed from `ident.Profile.LicensedResources.ToLicensesVector()` (source of
truth — CTX-TSW-06): one entry per licensed resource, mapping
`{AccID→subsAccId, TenantID→tenantId, Role→role, Perm→perm, Verified→verified}`.
`scaffolded` is an optional annotation = `exists(SubscriptionRoot(tenantId,
subsAccId))`. No on-disk scan drives the list; leaf dirs need not exist.

This endpoint resolves the profile but is agent-agnostic (lists across all
agents/roles the caller holds). It does not require `x-mycelium-service-name`;
if the gateway always injects it, it is simply ignored here.

---

## 5. Chat routing & filtering (TSW-05,06,07,08)

`POST /v1/chat/completions` gains two required body fields: `tenant_id` and
`subs_acc_id` (snake_case, alongside the existing `session_id`, `messages`, …).

```
agent      := resolveAgent(r)                         # existing: service-name + bearer token
ident      := Resolver.Resolve(profileHeader)         # existing: -> *mycelium.Profile (401)
tenantID   := parseUUID(body.tenant_id)   else 400
subsAccID  := parseUUID(body.subs_acc_id) else 400
if ident.Profile.AccID == subsAccID: 403              # account-switching guard (TSW-07)

_, err := ident.Profile.
    WithWriteAccess().
    OnTenant(tenantID).
    WithRoles([]string{agent.Key}).                   # role == agent key (CTX-TSW-07)
    OnAccount(subsAccID).
    GetRelatedAccountOrError()
if err != nil: 403                                    # unlicensed / wrong tenant / read-only / no role (TSW-05.2)

if !exists(SubscriptionRoot(tenantID, subsAccID)): 409  # not scaffolded (TSW-08)

userKey  := ident.Profile.AccID.String()
route to UserWorkspace(tenantID, subsAccID, agent.Key, userKey)  # leaves created lazily
sessionK := SessionKey(userKey, body.session_id)      # 2-part (TSW-10)
```

- **Staff/manager:** `GetRelatedAccountOrError()` short-circuits to allow for
  `IsStaff`/`IsManager` before the account/role filters (CTX-TSW-09) — elevated
  access still lands in `users/<their accId>`.
- **`EnsureRunning` signature change:** from `(agent, userKey, ownerEmail)` to a
  richer key — pass a small `WorkspaceKey{ TenantID, SubsAccID, Role, UserAccID }`
  (or the 4 strings) so the manager can build both the dir and the container
  name/labels. Streaming path (`sse.go`) threads the same key.
- **`409` vs on-demand:** unlike today, chat NEVER creates the subscription root;
  only `/v1/accounts` does. The manager gets an "ensure running, do not create the
  subscription root" contract — it may create the lazy `<role>/users/<u>` leaf and
  the container, but errors `409`-style if `SubscriptionRoot` is absent.

The old personal on-demand path (`SanitizeID(ident.AccID)` keyed on the caller's
own account) is **removed** (TSW-13) — there is no fallback.

---

## 6. Session key & history (TSW-10, TSW-11)

- **Session key:** `SessionKey(userAccID, session_id)` = existing
  `sha256(userAccID::session_id)[:32]`. The 3-part shared-workspaces variant is
  NOT used (CTX-TSW-08). Container + dir isolation already separate users; the key
  only separates one user's own conversations.
- **History:** `GET /v1/sessions/history` reads
  `SessionsDir(tenantID, subsAccID, agent.Key, userAccID)`; it requires the same
  `tenant_id`+`subs_acc_id` (query params) + `session_id`. Its full authz
  filtering is deferred (CTX-TSW-09), but this feature MUST move its path to the
  new layout and apply at least the same account-switching guard + a minimal
  existence check, so it never reads a stale/foreign location.

---

## 7. Config & wiring (TSW-02)

- **`webhookSecret`** — new top-level config field (`secret` type, env-resolvable
  like agent tokens), e.g. `webhookSecret: { env: "CRAB_WEBHOOK_SECRET" }`. The
  `/v1/accounts` handler compares `Authorization: Bearer <that>` in constant time.
  The mycelium webhook is registered (via `POST system-manager/webhooks`) with
  `secret = AuthorizationHeader{ prefix:"Bearer", token:<same> }` and
  `trigger = "subscriptionAccount.created"`, `url` pointing at the proxy
  (internal docker network, e.g. `http://crab-shell-proxy:8080/v1/accounts`).
  (Query-param secret is a documented alternative mycelium supports; header-only
  is implemented for MVP.)
- **Data root (D0/§0):** if Option A, `containerDataRoot`/`hostDataRoot` → `/data`,
  templates → `/data/templates`, compose bind `./data:/data`. Documented in the
  parent repo's compose (like shared-workspaces' T06 note); lives outside this
  submodule.
- No lifecycle changes: `mode`/`idleTimeout` still per-agent from `config.yaml`.

---

## 8. Reconcile (TSW-13)

`Reconcile` changes from walking `<dataRoot>/<agentKey>/*`:

- **Adopt running containers:** iterate `List(LabelManaged=true)`, read the new
  labels (`LabelTenant`/`LabelSubscription`/`LabelAgent`/`LabelUser`) to rebuild
  the `WorkspaceKey`, re-arm scale-to-zero timers (unchanged logic, richer key).
- **Continuous ensure:** walk the nested tree
  `tenants/*/subscriptions/*/agents/<continuous-agent>/users/*` and `EnsureRunning`
  each. Same "already-materialized dir" limitation as today (R3).

---

## 9. Component / file map

| Concern | Location |
| --- | --- |
| Layout path builders (Root/TemplatesDir/SubscriptionRoot/UserWorkspace/SessionsDir), `webhookSecret` | `internal/config` |
| `WorkspaceKey`, `EnsureRunning` signature, container name + labels, "don't create subscription root" | `internal/docker/manager.go` |
| Provision dest dir + extended owner marker | `internal/docker/provision.go` |
| Reconcile over nested tree + label-based adoption | `internal/docker/reconcile.go` |
| `POST /v1/accounts` (webhook, secret auth, Account parse, scaffold) | `internal/httpapi` |
| `GET /v1/subscriptions` (discovery from licensed_resources) | `internal/httpapi` |
| Chat: require tenant/subs, filtering chain, guard, 403/400/409, route | `internal/httpapi/handlers.go` (+ `sse.go`) |
| History path move | `internal/httpapi/handlers.go` + `config.SessionsDir` |
| Account payload struct (2 fields) | `internal/httpapi` (or `internal/webhook`) |

---

## 10. Risks & security

- **R1 — the filtering chain is the only gate.** A licensed member reaches a
  workspace solely because `WithWriteAccess().OnTenant().WithRoles().OnAccount().
  GetRelatedAccountOrError()` passed on the unforgeable profile. It must be
  server-side, require write + the exact role (NOTE: `verified` is intentionally
  NOT enforced per the 2026-07-16 user decision — unverified grants are accepted),
  and never trust
  client-supplied `tenant_id`/`subs_acc_id` beyond feeding them INTO the chain
  (used only after it passes). Table-test the deny paths exhaustively.
- **R2 — role-name contract is external (CTX-TSW-07).** If a mycelium guest-role
  is not named exactly like an agent, `WithRoles` silently yields no grant ⇒
  everyone `403`. Fails safe (closed), but is an operational footgun — document
  it, and surface a clear `403` reason.
- **R3 — webhook secret is a bearer credential.** Anyone who can POST
  `/v1/accounts` with the secret can scaffold arbitrary subscription roots (dir
  creation only — no data exposure). Keep the endpoint on the internal network,
  constant-time compare, and treat the secret like the agent tokens (env, never
  committed).
- **R4 — account-switching guard (TSW-07).** Kept as defensive despite the user's
  assertion it is unreachable; costs one comparison, closes the cross-user
  collision if the assumption ever breaks.
- **R5 — layout migration.** Existing flat `<agentKey>/<accId>` dirs are abandoned
  (clean start, dev stack); no in-place migration. State it so it is not mistaken
  for data loss.
