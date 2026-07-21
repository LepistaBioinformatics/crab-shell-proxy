# admin-model-override — proxy CORE report

Scope: config selectable-model list, override store, `resolveModel`,
`reapplyModel`, and wiring `resolveModel` into the first-provision path in
`EnsureRunning`. HTTP handlers, gateway config, and webapp are separate later
tasks (not touched).

## Files touched

- `internal/config/config.go` — `Agent.Models []*ModelConfig`, APIKey
  resolution + validation (provider+name required, no duplicate
  `{provider,name}` within `Models`) in `Load`; `Agent.SelectableModels()`,
  `Agent.FindModel(provider, name)`; `TenantModelOverrideFile`,
  `SubscriptionModelOverrideFile`, `UserModelOverrideFile` path builders.
- `internal/docker/model.go` (new) — `ModelSel`, `getModelOverride`,
  `setModelOverride`, `clearModelOverride`, `resolveModel` (user > sub >
  tenant > agent.Model, stale override logged-and-skipped), `reapplyModel`
  (free function, config.json + .security.yml read-modify-write),
  `setModelListEntry` helper.
- `internal/docker/manager.go` — `EnsureRunning` now calls
  `m.resolveModel(agent, key)` instead of passing `agent.Model` straight to
  `provision`. `RestartScope` untouched, as instructed.
- Tests: `internal/config/config_test.go`, `internal/docker/model_test.go`
  (new).

## yaml lib

`gopkg.in/yaml.v3` — matches the rest of the codebase (`config.go`,
`secrets.go`, `shared.go`). `reapplyModel` reuses the existing
`readSecurityConfig`/`writeSecurityConfig` helpers in `secrets.go` (the same
read-modify-write-preserving-siblings path `applyNativeSecrets` already uses
for native slots) rather than re-implementing YAML I/O.

## reapplyModel token/secret survival — confirmed

`TestReapplyModelPreservesTokenAndSecrets` seeds a `.security.yml` with
`channel_list.pico.settings.token: pico-existing-token` and a pre-existing
`model_list.legacy-model.api_keys: [legacy-key-xyz]`, calls `reapplyModel`
with a new model, and asserts: (1) the new `model_list[gpt-4o].api_keys`
entry is written with the new key, (2) `readPicoToken` still returns
`pico-existing-token` unchanged, and (3) `legacy-key-xyz` is still present.
Also asserts `config.json`'s `agents.defaults.provider/model_name` update
while an unrelated `workspace` field survives untouched. Passes.

## Test summary

- `go test ./internal/config/... -run 'Model|Selectable|FindModel'` — 8
  tests, all pass.
- `go test ./internal/docker/... -run 'Model|Selectable|ResolveModel|Reapply|Override'`
  — 7 tests (incl. pre-existing `TestApplyModel`), all pass.
- Full `go test ./...`: `internal/docker` has 8 pre-existing failures
  (`TestEnsureRunning*`, `TestCreateAddsReadOnlySecretsBind`,
  `TestRestartWorkspaceRestartsAndRearms`, `TestScaleToZeroIdleStop`,
  `TestContinuousDoesNotArmIdle`, `TestReconcileEnsuresContinuousWorkspaces`)
  — all `chown: operation not permitted` (sandbox has no root/CAP_CHOWN).
  Confirmed pre-existing via `git stash -u` + rerun before making any
  changes: identical failures on the unmodified branch. Unrelated to this
  change, not touched by it.

## Gate

- `gofmt -l` on all touched/new files: clean.
- `go build ./...`: clean.
- `go vet ./internal/...`: clean.
- Target test filter: pass (see above).

## Concerns / assumptions

1. **`reapplyModel` ownership gap (real, for the next task to handle).**
   `reapplyModel(userDir, model)` is a free function per the task's exact
   signature — it takes no `user`/Manager context, so it writes
   `config.json` (0600) and `.security.yml` (0600 then re-locked 0444 by
   `writeSecurityConfig`) as whatever uid the proxy process runs as, and
   never chowns to `cfg.PicoclawUser`. That's correct for *this* task (no
   caller invokes it yet — "make available, keep minimal, don't touch
   RestartScope"). But the later HTTP/RestartScope task that actually calls
   `reapplyModel` against an established non-root workspace **must chown the
   two files to `PicoclawUser` after calling it** (or extend the signature
   with a `user string` param), or a non-root picoclaw container will be
   unable to read its own re-applied config/secrets after an override
   change.
2. **0444 vs. the task's literal "write back 0600" for `.security.yml`.**
   `reapplyModel` ends up re-locking `.security.yml` to 0444 because it
   reuses `writeSecurityConfig` (which chmods 0444 after every write — the
   same helper `applyNativeSecrets` uses, and the established convention:
   ".security.yml is content-read-only at runtime"). This is a deliberate
   deviation from the literal 0600 in the task text, in favor of consistency
   with the codebase's one existing read-modify-write path for this file.
   `config.json` is plain 0600 as specified.
3. **`resolveModel` also swallows a malformed override file** (not just a
   stale-but-well-formed one) — logs and falls through to the next level
   rather than erroring out. Judgment call to keep a corrupt override file
   from bricking provisioning; flagging in case the later HTTP task wants
   stricter behavior on the read side of `GET /v1/admin/model`.
4. Duplicate-`{provider,name}` validation only rejects dupes *within*
   `Models`, not `Model` vs. `Models` — intentional, since `SelectableModels`
   is specified to dedupe the default against `Models` (overlap is legal,
   e.g. re-listing the default as one of the selectable choices).

## Commit

`feat(model): per-scope/user model override store, resolveModel, reapply`
