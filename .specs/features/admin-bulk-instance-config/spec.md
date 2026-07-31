# admin-bulk-instance-config — Specification

**Status:** Proxy slice IMPLEMENTED (T1–T9). Webapp slice not started.
**Size:** Large (3 new endpoints + new admin panel + a new on-disk artifact + a
revisit of `admin-instance-config-editor` DEC-5)
**Repos touched:**

| Repo | Role |
| --- | --- |
| `crab-shell-proxy` | `GET /v1/admin/scope/config/keys`, `GET /v1/admin/scope/config/inspect`, `PUT /v1/admin/scope/config` |
| `crab-exoskeleton-webapp` | New agent section (`?tab=config`) — key picker, inconsistency view, bulk apply |
| `zombie-crab-project` (parent) | Submodule pointer bump only — `/v1/admin/*` is already a single gateway wildcard |

---

## Problem Statement

`admin-instance-config-editor` gave an admin a way to repair **one** member's
`config.json`. Every setting that is really a *policy* — "this subscription's
agents may use Brave search", "raise `max_tokens` for this team" — therefore has
to be applied member by member, by hand, with no way to see beforehand which
members already have it and which do not.

Two consequences, both live today:

- **Drift is invisible.** An admin cannot answer "is this setting the same across
  my subscription?" without opening every instance in turn.
- **A policy change is O(members) manual edits**, each one a full-document
  round-trip through the raw editor, with no record of what changed.

The immediate trigger: the native web-search fix (`quick/001`) stores a Brave key
correctly, but `tools.web.brave.enabled` lives in `config.json` and ships
`false`. Turning search on for a real subscription is one manual edit per member
today.

## Goals

- [ ] An admin can change **one dotted key** of `config.json` across every
      instance of one agent in one subscription, in one action.
- [ ] Before applying, the admin sees the **current distribution** of that key —
      which instances hold which value, which lack it, which are unreadable — so
      "who will be affected" is answered from data, not assumed.
- [ ] Every applied change leaves a **`from`/`to` record** in the member's own
      environment, so the previous value is recoverable without guesswork.
- [ ] A change may optionally be written into the **agent template**, so members
      provisioned later inherit it instead of resurrecting the old value.

## Out of Scope

| Feature | Reason |
| --- | --- |
| Tenant-wide bulk edit | DEC-1. The ceiling is one subscription, chosen by the product owner. A tenant sweep crosses subscriptions that may be administered by different people. |
| Whole-subtree / first-level key replacement | DEC-2. The unit is a leaf at a dotted path. Replacing `tools` wholesale would overwrite every legitimate per-member difference under it. |
| A rollback button | DEC-6. The MVP writes the record; reverting is re-applying the old value through the same screen. Automatic rollback needs a conflict policy of its own. |
| Multiple keys per action | DEFER-1. One key per action keeps the inconsistency view, the diff and the migration record each about one thing. |
| Editing keys the proxy owns | FR-2.4. `ManagedConfigPaths` are rewritten by `materializeModels`/`alignWorkspace`; a bulk write there would be reverted by the very next materialization. |
| Semantic validation of values | Inherited from `admin-instance-config-editor` FR-2.3 — the proxy has no picoclaw config schema. |
| Hermes agents | `config.json` is picoclaw's file; hermes provisions from `config.yaml`. The section is picoclaw-only, like `model` and `persona`. |

---

## User Stories

### P1: See how one key varies across the subscription ⭐ MVP

**User Story**: As a subscription admin, I want to pick one `config.json` key and
see what every instance of an agent currently holds for it, so that I know which
members a change would actually touch.

**Why P1**: This is the half that does not exist anywhere today, and it is what
makes the write safe. Without it a bulk apply is a blind overwrite.

**Acceptance Criteria**:

1. WHEN the admin selects an agent and a subscription and opens the section THEN
   the system SHALL offer a key catalog built from that agent's **template**
   `config.json`, flattened to dotted leaf paths.
2. WHEN the admin needs a key the template does not carry THEN the system SHALL
   accept a **hand-typed dotted path** (a newer picoclaw's field, or one added by
   a previous repair).
