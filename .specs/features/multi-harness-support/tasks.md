# multi-harness-support Tasks (crab-shell-proxy)

> **⚠️ WITHDRAWN — implemented, verified live, then removed.**
>
> The Hermes (Nous Research `hermes-agent`) harness described here **shipped and worked
> end-to-end**, and was then removed for **current infrastructure compatibility**: a 180s startup
> deadline against a 35s global health-wait, turns sitting near mycelium's 60s `gatewayTimeout`, a
> heavyweight per-user image, and a second code branch no deployment exercised.
>
> This document is kept as the **future-implementation record** — it is not a description of the
> current codebase. Nothing below is in the shipped surface today.
>
> - **Why, and what exactly was withdrawn:** root `.specs/features/hermes-removal/DECISION.md`
>   and `.specs/features/hermes-removal/spec.md`.
> - **What was learned by running it** (read this first on a re-add): `./implementation-notes.md`.
> - **Recovery SHAs:** `d2f0a9a`, `748e0fe`, `3e9e95c`.

Sequential; each ends build-green. Gate after every task: `go build ./...`.
Final gate: `go vet ./... && go test ./...`.

- [x] **T1 — config**: `Agent.Harness` (default `picoclaw`, validate enum),
      `ModelConfig.BaseURL`, `ModelConfig.KeyEnvName`, `Config.HermesImage`. → `config.go`.
- [x] **T2 — turn package**: new `internal/turn` with `Request` struct.
- [x] **T3 — pico refactor**: `pico.RunTurn(ctx, turn.Request, onDelta)`. `turn_test.go` (processor-only) unaffected.
- [x] **T4 — Target + endpoint/port by harness**: `manager.go` `Target{Endpoint,AuthToken,Harness}`,
      `harnessPort`, `endpoint`, `waitHealthy(…,port)`. Updated `manager_test.go`.
- [x] **T5 — hermes turner**: `internal/hermes/turn.go` + `turn_test.go` (httptest OpenAI SSE). PASS.
- [x] **T6 — hermes provision**: `provision_hermes.go` + `defaulttemplate/hermes/{config.yaml,SOUL.md}`.
- [x] **T7 — EnsureRunning/create branch**: hermes path in `manager.go` + `createHermes` (in provision_hermes.go).
- [x] **T8 — handlers/sse wiring**: `Turner` struct sig, `turnerFor`, `Server.Hermes`, call sites.
      Updated `handlers_test.go` (fakeTurner sig, Target literal).
- [x] **T9 — main**: wired `hermes.Client`. (config.yaml docs → see parent spec follow-up note.)
- [x] **T10 — final gate**: `go build` ✓, `go vet` ✓, `go test ./...`: all runnable packages green
      (config, turn, pico, hermes, httpapi, history, authz, identity). The `internal/docker`
      chown-based tests fail **only** on `chown … operation not permitted` — a sandbox limitation
      in the pre-existing picoclaw path (verified identical at the clean committed state via
      `git stash -u`); no regression introduced. They pass where the runner can chown (CI/root).

## Deferred to follow-ups (not in this slice)
- config.yaml commented hermes agent example (T9 doc bit) — safe to add but touches the live
  deploy file; left for the operator alongside real service/token env.
- MHS-16/17 lifecycle tuning, MHS-18 Hermes history (SQLite `state.db` reader / durable-from-passthrough),
  MHS-19 further harnesses; shared secrets/skills/files, managed content, native secrets, model-override
  for Hermes.

## Traceability (→ parent MHS ids)
T1→MHS-01/02, T2/T3/T8→MHS-12, T4/T7→MHS-05/06/07/08, T5→MHS-09/10/11,
T6→MHS-13/14, T8 headers→MHS-15, T9→MHS-01. Deferred: MHS-16/17/18 (history+lifecycle), 19.
