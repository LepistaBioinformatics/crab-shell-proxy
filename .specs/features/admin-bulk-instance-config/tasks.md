# admin-bulk-instance-config — Tasks (proxy)

**Design**: `.specs/features/admin-bulk-instance-config/design.md`
**Status**: In Progress
**Webapp tasks**: `crab-exoskeleton-webapp/.specs/features/admin-bulk-instance-config/tasks.md`

## Progress

| Task | Status | Tests | Note |
| --- | --- | --- | --- |
| T1 | ✅ Done | 8 pass | `pathState` kept unexported (package-internal concept); only `ValidateConfigKey`, `IsManagedConfigPath`, `ErrPathConflict`, `ErrInvalidConfigKey`, `ErrManagedConfigPath` are exported, for `httpapi` |
| T2 | ✅ Done | 8 funcs + 6 subtests | `Template` field dropped from `ScopeConfigInspection` — the catalog already carries it. Emails come from `ListSubscriptionUsers`, not `workspacesInScope` |
| T3 | ✅ Done | 6 pass | Records are proxy-owned, no chown (FR-4.1 amended). Signature has no `user` param |
| T4 | ✅ Done | 11 funcs + 6 subtests | Subagent added `ErrInvalidConfigValue` (covers empty AND unparseable) and `json:"-"` on `By`/`AppliedAt` so a request body cannot forge a migration record's provenance — better than the brief |
| T5 | ✅ Done | 14 pass | Param renamed `agent` → `template` and `TemplateCatalog.Agent` → `.Template`: `template` is a config.yaml field distinct from the agent key, and two agents may share one |
| T6 | ✅ Done | (see T6–T8) | No `scope` parameter exists, so DEC-1's ceiling holds by construction. Handler passes `agent.Template`, never the key |
| T7 | ✅ Done | (see T6–T8) | — |
| T8 | ✅ Done | 16 pass across T6–T8 | Envelope cap uses `http.MaxBytesReader` so an oversize body is a real 413, not a JSON 400. Four `Orchestrator` methods + four `fakeOrch` methods were needed — the tasks did not anticipate that |
| T9 | ✅ Done | 72 new test funcs total | All four criteria run, not two. Full suite: `internal/docker` at exactly the 9 pre-existing `lchown` failures, every other package green. Traceability now NAMES the verifying test on all 24 proxy rows — and doing that exposed BULK-02 ("a typed key the catalog lacks is accepted") as having no test at all, so `TestScopeConfigAcceptsAKeyTheCatalogDoesNotCarry` was written. Test-file diff review: `handlers_test.go` is +58/-0; the three removed `t.Errorf` lines are all from `quick/001` and each was replaced by a STRICTER assertion (`braveKeys` type-asserts two levels before checking a one-element list, where the old line was a string equality) |

**Process deviation, recorded:** T1–T5 were TDD (tests first, RED confirmed). T6–T8
were NOT — implementation went in before the tests. Instead of claiming a RED phase
that did not happen, the three load-bearing assertions were **mutation-checked**:
flipping the notice default to `now`, dropping the `applied` filter from the restart
loop, and passing `agent.Key` where `agent.Template` belongs each made exactly the
intended test fail. That is stronger evidence than ordering, but it is not the same
process, and the next HTTP-layer task should go back to RED-first.

**Pre-existing, not fixed** (untouched by this work): `gofmt -l` flags
`internal/authz/authz_test.go` and `internal/registry/registry.go`.

**Subagent test counts were all understated** — reconciled by `grep -c '^func Test'`:
T2 wrote 9 `TestInspectScopeConfigKey*` (reported 8), T4 wrote 15
`TestApplyScopeConfigKey*` (reported 11), T5 wrote 17 (reported 14). The numbers in
this table are the reconciled ones. Take a subagent's self-reported count as a
claim to check, not a fact.

Baseline holds throughout: `internal/docker` shows exactly the 9 pre-existing
`lchown` failures at every checkpoint.

---

## Test matrix and gates (derived, not assumed)

`.specs/codebase/TESTING.md` does **not** exist in this repo, so the matrix below
is derived from the convention every sibling feature already follows
(`secrets_test.go`, `instance_config_test.go`, `admin_test.go`): Go unit tests in
the same package as the code, table- or case-driven, one test per requirement.

