# restart-control — Context (user decisions)

Decisions captured before design. Each records what was chosen and why, so a
later reader does not re-litigate it.

## DEC-1 — Self-restart requires the `write` permission

**Question:** may a member holding only `read` on the agent restart their own
instance?
**Decision:** no — `POST /v1/restart` is gated by
`{ name = "<agent>", permission = "write" }`, the same chain as `/v1/secrets`.
**Why:** a read-only member has no way to *cause* a change that needs applying,
so the button would only ever be a way to interrupt themselves. Status
(`GET /v1/restart`) stays read-gated so they still see a scheduled restart
coming.

## DEC-2 — The notice carries a reason category

**Question:** does the member see *why* a restart is needed, or just that one
is?
**Decision:** a closed enum (`shared-secret`, `shared-skills`, `shared-files`,
`model`, `own-secret`, `admin-request`) plus an optional free-text admin note.
**Why:** "your model changed" and "an administrator asked for a restart" call
for different urgency from the member. The cost is one field on the scope
record; the enum keeps the UI phrasing in the frontend where it can be
translated, rather than shipping English strings from the proxy.

## DEC-3 — A member's own secret write raises a notice instead of force-restarting

**Question:** `POST /v1/secrets` currently calls `RestartWorkspace`
unconditionally. Keep it?
**Decision:** no — it raises a self-notice; the member presses the button.
**Why:** this is exactly the scenario the user described ("após registrar uma
secret ele poderá reiniciar"). A forced restart mid-conversation is the
disruption the whole feature exists to remove, and the member is right there on
the screen when the banner appears, so the change cannot go unnoticed.
**Risk accepted:** a member who ignores the banner has a secret that is stored
but not yet live. Mitigated by the banner being actionable and persistent, and
by any cold start applying it anyway.
**Reversible:** if this proves annoying, flipping site FR-4.4 back to `now` is a
one-line change — the policy plumbing stays useful either way.

## DEC-4 — Click spam is handled client-side only

**Question:** server-side cooldown on repeated self-restarts?
**Decision:** no. The button is disabled while in flight; the proxy adds no
429 path.
**Why:** `RestartWorkspace` already takes the per-container lock, so concurrent
clicks serialize rather than corrupt. A duplicate restart is wasteful, not
harmful, and a cooldown adds a server timestamp check plus an error state the UI
must render — more surface than the problem justifies.

## DEC-5 — Three repositories, three pull requests

**Not a user decision — a constraint discovered during orientation, recorded
here so the execution plan does not trip on it.**

`crab-shell-proxy`, `crab-exoskeleton-webapp` and the parent
`zombie-crab-project` are independent git repositories (the parent tracks the
other two as nested checkouts). The proxy work is isolated in a worktree; the
webapp checkout is the user's own and currently has uncommitted changes on
`feat/model-registry-source-of-truth`, so nothing is committed there without
asking first.

## Assumptions flagged (not user-confirmed)

- **A-1** `guestRoles` declared in the gateway config (`{name = "alpha",
  permission = "write"}` / `{name = "alpha"}`) materialize as distinct guest-role
  rows, since `GuestRole.permission` is a field on the role. Verified from the
  schema, not from a live probe.
- **A-2** A 7-day ceiling on `scheduledAt` (FR-5.5) is chosen, not requested. It
  keeps an armed `time.AfterFunc` bounded and a mis-typed year from parking a
  timer forever.
- **A-3** The member banner polls rather than streams. The chat screen already
  polls other state; adding an SSE channel for this would be new machinery for a
  low-frequency event.