3. WHEN a key is chosen THEN the system SHALL return, for every instance of that
   agent in that subscription, the value at that path, grouped into **distinct
   value buckets** with a member list and a count per bucket.
4. WHEN an instance's `config.json` does not contain the path THEN that instance
   SHALL appear in an explicit `absent` bucket — not merged with `null`, which is
   a legal value.
5. WHEN an instance's `config.json` is missing or does not parse THEN that
   instance SHALL appear in an `unreadable` bucket carrying the parse error, and
   SHALL NOT be counted as holding any value.
6. WHEN the selected path is one of `ManagedConfigPaths`, or nested under one
   THEN the system SHALL refuse with `400 managed_path` on **both** verbs, so a
   credential that legacy layouts left under `model_list` is never read back
   either.

**Independent Test**: With two members on `alpha` holding different
`agents.defaults.max_tokens`, and a third whose `config.json` is corrupt, the
inspect response has two value buckets and one `unreadable` entry.

---

### P2: Apply the change to every affected instance ⭐ MVP

**User Story**: As a subscription admin, I want to set that key to one value
across the subscription in a single action, so that a policy change is not
O(members) manual edits.

**Why P2**: It is the point of the feature, but it is strictly downstream of P1 —
the write is only safe once the distribution is visible, and P1 supplies the
per-instance revisions the write needs.

**Acceptance Criteria**:

1. WHEN the admin submits `{key, value, revisions}` THEN the system SHALL set
   `value` at `key` in every instance whose current value **differs**, and SHALL
   leave instances that already match **untouched** — no write, no restart, no
   migration record.
2. WHEN an instance lacks the intermediate objects on the path THEN the system
   SHALL create them, and SHALL NOT replace an existing sibling.
3. WHEN a path segment traverses a **non-object** (e.g. `tools.web` holds a
   string) THEN that instance SHALL fail with `path_conflict` and the rest of the
   batch SHALL still be applied.
4. WHEN an instance's revision in the request does not match its bytes on disk
   THEN that instance SHALL be skipped as `stale`, and the admin SHALL be told to
   re-inspect. A concurrent `materializeModels` must never be silently
   discarded.
5. WHEN any single instance fails THEN the batch SHALL NOT fail wholesale: the
   response SHALL enumerate every instance with an outcome of
   `applied | unchanged | stale | path_conflict | unreadable | error`, mirroring
   `PropagateScope`'s "per-workspace failures are logged, not returned".
6. WHEN the write succeeds THEN the restart SHALL be delivered **per changed
   workspace** — iterating the `applied` outcomes and reusing the per-workspace
   reduction the single-instance editor already uses (`RestartWorkspace(key)` for
   `now`, `RaiseWorkspaceRestartNotice(key, ReasonConfig)` otherwise). An
   instance reported `unchanged`, `stale`, `path_conflict` or `unreadable` SHALL
   NOT be restarted and SHALL NOT receive a notice. — DEC-8
7. WHEN no `restart=` parameter is supplied THEN the mode SHALL default to
   **`notice`**, not `now`. The shared `parsePolicyFields` default stays `now`
   for every other endpoint; the substitution is local to this handler and a test
   asserts the shared default is untouched. — DEC-9
8. WHEN `restart=schedule` is supplied THEN it SHALL behave as `notice`, which is
   what the per-workspace reduction does at every existing site (the scheduler
   arms per scope, and this endpoint's target is a set of workspaces, not a
   scope).
7. WHEN the caller is not at least `TierSubscription` for the target
   subscription THEN the system SHALL answer `403` and write nothing.

**Independent Test**: Three instances, two differing; a PUT reports 2 `applied`
and 1 `unchanged`, and only the two files change on disk.

---

### P3: Leave a `from`/`to` record in each member's environment ⭐ MVP

**User Story**: As a subscription admin, I want each changed instance to carry a
record of its previous value, so that I can put it back without having to
remember what it was.

**Why P3**: Ordered last because the write is useful without it, but it ships in
the MVP: a bulk change with no trail is the failure mode this feature would
otherwise introduce at N× the blast radius.

**Acceptance Criteria**:

1. WHEN an instance is changed THEN the system SHALL write one JSON file to
   `<userDir>/.config-migrations/<UTC-timestamp>-<dotted-key>.json` — a sibling
   of `workspace/`, inside the same bind mount as `config.json`.