| Code layer | Required tests | Parallel-safe |
| --- | --- | --- |
| `internal/docker/*.go` (pure helpers) | unit | yes — `t.TempDir()`, no shared state |
| `internal/docker/*.go` (Manager methods touching the FS) | unit | yes — each test gets its own root |
| `internal/httpapi/*.go` (handlers) | unit via `s.Handler().ServeHTTP` | yes |

| Gate | Command |
| --- | --- |
| quick | `go build ./... && go test ./internal/docker/... -run <TaskPattern>` |
| full | `go test ./...` |
| baseline | `internal/docker` carries **9 pre-existing failures** on a clean HEAD, all `lchown … operation not permitted` (they need root). "Full gate passes" means **no new failure beyond those 9** — verify by `git stash` + re-run, as `quick/001` did. New tests MUST pass as non-root: pass `PicoclawUser: ""` so `chownTree` is a no-op, the way `TestApplyNativeSecretsPreservesSiblings` does. |

---

## Execution Plan

### Phase 1: docker layer

```
T1 ──┬──→ T2 ──┐
     │         ├──→ T4
     └──→ T3 ──┤
               └──→ T5
```

T2 and T3 are `[P]` (different new files). T4 and T5 are `[P]` (different files,
both depend on T2/T3 landing).

### Barrier: Phase 1 must complete before Phase 2 starts

This is a **hard barrier**, not a convention. T7 depends on T2 and T8 on T4, but
Phase 2 is drawn as the single chain `T6 → T7 → T8` because all three write the
same file. Those two dependencies are satisfied only by the barrier. Reordering
Phase 2, or starting T8 while T4 is unfinished, breaks T8 with no diagram arrow
to warn you.

### Phase 2: httpapi layer (sequential — one new file plus `handlers.go`)

```
[Phase 1 complete] ──→ T6 ──→ T7 ──→ T8
```

No `[P]` here: T6, T7 and T8 all write `internal/httpapi/admin_bulk_config.go`,
and T6 also writes `handlers.go`. Shared mutable files disqualify parallelism
regardless of logical independence.

### Phase 3: close-out

```
T8 ──→ T9
```

---

## Task Breakdown

### T1: Dotted-path helpers and key validation

**What**: `lookupPath` (tri-state), `setPath`, `ValidateConfigKey`,
`IsManagedConfigPath`.
**Where**: `internal/docker/config_path.go` (new), `internal/docker/config_path_test.go` (new)
**Depends on**: None
**Reuses**: `childMap` (`secrets.go:333`), `ManagedConfigPaths` (`instance_config.go:54`)
**Requirement**: BULK-04, BULK-06, BULK-08, BULK-09

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] `lookupPath` returns `PathFound` for a key whose value is JSON `null`, and
      `PathAbsent` for a missing segment — the two are distinguishable
- [ ] `lookupPath` returns `PathConflict` when a segment holds a non-object
- [ ] `setPath` creates intermediate objects and leaves siblings untouched
- [ ] `setPath` returns `ErrPathConflict` rather than replacing a non-object
- [ ] `ValidateConfigKey` rejects `""`, `"a..b"`, `"a/b"`, `"a."`, `".a"` and
      accepts `A-Za-z0-9._-`
- [ ] `IsManagedConfigPath` is true for all **three** relations: `model_list`
      (equal), `model_list.x.api_keys` (under), `agents` and `agents.defaults`
      (prefix of `agents.defaults.provider`)
- [ ] `valueAtPath` is **not** modified — grep gate
- [ ] Gate: `go build ./... && go test ./internal/docker/... -run 'ConfigPath|ConfigKey|ManagedConfigPath'`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(config): add tri-state dotted-path helpers for bulk config edits`

---

### T2: Inspect one key across a scope [P]

**What**: `ScopeConfigInspection`/`ConfigKeyBucket`/`ConfigKeyInstance` types and
`Manager.InspectScopeConfigKey`.
**Where**: `internal/docker/bulk_config.go` (new), `internal/docker/bulk_config_test.go` (new)
**Depends on**: T1
**Reuses**: `workspacesInScope` (`shared.go:508`), `ReadInstanceConfig`
(`instance_config.go:95`)
**Requirement**: BULK-03, BULK-04, BULK-05, BULK-06

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] Managed or malformed key is refused **before** any filesystem access
- [ ] Instances grouped by `string(json.Marshal(value))`; `1` and `1.0` and two
      objects differing only in key order share a bucket
- [ ] `absent`, `path_conflict` and `unreadable` are separate buckets and carry no
      `value`
- [ ] A workspace with no `config.json` is `unreadable` / `not_provisioned`; one
      that does not parse is `unreadable` with the parse error
- [ ] Each instance entry carries the **on-disk** `Revision` and the `Email` from
      `UserRef`
- [ ] Bucket order is deterministic: value buckets by count desc then encoded
      value, then `absent`, `path_conflict`, `unreadable`
- [ ] `scope.AgentKey` is honoured — a member with two agents contributes one
      instance
- [ ] Gate: `go test ./internal/docker/... -run 'InspectScopeConfig'`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(config): inspect one config key across a subscription`

