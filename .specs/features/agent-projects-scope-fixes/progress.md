# agent-projects-scope-fixes — Progress

Updated 2026-08-10. Nothing is committed; everything below is working-tree state
across both submodules.

**Gates green.** Proxy: `go build`, `go vet`, `gofmt` clean, and the whole suite
passing except the `internal/docker` chown failures (STATE.md L-001). **That
baseline moved from 9 to 10**: `TestEnsureRunningRecreatesOnImageDrift`, added for
the harness-image drift check, is chown-dependent like the rest of the
`TestEnsureRunning*` family. It was run as root in `golang:1.25-bookworm` and
passes, alongside the pre-existing `TestEnsureRunningRecreatesOnPersonaDrift` —
which also confirms those failures are sandbox noise rather than real. Webapp: 797
tests in 58 files passing (was 783 in 56), `yarn build` clean.

---

## Done — B1, B2, B3 and the four adjacent defects

### B2 — uploads from a project (P1)

All three layers that dropped `project`:

- `internal/httpapi/handlers.go` `handleMediaPost` reads it from the **form**
  (`r.FormValue`) and validates with `checkProject`, the seam the folder/move
  handlers already use. `workspaceSegmentFor` is left query-only and now says so:
  widening it would make every GET and DELETE on it parse a body they do not have.
- `lib/media.ts` `uploadMedia` sets `project` on the FormData when `workspace.p`
  is set, and omits the field otherwise.
- `app/api/media/route.ts` `POST` names it on the `upstream` FormData it rebuilds.

### B1 — scheduled tasks (P2)

- `config.CronFile` no longer takes a segment. Its comment carries the picoclaw
  citation — one `CronService` per gateway, given `cfg.WorkspacePath()`
  (`gateway.go:416`, `:664`, composed at `:843`) — so nobody re-derives a
  per-project path that picoclaw never writes.
- `cron.JobProject` attributes a job by its `Payload.To`, built from
  `identity.ProjectSeparator` rather than a literal `"."`.
- `httpapi.scopedJobs` filters the one store: a project sees only its own; the
  global scope sees the project-less jobs plus any job whose project is gone
  (FR-3a — it still fires, so it has to be visible somewhere).
- The package comments now record that *execution* was already per-project:
  `ExecuteJob` → `ProcessDirectWithChannel` → `processMessage` →
  `resolveMessageRoute`, so a project's cron turn is dispatched to the project's
  agent and its transcript lands in that project's `sessions/`. That is why the
  runs stay per-segment while the jobs are filtered out of a shared file.

### B3 — the sidebar (P1)

`workspace.p` added to all eleven effects across the four panels. Two of them
also needed FR-10: `scheduled-tasks-panel` now clears `data` on a scope change,
and `memory-graph-panel`/`memory-editor` already reset through `reset()` and the
`loaded` flag.

### Adjacent

- **`handleMemoryPut` (found while auditing, worst of the four).** It read
  `project` from the query while the client had always sent it in the body, so a
  note edited inside a project overwrote the MAIN workspace's `MEMORY_CUSTOM.md`.
  The matching GET was correct, which is what hid it: the member opened the
  project's note, edited it, and destroyed a different one. Now body-borne and
  validated with `checkProject`.
- `app/api/media/route.ts` `DELETE` and `app/api/media/download/route.ts` forward
  `project`. Both are hand-written rather than on `proxyRead`/`proxyMediaWrite`,
  which is exactly why they were missed; each now carries a comment saying so.
- `handleSessionsResolve` resolves the caller's project and applies
  `projectSessionID`, matching `handleSessionsHistory` eleven lines above it.

---

## Tests added, and verified to discriminate

Three of them were checked by temporarily breaking the implementation and
confirming the test failed, then restoring it. A test that only ever passes proves
nothing — the same discipline `agent-projects` T-07 used.

| Test | Discriminates? |
| --- | --- |
| `internal/cron` `TestJobProject` — 9 cases incl. an id containing `-` | By construction (table) |
| `internal/httpapi` `TestCronTasksScopedByProject` | **Verified** — filter stubbed to `return all`, all three subtests failed |
| `internal/httpapi` `TestCronTasksUnknownProjectIs404` | — |
| `internal/httpapi` `TestMediaUpload{ResolvesTheProjectFromTheForm,WithoutAProjectIsUnchanged,UnknownProjectIs404}` | By construction — `fakeOrch.StoreMedia` now records the project it was handed |
| `internal/httpapi` `TestSessionsResolveIsProjectScoped` | — |
| `internal/httpapi` `TestMemoryPut{TakesTheProjectFromTheBody,WithoutAProjectIsUnchanged,UnknownProjectIs404}` | **Verified** — handler reverted to the query-based lookup, two of the three failed |
| `lib/media.test.ts` — upload form carries / omits `project` | — |
| `app/api/media/project-forwarding.test.ts` — 7 cases over POST/DELETE/download | — |
| `app/chat/workspace-panel-scope.test.ts` | **Verified** — `p` removed from one deps list, that file's case failed |

`app/api/media/project-forwarding.test.ts` is the **first test in the webapp over
a BFF route handler**, and it exists because this defect class is invisible to
every other kind: the client sent the parameter, the proxy understood it, and the
layer between silently dropped it. It mocks `@/lib/session` and `@/lib/mycelium`
and asserts on what `fetchMycelium` was called with.

`app/chat/workspace-panel-scope.test.ts` reads the four components' SOURCE and
asserts that any effect keyed on `workspace.t` is also keyed on `workspace.p`.
That is unusual and deliberate: the suite runs `environment: "node"`, where no
effect fires, so a render test cannot observe a re-fetch. It guards the exact
regression (a new effect added without `p`) and claims nothing more; the
request-scoping half is covered by the two tests above it.

---

## Audited and clean

Recorded because the audit was only run after `agent-projects/progress.md` — which
claims the webapp side is complete — turned out to be wrong about four routes. These
four are genuinely correct, and nobody needs to re-check them:

- `proxyMediaWrite` spreads the body minus `role`, so folder create/move/delete
  forward `project`; the proxy's `mediaFolderCaller` reads and validates it there.
- `storeTurnAttachment` is called with the turn's own `req.Project`
  (`handlers.go:651`), so a file a PROJECT agent delivers lands in that project's
  `attachments/`.
- `ChatView` receives ChatShell's composed workspace (`chat-shell.tsx:320`), so
  `workspace.p` is populated on the upload path — without which T-01 would be inert.
- `readMemory` / memory `GET` were already query-scoped end to end.

## Not done

- **T-08 / B4** — still blocked on one observation from the live stack. See
  spec.md's B4 section for the two commands and how to read each outcome. Nothing
  was changed on its behalf, including the `UPLOAD_SETTLE_MS` delay and the
  folklore comment at `app/chat/turn-store.ts:68` that justifies it.
- **The OpenAPI document** declares neither `/v1/media`, `/v1/cron/tasks` nor
  `/v1/sessions/resolve` at all — a pre-existing gap in `internal/httpapi/openapi.json`,
  not something this batch widened. Worth its own task.
