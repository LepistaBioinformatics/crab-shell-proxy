# restart-control — Specification

**Status:** Draft
**Size:** Large (multi-component, three repositories)
**Repos touched:**

| Repo | Role |
| --- | --- |
| `crab-shell-proxy` | Restart policy engine, notice store, scheduler, user + admin endpoints |
| `crab-exoskeleton-webapp` | User restart banner/button, admin restart-policy chooser |
| `zombie-crab-project` (parent) | Gateway route declarations for the new user-facing `/v1/restart` |

---

## Problem

Every admin action that needs a container bounce forces one **immediately and
unconditionally**. Today the proxy calls `RestartScope` / `RestartWorkspace`
inline at seven sites; a member mid-conversation is interrupted with no warning
and no explanation, and there is no way for an admin to say "apply this at
02:00" or "tell them, let them pick the moment".

Symmetrically, a member has **no way to restart their own instance**. They can
write a secret (`POST /v1/secrets`), but whether it lands depends on a restart
they neither trigger nor observe.

## Goal

Make the restart an explicit, observable, policy-driven event:

- A member can restart **their own** container from their own screen, without
  being an admin.
- An admin choosing an action that needs a bounce picks the policy: **now**,
  **scheduled at T**, or **notice only**.
- A member always sees the current state: nothing pending, a restart scheduled
  for T, or "a restart is needed — press when you're ready".

## Non-goals

- Restarting another member's container from the member UI (admins already have
  scope-level restart; there is no per-user targeted admin restart in this
  feature).
- Recreating containers (transcript loss). Every path here is stop+start, as
  `RestartScope` already documents.
- A cron subsystem. Scheduling reuses the existing `time.AfterFunc` idiom that
  drives the idle timer.

---

## Requirements

### FR-1 — Member self-restart

- **FR-1.1** `POST /v1/restart` restarts the caller's own container for the
  routed agent. The workspace key is derived from the mycelium profile `accId`
  and the injected `x-mycelium-service-name` — **never** from a request body or
  query field. A body naming another user is ignored, not honoured.
- **FR-1.2** The route is gated by the agent guest role **with `write`
  permission** (same chain as `/v1/secrets` and `/v1/chat/completions`). A
  read-only member gets 403 from the gateway. — DEC-1
- **FR-1.3** The handler calls `RestartWorkspace` directly, not `RestartScope`:
  the member is waiting on the result, so a Docker failure must surface as a
  500 with the real message, not be swallowed into a log line.
- **FR-1.4** When the container is absent or scaled to zero, the call succeeds
  with `{"status":"noop"}` — the next cold start already picks up every pending
  change. It still clears the pending notice (FR-3.4).
- **FR-1.5** On success the response carries the new `lastRestartAt`.

### FR-2 — Member restart status

- **FR-2.1** `GET /v1/restart` returns, for the caller's own workspace:
  `pending`, `reason`, `note`, `noticeAt`, `scheduledAt`, `lastRestartAt`,
  `running`.
- **FR-2.2** Gated by the agent guest role, **read permission** — a read-only
  member sees the notice even though FR-1.2 denies them the button.
- **FR-2.3** `reason` is a small closed enum, so the UI can phrase it:
  `shared-secret`, `shared-skills`, `shared-files`, `model`, `own-secret`,
  `admin-request`. `note` is optional free text the admin may attach. — DEC-2
- **FR-2.4** A notice raised at a broader scope is visible at the narrower one:
  a tenant-scope notice reaches every subscription under it, a
  subscription-scope notice every agent in it, and an agent-scoped notice only
  that agent's workspaces.

### FR-3 — The notice state model

- **FR-3.1** A notice is stored **per scope**, not fanned out per workspace: an
  admin action targets a scope, and workspaces that are scaled to zero or have
  never been created must still show the notice once they appear.
- **FR-3.2** Each workspace carries a `lastRestartAt` marker, stamped by
  `RestartWorkspace` **and** by container create/ensure (a freshly created
  container has by definition already applied everything).
- **FR-3.3** A notice is **live for a workspace** iff
  `lastRestartAt < noticeAt` for the most recent notice reachable through that
  workspace's scope cascade (tenant → subscription → agent), using the same
  cascade order as the shared-secret resolution.
- **FR-3.4** It follows that nothing is ever explicitly "cleared" per user: any
  restart — self-service, scheduled, admin-immediate, or a cold start — clears
  the notice for that workspace by advancing its marker.
- **FR-3.5** An admin may withdraw a notice or schedule for the whole scope
  (`DELETE`), which removes the scope record.

### FR-4 — Admin restart policy on mutating actions

Every admin action that currently forces a bounce accepts a `restart` policy:

| Policy | Effect |
| --- | --- |
| `now` (default) | Propagate + bounce immediately — today's behaviour |
| `notice` | Propagate, raise a scope notice, do not bounce |
| `schedule` | Propagate, raise a scope notice carrying `scheduledAt`, arm the timer |

- **FR-4.1** **Data propagation is never deferred.** Rebuilding the effective
  secret view and merging native slots into each workspace happens on every
  policy; only the container stop/start is subject to the policy. This forces
  `RestartScope` to split into `PropagateScope` (always) + `BounceScope`
  (policy-gated).
- **FR-4.2** The default is `now`, so an unmodified client keeps today's exact
  behaviour.
- **FR-4.3** The affected call sites, all of which must become policy-driven:

  | # | Site | Was | Reason stamped |
  | --- | --- | --- | --- |
  | 1 | `handleAdminSharedSecretsPost` | `RestartScope` | `shared-secret` |
  | 2 | `handleAdminSharedSecretsDelete` | `RestartScope` | `shared-secret` |
  | 3 | `handleAdminSkillsPost` | `RestartScope` | `shared-skills` |
  | 4 | `handleAdminSkillsDelete` | `RestartScope` | `shared-skills` |
  | 5 | `handleAdminModelUpdate` → `ReapplyModelForModel` | `RestartWorkspace` per key | `model` |
  | 6 | `reapplyForScope` → `ReapplyModelScope` (model default set/clear) | `RestartWorkspace` per key | `model` |
  | 7 | `handleAdminModelAssignmentSet` / `…Clear` → `ReapplyModelUser` | `RestartWorkspace` | `model` |

  **Verified during implementation.** An earlier draft of this table named
  `handleAdminRegisteredModelApply` as site 5; that handler does not exist on
  this branch — it belongs to the pre-model-registry code on `main` and was
  replaced by the registry endpoints above. The gate is the grep, not the table:
  `grep -n 'RestartScope\|RestartWorkspace\|BounceScope\|PropagateScope'
  internal/httpapi internal/docker` must leave only the policy helper, the
  member self-restart, the scheduler, and `BounceScope`'s own internals.

  The unconditional `Manager.RestartScope` was **removed** rather than kept as a
  composition: with every caller migrated it had none left, and leaving it on the
  `Orchestrator` interface would be a standing invitation for a future handler to
  bypass the policy silently.

- **FR-4.4** A member's own secret write/delete (`POST`/`DELETE /v1/secrets`,
  sites in `handlers.go`) stops force-restarting and instead raises a
  **self-notice** for that member's own workspace. The member then presses the
  button. — DEC-3

### FR-5 — Admin restart endpoints

- **FR-5.1** `POST /v1/admin/restart` — body `{tenant_id, subs_acc_id?,
  agent_key?, mode: "now"|"notice"|"schedule", at?, reason?, note?}`. Raises,
  reschedules, or executes a restart for the scope.
- **FR-5.2** `GET /v1/admin/restart?tenant_id=&subs_acc_id=&agent_key=` —
  returns the scope's current notice/schedule, so the admin UI can show and
  amend it.
- **FR-5.3** `DELETE /v1/admin/restart?...` — withdraws the notice/schedule.
- **FR-5.4** All three are authorized by `authz.AuthorizeSharedScope` for the
  target scope kind — the same authority-over-target check the shared-content
  endpoints use. A subscriptions-manager can act on their own subscription; a
  tenant manager on the whole tenant.
- **FR-5.5** `at` must be in the future and within 7 days; otherwise 400.
- **FR-5.6** They need no gateway change — `/v1/admin/*` is already a single
  wildcard route.

### FR-6 — Scheduling

- **FR-6.1** A scheduled restart arms a `time.AfterFunc` that, at `at`, runs
  `BounceScope` for the scope, then removes the schedule (keeping the notice
  record's `noticeAt` so a container that never came up still shows nothing —
  its marker advances on cold start anyway).
- **FR-6.2** Schedules are **re-armed from disk during `Reconcile`** at proxy
  boot. A schedule that elapsed while the proxy was down fires immediately on
  boot.
- **FR-6.3** Re-scheduling the same scope replaces the pending timer; it never
  stacks two.

### FR-7 — Webapp: member surface

- **FR-7.1** The chat screen polls `GET /api/restart` (BFF → `GET /v1/restart`)
  and renders a banner when `pending`.
- **FR-7.2** With `scheduledAt` set: an informational banner — "your assistant
  will restart at &lt;local time&gt;", no button.
- **FR-7.3** Without `scheduledAt`: an actionable banner with a **Restart now**
  button, plus the reason phrased from the enum.
- **FR-7.4** The button is disabled while the request is in flight; there is no
  server-side cooldown (the per-container lock already serializes). — DEC-4