---

### T3: Migration record writer [P]

**What**: `ConfigMigration` type and `writeConfigMigration(dir, rec) (string, error)`.
**Where**: `internal/docker/config_migration.go` (new), `internal/docker/config_migration_test.go` (new)
**Depends on**: T1
**Reuses**: `chownTree` (`provision.go:190`), `containedJoin` (`paths.go:62`)
**Requirement**: BULK-14, BULK-15, BULK-16, BULK-17

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] Writes `<dir>/<yyyymmdd>T<hhmmss>Z-<key>.json`, dir `0o700`, file `0o600`,
      **proxy-owned with no chown** (spec FR-4.1, amended during this task)
- [ ] Two records for the same key inside one second **both** exist — the
      `O_CREATE|O_EXCL` retry appends `-2`
- [ ] `fromAbsent: true` omits `from`; a `from` of JSON `null` is written as
      `null` with `fromAbsent` absent — the two are distinguishable in the file
- [ ] Record carries `key`, `to`, `appliedAt`, `by`, `scope`, `revisionBefore`,
      `revisionAfter`
- [ ] The filename is joined through `containedJoin`
- [ ] The file's doc comment states DEC-5: these are recovery aids, not an audit
      log, because the directory sits in a container-writable mount
- [ ] Gate: `go test ./internal/docker/... -run 'ConfigMigration'`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(config): record from/to for each bulk config change`

---

### T4: Apply one key across a scope [P]

**What**: `ScopeConfigChange`/`InstanceOutcome`/`ScopeConfigResult` types and
`Manager.ApplyScopeConfigKey`.
**Where**: `internal/docker/bulk_config.go` (modify), `internal/docker/bulk_config_test.go` (modify)
**Depends on**: T2, T3
**Reuses**: `ReadInstanceConfig`, `WriteInstanceConfig` (`instance_config.go:156`)
**Requirement**: BULK-07 … BULK-11, BULK-18, BULK-19

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] An instance already holding the target value is `unchanged`: no write, no
      record, and its `modifiedAt` is untouched
- [ ] An instance absent from `Revisions` is `stale` and is **not** written
- [ ] A stale revision is `stale` and the file is byte-identical afterwards
- [ ] `path_conflict` on one instance does not stop the others
- [ ] The edited document comes from `cfg.Raw` (redacted) and goes through
      `WriteInstanceConfig` — a legacy `model_list[*].api_keys` survives the
      round-trip unmasked on disk
- [ ] `WriteInstanceConfig`'s `ReapplyResult` reaches the outcome; a failing
      reapply leaves the outcome `applied`
- [ ] A failing record write leaves the outcome `applied` with `recordError` set
- [ ] `Summary` counts equal the outcome tally
- [ ] **NFR-3 gate**: a test asserts the bulk path opens no `config.json` of its
      own — the only writer is `WriteInstanceConfig`
- [ ] Gate: `go test ./internal/docker/... -run 'ApplyScopeConfig'`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(config): apply one config key to every differing instance`

---

### T5: Template catalog and opt-in template write [P]

**What**: `TemplateConfigKeys(agent)` and `ApplyTemplateConfigKey(...)`.
**Where**: `internal/docker/template_config.go` (new), `internal/docker/template_config_test.go` (new)
**Depends on**: T1, T3
**Reuses**: `config.TemplatesDir` (`config/config.go:442`), `parseConfigObject`,
`writeConfigAtomic`, `revisionOf` (all `instance_config.go`), `writeConfigMigration`
**Requirement**: BULK-01, BULK-20, BULK-22

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] `TemplateConfigKeys` returns flattened dotted **leaf** paths with values,
      a `managed` flag from `IsManagedConfigPath`, and `templateRevision`
