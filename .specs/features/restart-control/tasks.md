# restart-control — Tasks

From [design.md](design.md). `[P]` = parallelizable with its siblings.

Gate check for every proxy task: `go build ./... && go vet ./... && go test ./...`.
Known-red baseline: the `TestEnsureRunning*` chown tests fail in this sandbox
(STATE.md L-001) — sandbox noise, not a regression. Compare against the base
commit before blaming a change.

---

## Phase 1 — State layer (proxy)

### T-01 — Path helpers for the restart area
- **What:** `RestartRoot`, `RestartScopeFile`, `RestartWorkspaceFile` in
  `internal/config/config.go`, documented like their neighbours (say *why* they
  live outside the tenant tree — the workspace is bind-mounted into the agent).
- **Where:** `internal/config/config.go`
- **Depends on:** —
- **Reuses:** `identity.SanitizeID`, the existing helper docblock style.
- **Done when:** paths compose as in design §2; a tenant record resolves to
  `_tenant.json`.
- **Tests:** table test over (tenant-only, tenant+subs) and an id needing
  sanitization.

### T-02 — `internal/restart` store
- **What:** the package in design §3 — `Notice`, `Store`, `Raise`, `Withdraw`,
  `Get`, `Resolve`, `Stamp`, `LastRestart`, `Status`, `Schedules`.
- **Where:** new `internal/restart/{restart.go,restart_test.go}`
- **Depends on:** T-01
- **Reuses:** the cascade *order* from `Manager.sharedSecretsCascade`
  (`internal/docker/shared.go`) — read it and mirror it; do not invent a
  different precedence.
- **Done when:** `Status` answers FR-3.3 for all four cascade positions, and
  `Resolve` picks the newest `noticeAt` when two levels both carry one.
- **Tests:** cascade precedence; newest-wins; missing marker ⇒ pending;
  marker-after-notice ⇒ not pending; `Schedules` returns future *and* elapsed.
- **Gate:** package tests green (no Docker needed — this is why it is its own
  package).

---

## Phase 2 — Manager integration (proxy)

### T-03 — Split `RestartScope` into propagate + bounce
- **What:** `PropagateScope` (every workspace in scope, running or not) and
  `BounceScope` (running containers only); `RestartScope` becomes their
  composition so no current caller changes meaning.
- **Where:** `internal/docker/shared.go`
- **Depends on:** —
- **Reuses:** `workspacesInScope`, `syncEffectiveSecrets`,
  `applyNativeToWorkspace`, `RestartWorkspace` — this is a re-grouping, not new
  logic.
- **Done when:** `RestartScope` behaves identically to before (same log lines,
  same best-effort semantics) and `PropagateScope` also covers stopped
  workspaces.
- **Tests:** existing shared-secret / native-overlay tests stay green;
  add one asserting a *stopped* workspace gets its native overlay applied by
  `PropagateScope`.

### T-04 — Stamp the workspace marker
- **What:** `Manager` holds a `*restart.Store`; `RestartWorkspace` stamps after
  a successful restart **and** on its no-op branch; `EnsureRunning` stamps after
  a create.
- **Where:** `internal/docker/manager.go` (+ wiring where `Manager` is
  constructed)
- **Depends on:** T-02
- **Done when:** design §7 holds; a marker write failure logs and does not fail
  the restart.
- **Tests:** unit on the no-op branch (absent container → marker advanced,
  `nil` error). The create path is covered by the chown-dependent tests that are
  red in this sandbox — verify by reading, and note it in the report.

### T-05 — Scheduler on `Manager`
- **What:** `ArmScheduledBounce` / `CancelScheduledBounce` per design §6,
  replacing (never stacking) a pending timer for the same scope key.
- **Where:** `internal/docker/manager.go`
- **Depends on:** T-02, T-03
- **Reuses:** the `armLocked`/`disarmLocked` idle-timer idiom — same shape, same
  locking discipline.
- **Done when:** firing bounces the scope and clears `ScheduledAt` while keeping
  `noticeAt`; re-arming the same key leaves exactly one timer.
- **Tests:** arm-twice ⇒ one timer; fire ⇒ `ScheduledAt` cleared. Use a short
  duration, not a sleep-heavy test.

### T-06 — Re-arm schedules in `Reconcile`
- **What:** final step of `Reconcile`: `Schedules()` → arm each; an elapsed time
  arms at zero so it fires immediately.
- **Where:** `internal/docker/reconcile.go`
- **Depends on:** T-05
- **Done when:** FR-6.2 holds across a proxy restart.
- **Tests:** seed a future and an elapsed schedule on disk, run the re-arm step,
  assert both are armed and the elapsed one fires.

---

## Phase 3 — HTTP surface (proxy)

### T-07 — Member `GET`/`POST /v1/restart`
- **What:** the two handlers in design §8, plus routes in `handlers.go`.
- **Where:** new `internal/httpapi/restart.go`, `internal/httpapi/handlers.go`
- **Depends on:** T-02, T-04
- **Reuses:** `resolveSecretCaller` — the workspace key must come from it, so
  there is no code path reading a user id from the request (FR-1.1).
- **Done when:** FR-1.1–1.5, FR-2.1–2.4 hold; a Docker failure is a 500 carrying
  the real message (not swallowed like `RestartScope` does).
- **Tests:** body-supplied `user_acc_id` ignored; stopped container → `noop`;
  Docker error → 500; status reflects a tenant-scope notice.

### T-08 — `applyRestartPolicy` helper
- **What:** design §5, including the query-parameter parse
  (`restart`, `restart_at`, `restart_note`) and the "`now` raises no notice"
  rule, with the reasoning kept as a comment.
