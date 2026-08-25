# admin-managed-storage-limits — Tasks (proxy)

**Design**: `.specs/features/admin-managed-storage-limits/design.md`
**Status**: Planned
**Webapp tasks**: `crab-exoskeleton-webapp/.specs/features/admin-managed-storage-limits/tasks.md`
**Blocked by**: `.specs/quick/002-drop-media-ext-allowlist` (this edits the same
handler region)

## Test matrix and gates

Same derivation as `admin-bulk-instance-config`: no `.specs/codebase/TESTING.md`
in this repo, so the matrix follows `usermodels_test.go`, `media_test.go` and
`admin_test.go`.

| Code layer | Required tests |
| --- | --- |
| `internal/registry` | unit, own bbolt in `t.TempDir()` |
| `internal/docker` | unit, own root in `t.TempDir()` |
| `internal/httpapi` | unit via `s.Handler().ServeHTTP` |

| Gate | Command |
| --- | --- |
| quick | `go build ./... && go test ./internal/<pkg>/... -run <Pattern>` |
| full | `go test ./...` |
| baseline | `internal/docker` carries **9 pre-existing failures** on a clean HEAD, all `lchown … operation not permitted` (they need root). "Full gate passes" means no new failure beyond those 9. New tests MUST pass as non-root: pass `PicoclawUser: ""` so `chownTree` is a no-op. |

---

## Dependencies

```
T1 ──→ T2 ──→ T5 ──→ T6 ──→ T7
       T3 ────┘
       T4 [P]
```

T1 is a pure refactor and must land alone. T5 and T6 both write
`handlers.go`/the httpapi package and are sequential for that reason, not a
logical one.

---

## Task Breakdown

### T1: Generalize the policy cascade

**What**: `policyCascadeTx` becomes generic over the field type.
**Where**: `internal/registry/usermodels.go`, `usermodels_test.go`
**Depends on**: None
**Reuses**: itself — this is a type change, not a behaviour change
**Requirement**: FR-2.3

**Done when**:

- [ ] Signature is `policyCascadeTx[T any](tx, ref, pick func(ScopePolicy) *T) (T, ScopeLevel, bool)`
- [ ] `userModelsAllowedTx` and `CustomEndpointAllowed` are unchanged at the call
      site and their existing tests pass **without edits** — an edited test is not
      evidence a refactor preserved behaviour
- [ ] Walk order (subscription → tenant → agent → global) has a test that pins it
      by asserting the returned `ScopeLevel`, not just the value
- [ ] Gate: `go test ./internal/registry/...`

**Tests**: unit · **Gate**: quick
**Commit**: `refactor(registry): make the scope-policy cascade generic over the field`

---

### T2: Media limits on ScopePolicy

**What**: `MaxUploadBytes`/`QuotaBytes` fields, two `PolicyField` constants,
`MediaLimits(ref)` resolution.
**Where**: `internal/registry/usermodels.go` (fields),
`internal/registry/media_policy.go` (new), `media_policy_test.go` (new)
**Depends on**: T1
**Reuses**: `SetScopePolicy`, `ClearScopePolicy`, `GetScopePolicy`,
`policyCascadeTx`
**Requirement**: FR-2.1, FR-2.2, FR-2.3, FR-2.4, FR-2.5

**Done when**:

- [ ] Unset everywhere → `MaxUpload` nil, `Quota` nil, and the caller (not the
      registry) applies `mediaMaxBytes`
- [ ] `0` round-trips as a set value distinct from unset — the test asserts both
      the value and `found`
- [ ] A subscription value shadows a tenant value; clearing it returns the
      tenant's, not zero
- [ ] A record written before these fields existed unmarshals with both nil
      (write one with the old struct shape in the test)
