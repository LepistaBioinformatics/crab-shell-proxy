# admin-instance-config-editor — Tasks (crab-shell-proxy)

From [design.md](design.md). `[P]` = parallelizable with its siblings.

Gate check for every task: `go build ./... && go vet ./... && go test ./...`.
Known-red baseline: the `TestEnsureRunning*` chown tests fail in this sandbox
(STATE.md L-001) — sandbox noise, not a regression. Record the baseline before
the first change and compare against it, not against green.

---

## Phase 1 — `docker` layer

### T-01 — `ManagedConfigPaths` + the anti-drift gate
- **What:** the exported constant from design §"Data model", with the docblock
  saying what adding a writer without adding a path costs. Plus
  `TestManagedConfigPathsMatchWriters`: seed a config whose every listed path
  holds a sentinel, run `materializeModels` + `alignWorkspace`, assert that
  **exactly** the listed paths changed.
- **Where:** new `internal/docker/instance_config.go`,
  `internal/docker/instance_config_test.go`
- **Depends on:** —
- **Reuses:** `materializeModels`, `alignWorkspace`, `migrate_models_test.go`'s
  fixture style (`t.TempDir` + a real registry).
- **Done when:** the test fails if a path is added to or removed from the
  constant without a matching change in the writers.
- **Verifies:** NFR-4, FR-3.1

### T-02 — `ReadInstanceConfig`
- **What:** read the bytes, `os.Stat`, `revisionOf`, parse-attempt →
  `Valid`/`ParseError`/`Offset`, `ManagedPaths`. `ErrNotProvisioned` when the
  file is absent. A file that does not parse is **data**: 0 errors returned.
- **Where:** `internal/docker/instance_config.go`
- **Depends on:** T-01
- **Reuses:** `config.UserWorkspace`, the `*json.SyntaxError` offset idiom.
- **Done when:** `Raw` is byte-identical to disk for a valid *and* a broken file.
- **Tests:** `TestReadInstanceConfigReturnsBytesVerbatim`,
  `…OnBrokenJSON`, `…NotProvisioned`
- **Verifies:** FR-1.2, FR-1.3, FR-1.5, FR-1.7

### T-03 — Legacy credential redaction [P with T-04]
- **What:** `redactModelKeys` over `model_list` (array **and** legacy object
  layout), replacing each `api_keys` value with `"***"` and collecting dotted
  paths into `RedactedPaths`. Read path only. `Revision` stays the **disk**
  revision, or the first save 409s — document that in the function.
- **Where:** `internal/docker/instance_config.go`
- **Depends on:** T-02
- **Reuses:** `migrate_models.go:542`'s knowledge of where legacy keys sat.
- **Done when:** a post-migration config comes back byte-verbatim (no reformat)
  and a legacy one comes back masked with its paths listed.
- **Tests:** `TestReadInstanceConfigRedactsLegacyModelKeys`
- **Verifies:** FR-6.2

### T-04 — `WriteInstanceConfig` [P with T-03]
- **What:** the 7-step sequence in design §"Write sequence": size cap → parse +
  object check → revision check → atomic temp+rename `0o600` → `chownTree` →
  `reapplyWorkspace` → re-read. Sentinels `ErrStaleRevision`,
  `ErrConfigNotObject`, `ErrConfigTooLarge`. `ReapplyResult` is **reported**, and
  a reapply failure never fails the call or reverts the bytes.
- **Where:** `internal/docker/instance_config.go`
- **Depends on:** T-02
- **Reuses:** `reapplyWorkspace`, `chownTree`. **No** new merge logic — DEC-2 is
  the whole reason the reapply is called instead.
- **Done when:** a submitted `agents.defaults.model_name` is on disk after the
  write and back to the registry's value after the reapply, in one call.
- **Tests:** `…RejectsInvalidJSON`, `…RejectsStaleRevision`, `…RejectsOversize`,
  `…IsAtomicAnd0600`, `…ReapplyRestoresManagedPaths`,
  `…SurvivesReapplyFailure`, `…RepairsUnparseableFile`
- **Verifies:** FR-2.2, FR-2.4, FR-2.5, FR-2.6, FR-2.7, FR-3.2, FR-3.3, FR-3.4

