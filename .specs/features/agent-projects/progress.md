# agent-projects — Progress

Updated 2026-08-07. **The tree builds and the test suite is at its known
baseline**: the nine `internal/docker` failures are STATE.md L-001 sandbox noise
(all `lchown ... operation not permitted`; the assertion lines without "chown" in
them are downstream of a chown that already failed in the same test). 14 packages
green.

Nothing is committed. Everything below is working-tree state.

---

## Done

### Phase 0 — patched picoclaw (T-01, T-02, T-03) ✅

- `deploy/picoclaw-glob/dispatch-selector-glob.patch` (parent repo) —
  `selectorMatches` + `globMatch` wired into the six string selectors; 108 lines
  of new tests appended to `route_test.go` with **zero deletions** (AC-2
  structurally); `docs/guides/routing-guide.md` wildcard section.
- `deploy/picoclaw-glob/Dockerfile` — pinned tag `v0.3.1`, applies the patch,
  **runs `go test ./pkg/routing/...` as a build step**, then builds.
- Image `zombie-crab/picoclaw:0.3.1-glob` built. Verified upstream-equivalent
  gates on the patched tree (`go build`/`go test` with `-tags goolm,stdjson`,
  `go vet`, `gofmt`, `./scripts/lint-docs.sh`).
- Verified the patch reaches the shipped binary: `routing.globMatch` /
  `routing.selectorMatches` symbols present in the patched image, **absent in
  `sipeed/picoclaw:latest`**.

⚠ The picoclaw working copy was in a scratch dir and is gone. The `.patch` is the
durable artifact. To iterate: re-clone at the pinned tag and `git apply` first.

### Phase 1 — store (T-04, T-05) ✅

- `internal/config/config.go`: `MainWorkspace`, `ProjectWorkspace(id)`,
  `ProjectsFile(...)`; `SessionsDir`/`CronFile`/`UploadsDir` take a workspace
  segment, all 18 call sites pass `config.MainWorkspace`.
- `internal/projects` — the store, with tests. The load-bearing one is
  `TestGenerateIDIsPicoclawFixedPoint`: a generated id must already satisfy
  picoclaw's own `validIDRe`, or picoclaw rewrites it and the registered agent
  stops matching the dispatch rule we wrote.

### Phase 2 — projection (T-06, T-07, T-08) ✅

- `internal/docker/projects.go` `projectAgents` — rebuild, never merge.
- Called inside `materializeModels`' read-modify-write, so it cannot be clobbered.
- **T-07 was verified to discriminate, not just to pass**: the implementation was
  temporarily replaced with a merge-instead-of-rebuild version and
  `TestProjectAgentsRebuildsRatherThanMerges` failed, then passed again on
  restore. A test that only ever passes proves nothing about DEC-6.

### Phase 3 — workspace and container (T-09, T-10, T-11) ✅

- `seedProjectWorkspace` / `composeProjectAgentMD` / `splitFrontmatter`, with
  tests including instructions that contain `---` (a user must not be able to
  forge or truncate the inherited frontmatter).
- **Defect found and fixed after review:** `USER.md` was being re-copied on every
  ensure, which would have erased everything a project agent learned about the
  user on the user's next message. It is now seeded only when absent (FR-9e),
  matching what the main workspace has always done. `TestSeedProjectWorkspace\
KeepsAgentWrittenUserFile` is the regression.
- `syncProjectWorkspaces` on the ensure path, including an orphan sweep for
  workspaces whose project record is gone.
- `projectSecretsBinds` + `projectBindDrift`, shipped together as tasks.md
  required, wired into `create()` and the recreate branch of `EnsureRunning`.

### Phase 4 — routing and API (T-12, T-14) ✅

- `identity.ProjectSessionID` / `identity.ProjectChatPattern` — deliberately
  adjacent, because they are one fact written twice and a disagreement produces
  silence, not an error.
- `internal/httpapi/projects.go` — `GET|POST /v1/projects`,
  `PATCH|DELETE /v1/projects/{id}`, read gated on `read`, mutations on `write`.
  14 HTTP tests.

---

## Knowledge graph per project ✅

Decided by the user: each project gets its own graph.

The obstacle, found before implementing: `tools.mcp.servers` is **global to the
container** (`mcp_config.go`), so every agent — main and all projects — reads the
same block and presents the same bearer token. A per-project graph cannot come
from the token alone.

The lever that works is picoclaw's per-agent MCP **allowlist**, verified in
source at `pkg/agent/tool_allowlist.go:167` and `pkg/agent/definition.go:37`
(frontmatter field `mcpServers`, matched by server NAME). So:

