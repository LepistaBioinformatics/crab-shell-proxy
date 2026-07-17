# tenant-scoped-workspaces Tasks

Verification gate (repo standard): `docker build --network=host .` must pass
`go vet` + `go test ./...`. Runtime checks use hand-crafted `x-mycelium-profile`
headers (via `mycelium.CompressAndEncodeProfileToBase64`) hitting the proxy
directly, plus a captured subscription-`Account` JSON for the webhook.
**Planning only — no code in this doc.** Legend: `[P]` = parallelizable.

**D0 resolved (design §0):** data-root = `/data` (Option A). Tree `/data/tenants/…`,
templates `/data/templates`, compose bind `./data:/data`. T01/T09 bake this in.

## Implementation status (2026-07-16)

- **T01–T08: DONE** (uncommitted). Gate green: `go vet ./...` clean; all tests pass
  (`config`, `identity`, `httpapi`, `pico`, `history` as UID; `docker` as root for
  the chown path). Files: `config.go`+`config.yaml`, `docker/{manager,provision,
  reconcile}.go`, `httpapi/{handlers,sse}.go` (+ tests).
- **`verified` NOT enforced (user decision 2026-07-16):** the SDK chain does not
  filter on `verified` and the gateway injects unverified grants
  (`fetch_profile_from_email` passes `was_verified=None`). A `verifiedProfile`
  pre-filter was briefly added, then **removed at the user's explicit request** —
  unverified/pending-invite grants that otherwise match ARE accepted for chat.
  Locked by `TestChatUnverifiedAccepted` (asserts 200). spec AC2 / design R1
  updated to match.
- **T09: PENDING** (parent repo — compose bind `./data:/data`, `CRAB_WEBHOOK_SECRET`
  in `.env`/compose, mycelium webhook registration doc). Not started.
- **T10: PENDING** — live e2e is operator-gated (needs stack up + real webhook +
  seeded `/data/templates`); unit-level equivalents are covered by the httptest/
  faked-Docker suite.

---

## Phase 0 — Config & layout foundations

### T01 — Layout path builders + `webhookSecret` — TSW-04, TSW-02
- **What:** in `internal/config`, replace the flat `SessionsDir(agentKey, userKey)`
  with the new builders: `TemplatesDir(template)`, `SubscriptionRoot(t, s)`,
  `UserWorkspace(t, s, role, u)`, `SessionsDir(t, s, role, u)` (design §1); add the
  `webhookSecret` secret field (env-resolvable, resolved in `Load`).
- **Done when:** `config_test.go` covers each builder's exact path + a resolved
  `webhookSecret` from env; `go vet`+tests pass.
- **Depends on:** D0

### T02 [P] — `WorkspaceKey` + manager signature — TSW-04
- **What:** introduce `WorkspaceKey{ TenantID, SubsAccID, Role, UserAccID }` (or 4
  strings) and thread it through the `Orchestrator` interface / `EnsureRunning` /
  `ArmIdle` / `ContainerName`, replacing `(agent, userKey, ownerEmail)`.
- **Done when:** signatures compile across `httpapi` + `docker`; the fake
  Orchestrator in `handlers_test.go` updated; tests green.
- **Depends on:** T01

---

## Phase 1 — Orchestration

### T03 — Manager: new dir/name/labels + "don't create subscription root" + provision — TSW-06, TSW-13
- **What:** container name `picoclaw-<role>-<subsAccId>-<userAccId>`; new labels
  (`LabelTenant`/`LabelSubscription`/`LabelUser` + existing); provision into
  `UserWorkspace(...)` from `TemplatesDir(agent.Template)`; extend
  `.crab-owner.json` to `{tenantId,subsAccId,role,userAccId,email}`; `EnsureRunning`
  errors (not creates) when `SubscriptionRoot` is absent; still creates the lazy
  `<role>/users/<u>` leaf + container.
- **Done when:** faked-Docker tests: missing subscription root ⇒ not-created error;
  present ⇒ leaf+container created with correct name/labels/dir; provision seeds
  config-only allowlist (unchanged).
- **Depends on:** T01, T02

### T04 — Reconcile over the nested tree + label adoption — TSW-13
- **What:** adopt running managed containers by reading the new labels to rebuild
  `WorkspaceKey` (re-arm timers); continuous-ensure walks
  `tenants/*/subscriptions/*/agents/<continuous-agent>/users/*`.
