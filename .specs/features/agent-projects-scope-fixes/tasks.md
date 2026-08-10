# agent-projects-scope-fixes — Tasks (proxy)

Requirements in `spec.md`. Webapp tasks live in
`crab-exoskeleton-webapp/.specs/features/agent-projects-scope-fixes/tasks.md`
and are independent of these — the two repos can be worked in either order,
because each layer of B2's fix is separately harmless and jointly required.

Gate for every task: `go build ./... && go vet ./... && go test ./...`, with the
nine known `internal/docker` chown failures as the accepted baseline (STATE.md
L-001). A tenth failure, or a failure in any other package, is a regression.

---

## P1 — B2, the upload path (do this first; it also explains B4's log)

### T-01 — resolve the project from the multipart form
- **What:** `handleMediaUpload` reads `project` from `r.FormValue("project")`,
  validates it with the existing `checkProject`, and passes it to `StoreMedia`.
  Drop the `workspaceSegmentFor` call from that one handler — it cannot see a
  form field, which is the defect.
- **Where:** `internal/httpapi/handlers.go` (the upload handler, ~line 1146).
- **Reuses:** `checkProject` + `workspaceSegmentOf` (`internal/httpapi/projects.go:232,251`)
  — the seam `handleMediaFolder` and `handleMediaMove` already use for
  body-borne projects. Do not add a third resolution path.
- **Done when:** FR-8, FR-8a. An unknown project 404s before `StoreMedia` is
  called.
- **Tests:** upload with `project=<known>` lands in `workspace-<id>/uploads/`;
  upload with `project=<unknown>` returns 404 and leaves the main uploads dir
  empty; upload with no `project` is unchanged.
- **Note:** leave `workspaceSegmentFor` itself alone. Its query-only lookup is
  correct for every GET/DELETE that uses it; widening it to also read the form
  would make every one of those handlers parse a body they do not have.

### T-02 — comment the asymmetry
- **What:** one sentence at `workspaceSegmentFor` recording that the multipart
  upload uses `checkProject` instead, and why (a form field is not a query
  parameter). Without it the next reader will "unify" the two and reintroduce T-01.
- **Where:** `internal/httpapi/projects.go` near line 202.
- **Depends on:** T-01.
- **Done when:** the note names the handler that diverges.

---

## P2 — B1, cron attribution

### T-03 — `CronFile` resolves the one container-wide store
- **What:** drop the `segment` parameter from `config.CronFile`. Its doc comment
  states the picoclaw fact with its citation: `setupCronTool` receives
  `cfg.WorkspacePath()` (`pkg/gateway/gateway.go:416` and `:664`) and composes
  `<workspace>/cron/jobs.json` (`:843`), so there is one store per container and a
  per-project path cannot exist.
- **Where:** `internal/config/config.go:520-527`; the one call site in
  `internal/httpapi/cron.go:56`.
- **Done when:** FR-1. `SessionsDir` and `UploadsDir` keep their segment — they
  are genuinely per-workspace and must not be swept along.
- **Tests:** existing config tests updated only where they assert the removed
  parameter (AC-10).

### T-04 — attribute a job to a project
- **What:** a pure exported helper in `internal/cron` that maps a job's
  `Payload.To` to a project id: `pico:p.<id>.<rest>` → `<id>`, anything else →
  `""`. It must consume `identity.ProjectSeparator` rather than a literal `"."`,
  so the two encodings cannot drift apart.
- **Where:** `internal/cron/cron.go` (new function; the package stays read-only).
- **Reuses:** `identity.ProjectSeparator` (`internal/identity/identity.go:176`),
  and the `"pico:" + sessionID` form from `pkg/channels/pico/pico.go:1195`.
- **Done when:** FR-2.
- **Tests:** the AC-3 table — bare `pico:<hex>`; `pico:p.seedtrial.<hex>`; an id
  containing `-` (`p.my-proj.<hex>` → `my-proj`, never `my`); `pico:p.` alone;
  empty `To`; a `To` from another channel. This test is the whole task: a helper
  that silently returns `""` for a real project id produces an empty panel, which
  looks exactly like the bug being fixed.

### T-05 — filter the listing by scope
- **What:** `handleCronTasks` reads the single store and partitions it. With
  `?project=<id>`, keep only jobs attributed to `<id>`. Without it, keep jobs
  attributed to no project **plus** jobs attributed to a project absent from the
  caller's own store.
- **Where:** `internal/httpapi/cron.go:44-101`.
- **Depends on:** T-03, T-04.
- **Reuses:** `s.Mgr.ListProjects(key)` for the known-project set — the caller's
  own store, so one member's project can never label another's job.
- **Done when:** FR-3, FR-3a. Runs still come from the requested segment's
  sessions dir (FR-4), and the orphan-run grouping is untouched.
- **Tests:** one store with a global job, a job for project A, a job for project
  B and a job for deleted project C; asserted for no-`project` (global + C), `A`,
  and `B`. Plus the existing cron handler tests green unmodified.

### T-06 — record why the store is not per-project
- **What:** extend the note at the top of `internal/cron/cron.go` (and the one in
  `internal/httpapi/cron.go`) with the single-store fact and the fact that
  *execution* is nonetheless correctly routed — `ExecuteJob` →
  `ProcessDirectWithChannel` → `processMessage` → `resolveMessageRoute`
  (`pkg/agent/agent_message.go:149`), so a project's job is answered by the
  project's agent and its transcript lands in that project's `sessions/`.
- **Depends on:** T-05.
- **Done when:** a reader can tell why the store is read globally but the runs are
  read per-segment, without re-deriving it from picoclaw.

---

## P3 — the adjacent proxy defect

### T-09 — `handleMemoryPut` reads the project from its body
- **What:** add `Project` to the PUT request struct and validate it with
  `checkProject`, replacing the query-based `workspaceSegmentFor` call.
- **Where:** `internal/httpapi/handlers.go:1341`.
- **Done when:** FR-14, AC-11.
- **Why it is first among the adjacent ones:** it is a WRITE whose matching READ is
  correct, so it destroyed the main workspace's note while showing the project's.
- **Tests:** body-borne project honoured; no project unchanged; unknown project 404s
  and `WriteMemory` is never called.

### T-07 — `handleSessionsResolve` resolves the caller's project
- **What:** replace the hardcoded `config.MainWorkspace` with
  `workspaceSegmentFor`, and apply `projectSessionID` to the session key —
  mirroring `handleSessionsHistory`, which is eleven lines above and already
  does both.
- **Where:** `internal/httpapi/handlers.go:881`.
- **Done when:** FR-13, AC-9.
- **Tests:** `?project=<id>` returns the session file from
  `workspace-<id>/sessions/`; no `project` is unchanged.

---

## P4 — B4, blocked

### T-08 — observe, do not fix
- **What:** nothing is implemented. After P1 ships, run the two commands in
  spec.md's B4 section against the live stack and record the answer in this
  folder as `b4-observation.md`.
- **Blocked on:** an observation only the operator of the live stack can make.
- **Do not:** add a settle delay, disable the idle timer, or loosen
  `projectBindDrift` on the strength of the current evidence. All three would be
  changes to code that has not been shown to be wrong.
- **Also:** if B4 turns out to be real, delete or correct the folklore comment at
  `crab-exoskeleton-webapp/app/chat/turn-store.ts:68`, which asserts a picoclaw
  reload that does not exist. If B4 turns out to be B2, delete it anyway.