2. WHEN the record is written THEN it SHALL contain at minimum `from` and `to`,
   plus the provenance a revert needs: `key`, `appliedAt`, `by`, `scope`,
   `revisionBefore`, `revisionAfter`.
3. WHEN the key was **absent** before the change THEN the record SHALL carry
   `"fromAbsent": true` and omit `from`, so a revert deletes the key instead of
   writing a literal `null`.
4. WHEN two bulk changes touch the same key on the same day THEN both records
   SHALL survive — the timestamp makes the filename unique, and a later change
   never overwrites an earlier record.
5. WHEN an instance is `unchanged`, `stale` or failed THEN **no** record SHALL be
   written for it.
6. WHEN the record cannot be written THEN the config write SHALL still stand and
   the failure SHALL be logged: the repair already landed, and discarding it to
   preserve bookkeeping is the worse trade.

**Independent Test**: After a bulk change, each changed workspace has exactly one
new file under `.config-migrations/` whose `from` equals what inspect reported
for it; unchanged workspaces have none.

---

### P4: Optionally write the change into the agent template

**User Story**: As an admin, I want to mark a change as durable so that members
provisioned later inherit it, instead of arriving with the old value.

**Why P4**: Without it, the Brave case is only half-fixed — `provisionUser`
treats an existing `config.json` as "returning user, leave as-is", but a **new**
member is seeded from the template and comes back with the old value.

**Acceptance Criteria**:

1. WHEN the admin sets `alsoTemplate: true` THEN the system SHALL apply the same
   key/value to `templates/<agent>/config.json` using the same path semantics.
2. WHEN `alsoTemplate` is set THEN the caller SHALL need no authority beyond the
   `AuthorizeUserManagement` gate the rest of the feature uses — a subscription
   manager may write the template. The consequence is real and must be
   **surfaced, not hidden**: the template seeds members of *every* subscription,
   and `template` is a `config.yaml` field distinct from the agent key — two
   agents may declare the same one, so the write reaches every agent that does.
   The UI SHALL state both before the save and FR-5.3 SHALL log the write
   separately. — DEC-4
3. WHEN the template is changed THEN a migration record SHALL be written to
   `templates/<agent>/.config-migrations/` on the same terms as P3.
4. WHEN the template write fails THEN the instance results SHALL still stand and
   the response SHALL report `template: {ok: false, detail}` — the same
   best-effort shape `reapplied` already uses.
5. WHEN `alsoTemplate` is false or absent THEN the template SHALL NOT be read or
   written, and `admin-instance-config-editor` DEC-5 SHALL continue to hold for
   every other path.

**Independent Test**: With `alsoTemplate: true`, the template file changes and a
newly provisioned member is seeded with the new value; with it false or absent
the template is byte-identical and no template migration record exists.

---

## Edge Cases

- WHEN the subscription has no provisioned instances of that agent THEN inspect
  SHALL return an empty bucket list and the apply form SHALL be unavailable —
  not a 404, because the subscription and agent both legitimately exist.
- WHEN the admin is at **tenant** scope rather than subscription THEN the section
  SHALL explain that bulk editing is subscription-level and require a
  subscription selection (DEC-1). No partial tenant sweep.
- WHEN no agent is selected THEN the section SHALL not render — every path in
  this feature is per-agent.
- WHEN the selected agent is not a picoclaw agent THEN the section SHALL be
  **absent**, not present-and-explaining-itself (`agent-scope.ts` convention).
- WHEN the dotted key contains a path separator, `..`, or an empty segment THEN
  the system SHALL answer `400 invalid_key` — the key becomes part of a filename
  in P3.
- WHEN the key exists in the template but in **no** instance THEN every instance
  is in the `absent` bucket and the apply creates it in all of them.
- WHEN `value` is `null` THEN it SHALL be written as JSON `null` — a legal value,
  distinct from absent. Deleting a key is not this endpoint's job (DEFER-2).
- WHEN the same member holds grants on two agents THEN only the selected agent's
  workspace is touched; `workspacesInScope` already filters by `AgentKey`.

---

## Requirements