- [ ] A stale `templateRevision` refuses the write, file byte-identical
- [ ] The write goes through `writeConfigAtomic`
- [ ] The template is **not** chowned and **not** reapplied — a test pins both
      omissions, with the reason in its name and comment (it is unmounted and is
      not a workspace)
- [ ] A record lands in `templates/<agent>/.config-migrations/`
- [ ] Gate: `go test ./internal/docker/... -run 'TemplateConfig'`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(config): read and optionally write the agent template config`

---

### T6: Scope guard and the keys endpoint

**What**: `adminBulkConfigScope`, the three route registrations, and
`handleAdminScopeConfigKeys`.
**Where**: `internal/httpapi/admin_bulk_config.go` (new),
`internal/httpapi/handlers.go` (modify),
`internal/httpapi/admin_bulk_config_test.go` (new)
**Depends on**: T5
**Reuses**: `adminInstanceKey`'s shape (`admin_instance_config.go:43`),
`resolveAgentTarget`, `authz.AuthorizeUserManagement`
**Requirement**: BULK-01, BULK-13

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] Routes registered next to the existing `users/config` pair:
      `GET /v1/admin/scope/config/keys`, `GET /v1/admin/scope/config/inspect`,
      `PUT /v1/admin/scope/config`
- [ ] `tenant_id`, `subs_acc_id`, `agent` all required; non-UUID → 400; unknown
      agent → 400
- [ ] **No `scope` parameter is read** — grep gate. DEC-1's ceiling holds because
      there is no tenant form of the request, not because one is rejected
- [ ] Caller below `TierSubscription` → 403, and the refusal is logged
- [ ] The file's doc comment states the `admin-shared-content` FR-7 distinction
      (proxy-materialized provisioning state, not member-authored content) and
      the FR-6.4 sandbox-boundary consequence times the member count
- [ ] Gate: `go test ./internal/httpapi/... -run 'ScopeConfigKeys|BulkConfigScope'`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(api): add scope config keys endpoint`

---

### T7: The inspect endpoint

**What**: `handleAdminScopeConfigInspect`.
**Where**: `internal/httpapi/admin_bulk_config.go` (modify), test file (modify)
**Depends on**: T2, T6
**Reuses**: `Manager.InspectScopeConfigKey`, `writeJSON`
**Requirement**: BULK-03, BULK-06

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] `?key=` missing → 400 `invalid_key`; managed → 400 `managed_path`
- [ ] A scope with zero instances → 200 with empty buckets, **not** 404
- [ ] Response shape matches the design's `ScopeConfigInspection`
- [ ] Gate: `go test ./internal/httpapi/... -run 'ScopeConfigInspect'`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(api): add scope config inspect endpoint`

---

### T8: The apply endpoint, audit and restart delivery

**What**: `handleAdminScopeConfigPut` — body decode, 256 KiB envelope cap,
`ApplyScopeConfigKey`, optional template write, audit lines, restart policy.
**Where**: `internal/httpapi/admin_bulk_config.go` (modify), test file (modify)
**Depends on**: T4, T5, T6
**Reuses**: `parsePolicyFields` (`restart.go:316`), `bounceOrNotify`
(`model.go:112`), `RestartWorkspace` / `RaiseWorkspaceRestartNotice`
(`restart_control.go:62`), `restart.ReasonConfig` (`restart.go:51`),
`logInstanceConfigRefusal`'s pattern
**Requirement**: BULK-11, BULK-12, BULK-12b, BULK-20, BULK-21, BULK-23

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] Restart policy parsed **before** the apply; delivered after, **only** to the
      workspaces whose outcome is `applied` (DEC-8) — a test with one `applied` and
      three non-`applied` instances asserts exactly one restart/notice
- [ ] `applyRestartPolicy` / `BounceScope` are **not** called — grep gate, with the
      reason in the test name (a scope bounce cannot know which instances changed)
- [ ] An absent `restart=` parameter defaults to **`notice`** (DEC-9), and a test
      asserts `parsePolicyFields`' shared default is still `now` for a sibling
      endpoint — the substitution must stay local
- [ ] `restart=schedule` behaves as `notice`
- [ ] Body over 256 KiB → 413 via `io.LimitReader`, nothing written
- [ ] Always 200 when the batch ran, whatever the individual outcomes
- [ ] `alsoTemplate` requires no extra tier (DEC-4) and produces a **separate**
      log line
- [ ] `template.ok:false` leaves the instance outcomes intact
- [ ] **FR-5.1 gate**: a test asserts no log line contains the submitted value,
      for both a successful apply and a refusal
- [ ] `restart.ReasonConfig`'s doc comment widened to cover a bulk change
- [ ] Gate: `go test ./internal/httpapi/... -run 'ScopeConfigPut'`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(api): apply one config key across a subscription`