- [ ] Gate: `go test ./internal/registry/... -run MediaPolicy`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(registry): per-scope media upload cap and storage quota`

---

### T3: Usage accounting

**What**: `Manager.UsageBytes(key)`, `Manager.ExistingSize(key, project, name)`.
**Where**: `internal/docker/media.go`, `media_test.go`
**Depends on**: None
**Reuses**: `ListMedia`'s walk guards, `publicRoot`, `openTree`
**Requirement**: FR-3.1, FR-3.2, FR-3.3, FR-3.5

**Done when**:

- [ ] `UsageBytes` sums the main workspace **and** every project segment — the
      test creates files in two projects and asserts the total
- [ ] Files under `attachments/` are counted
- [ ] No `maxListedMedia` cap: a tree with more entries than the listing cap
      still sums all of them
- [ ] Symlinks are neither followed nor counted
- [ ] An unreadable subtree does not zero the total
- [ ] `ExistingSize` returns 0 (not an error) for a name that is not there
- [ ] Passes as non-root (`PicoclawUser: ""`)
- [ ] Gate: `go test ./internal/docker/... -run 'UsageBytes|ExistingSize'`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(media): sum a member's workspace usage across projects`

---

### T4: Structured error bodies [P]

**What**: An `errBody` variant carrying `code` plus numeric fields.
**Where**: `internal/httpapi/` (wherever `errBody` lives), its test
**Depends on**: None
**Reuses**: `errBody`
**Requirement**: FR-4.2

**Done when**:

- [ ] Existing `errBody` output is byte-identical — every current error body is
      untouched
- [ ] The new variant emits `{"error":{"code":…,"message":…,"limit":…,"size":…}}`
- [ ] Gate: `go test ./internal/httpapi/... -run ErrBody`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(httpapi): error bodies that carry a code and its numbers`

---

### T5: Enforce both limits on upload

**What**: The two checks between `checkProject` and `StoreMedia`.
**Where**: `internal/httpapi/handlers.go`, `handlers_test.go`
**Depends on**: T2, T3, T4
**Reuses**: `Reg.MediaLimits`, `Mgr.UsageBytes`, `Mgr.ExistingSize`
**Requirement**: FR-1.2, FR-1.3, FR-4.1, FR-4.3, FR-4.4

**Done when**:

- [ ] Effective cap is `min(scope, Cfg.MediaMaxBytes)` — a scope cap **above** the
      config ceiling does not raise it, and there is a test for exactly that
- [ ] Cap is checked before quota (a huge file on a full workspace reports
      `media_too_large`, not `media_quota_exceeded`)
- [ ] A quota refusal leaves the tree untouched: assert the listing is unchanged
      and any same-named file still has its original bytes
- [ ] Re-uploading an existing name on a full workspace succeeds when the
      replacement is not larger (FR-3.5)
- [ ] No policy set → behaviour byte-identical to today
- [ ] Gate: `go test ./internal/httpapi/... -run Media`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(media): enforce the per-scope upload cap and storage quota`

---

### T6: Admin API and the usage field

**What**: `GET`/`PUT`/`DELETE /v1/admin/media-policy`; `usage` on `GET /v1/media`.
**Where**: `internal/httpapi/admin_media_policy.go` (new), `handlers.go`
(registration + listing response), tests alongside
**Depends on**: T5
**Reuses**: `admin_user_models.go`'s policy handlers as the template — scope
parsing, authorization, patch semantics, `?field=` delete
**Requirement**: FR-5.1 … FR-5.5

**Done when**:

- [ ] `GET` returns the level's own values **and** the resolved value with its
      deciding level
- [ ] `PUT` with one field leaves the other alone
- [ ] `DELETE ?field=quota_bytes` clears only that field, and the level inherits
      again
- [ ] A negative value is 400; `0` is accepted
- [ ] A caller without authority over the scope is refused, by the same call the
      model-policy routes use
- [ ] `GET /v1/media` carries `usage.bytes`, `usage.quota_bytes` (null when
      unlimited) and `usage.max_upload_bytes`
- [ ] Gate: `go test ./internal/httpapi/...`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(admin): read and write per-scope media limits`

---

### T7: Close-out

**What**: Full gate, acceptance walk, spec reconciliation.
**Where**: the spec's Reconciliation section, this Progress table
**Depends on**: T6

**Done when**:

- [ ] All seven acceptance items exercised
- [ ] Gate: `go test ./...` with no new failure beyond the 9 `lchown` baseline
      (verify by `git stash` + re-run)
- [ ] `gofmt -l` shows nothing new (`internal/authz/authz_test.go` and
      `internal/registry/registry.go` are pre-existing)

**Tests**: full suite · **Gate**: full
**Commit**: `docs(specs): reconcile admin-managed-storage-limits with what shipped`