- **Where:** new `internal/httpapi/restart_policy.go`
- **Depends on:** T-02, T-03, T-05
- **Done when:** all three modes behave per FR-4; absent `restart` ⇒ `now`
  ⇒ byte-identical to today.
- **Tests:** each mode; `at` in the past → 400; `at` > 7d → 400; propagation
  runs under `notice` (FR-4.1).

### T-09 — Migrate the admin mutation sites [P after T-08]
- **What:** replace every unconditional `RestartScope` / model-reapply restart
  with `applyRestartPolicy`, stamping the reason from spec FR-4.3.
- **Where:** `internal/httpapi/admin.go` (shared-secrets post/delete, skills
  post/delete, registered-model apply), `internal/docker/model.go`
  (`ReapplyModelScope`, `ReapplyModelUser`, `ReapplyModelForModel` — these need a
  policy parameter threaded from their callers).
- **Depends on:** T-08
- **Done when:** no admin mutation path forces a bounce without consulting a
  policy. **Re-verify the site list by grep before starting** — spec FR-4.3's
  table is a snapshot, not a contract.
- **Tests:** per site, `restart=notice` leaves the container running and raises
  the notice.
- **Gate:** `grep -n 'RestartScope\|RestartWorkspace' internal/httpapi internal/docker`
  reviewed; every remaining call is either inside the policy helper, the member
  self-restart, or a documented exception.

### T-10 — Member secret write raises a self-notice (DEC-3)
- **What:** `handleSecretsPost` / `handleSecretsDelete` stop calling
  `RestartWorkspace`; they raise a `own-secret` notice scoped to that member's
  own (subscription, agent).
- **Where:** `internal/httpapi/handlers.go`
- **Depends on:** T-02, T-07
- **Note:** this is the one **behaviour change to an existing endpoint**. Call it
  out in the PR body; DEC-3 records the rationale and that reverting is one line.
- **Tests:** secret write → container untouched, status now `pending` with
  reason `own-secret`.

### T-11 — Admin restart endpoints
- **What:** `GET`/`POST`/`DELETE /v1/admin/restart` per design §8, authorized
  with `authz.AuthorizeSharedScope`.
- **Where:** `internal/httpapi/admin.go` (or `restart.go`), `handlers.go` routes
- **Depends on:** T-08
- **Done when:** FR-5.1–5.5 hold.
- **Tests:** subscriptions-manager on a foreign subscription → 403; tenant
  manager on any subscription under their tenant → 200; bad `at` → 400.

### T-12 — OpenAPI + gateway routes
- **What:** add both `/v1/restart` operations to `internal/httpapi/openapi.json`;
  add the two `[[<agent>.path]]` blocks per agent to
  `deploy/standalone/config.standalone.toml`, `deploy/prod/config.base.toml`,
  `deploy/dokploy/config.base.toml` **in the parent repo**.
- **Where:** proxy repo + `zombie-crab-project`
- **Depends on:** T-07
- **Blocking check:** confirm mycelium accepts two blocks with the same `path`
  differing only in `methods`. If it rejects them, take design §8's documented
  fallback (single read-gated route, permission checked in-proxy from the
  profile) and record the switch in `context.md`.
- **Done when:** a live GET succeeds for a read-only member and POST 403s for
  them.

---

## Phase 4 — Webapp

### T-13 — BFF routes
- **What:** `app/api/restart/route.ts` (GET/POST, agent-routed) and
  `app/api/admin/restart/route.ts` (GET/POST/DELETE via `forwardAdmin`).
- **Where:** `crab-exoskeleton-webapp`
- **Depends on:** T-07, T-11
- **Reuses:** `app/api/secrets` for the agent-routed shape; `lib/adminProxy.ts`
  for the admin shape.
- **Tests:** route tests mirroring the existing ones for secrets/admin.

### T-14 — `lib/restart.ts`
- **What:** status type, reason→i18n-key map, fetch helpers.
- **Depends on:** T-13
- **Tests:** every reason enum value maps to a key present in every locale.

### T-15 — Member banner [P with T-16]
- **What:** `app/chat/restart-banner.tsx`, mounted in `chat-shell.tsx`; three
  states (hidden / scheduled-informational / actionable); button disabled
  in-flight; re-fetch after success.
- **Depends on:** T-14
- **Done when:** FR-7.1–7.6 hold; a read-only member sees the banner and no
  button.
- **Tests:** component tests per state.

### T-16 — Secrets drawer affordance [P with T-15]
- **What:** after a successful secret write, surface the restart affordance
  inline (FR-7.7).
- **Where:** `app/chat/secrets-drawer.tsx`
- **Depends on:** T-14

### T-17 — Admin policy chooser
- **What:** `app/admin/restart-policy-select.tsx`, state held in the admin
  screen (session-scoped, FR-8.2); wired into the shared-secrets, shared-skills
  and model panels; pending-notice display + withdraw.
- **Where:** `app/admin/*`
- **Depends on:** T-13
- **Done when:** FR-8.1–8.3 hold.
- **Tests:** the chosen policy reaches the mutation call as query params.

### T-18 — i18n
- **What:** every new string in `lib/i18n` for all shipped locales.
- **Depends on:** T-15, T-16, T-17
- **Gate:** no bare user-facing literal in the new JSX.

---

## Phase 5 — Close

### T-19 — Manual UAT
Walk the three journeys end to end: (a) member writes a secret → banner →
restart → banner gone; (b) admin changes a shared secret with `notice` → member
sees the reason, container untouched; (c) admin schedules → member sees the
time → timer fires → banner gone. Then restart the proxy with a schedule armed
and confirm it survives (FR-6.2).

### T-20 — Update `.specs/project/STATE.md`
Record the DEC-3 behaviour change and anything learned about the mycelium
two-blocks-same-path question (T-12).
