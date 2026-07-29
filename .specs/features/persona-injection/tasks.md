# Tasks — Persona injection (delivery A)

Gate for every task: `go build ./...`, `go vet ./internal/...`, and
`go test ./internal/...` with no NEW failures.

Baseline: `internal/docker` already fails 8 tests in an unprivileged sandbox
(`TestEnsureRunning*`, `TestCreateAddsReadOnlySecretsBind`,
`TestRestartWorkspaceRestartsAndRearms`, `TestScaleToZeroIdleStop`,
`TestContinuousDoesNotArmIdle`, `TestReconcileEnsuresContinuousWorkspaces`) —
all `lchown: operation not permitted`. Verified pre-existing. Compare against
that set, not against zero.

## T1 — Config paths and the shrunken seed

- **What:** `TenantAgentPersonaDir`, `SubscriptionAgentPersonaDir`,
  `EffectivePersonaDir`; `WorkspaceSeed` → `["USER.md", "memory/", "skills/"]`.
- **Depends on:** —
- **Reuses:** `tenantAgentSharedRoot`, `subscriptionAgentSharedRoot`,
  `identity.SanitizeID`.
- **Done when:** the new helpers compose like the `…SharedFilesDir` family and
  `config_test.go`'s seed assertion matches R6.1.
- **Requirements:** R2.3, R6.1, R6.2

## T2 — `HEARTBEAT.md` in the template

- **What:** add `defaulttemplate/picoclaw/workspace/HEARTBEAT.md` with the
  operator-supplied content; update `default_template_test.go`'s expected list.
- **Depends on:** —
- **Done when:** a materialized default template contains it.
- **Requirements:** R5.1, R5.2

## T3 — `persona.go`: cascade and bind set (pure)

- **What:** `PersonaFiles`, `PersonaMounted`, `IsPersonaFile`,
  `resolvePersonaSources`, `personaBinds`.
- **Depends on:** T1
- **Reuses:** the `sharedFileBinds` shape — pure, no Docker, no root.
- **Done when:** precedence is subscription+agent → tenant+agent → template; a
  file with no source anywhere yields NO bind; `USER.md` never appears in the
  bind set.
- **Tests:** `persona_test.go` — the precedence table and the bind strings.
- **Requirements:** R1.1, R1.3, R2.1, R2.2, R4.1

## T4 — `persona.go`: effective sync

- **What:** `syncEffectivePersona` — resolve, then write each file IN PLACE into
  `EffectivePersonaDir`; `chownTree` the dir.
- **Depends on:** T3
- **Done when:** a second call with changed source content updates the same
  inode (assert via `os.Stat` inode equality, which is what R3.1 is really
  about).
- **Requirements:** R2.3, R3.1, R3.2

## T5 — `persona.go`: admin CRUD

- **What:** `ListPersona`, `ReadPersona`, `WritePersona`, `DeletePersona` over
  `personaDir(scope)`.
- **Depends on:** T1
- **Reuses:** the shared-skills method shape.
- **Done when:** write/list/read/delete round-trip; delete is idempotent; a name
  outside `PersonaFiles` is refused.
- **Requirements:** R7.1

## T6 — Wire into `m.create`

- **What:** call `syncEffectivePersona` before create and append
  `personaBinds(...)` to the container's binds.
- **Depends on:** T3, T4
- **Done when:** a picoclaw create carries `…/AGENT.md:…/workspace/AGENT.md:ro`
  and the hermes path carries none.
- **Tests:** extend the existing bind-set assertion (`shared_test.go:322`'s
  neighbour) rather than a new Docker test.
- **Requirements:** R1.1, R1.2

## T7 — `USER.md` seeded from the cascade

- **What:** `seedWorkspace` reads `USER.md` from the effective persona dir, the
  rest from the template. Materialize the effective set before provisioning.
- **Depends on:** T4
- **Done when:** an injected `USER.md` is what a first provision seeds; a
  returning user's `USER.md` is untouched.
- **Tests:** rewrite `provision_test.go`'s no-clobber assertion onto `USER.md`
  (see spec § Test impact).
- **Requirements:** R4.2, R4.3

## T8 — Admin API

- **What:** four handlers in `admin.go`, four routes in `handlers.go`, the
  orchestrator interface methods, and the fake in `handlers_test.go`.
- **Depends on:** T5
- **Done when:** a non-persona `name` is a 400; an agent-less scope is a 400;
  POST/DELETE honour the restart policy.
- **Requirements:** R7.1, R7.2, R7.3
