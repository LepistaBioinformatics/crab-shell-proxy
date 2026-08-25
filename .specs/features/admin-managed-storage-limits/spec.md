# admin-managed-storage-limits — Specification

**Status:** Planned
**Size:** Large (new policy fields, a new enforcement point, a new admin surface)

| Repo | Role |
| --- | --- |
| `crab-shell-proxy` | This spec: scope policy fields, the cascade, quota accounting, two enforcement points, `GET`/`PUT`/`DELETE /v1/admin/media-policy` |
| `crab-exoskeleton-webapp` | Admin editor + the member-facing usage readout — own `spec.md` in that repo |
| `zombie-crab-project` / `zombie-crab-project-mkt` | Submodule pointer bumps only |

**Follows** `unrestricted-upload-types`. With the type gate gone, size is the
**only** thing standing between a workspace and whatever a member drags into it —
and `paste-and-drop-upload` makes dragging the easy path. A fixed 10 MiB compiled
into a deploy is the wrong shape for that: it is either too small for the
research files this now accepts, or too generous for a subscription that should
not be storing hundreds of them.

---

## Problem

Two numbers govern uploads and neither is reachable by an administrator:

- `mediaMaxBytes` — 10 MiB, set in `config.yaml` at deploy time, identical for
  every tenant, every subscription and every member on the instance. Changing it
  is a redeploy.
- **There is no quota at all.** Nothing bounds a workspace's *total* storage. A
  member can upload 10 MiB at a time, forever, and the first sign of trouble is
  the host's disk.

The failure is also mute. The proxy answers 413 with the prose
`"file exceeds the N-byte limit"`, the BFF forwards it verbatim, and the webapp —
which maps *codes*, not sentences — shows "Algo deu errado."

## Goal

An administrator sets, for a scope they manage, the largest single upload and the
total a member's workspace may hold. Both inherit down the scope cascade the
proxy already has. Both refuse legibly, in the member's language, naming the
number they hit.

## Non-goals

- **Not** an instance-wide storage dashboard. Usage is answered per workspace, on
  demand.
- **Not** a retention or clean-up policy. Nothing here deletes a member's files;
  a full workspace refuses new uploads and says so.
- **Not** a quota on anything but member uploads. Agent deliveries under
  `attachments/` count toward the total read (they occupy the same disk) but no
  agent write is ever refused — see DEC-4.

---

## Requirements

### The two limits, and why they are two

- **FR-1.1** `mediaMaxBytes` **stays** in `config.yaml` and keeps its current job:
  it is the **hard parse ceiling**, the bound on what this process will read off
  the wire (`MaxBytesReader` / `ParseMultipartForm`). It is an operator and
  capacity concern, not a policy one.
- **FR-1.2** The administrable per-upload cap is a **second, separate check**,
  applied **after** the request body has been parsed.

  This split is forced, not stylistic. `handleMediaPost` bounds the body
  *before* it reads `tenant_id`/`subs_acc_id`, and those arrive as **form fields
  inside that same multipart body** — the scope cannot be known until the body
  has been read, so the parse bound cannot be per-scope. Anyone trying to make
  the ceiling dynamic will find this out the hard way; it is written down here so
  they do not have to.
- **FR-1.3** The effective per-upload cap is `min(scope cap, mediaMaxBytes)`. An
  administrator cannot raise their scope above what the process will read; the
  admin API accepts the larger value but the enforcement is the minimum, and the
  webapp shows the effective number (webapp spec).

### Policy storage and resolution

- **FR-2.1** `registry.ScopePolicy` gains `MaxUploadBytes *int64` and
  `QuotaBytes *int64`. Same struct, same `scope_policy` bucket, same
  `Set`/`Clear`/`Get` plumbing as the model policy — a second bucket would be a
  second set of the same bugs.
- **FR-2.2** Pointers, for the reason the existing fields are pointers: "not set
  at this level" and "set here to this value" are different answers, and only the
  pointer distinguishes an inherited value from a deliberate one.
- **FR-2.3** Resolution walks the existing cascade, most-specific first:
  subscription → tenant → agent → global. `policyCascadeTx` is generalized over
  the field type (it is `*bool`-only today) rather than copied.
- **FR-2.4** Unset at every level means: per-upload cap falls back to
  `mediaMaxBytes`, and **quota is unlimited**. Unlimited is the honest default —
  a quota nobody chose, applied to workspaces that already hold files, would
  refuse uploads for a rule no administrator ever set.
- **FR-2.5** `0` is a **valid, meaningful value** and is not the same as unset: it
  means "no uploads at all" for the cap, and "no storage at all" for the quota.
  A negative value is rejected with 400.

### Accounting