- `memgraph.Scope` gains `Project`; `Store.Dir` resolves `memory-graph-<id>`,
  still above `workspace/` and still root-owned.
- `mcptoken` carries the project as an OPTIONAL fifth field, so tokens already in
  running containers keep verifying — a project-less token is byte-identical to
  the pre-feature form.
- One MCP server entry per project (`memory-<id>`), each with its own token.
  Rebuilt, not merged, so a deleted project's server cannot linger.
- Each project's `AGENT.md` frontmatter gets `mcpServers: [memory-<id>]`. This is
  **the one deliberate edit** to the inherited frontmatter (FR-9a otherwise says
  verbatim), and an inherited `mcpServers:` is REPLACED rather than extended — a
  parent that already restricted its servers would otherwise hand the project a
  route back to the parent's own graph.
- `/v1/memory-graph*` takes `?project=`.

**Asymmetric, knowingly.** `AllowsMCPServer` returns true when the allowlist is
nil, and the main agent's `AGENT.md` comes from the admin persona cascade — not
this proxy's file to edit. The main agent can therefore still reach a project's
memory server. That is a context boundary, not a security one: both agents belong
to the same user, same workspace, same credentials.

---

## Partially done

### T-13 / T-15 — project-scoping the remaining endpoints ⚠

**Done:** `/v1/chat/completions`, `/v1/sessions/history`, `/v1/memory-graph*`.
Unknown project is a 404 on all of them, before any container work (AC-8).

**Proxy side now complete.** `/v1/media*`, `/v1/media/folder*`, `/v1/cron/*` and
`/v1/memory` all resolve the caller's project. Thirteen `Manager` methods took a
`project` parameter and resolve the directory through one helper,
`workspaceSegment` — including `StoreAgentAttachment`, so a file a project agent
delivers lands in that project's uploads rather than somewhere its own agent
cannot open.

Two deliberate exceptions: the ADMIN file views (`admin.go`) pass `""`, because
an admin browsing a member's files is not inside that member's project and has no
selector for one; and the secrets handlers take no project, because a secret is
per `(user, agent)` and shared by every agent in the container.

**Webapp side now complete too.** The project rides on the `Workspace` type
(`fragment.ts`, field `p`) rather than being threaded as a separate argument
through ten client functions. It qualifies exactly what `t`/`s`/`r` already
qualify — WHICH workspace a request addresses — and a parameter each client could
forget to pass is one some of them would.

That made it four edits instead of forty: `workspaceApi.workspaceQuery` (covers
scheduled tasks AND the knowledge graph), the two query builders in `media.ts` /
`memory.ts`, and the body-carried folder/move/memory writes. On the BFF,
`proxyRead` forwards it for every route on that helper — deliberately not via its
`passThrough` allowlist, because it is not a per-route feature parameter — and
`proxyMediaWrite` already forwards the whole body minus `role`.

`ChatShell` composes the workspace with the route's project, so the chat, the
files, the tasks, the memory note and the graph cannot disagree about which
project they are showing.

---

## Not started

- **T-16** — end-to-end on the real stack. Needs `picoclawImage` pointed at the
  patched image (see the open decision below). Doubles as the upstream PR's
  evidence if that track is taken up.
- **T-17** — declare `/v1/projects` in the parent repo's gateway route config.
  Until then the surface is unreachable from the webapp.

---

## Spec amended during implementation

**FR-6a** was added and FR-6 narrowed. The original said the explicit `main`
entry is written *unconditionally, including when there are no projects*. That
would add an `agents.list` key to **every existing user's** `config.json` on
their next chat — a fleet-wide rewrite, and a drift signal, to state something
picoclaw already does for free. The projection now emits neither `list` nor
`dispatch` when there are zero projects, which is what makes NFR-5 literally
rather than approximately true.

**design.md's "AGENT.md is NOT refreshed on later ensures" is superseded.**
Because the instructions live in `.projects.json` and not in the file, the file
is fully derived and is recomposed on every ensure — so an admin persona change
reaches project agents. The tradeoff, documented at `composeProjectAgentMD`: the
agent can write to that path (it is a copy, where the main workspace uses a
read-only bind) and such an edit is reverted on the next ensure.

---

## Open decision, unresolved

`config.yaml`'s `picoclawImage` still points at `docker.io/sipeed/picoclaw:latest`.
Pointing it at `zombie-crab/picoclaw:0.3.1-glob` changes what **every** spawned
container runs, not just this feature's, so it was left for the user. Nothing
routes to a project agent until that switch is made.