### FR-1 — Key catalog (`GET /v1/admin/scope/config/keys`)

- **FR-1.1** `?tenant_id=&subs_acc_id=&agent=` returns the flattened dotted leaf
  paths of `templates/<agent>/config.json`, each with its template value and a
  `managed: true` flag for `ManagedConfigPaths` members.
- **FR-1.2** The catalog is a **suggestion list**, not a whitelist: FR-3 accepts
  any syntactically valid dotted path (P1 AC-2).
- **FR-1.3** Reading the template here is not a revisit of DEC-5 — DEC-5 forbade
  *editing* it. Writing is P4 and gated separately (DEC-4).

### FR-2 — Inspect (`GET /v1/admin/scope/config/inspect`)

- **FR-2.1** `?tenant_id=&subs_acc_id=&agent=&key=<dotted>` enumerates instances
  via the existing `workspacesInScope(Scope{Kind: ScopeSubscription, …,
  AgentKey: agent})` — no new enumeration logic.
- **FR-2.2** Response groups instances by **distinct JSON value** at `key`, plus
  the two non-value buckets `absent` and `unreadable`. Each instance entry
  carries `userAccId`, `email` (already available via `UserRef`), and its
  `revision` (`sha256:` of the current bytes), which the apply step echoes back.
- **FR-2.3** Values are compared by canonical JSON encoding, so `1` and `1.0`,
  and two objects with different key order, do not read as different buckets.
- **FR-2.4** `key` is refused with `400 managed_path` in **three** relations to
  `ManagedConfigPaths`, on this verb as well as FR-3 (P1 AC-6):
  1. it **is** a managed path (`model_list`);
  2. it is **under** one (`model_list.foo.api_keys`) — the constant already
     documents a listed path as covering its subtree;
  3. it is a **prefix** of one (`agents`, `agents.defaults`) — setting a value
     there would replace the object that holds `agents.defaults.provider`.
  The third relation is what a leaf-only rule (DEC-2) does not cover on its own,
  because nothing stops an admin typing an interior path.
- **FR-2.5** No Docker call. `os.ReadFile` per instance, as `ReadInstanceConfig`
  already does (NFR-2 of the single-instance editor).

### FR-3 — Apply (`PUT /v1/admin/scope/config`)

- **FR-3.1** Body:

  ```json
  {
    "key": "tools.web.brave.enabled",
    "value": true,
    "revisions": { "<userAccId>": "sha256:…" },
    "alsoTemplate": false
  }
  ```

  `value` is **any JSON value**, taken verbatim — not a string to be coerced.
  `true` and `"true"` are different requests.

  `revisions` is keyed by **`userAccId`**, which is unique within one
  subscription + agent — the pair is already fixed by the query parameters, so
  the rest of `WorkspaceKey` (`TenantID`, `SubsAccID`, `Role`) is not repeated in
  the key. FR-2.2 returns them in exactly this form; no composite string.