---

### T9: Full-suite regression against baseline

**What**: Prove no new failures and close the traceability table.
**Where**: `.specs/features/admin-bulk-instance-config/spec.md` (status column),
no code
**Depends on**: T8
**Reuses**: the baseline procedure from `.specs/quick/001-native-web-secret-shape`
**Requirement**: NFR-2, NFR-3

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] `go build ./...` clean
- [ ] `go test ./...` — `internal/httpapi` and `internal/registry` green;
      `internal/docker` shows **exactly** the 9 pre-existing `lchown` failures and
      no others, proven by a `git stash` baseline run
- [ ] Every BULK-xx row in the spec's traceability table names the test that
      verifies it
- [ ] No existing test file's assertions were weakened (`git diff` review of
      `*_test.go` limited to additions)

**Tests**: none (verification task) · **Gate**: full
**Commit**: `test(config): close bulk config traceability`

---

## Validation

### Check 1 — Task granularity

| Task | Scope | Status |
| --- | --- | --- |
| T1 | 4 pure functions, 1 new file, one concept (dotted paths) | ✅ cohesive |
| T2 | 1 Manager method + its types | ✅ granular |
| T3 | 1 writer + 1 type | ✅ granular |
| T4 | 1 Manager method + its types | ✅ granular |
| T5 | 2 functions, one concept (the template document) | ✅ cohesive |
| T6 | 1 guard + 1 endpoint + route wiring | ✅ cohesive |
| T7 | 1 endpoint | ✅ granular |
| T8 | 1 endpoint | ✅ granular |
| T9 | verification only | ✅ granular |

### Check 2 — Diagram / definition cross-check

| Task | `Depends on` (body) | Diagram arrows | Status |
| --- | --- | --- | --- |
| T1 | None | root | ✅ |
| T2 | T1 | T1 → T2 | ✅ |
| T3 | T1 | T1 → T3 | ✅ |
| T4 | T2, T3 | T2 → T4, T3 → T4 | ✅ |
| T5 | T1, T3 | T1 → T5 (via T3 branch), T3 → T5 | ✅ |
| T6 | T5 | T5 → T6 | ✅ |
| T7 | T2, T6 | T6 → T7 (T2 already upstream of T6 via T4/T5? **no**) | ⚠️ see note |
| T8 | T4, T5, T6 | T6 → T8 | ⚠️ see note |
| T9 | T8 | T8 → T9 | ✅ |

**Note on T7/T8**: the Phase 2 chain is drawn as a sequential line because all
three tasks write the same file. T7's dependency on T2 and T8's on T4/T5 are
satisfied by the **stated barrier** above, which is why that barrier is part of
the execution plan rather than a footnote here. The bodies name the real data
dependencies; if Phase 2 is ever reordered, the bodies are what remain correct.

**Parallel check**: T2 ∥ T3 touch different new files; T4 ∥ T5 touch different
files (`bulk_config.go` vs `template_config.go`). No `[P]` pair depends on the
other. ✅

### Check 3 — Test co-location

| Task | Layer | Matrix requires | Task says | Status |
| --- | --- | --- | --- | --- |
| T1 | docker pure helpers | unit | unit | ✅ |
| T2 | docker Manager method | unit | unit | ✅ |
| T3 | docker FS writer | unit | unit | ✅ |
| T4 | docker Manager method | unit | unit | ✅ |
| T5 | docker Manager method | unit | unit | ✅ |
| T6 | httpapi handler | unit | unit | ✅ |
| T7 | httpapi handler | unit | unit | ✅ |
| T8 | httpapi handler | unit | unit | ✅ |
| T9 | none (no code) | none | none | ✅ |

No task defers its tests to a later one.