## Phase 2 — HTTP surface

### T-05 — `ReasonConfig`
- **What:** `ReasonConfig Reason = "config"` in the enum, in the existing block.
- **Where:** `internal/restart/restart.go`
- **Depends on:** —
- **Done when:** the value round-trips through a notice like its siblings.
- **Verifies:** FR-6.1

### T-06 — `Orchestrator` methods + fake
- **What:** the two methods under a new
  `// --- admin-instance-config-editor ---` heading; the test fake in
  `internal/httpapi/handlers_test.go` gains both.
- **Where:** `internal/httpapi/handlers.go`, `internal/httpapi/handlers_test.go`
- **Depends on:** T-02, T-04
- **Done when:** `go build ./...` is green with no other handler touched.

### T-07 — `adminInstanceKey`
- **What:** the key resolver from design §"httpapi layer": three UUIDs +
  a **required, non-`all`** `agent`, then `authz.AuthorizeUserManagement`. Its
  docblock says why it does not inherit the vehicle's agent the way
  `adminUserFileKey` does.
- **Where:** new `internal/httpapi/admin_instance_config.go`
- **Depends on:** T-06
- **Reuses:** `resolveAgentTarget`, `adminUserFileKey`'s validation order.
- **Done when:** a request routed as `alpha` with `agent=beta` yields a beta key.
- **Tests:** `TestInstanceConfigRejectsBadIDs`, `…RequiresExplicitAgent`,
  `…TargetsTheNamedAgentNotTheVehicle`, `…RequiresUserManagement`
- **Verifies:** FR-1.6, FR-4.1, FR-4.2, FR-4.3, DEC-1

### T-08 — The two handlers + routes
- **What:** `handleAdminInstanceConfigGet` / `…Put`, the status mapping table,
  the audit log line (caller + key + sizes + result, **never** the body), the
  restart delivery via `bounceNow` + `RestartWorkspace` /
  `RaiseWorkspaceRestartNotice`. The policy is parsed **before** the write. The
  file opens with the FR-7 exception comment from design — that comment is a
  deliverable, not decoration.
- **Where:** `internal/httpapi/admin_instance_config.go`,
  `internal/httpapi/handlers.go` (2 `mux.HandleFunc` lines beside the existing
  `users/files` routes)
- **Depends on:** T-05, T-07
- **Reuses:** `resolveSecretCaller`, `writeJSON`, `errBody`, `bounceNow`,
  `skillErrStatus`'s error-mapping shape.
- **Done when:** both verbs work end to end against a real temp workspace, and
  the FR-7 comment is present in the file.
- **Tests:** `…GetIncludesManagedPaths`, `…PutRestartNow`, `…PutRestartNotice`,
  `…PutSchedulesAsNotice`, `…PutLogsWithoutBody`,
  `…PutReturnsPostReapplyState`, `…NoFileNameParameter`
- **Verifies:** FR-1.1, FR-1.4, FR-2.1, FR-3.1, FR-5.1, FR-5.2, FR-5.3, FR-6.1,
  FR-6.3, NFR-3

## Phase 3 — Close-out

### T-09 — Grep gates + spec reconciliation
- **What:** run and record the two gates: (a) no `name`/`path`/`file` parameter
  is read in `admin_instance_config.go` (FR-1.4); (b) the three existing "no
  content route here" comments are unchanged
  (`git diff` touches neither `admin.go:530`'s block nor the webapp's). Then
  reconcile `spec.md` with what shipped — any FR that changed during
  implementation gets an inline note, in the style `restart-control/spec.md`
  FR-4.3 uses.
- **Where:** `.specs/features/admin-instance-config-editor/spec.md`
- **Depends on:** T-08
- **Done when:** both gates recorded and every FR either verified or annotated.

---

## Commit plan

One commit per task, message scoped to the slice:

```
feat(admin): managed config paths + anti-drift gate      (T-01)
feat(admin): read one instance's config.json             (T-02, T-03)
feat(admin): replace one instance's config.json          (T-04)
feat(admin): config restart reason                       (T-05)
feat(admin): instance-config endpoints                   (T-06..T-08)
docs(specs): reconcile instance-config spec with shipped (T-09)
```