- **Done when:** faked-Docker tests: a running container with the new labels is
  adopted + re-armed; a continuous agent's existing user dirs are ensured.
- **Depends on:** T03

---

## Phase 2 — HTTP surface

### T05 — `POST /v1/accounts` webhook — TSW-01, TSW-02, TSW-03
- **What:** secret auth (`Authorization: Bearer <webhookSecret>`, constant-time);
  parse the bare `Account` (`id`, `accountType.subscription.tenantId`); `MkdirAll`
  the `SubscriptionRoot` (chown to picoclawUser); idempotent; NOT routed by
  service-name.
- **Done when:** httptest: `201` first create, `200` retry, `401` bad/no secret,
  `400` non-subscription/missing fields; scaffold dir present.
- **Depends on:** T01

### T06 [P] — `GET /v1/subscriptions` discovery — TSW-09
- **What:** resolve profile (401 if none); map `LicensedResources.ToLicensesVector()`
  → `[{tenantId, subsAccId, role, perm, verified, scaffolded}]`; `scaffolded` =
  `exists(SubscriptionRoot)`; no on-disk scan drives the list.
- **Done when:** httptest w/ a hand-crafted profile (records + urls variants):
  returns all licensed tuples; empty for an unlicensed profile; 401 no profile.
- **Depends on:** T01

### T07 — Chat: filtering chain + new routing (replace personal path) — TSW-05,06,07,08,10
- **What:** require `tenant_id`+`subs_acc_id` (400 if absent/malformed); account
  guard `accId==subs ⇒ 403`; run
  `WithWriteAccess().OnTenant().WithRoles([agent.Key]).OnAccount().GetRelatedAccountOrError()`
  ⇒ 403 on fail; `409` if not scaffolded; route to `UserWorkspace`; 2-part session
  key; thread `WorkspaceKey` through streaming; delete the old personal on-demand
  path.
- **Done when:** httptest: authorized chat routes to the user leaf; 403 for
  unlicensed/read-only/wrong-tenant/missing-role/`accId==subs`; 400 missing
  tenant/subs; 409 unscaffolded; two members isolate; staff short-circuit allowed.
- **Depends on:** T02, T03, T05

### T08 — History path move + guard — TSW-11
- **What:** `/v1/sessions/history` reads `SessionsDir(t,s,role,u)`; require
  `tenant_id`+`subs_acc_id`+`session_id`; apply the account-switching guard + a
  minimal existence check (full authz filtering deferred per CTX-TSW-09).
- **Done when:** httptest: history read from the new path for the caller's own
  leaf; guard/404 paths covered.
- **Depends on:** T03, T07

---

## Phase 3 — Wiring & end-to-end

### T09 — Compose/env wiring + webhook registration doc (parent repo)
- **What:** if D0=Option A, set `CRAB_*_DATA_ROOT=/data` + compose bind `./data:/data`
  + relocate templates to `/data/templates`; add `CRAB_WEBHOOK_SECRET` to
  `.env`/`.env.example`/compose; document the manual mycelium webhook registration
  (`POST system-manager/webhooks`, trigger `subscriptionAccount.created`, url →
  proxy, `secret = AuthorizationHeader{prefix:"Bearer",token:$CRAB_WEBHOOK_SECRET}`)
  — a manual operator step, same posture as AD-002.
- **Done when:** `docker compose config -q` valid; the registration steps are
  written in the repo README/compose comment. **Note:** lives in the parent repo.
- **Depends on:** T05, T07

### T10 — End-to-end verification (direct-to-proxy)
- **What:** POST a captured subscription `Account` to `/v1/accounts` (correct
  secret) → scaffold appears (non-root); two profiles licensed (write, verified,
  role=`alpha`) into `subs-X`/`tenant-T` chat → each hits its own
  `.../agents/alpha/users/<own-accId>`, isolated histories; `GET /v1/subscriptions`
  lists the tuple from each profile; unlicensed/read-only ⇒ 403; unscaffolded ⇒
  409; `accId==subs` ⇒ 403.
- **Done when:** each spec §Success Criteria observed and logged.
- **Depends on:** T04, T06, T08, T09

---

## Dependency graph

```
D0 ─ T01 ─┬─ T02 ─┬─ T03 ─ T04 ─┐
          │       │             │
          ├─ T05 ─┼─ T07 ─ T08 ─┤
          └─ T06 ─┘             │
                       T09 ─────┴─ T10
```

Parallel: {T02, T05, T06} after T01; T03 after T02; T07 after T02+T03+T05.
