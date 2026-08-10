# agent-projects-scope-fixes — Spec

Corrections to the shipped `agent-projects` feature (see `.specs/features/agent-projects/`).
Four defects were reported from live use on 2026-08-10. Three have root causes
confirmed at source level and are specified here in full; the fourth is recorded
as blocked on one observation nobody has made yet.

Spans two repos. The webapp's task list lives at
`crab-exoskeleton-webapp/.specs/features/agent-projects-scope-fixes/tasks.md`
and points back here for the requirements.

---

## The reported defects

| # | Reported | Root cause | Status |
| --- | --- | --- | --- |
| B1 | A task scheduled from inside a project is saved in the global workspace | picoclaw's cron store is **one file per container** | Confirmed |
| B2 | A file uploaded from inside a project lands in the global workspace | `project` is dropped by all three layers of the upload path | Confirmed |
| B3 | The workspace sidebar does not refresh when entering or leaving a project | Eleven effects omit `workspace.p` from their dependencies | Confirmed |
| B4 | Uploading a file makes the agent lose its memory, "as if restarted" | Unknown | **Blocked** — see below |

The picoclaw log excerpt supplied with the report is **B2's downstream symptom,
not B4's evidence**. `[anexo: uploads/analysis.zip]` named a file that was never
in the project agent's workspace, so the agent flailed: `list_dir` on a `sources`
directory that does not exist, an `exec` call missing its `action` property, and
`unzip: can't open /data/.picoclaw/workspace-marco-dos-biol-gicos/uploads/analysis.zip`
— the exact path the upload should have written to and did not. The three
`path outside working dir` refusals at iterations 2, 3 and 8 are the same search
widening past the sandbox. Fixing B2 removes all of it.

---

## B1 — scheduled tasks are not per-project, and cannot be

### The constraint

picoclaw creates **one `CronService` per gateway**, and its store path is fixed
to the default workspace:

```go
// pkg/gateway/gateway.go:416 (and :664, the second startup path)
runningServices.CronService, err = setupCronTool(… cfg.WorkspacePath() …)

// pkg/gateway/gateway.go:843
cronStorePath := filepath.Join(workspace, "cron", "jobs.json")
```

`cfg.WorkspacePath()` is `agents.defaults.workspace` — the main workspace. It is
not per-agent and there is no per-agent override. So
`workspace-<project>/cron/jobs.json`, which `config.CronFile` currently composes
for a project, **will never exist**, and the proxy reads an absent file as "no
tasks". Meanwhile the global panel reads the one real store and shows every job,
including the project's. That is exactly what was reported.

### The discriminator

A job records the conversation it was created in. `CronTool.addJob`
(`pkg/tools/cron.go:167-191`) reads the live turn context and passes it through:

```go
channel := ToolChannel(ctx)   // "pico"
chatID  := ToolChatID(ctx)    // "pico:" + sessionID
…
job, err := t.cronService.AddJob(messagePreview, schedule, message, channel, chatID)
```

and the pico channel builds `chatID` as `"pico:" + sessionID`
(`pkg/channels/pico/pico.go:1195`). The proxy already stamps the project into
that session id — `identity.ProjectSessionID` yields `p.<projectID>.<hash>`. So:

| Conversation | `Payload.Channel` | `Payload.To` |
| --- | --- | --- |
| Main workspace | `pico` | `pico:<32-hex>` |
| Project `seedtrial` | `pico` | `pico:p.seedtrial.<32-hex>` |

`Payload.To` is therefore a reliable per-job project label, derived from the same
one fact `identity.ProjectSessionID` and `identity.ProjectChatPattern` already
encode. Nothing new has to be written for a job to be attributable.

### Where the runs live

A job's **transcript** is per-project already, and does not need changing.
`CronTool.ExecuteJob` calls `ProcessDirectWithChannel(ctx, message, sessionKey,
channel, chatID)` (`pkg/tools/cron.go:647`), which builds an inbound message
carrying that `chatID` and hands it to `processMessage`, which resolves the route
through `resolveMessageRoute(msg)` (`pkg/agent/agent_message.go:149`). A cron
turn is dispatched like any other message, so a job whose `To` is
`pico:p.seedtrial.…` is answered by the `seedtrial` agent and its transcript is
written under that agent's workspace. `history.CronRuns(<project sessions dir>)`
is already correct.