- **FR-7.5** After a successful restart the banner disappears (re-fetch status).
- **FR-7.6** A read-only member sees the banner without the button.
- **FR-7.7** The secrets drawer, after a successful secret write, surfaces the
  same restart affordance inline rather than silently restarting.

### FR-8 — Webapp: admin surface

- **FR-8.1** Each admin panel whose action triggers a bounce (shared secrets,
  shared skills, model) offers the policy chooser: **Restart now** / **Notify
  members** / **Schedule for…** with a datetime picker.
- **FR-8.2** The choice is remembered per session (not persisted server-side) so
  an admin making five changes picks once.
- **FR-8.3** A scope with a pending notice/schedule shows it in the admin
  screen, with a withdraw action (FR-5.3).

**FR-8.3 shipped late.** The webapp's first pass built FR-8.1/8.2 and stopped:
`GET`/`POST`/`DELETE` on `/api/admin/restart` were routed end to end and left
with no caller, so an admin who armed a notice from a save could not see it,
amend it or withdraw it — and no scope could be bounced without inventing a
change to save. Closed in crab-exoskeleton-webapp#11; the webapp-side detail is
recorded in that repo's `restart-control/spec.md`. Two things about it constrain
this side:

- The admin read uses `Store.Get`, which returns the notice at **exactly** the
  scope asked for. Only the member path (`Store.Resolve`) walks the four cascade
  positions. So an admin standing on a subscription cannot see a tenant-wide
  notice, and one filtered to an agent cannot see the all-agents record. The UI
  mitigates by naming the slot it read rather than claiming nothing is armed
  anywhere; showing the cascade instead would be worse, because `DELETE`
  withdraws the exact slot and a withdraw button under a wider scope's notice
  would appear to do nothing. Reading the cascade honestly needs a proxy change
  — a read that reports which position each notice came from — which is why the
  webapp did not attempt it.
- The immediate action reaches whatever `BounceScope` reaches: every **running**
  container under the scope. The confirmation dialog states that reach, so a
  change to `BounceScope`'s filtering makes that copy wrong.

**Not built, and deliberately.** An admin-initiated restart of ONE member's
workspace. `Manager.RestartWorkspace` takes a full `WorkspaceKey` and would serve
it, but there is no admin-authorized route: `POST /v1/restart` builds the key
from the caller's own profile and `TestSelfRestartIgnoresACallerSuppliedUserID`
exists to keep it that way (FR-1.1). Adding one is proxy work, and since the
notice model is per scope it would be a bounce with no notice attached.

### NFR

- **NFR-1** No new dependency; scheduling uses `time.AfterFunc` as the idle
  timer already does.
- **NFR-2** Notice records are small JSON files under a new `<root>/restart/`
  area, **outside** the tenant tree, so they are never visible inside a
  container (the whole `UserWorkspace` is bind-mounted into the agent).
- **NFR-3** Reading restart status is on the chat screen's hot path; it must be
  a stat + small read, no Docker call beyond the cheap `Inspect` already used.
- **NFR-4** Every existing caller that does not pass `restart` keeps today's
  behaviour exactly (FR-4.2), so the change is additive at the API boundary.

---

## Traceability

| ID | Verified by |
| --- | --- |
| FR-1.1 | Unit: handler ignores a body-supplied `user_acc_id`; key comes from profile |
| FR-1.2 | Gateway config review + integration probe with a read-only role |
| FR-1.3 | Unit: Docker error propagates as 500 with message |
| FR-1.4 | Unit: absent/stopped container → 200 `noop`, marker still advanced |
| FR-2.3 | Unit: each reason enum round-trips through the status payload |
| FR-2.4 | Unit: tenant notice visible from an agent-scoped workspace |
| FR-3.3 | Unit: marker after notice → not pending; marker before → pending |
| FR-3.4 | Unit: `RestartWorkspace` advances the marker and clears pending |
| FR-4.1 | Unit: `notice` policy still rebuilds the effective secret view |
| FR-4.2 | Unit: omitted `restart` behaves identically to today |
| FR-4.3 | Grep gate (run): no unconditional restart left at an admin mutation site; `RestartScope` removed outright |
| FR-5.4 | Unit: subscriptions-manager on another subscription → 403 |
| FR-5.5 | Unit: past `at` and `at > 7d` → 400 |
| FR-6.2 | Unit: `Reconcile` re-arms a persisted future schedule; elapsed one fires |
| FR-7.x | Component tests + manual UAT |
| FR-8.1, FR-8.2 | Component tests in crab-exoskeleton-webapp (`restart-policy-select.test.tsx`) |
| FR-8.3 | Component + client tests in crab-exoskeleton-webapp (`restart-notice.test.tsx`, `adminRestart.test.ts`) |