- **FR-3.1** Usage is summed per `WorkspaceKey` — tenant + subscription + agent
  role + user account — **across that member's project workspaces**, by walking
  the user root above the per-project segments rather than `publicRoot` (which is
  one project's directory). A member does not experience their projects as
  separate storage allowances, and a per-project quota would be a rule they could
  not see.
- **FR-3.2** The walk is **uncapped**, unlike `ListMedia` (`maxListedMedia`). A
  sum that stops counting is a quota that stops enforcing.
- **FR-3.3** Symlinks are not followed and are not counted, matching `ListMedia`.
- **FR-3.4** A full walk **per upload** is accepted, deliberately, and recorded
  here so it is not later "optimized" into a cached counter: the agent writes
  into this same tree from inside its container, so any counter the proxy keeps
  drifts from the truth and the drift is invisible.
- **FR-3.5** When the incoming filename **collides** with an existing file, that
  file's size is subtracted before the check. `StoreMedia` opens `O_TRUNC`, so a
  re-upload replaces rather than adds — without the subtraction, replacing a
  90 MiB file with an 80 MiB one is refused on a full workspace, which is both
  wrong and unexplainable.

### Enforcement

- **FR-4.1** `handleMediaPost`, after the scope resolves and the project check
  passes, refuses with **413** and a machine code when the file exceeds the
  effective per-upload cap, and with **413** and a distinct code when storing it
  would exceed the quota.
- **FR-4.2** Both bodies carry a **code plus the numbers**, e.g.
  `{"error":{"code":"media_too_large","limit":52428800,"size":81000000}}`. The
  prose sentence goes: the webapp translates codes, and a sentence it cannot map
  is what produced "Algo deu errado."
- **FR-4.3** The check is in the **proxy**, which is the boundary. A webapp
  pre-check is a courtesy for a better message and may be added later; it is
  never the enforcement. Same posture as `isReservedFolder`.
- **FR-4.4** Nothing partial survives a refusal: the quota check happens before
  `StoreMedia`, so no bytes are written and no existing file is truncated.

### Reading the policy

- **FR-5.1** `GET /v1/media` gains a `usage` object: bytes used, the effective
  quota (`null` when unlimited) and the effective per-upload cap. One extra field
  on a response the files panel already fetches — a second round trip for a
  number shown beside a list is a request nobody needs.
- **FR-5.2** `GET /v1/admin/media-policy?scope=…` returns the level's own values
  **and** what each field currently resolves to, with the level that decided it —
  the shape `GET /v1/admin/model-policy` already returns, so the admin screen can
  say "inherited from tenant" rather than showing an empty box.
- **FR-5.3** `PUT /v1/admin/media-policy` patches one or both fields; a field
  omitted from the body is left alone (`SetScopePolicy`'s existing contract).
- **FR-5.4** `DELETE /v1/admin/media-policy?field=…` clears a level so it
  inherits again — the `ClearScopePolicy` contract, not "set it to zero".
- **FR-5.5** All three carry the same authorization as the model-policy routes:
  an administrator of the scope being written. No new authz concept.

---

## Decisions

- **DEC-1 — one policy struct, two subjects.** Media limits go into
  `ScopePolicy` beside the model flags rather than into a bucket of their own.
  The cascade, the "pointer means unset" rule and the clear-to-inherit semantics
  are the parts that are easy to get wrong, and they are already written and
  tested once.
- **DEC-2 — quota per member workspace, not per account.** A member with grants
  on two agents has two workspaces and two allowances. That is what the
  `WorkspaceKey` names, what the directory layout separates, and what an
  administrator scoping by subscription is thinking about.
- **DEC-3 — no reservation, no locking.** Two simultaneous uploads can both pass
  the check and jointly exceed the quota by up to one file. Accepted: the
  alternative is a lock across an I/O path, for an overshoot that the next upload
  refuses anyway.
- **DEC-4 — the agent is never refused.** Quota is checked in the upload
  handler only. Files the agent writes into `attachments/` count toward the total
  (they are on the same disk and the member can see them) but a turn that
  produces a deliverable must not fail because the workspace is near its limit —
  that would surface as a broken agent, not as a full disk.

---

## Acceptance

1. With no policy set anywhere, uploads behave exactly as before: bounded by
   `mediaMaxBytes`, no quota.
2. A subscription-level cap of 50 MiB accepts a 40 MiB file and refuses a 60 MiB
   one with `media_too_large` and the number.
3. A tenant-level quota is inherited by a subscription that sets none, and the
   `GET` names `tenant` as the deciding level.
4. Clearing the subscription's cap returns it to the tenant's value, not to zero.
5. Re-uploading a file that already exists succeeds on a workspace at quota when
   the replacement is not larger (FR-3.5).
6. A refused upload leaves the existing file intact and adds nothing to the tree.
7. `GET /v1/media` reports a usage total that matches the sum of the listing,
   including files under `attachments/` and inside projects.