### Requirements

- **FR-1** `config.CronFile` no longer takes a workspace segment. It resolves the
  one container-wide store under `MainWorkspace`, and its doc comment records why
  (with the `gateway.go` citation), so nobody re-introduces a per-project path.
- **FR-2** A new pure helper in `internal/cron` attributes a job to a project by
  parsing `Payload.To`: the prefix `pico:p.<id>.` yields `<id>`, anything else
  yields `""`. It must match `identity.ProjectSessionID`'s form exactly — one
  separator convention, stated once (`identity.ProjectSeparator`).
- **FR-3** `GET /v1/cron/tasks?project=<id>` returns only the jobs attributed to
  `<id>`. Without `project`, it returns only the jobs attributed to no project.
- **FR-3a** A job attributed to a project that is **not in the caller's store**
  is listed in the **global** response. Deleting a project removes its dispatch
  rule but not its jobs, so such a job still fires — as the default agent. It has
  to be visible somewhere or the member cannot delete a task that keeps running.
- **FR-4** Runs continue to be read from the requested segment's sessions dir
  (unchanged behaviour, now justified above rather than assumed).
- **FR-5** `GET /v1/cron/runs` keeps resolving the project, because the run file
  it names lives in that project's sessions dir.
- **NFR-1** The member surface stays read-only. Nothing here writes `jobs.json`;
  the reason recorded at the top of `internal/cron/cron.go` still holds.

### Out of scope, deliberately

Making the cron store genuinely per-agent is a picoclaw change (a second
`CronService`, or a per-agent store path). It is not attempted here. Filtering
one store gives the member the correct view; the *execution* is already routed to
the right agent by dispatch, which is the part that would actually be wrong.

---

## B2 — uploads from a project land in the global workspace

`project` is lost three times over, and each layer alone is enough to cause it:

| Layer | Site | Defect |
| --- | --- | --- |
| Browser | `lib/media.ts:47` `uploadMedia` | Sets `role`/`tenant_id`/`subs_acc_id`/`file` on the form. Never sets `project` — the only client function that does not call `withProject` |
| BFF | `app/api/media/route.ts:124-127` `POST` | Rebuilds a fresh `upstream` FormData with three fields. `project` is not among them, so even a client that sent it would be stripped |
| Proxy | `internal/httpapi/projects.go:205` `workspaceSegmentFor` | Reads `r.URL.Query().Get("project")` only. The upload is `multipart/form-data` and takes `tenant_id`/`subs_acc_id` from `r.FormValue`, so a query-only lookup can never see it |

### Requirements

- **FR-6** `uploadMedia` sends `project` as a form field when `workspace.p` is
  set, and omits the field entirely when it is not — a project-less upload stays
  byte-identical to today's request.
- **FR-7** The BFF's `POST /api/media` forwards `project` onto the upstream
  multipart body when present.
- **FR-8** `handleMediaUpload` resolves the project from the **form**, not the
  query, and validates it through the existing `checkProject` seam — the
  body-borne twin that `handleMediaFolder`/`handleMediaMove` already use. No new
  resolution path is introduced.
- **FR-8a** An unknown project is a 404 **before** any byte is written, matching
  AC-8 of the original feature: falling through to the main workspace is what
  produced this defect's user-visible shape in the first place.

---

## B3 — the workspace sidebar shows the wrong project's content

`workspace.p` is composed correctly (`app/chat/chat-shell.tsx:56`, from
`fragment?.p ?? null`, so it is always `string | null` and never alternates with
`undefined`). Every request function passes it. But the effects that *fire* those
requests list only `t`, `s` and `r` as dependencies, so entering or leaving a
project re-renders the panels without re-fetching. The panel keeps whatever it
loaded for the previous scope, which is why the global content persists after
entering a project and the project's content is not what appears on leaving.

Eleven effects, four components:

| File | Lines | What is stale |
| --- | --- | --- |
| `app/chat/uploads-sidebar.tsx` | 329 | The files tree |
| `app/chat/memory-editor.tsx` | 42, 68 | `MEMORY_CUSTOM.md` load, and its save/dirty tracking |
| `app/chat/memory-graph-panel.tsx` | 146, 172, 193, 211 | Graph reset, both view fetches, and the node read |
| `app/chat/scheduled-tasks-panel.tsx` | 162, 189, 207 | Task list (×2) and the open run's transcript |

### Requirements

- **FR-9** Every one of the eleven effects includes `workspace.p` in its
  dependency list.
- **FR-9a** `scheduled-tasks-panel.tsx:207` is included even though it is keyed
  on `open?.run.basename`: a run basename is only meaningful inside the sessions
  dir it was listed from, so the same basename in a different scope is a
  different file.
- **FR-10** Entering or leaving a project clears the previous scope's content
  before the new fetch resolves, rather than showing it while loading. The files
  effect already does this (`setFiles(null)` at line 317); the others must not
  present another scope's data as if it were this one's.

---

## Adjacent defects in the same family (confirmed, not reported)

Same cause as B2 — `project` dropped by one layer — and reachable today from any
project view. Included at the user's direction.

They were found by auditing every route that carries a project, rather than by
trusting `agent-projects/progress.md`, which claims the "webapp side now complete"
and was wrong about four routes. The audit's clean results are worth recording so
the next reader does not repeat it: `proxyMediaWrite` (`lib/mediaFolderProxy.ts`)
spreads the body minus `role`, so folder create/move/delete forward `project`
correctly; the proxy's `mediaFolderCaller` reads it from the body and validates it;
`storeTurnAttachment` receives the turn's own `req.Project`, so a file a project
agent delivers lands in that project's `attachments/`; and `ChatView` is handed
ChatShell's composed workspace, so `workspace.p` is live on the upload path.

- **FR-11** `app/api/media/route.ts:71` (`DELETE`) forwards `project`. Without
  it, deleting a file from a project's panel addresses the **main** workspace: a
  name collision deletes the wrong file, and no collision deletes nothing while
  reporting success.
- **FR-12** `app/api/media/download/route.ts:23` forwards `project`. Every
  download from a project's files panel and every `[anexo: …]` chip in a project
  conversation currently reads the main workspace.
- **FR-14** `handleMemoryPut` (`internal/httpapi/handlers.go:1341`) reads `project`
  from its **JSON body**, not from the query. This is the worst of the four
  because it is a WRITE and the matching read is correct: the member opened a
  project's memory note (GET, project-scoped via the query), edited it, and the
  save overwrote the **main workspace's** `MEMORY_CUSTOM.md`. The webapp has been
  sending `project` in that body all along
  (`lib/memory.ts:34`, `app/api/memory/route.ts` PUT), so the whole leak is on the
  proxy side. Validate with `checkProject`, like the folder/move handlers.
- **FR-13** `internal/httpapi/handlers.go:881` (`handleSessionsResolve`) resolves
  the caller's project instead of hardcoding `config.MainWorkspace`, and applies
  `projectSessionID` to the session key like `handleSessionsHistory` does eleven
  lines above it. It currently answers with a `sessionFile` from the wrong
  directory for every project conversation.

---

## B4 — the agent loses context around an upload (BLOCKED)

**No root cause. Nothing is specified for it, and nothing should be changed on a
guess.**

Eliminated by reading source:

- The proxy never calls picoclaw's `POST /reload`, on upload or otherwise (no
  such call anywhere in `internal/`).
- `StoreMedia` (`internal/docker/media.go:102`) writes the file and chowns the
  tree. It does not touch the container.
- picoclaw has no filesystem watcher, and the shipped template sets
  `gateway.hot_reload: false` (`internal/docker/defaulttemplate/picoclaw/config.json:267`).
  The webapp's comment at `app/chat/turn-store.ts:68` — *"After an upload,
  picoclaw reloads to pick up the new workspace file"* — has no mechanism behind
  it and should be treated as folklore until B4 is diagnosed.
- `projectBindDrift` (`internal/docker/projects.go:364`) compares mount
  destinations, not whole bind strings, and is correct as written.