- **FR-3.2** Per instance: read → compare → set-if-different → write, where the
  write goes through the existing `WriteInstanceConfig(key, raw, revision)`
  rather than a new writer. Setting the path reuses the `childMap`
  get-or-create idiom. Three consequences, all accepted deliberately:
  - **Re-materialization runs per changed instance.** `WriteInstanceConfig`
    calls `reapplyWorkspace` after every write (the earlier feature's FR-3.3),
    so a 50-member change fires up to 50 materializations, each resolving the
    model registry. This is the cost of DEC-2 of that feature — managed paths
    stay correct *by construction* instead of by a second copy of the merge
    rules. See NFR-4 for what that means for the endpoint's cost profile.
  - **The document is re-marshalled.** Changing a value means the file is
    re-encoded, so a member's or an admin's own formatting is not preserved the
    way the single-instance editor preserves it for an unmodified document. The
    encoding matches what `materializeModels` already writes
    (`json.MarshalIndent`, two spaces), so the file stays in the shape the proxy
    produces elsewhere.
  - **The redaction round-trip is inherited, not re-implemented.** A legacy
    workspace can still carry `model_list[*].api_keys` in `config.json`, which
    `ReadInstanceConfig` masks; `WriteInstanceConfig`'s `unmaskAgainst` restores
    it before writing. The bulk path must use **both** halves and never pair a
    raw read with that writer. FR-2.4 makes the masked value unreachable as a
    *comparison target* anyway, since `model_list` is a managed path.
- **FR-3.3** Response is per-instance outcomes (P2 AC-5) plus a summary count per
  outcome. HTTP `200` whenever the batch ran, whatever the individual outcomes —
  the outcomes are the payload, not the status.
- **FR-3.4** An instance missing from `revisions` is treated as `stale`: the
  admin's inspect predates it, so it is a member provisioned since. Never write
  to an instance the admin did not see.
- **FR-3.5** Body cap `256 KiB` — this endpoint carries one value and a revision
  map, not documents. (The single-instance editor's 1 MiB cap is for whole files.)

### FR-4 — Migration records

- **FR-4.1** Path `<userDir>/.config-migrations/<RFC3339-compact-UTC>-<key>.json`,
  dir `0o700`, files `0o600`, and **not chowned** — they stay proxy-owned, unlike
  `config.json` and `.security.yml`. Those two are chowned because picoclaw reads
  them; picoclaw never reads a migration record, so granting the container user
  access would buy nothing and cost the only tamper-resistance available here.
  (Amended during T3: the first draft said "chowned like every sibling in the
  mount", which would have put an agent-owned file inside a directory the agent
  cannot traverse.)
- **FR-4.1a** The timestamp is second-resolution
  (`20260731T134502Z`) for a filename a human can read, so it is **not** unique
  on its own: two applies of the same key within one second would collide, and
  P3 AC-4 requires both to survive. The writer therefore appends `-2`, `-3`, …
  on an existing name (`O_CREATE|O_EXCL`, retry), which keeps the common case
  clean and makes accumulation a guarantee rather than a hope. Nanosecond
  precision was rejected: it makes every filename unreadable to buy uniqueness
  the retry already provides.
- **FR-4.2** These records are **not** the audit log of record; FR-5's proxy log
  is the authority. The record's job is operational recovery, and the distinction
  is stated in the code so a later reader does not treat it as tamper-evident.
  With FR-4.1's ownership the precise claim is narrower than the first draft's:
  the agent cannot read or rewrite an individual record, but it owns the parent
  directory and could remove the folder — `restrict_to_workspace: true` is what
  actually keeps it out. — DEC-5
- **FR-4.3** The directory is a sibling of `workspace/`, so it sits **outside**
  the agent's reach: the seeded config sets `restrict_to_workspace: true` and
  `allow_read_outside_workspace: false`. It is the same place `config.json` and
  `.security.yml` already live.

### FR-5 — Observability

- **FR-5.1** One log line per bulk apply naming the caller, the scope, the agent,
  the key, the outcome counts, and `alsoTemplate`. Never the value: a
  hand-typed path could address a credential-bearing field.
- **FR-5.2** Refusals (403, 400, `managed_path`) are logged at the same site, on
  the same terms `logInstanceConfigRefusal` established.
- **FR-5.3** One line per template write, separately, because its blast radius is
  every subscription running that agent.

### FR-6 — Authorization

- **FR-6.1** All three endpoints require
  `authz.AuthorizeUserManagement(profile, tenantID, subsAccID)` — identical to
  the single-instance editor (its DEC-3).
- **FR-6.2** `alsoTemplate: true` requires **no additional tier** (P4 AC-2,
  DEC-4). Its blast radius crosses subscriptions, so it is controlled by
  disclosure and audit — FR-5.3's separate log line and the UI statement — rather
  than by authority. This is the one place the feature grants reach beyond the
  caller's own branch, and it is deliberate.
- **FR-6.3** Every id is parsed as a UUID and `agent` matched against the
  configured agent set before any path join.
- **FR-6.4** The security consequence the single-instance editor stated in its
  FR-4.4 — that `config.json` carries the agent's sandbox boundary
  (`restrict_to_workspace`, `allow_read_outside_workspace`,
  `tools.allow_read_paths`) — applies here **times the member count**. Same
  accepted tier, so the same statement is required in the handler comment, and
  FR-5.1 is what makes it reconstructable.

### FR-7 — Webapp slice

- **FR-7.1** New section key `config` added to `TAB_KEYS`, `SECTION_TABS` and
  `PICOCLAW_ONLY` in `app/admin/tabs.ts` / `agent-scope.ts`. `CONTENT_TABS`
  derives itself, so hermes agents drop it with no second edit.
- **FR-7.2** The panel requires a **subscription** scope; at tenant scope it
  renders the explanation from the DEC-1 edge case instead of a form.
- **FR-7.3** Flow: pick key (catalog or typed) → inspect → the buckets with
  counts and member lists → enter the new value → a preview stating "N will
  change, M already match, K unreadable" → save with the restart-policy control
  every other panel uses.
- **FR-7.4** The `alsoTemplate` checkbox states, next to itself and before the
  save, that the template seeds **every subscription** running that agent — not
  only the one being edited. Since DEC-4 controls this by disclosure rather than
  by authority, that sentence is the control, and it may not be softened into a
  tooltip.
- **FR-7.5** Both locales in `lib/i18n/admin.ts`; `parity.test.ts` is the gate.

### NFR

- **NFR-1** No new Go dependency: `encoding/json`, `crypto/sha256`, `os`, `time`.
- **NFR-2** Additive only. No existing handler, response shape or authorization
  decision changes.
- **NFR-3** Reuse over reimplementation, specifically: `workspacesInScope`,
  `ReadInstanceConfig`/`WriteInstanceConfig`, `ManagedConfigPaths`, `valueAtPath`,
  `childMap`, `RestartWorkspace` / `RaiseWorkspaceRestartNotice`, `ReasonConfig`.
  (`applyRestartPolicy` is deliberately **not** reused — DEC-8.) A test asserts the bulk path
  writes through `WriteInstanceConfig` rather than its own writer.
- **NFR-4** Cost profile, stated rather than implied:
  - **Inspect** is N × `os.ReadFile` and wakes no container (NFR-2 of the
    single-instance editor). A 50-member subscription is cheap.
  - **Apply** is N × (read + write + re-materialization) for the instances that
    actually change — see FR-3.2. Materialization resolves the model registry
    and writes `.security.yml` twice plus `config.json` once, so the real cost of
    a bulk change scales with the number of **changed** members, not the number
    of members. Instances that already match are read and skipped.
  - This is accepted because the endpoint is an admin action behind a preview
    step, not a hot path. The leaner alternative — a bulk-only writer that skips
    materialization — was rejected: it would make managed paths safe only by
    FR-2.4's refusal, and a second write path is exactly the drift the earlier
    feature's DEC-2 exists to prevent.
  - The apply is **not** wrapped in a single transaction; there is no such thing
    across N workspaces. DEC-7's per-instance outcomes are the honest report of
    a partially applied batch.

---

## Decisions

| ID | Decision | Rationale |
| --- | --- | --- |
| DEC-1 | Ceiling is one subscription, and an agent must be selected first | Product owner. A tenant sweep crosses subscriptions with different administrators; the agent is already the first thing the admin screen asks for |
| DEC-2 | The unit is a leaf at a dotted path, not a first-level subtree | Product owner. The driving case (`tools.web.brave.enabled`) is nested, and replacing `tools` wholesale would flatten every legitimate per-member difference under it. `valueAtPath` already exists |
| DEC-3 | One timestamped record per migration, kept | Product owner. A single overwritten file loses the first of two same-day changes, and rollback would only ever reach back one step |
| DEC-4 | `alsoTemplate` is open to `TierSubscription`; its cross-subscription reach is handled by disclosure + audit, not by a higher tier | Product owner, after the design proposed an instance-tier gate and it was rejected. The gate would have made the checkbox unusable for the very admins the feature is for. The accepted trade is explicit: a change made by an admin of subscription A applies to **future** members of subscription B — and, since `template` is a config field two agents may share (T5), to future members of every agent declaring that template. The UI states both (P4 AC-2) and FR-5.3 logs it separately. Revisit if agents are ever templated per subscription |
| DEC-5 | Migration records are recovery aids, not an audit log | They live in a container-writable mount; FR-5's proxy log is the tamper-resistant record. Stating this prevents a later reader from trusting the wrong artifact |
| DEC-6 | No rollback button in the MVP | Product owner. Reverting is re-applying the old value through the same screen; an automatic revert needs a policy for "the value changed again after the migration", which is its own decision |
| DEC-7 | Per-instance outcomes, batch never fails wholesale | Mirrors `PropagateScope`. One member with a corrupt `config.json` must not block a policy change for the other 49 |
| DEC-8 | Restart is delivered per **changed** workspace, not via `applyRestartPolicy(scope, …)` | Product owner, after the design found that `BounceScope` filters by container **label** only — it cannot know which instances changed, so a scope bounce would restart members reported `unchanged`, `stale` and `unreadable` as well. It also runs `PropagateScope` (secrets sync) that this change does not need. The apply already knows the exact `applied` set |
| DEC-9 | Default mode is `notice` for this endpoint alone | Product owner. Every sibling defaults to `now` because its target is one workspace or one scope's secrets; here the default would be "N members lose their running agent at once from one click". `now` remains available explicitly. The shared `parsePolicyFields` default is **not** changed — the substitution is local, and a test pins that |

## Deferred

| ID | Idea | Why not now |
| --- | --- | --- |
| DEFER-1 | Several keys in one action | Each of inspect, preview and the migration record is about one key; batching them multiplies the UI and the failure matrix without changing what is possible |
| DEFER-2 | Deleting a key in bulk | Distinct semantics from setting one (and from setting `null`), and no driving case yet |
| DEFER-3 | Automatic rollback from the records, with conflict detection | DEC-6. The artifact is designed to make it possible later |
| DEFER-4 | Tenant-wide sweep | DEC-1 |
| DEFER-5 | A picoclaw config schema so the catalog can type-check values | Inherited DEFER-4 of the single-instance editor — picoclaw publishes no schema |
| DEFER-6 | Reading the catalog from the union of template + observed instance keys | The typed-path escape hatch (FR-1.2) covers the case at a fraction of the cost |

---

## Requirement Traceability

| ID | Story | Verified by | Status |
| --- | --- | --- | --- |
| BULK-01 | P1: catalog from template | `TestScopeConfigKeysPassesTheTemplateNotTheAgentKey, TestTemplateConfigKeysFlattensLeafShapes` | ✅ Verified |
| BULK-02 | P1: typed dotted path accepted | `TestScopeConfigAcceptsAKeyTheCatalogDoesNotCarry` | ✅ Verified |
| BULK-03 | P1: value buckets + counts + members | `TestInspectScopeConfigKeyGroupsEquivalentValues, TestInspectScopeConfigKeyCarriesRevisionAndEmail` | ✅ Verified |
| BULK-04 | P1: `absent` distinct from `null` | `TestLookupPathSeparatesNullFromAbsent, TestInspectScopeConfigKeySeparatesNullFromAbsent` | ✅ Verified |
| BULK-05 | P1: `unreadable` bucket with parse error | `TestInspectScopeConfigKeyReportsAnUnprovisionedWorkspace, TestInspectScopeConfigKeyReportsAnUnparseableConfig` | ✅ Verified |
| BULK-06 | P1: managed paths refused on both verbs | `TestIsManagedConfigPathCoversAllThreeRelations, TestInspectScopeConfigKeyRefusesABadKeyBeforeTouchingDisk, TestScopeConfigInspectMapsKeyRefusals` | ✅ Verified |
| BULK-07 | P2: set-if-different, unchanged untouched | `TestApplyScopeConfigKeyLeavesAMatchingInstanceUntouched` | ✅ Verified |
| BULK-08 | P2: intermediate objects created, siblings kept | `TestSetPathCreatesBranchAndKeepsSiblings, TestSetPathOverwritesAnExistingLeaf` | ✅ Verified |
| BULK-09 | P2: `path_conflict` isolated to its instance | `TestSetPathRefusesToReplaceANonObject, TestApplyScopeConfigKeyKeepsGoingPastAPathConflict` | ✅ Verified |
| BULK-10 | P2: stale revision skipped, never overwritten | `TestApplyScopeConfigKeyRefusesAnInstanceTheAdminNeverSaw` | ✅ Verified |
| BULK-11 | P2: per-instance outcomes, no wholesale failure | `TestApplyScopeConfigKeySummarizesEveryOutcome, TestApplyScopeConfigKeyReportsAWriteFailurePerInstance` | ✅ Verified |
| BULK-12 | P2: restart only the changed workspaces (DEC-8) | `TestScopeConfigPutRestartsOnlyTheChangedInstances, TestScopeConfigPutScheduleBehavesAsNotice` | ✅ Verified |
| BULK-12b | P2: default mode is `notice`, shared default untouched (DEC-9) | `TestScopeConfigPutDefaultsToNotice, TestSharedRestartDefaultIsStillNow` | ✅ Verified |
| BULK-13 | P2: `TierSubscription` gate | `TestScopeConfigRequiresUserManagement` | ✅ Verified |
| BULK-14 | P3: record written per changed instance | `TestWriteConfigMigrationNamesAndModes, TestApplyScopeConfigKeyRecordsWhatItChanged` | ✅ Verified |
| BULK-15 | P3: `from`/`to` + provenance fields | `TestWriteConfigMigrationRoundTripsProvenance, TestApplyScopeConfigKeyRefusesToTakeProvenanceFromTheBody, TestScopeConfigPutIgnoresForgedProvenance` | ✅ Verified |
| BULK-16 | P3: `fromAbsent` for a created key | `TestWriteConfigMigrationAbsentBeforeOmitsFrom, TestWriteConfigMigrationNullBeforeKeepsFromNull, TestApplyScopeConfigKeyAcceptsAJSONNull` | ✅ Verified |
| BULK-17 | P3: two same-day records both survive | `TestWriteConfigMigrationSameSecondBothSurvive` | ✅ Verified |
| BULK-18 | P3: no record for unchanged/stale/failed | `TestApplyScopeConfigKeyLeavesAMatchingInstanceUntouched, TestApplyScopeConfigKeyRefusesAnInstanceTheAdminNeverSaw` | ✅ Verified |
| BULK-19 | P3: record failure does not undo the write | `TestApplyScopeConfigKeyKeepsTheChangeWhenTheRecordFails, TestApplyScopeConfigKeyKeepsAWriteWhoseReapplyFailed` | ✅ Verified |
| BULK-20 | P4: template write under `alsoTemplate` | `TestApplyTemplateConfigKeySetsOnlyTheTargetedLeaf, TestApplyTemplateConfigKeyStaleRevisionWritesNothing, TestScopeConfigPutTemplateIsSeparatelyReportedAndLogged` | ✅ Verified |
| BULK-21 | P4: template reach disclosed in UI + logged separately (DEC-4) | `TestScopeConfigPutTemplateIsSeparatelyReportedAndLogged, TestScopeConfigPutTemplateFailureLeavesInstanceOutcomes` | ✅ Verified |
| BULK-22 | P4: template migration record | `TestApplyTemplateConfigKeyRecordsTheMigrationBesideTheTemplate, TestApplyTemplateConfigKeyRecordsFromAbsentForANewKey` | ✅ Verified |
| BULK-23 | FR-5: audit line without values | `TestApplyScopeConfigKeyNeverLogsTheValue, TestScopeConfigPutNeverLogsTheValue` | ✅ Verified |
| BULK-24 | FR-7: picoclaw-only section, subscription-scoped | Webapp | Pending |

**Coverage:** 25 rows (BULK-12b added by DEC-9). BULK-01…BULK-23 verified by the
proxy suite; BULK-24 is the webapp slice and is still Pending.

---

## Success Criteria

- [ ] Turning on `tools.web.brave.enabled` for a real subscription is **one**
      action, and the admin can state beforehand how many members it changes.
- [ ] After a bulk change, every changed workspace can be returned to its
      previous value using only the record in its own `.config-migrations/`.
- [ ] A member whose `config.json` is corrupt does not block a policy change for
      the rest of the subscription, and is named in the result.
- [ ] A new member provisioned after a bulk change with `alsoTemplate` inherits
      the new value, and the admin was told before saving that the template
      reaches every subscription on that agent.
- [ ] No existing endpoint's behaviour changes (NFR-2), proven by the untouched
      suites of `admin-instance-config-editor` and `restart-control`.