- picoclaw's session history is JSONL-backed on disk (`pkg/memory/jsonl.go`), so
  a plain restart should *not* by itself lose a conversation.

The log excerpt cannot settle it: it opens with a boot banner at 18:10:02 and the
upload is at 18:11, but a `docker logs` dump always opens at the top of its
buffer, so the banner is not evidence that a restart happened *then*.

### The one observation needed

On the live stack, with the container that answered:

```sh
docker inspect --format '{{.State.StartedAt}} {{.RestartCount}}' <crabshell-…>
docker logs <crab-shell-proxy> 2>&1 | grep 'mounts stale, recreating'
```

- `StartedAt` ≈ the upload, **and** a `mounts stale, recreating` line → a
  `projectBindDrift` false positive, recreating the container on requests it
  should not. That is a proxy bug and gets its own spec.
- `StartedAt` ≈ the upload with **no** such line → idle scale-to-zero, i.e. the
  container stops between messages and every conversation restarts cold. A
  different bug from "upload causes amnesia", and a more serious one.
- `StartedAt` well before the upload → no restart at all, and the amnesia is B2:
  the agent could not read the file it was told about, so the turn produced
  nothing to remember.

The third is the current best guess precisely because B2 explains every line of
the supplied log. Fix B2 first, then re-observe.

---

## Acceptance criteria

| ID | Criterion |
| --- | --- |
| AC-1 | A task scheduled inside project `X` appears in `X`'s panel and **not** in the global panel; a task scheduled in the main workspace appears only in the global panel |
| AC-2 | A job whose `Payload.To` names an unknown project appears in the global panel (FR-3a) |
| AC-3 | Attribution matches `identity.ProjectSessionID` for a generated project id, including one containing `-` — the separator is `.`, and `p.a.<hash>` must not be read as project `a.<hash>` |
| AC-4 | A file uploaded from project `X` lands in `workspace-X/uploads/`, and the returned `uploads/<name>` path is the one the agent can open |
| AC-5 | An upload naming an unknown project answers 404 and writes nothing |
| AC-6 | A project-less upload sends no `project` field at any layer (regression: the existing request shape is unchanged) |
| AC-7 | Entering a project re-fetches files, memory, graph and tasks; leaving it re-fetches all four. No panel shows the other scope's content after the switch settles |
| AC-8 | Delete and download from a project's files panel address `workspace-X/uploads/`, verified by a file that exists in only one of the two workspaces |
| AC-9 | `GET /v1/sessions/resolve?project=X` returns the session file from `workspace-X/sessions/` |
| AC-11 | `PUT /v1/memory` with `"project":"X"` in the body writes `workspace-X/MEMORY_CUSTOM.md` and leaves the main workspace's document untouched; an unknown project 404s and writes nothing |
| AC-10 | The whole existing suite stays green in both repos, unmodified except where a test asserted the removed `CronFile` segment parameter |

## Tests

Proxy (`go test ./...`):

- Job attribution table: bare `pico:<hex>` → `""`; `pico:p.seedtrial.<hex>` →
  `seedtrial`; a project id containing `-`; `p.` with no trailing separator;
  empty `To`; a non-pico channel. AC-3 is the load-bearing case.
- `handleCronTasks` filtering: one store holding a global job and two projects'
  jobs, asserted three times (no `project`, each project), plus the unknown-project
  job landing in the global response (AC-2).
- `handleMediaUpload` with `project` in the multipart form: stores under the
  project's uploads dir (AC-4); an unknown project 404s and leaves the main
  uploads dir untouched (AC-5).
- `handleSessionsResolve` with `?project=` (AC-9).

Webapp (`yarn test`):

- `uploadMedia` sets `project` when `workspace.p` is set and omits the field
  otherwise (AC-6).
- The BFF `POST`/`DELETE`/download routes forward `project` and omit it when
  absent.
- A panel re-fetch on `workspace.p` change, for at least the files tree and the
  tasks list — the two the report named (AC-7).

Chown-dependent proxy paths follow STATE.md L-001: verified through the pure
helpers, or run as root in `golang:1.23-bookworm`. The nine known
`internal/docker` sandbox failures are the baseline, not a regression.
