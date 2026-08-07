# agent-projects — Tasks

From [design.md](design.md). `[P]` = parallelizable with its siblings.

Gate check for every proxy task: `go build ./... && go vet ./... && go test ./...`.
Known-red baseline: the `TestEnsureRunning*` chown tests fail in this sandbox
(STATE.md L-001) — sandbox noise, not a regression. Compare against the base
commit before blaming a change.

**Two orderings are load-bearing and easy to get wrong:**

1. **T-11 must not merge without T-10.** The `.secrets` bind and the
   recreate-on-project-drift detection are one change. Shipped apart, the first
   project a user creates boots with no credentials and the symptom reads as a
   picoclaw bug, not as a missing mount.
2. **T-07 is written before T-06.** It is the regression test for DEC-6, the
   likeliest silent failure in the feature. A test written after the code it
   guards tends to assert what the code does rather than what the design
   requires.

---

## Phase 0 — Unblock: patched picoclaw image

Nothing downstream can be exercised end-to-end without this, and it is
self-contained. Start here.

### T-01 — Glob matching in the dispatch selector
- **What:** `selectorMatches` + `globMatch` in `pkg/routing/route.go`, wired into
  the six string comparisons in `ruleMatchesView`. `*` only; a pattern without
  `*` keeps the byte-exact path (FR-2, FR-3, DEC-5).
- **Where:** the picoclaw working copy, branched from tag `v0.3.1`
- **Depends on:** —
- **Reuses:** nothing — `filepath.Match` is deliberately NOT used (its `*` does
  not cross `/`, and chat IDs on other channels contain `/`).
- **Done when:** the patch applies cleanly to `v0.3.1` and `make check` is green.
- **Tests:** table test — prefix `a*`, suffix `*z`, middle `a*z`, bare `*`,
  no-`*` literal, empty pattern, and literal `?` / `[` / `\` treated as
  ordinary characters. Existing `route_test.go` cases stay **unmodified**
  (AC-2).
- **Gate:** `make check` inside a Go 1.25 toolchain (this repo is on 1.23 — the
  patch needs its own).

### T-02 — Patch overlay build
- **What:** `deploy/picoclaw-glob/` — a `Dockerfile` cloning `sipeed/picoclaw`
  at a **pinned tag**, applying `dispatch-selector-glob.patch`, building the
  image. Header comment says how to delete the whole directory once upstream
  ships it.
- **Where:** new `deploy/picoclaw-glob/`
- **Depends on:** T-01
- **Reuses:** the upstream `Dockerfile` as the base; do not re-derive the runtime
  stage.
- **Done when:** the image builds and `picoclawImage` in `config.yaml` can point
  at it. Pinned tag, never a branch: `git apply` must fail loudly on an upgrade.
- **Tests:** build succeeds; the built binary routes a `p.demo.*` rule to the
  right agent and a non-matching session to the default (AC-1). This is a real
  container run, not a unit test.

### T-03 — [P] Upstream docs for the patch
- **What:** the `docs/guides/routing-guide.md` change that accompanies T-01, kept
  in the same `.patch`, so opening the PR later is transcription not rework
  (FR-4a).
- **Where:** picoclaw working copy
- **Depends on:** T-01
- **Done when:** `make lint-docs` green. The `.zh.md` sibling is left stale —
  `scripts/lint-docs.sh:208` only requires a translation to *have* an English
  source, never the reverse (checked).

---

## Phase 1 — Project store (proxy)

### T-04 — Path helpers and the project record
- **What:** `ProjectsFile(root, tenant, subs, role, user)` →
  `UserWorkspace/.projects.json`, and `ProjectWorkspaceSegment(id)` →
  `"workspace-<id>"`. Documented like their neighbours, saying *why* the store
  sits above `workspace/` (the agent must not read or edit the project list).
- **Where:** `internal/config/config.go`
- **Depends on:** —
- **Reuses:** `UserWorkspace`, `identity.SanitizeID`, the existing helper
  docblock style.
- **Done when:** paths compose as in design §2.
- **Tests:** table test including an ID needing sanitization.

### T-05 — `internal/projects` store
- **What:** `Project{ID,Name,Instructions,CreatedAt}`, `Store` with `List`,
  `Create`, `Rename`, `SetInstructions`, `Delete`; ID generation from the name
  (FR-5a) with uniqueness, `main` rejected (FR-5b).
- **Where:** new `internal/projects/{projects.go,projects_test.go}`
- **Depends on:** T-04
- **Reuses:** `routing.NormalizeAgentID`'s alphabet — mirror
  `^[a-z0-9][a-z0-9_-]{0,63}$` exactly so a generated ID survives picoclaw's
  normalization unchanged (VER-11). Read that regex; do not approximate it.
- **Done when:** a generated ID can never contain `.` or `*` (NFR-3), and two
  projects named "Seed Trial" get distinct IDs.
- **Tests:** slug normalization; collision suffixing; `main` rejected; unicode
  and punctuation-only names; empty name rejected.
- **Gate:** package tests green — no Docker, which is why it is its own package.

---

## Phase 2 — Config projection (proxy)

### T-06 — Project the store into `agents.list` + `agents.dispatch`
- **What:** `projectAgents(cfg map[string]any, projects []projects.Project)` —
  **rebuild**, never merge. Always emits the explicit `{id:"main",default:true}`
  entry (FR-6, VER-8). Project entries carry `id`, `name`, `workspace` and
  **nothing else** (FR-7). One rule per project, no catch-all (FR-7a). Never
  emits `subagents` (NFR-4a).
- **Where:** new `internal/docker/projects.go`
- **Depends on:** T-05, T-07
- **Reuses:** `childMap` from `internal/docker/materialize.go`.
- **Done when:** `agents.defaults` is untouched (FR-7c) and a deleted project
  leaves no trace.
- **Tests:** zero projects still emits `main`; N projects emit N rules; no
  `model` or `skills` key on any project entry (AC-3); no `subagents` key
  anywhere; no `"*"` in any `allow_agents` (NFR-4a); idempotent across two runs.

### T-07 — The DEC-6 regression test (**write before T-06**)
- **What:** the AC-4 assertion: after any ensure, `agents.list` and
  `agents.dispatch` in `config.json` **equal** the pure projection of
  `.projects.json`. Equality, not survival — a survival check passes for a merged
  list, a stale rule, and an out-of-band write alike.
- **Where:** `internal/docker/projects_test.go`
- **Depends on:** T-05
- **Done when:** it fails against an unimplemented `projectAgents`, and against a
  deliberately merge-instead-of-rebuild implementation.

### T-08 — Call the projection from `materializeModels`
- **What:** thread the project list into `materializeModels` and apply the
  projection inside its existing read-modify-write (FR-7b). Not a second write:
  a separate write is erased on the next ensure.
- **Where:** `internal/docker/materialize.go`, `resolveAndMaterialize`
- **Depends on:** T-06
- **Reuses:** the existing three-step `.security.yml`/`config.json` ordering —
  do not disturb it; the projection touches only the in-memory map before the
  `config.json` write.
- **Done when:** T-07 passes for real.
- **Tests:** T-07; plus existing `materialize_test.go` and `migrate_models_test.go`
  stay green.

---

## Phase 3 — Workspace and container (proxy)

### T-09 — Seed a project workspace
- **What:** create `workspace-<id>/{sessions,memory,uploads}`; compose `AGENT.md`
  from the resolved parent's frontmatter (verbatim) plus the project
  instructions as body (FR-9a); copy `USER.md`/`SOUL.md`/`HEARTBEAT.md` from the
  resolved persona cascade on **every** ensure (FR-9b); chown.
- **Where:** `internal/docker/projects.go`
- **Depends on:** T-05
- **Reuses:** `seedWorkspace`, `copyTree`, `chownTree` (`provision.go`) and the
  persona resolution in `persona.go`. `PersonaMounted` vs `PersonaFiles` matters:
  read `persona.go:26,35` before choosing which list to copy.
- **Done when:** frontmatter is preserved byte-for-byte and only the body is
  replaced (AC-6). `AGENT.md` is NOT refreshed on later ensures — it holds
  user-authored text.
- **Tests:** frontmatter round-trip; no frontmatter in parent; instructions
  containing `---`; re-ensure refreshes persona copies but leaves `AGENT.md`.

### T-10 — `.secrets` bind per project **(ships with T-11)**
- **What:** one read-only bind of the effective secrets dir at
  `workspace-<id>/.secrets`, mirroring the main workspace's mount (FR-9c).
- **Where:** `internal/docker/manager.go`
- **Depends on:** T-05
- **Reuses:** the existing `secretsMount` construction — same source dir, extra
  destination.
- **Tests:** bind list contains one entry per project; none when there are no
  projects.

### T-11 — Recreate on project-set drift **(ships with T-10)**
- **What:** the project set joins what drift detection compares, so creating or
  deleting a project recreates the container on its next ensure (FR-10).
- **Where:** `internal/docker/drift.go`
- **Depends on:** T-10
- **Reuses:** `personaMountDests` — the same destination-not-bind-string
  reasoning applies verbatim (comparing whole binds would read a `HostDataRoot`
  change as fleet-wide drift and truncate every session).
- **Done when:** adding a project marks drift; a rename or instructions edit does
  **not** (FR-10a).
- **Tests:** add/remove project ⇒ drift; rename ⇒ no drift; `HostDataRoot` change
  ⇒ no drift.

---

## Phase 4 — Routing and API (proxy)

### T-12 — Project-scoped session IDs
- **What:** `ProjectSessionID(projectID, sessionKey)` → `p.<id>.<key>`, empty
  project returns the key unchanged (FR-8, DEC-4).
- **Where:** `internal/identity/identity.go`
- **Depends on:** —
- **Reuses:** `SessionKey` unchanged — the hash stays in one place.
- **Tests:** prefix shape; empty project is a no-op; a bare key can never match a
  project pattern.

### T-13 — [P] Parameterize the workspace segment
- **What:** `SessionsDir`, `CronFile`, `UploadsDir`
  (`internal/config/config.go:501,510,518`) and the memory dir
  (`internal/docker/memory.go:24`) take the segment. Every existing caller passes
  `"workspace"` (FR-12a).
- **Where:** `internal/config/config.go`, `internal/docker/memory.go`, callers
- **Depends on:** T-04
- **Done when:** behavior with no project is byte-identical (NFR-5).
- **Tests:** existing history/media/cron tests stay green unmodified.

### T-14 — Project CRUD endpoints
- **What:** `GET|POST /{agent}/v1/projects`, `PATCH|DELETE /{agent}/v1/projects/{id}`
  (FR-11). Delete removes store entry, workspace and transcripts (FR-10b).
- **Where:** new `internal/httpapi/projects.go`
- **Depends on:** T-05, T-09
- **Reuses:** the permission chain from `/v1/secrets` — read on GET, write on the
  rest (FR-13).
- **Tests:** create/list/rename/delete round-trip; duplicate name; `main`
  rejected; read-only caller refused on POST.

### T-15 — `project` on the existing endpoints
- **What:** optional `project` on `/v1/chat/completions`, `/v1/sessions/history`,
  `/v1/media`, `/v1/cron/tasks` (FR-12). Unknown project ⇒ **404 before any
  container work** (FR-8a) — falling through to the default agent would write the
  conversation into the wrong workspace, and the symptom would read as lost
  history rather than a bad request.
- **Where:** `internal/httpapi/handlers.go`
- **Depends on:** T-12, T-13, T-14
- **Tests:** AC-5 (transcript lands under `workspace-<id>/sessions/`, unscoped
  history does not return it); AC-8 (unknown project 404s, no session created).

---

## Phase 5 — Verification

### T-16 — End-to-end on the real stack
- **What:** create a project, chat in it, confirm the transcript location;
  confirm a tenant-scope skill is visible to the project agent with no
  project-side action (AC-7); chat on the main agent and confirm the project
  still routes (AC-4 in the field); delete and confirm cleanup (AC-9).
- **Depends on:** T-02, T-15
- **Note:** this run is also the evidence the upstream PR template requires, if
  that track is taken up.

### T-17 — [P] Parent-repo gateway routes
- **What:** declare `/v1/projects` in the mycelium gateway route config so the
  new surface is reachable.
- **Where:** `zombie-crab-project` (parent), deploy configs
- **Depends on:** T-14
- **Reuses:** the existing `/v1/secrets` declaration as the shape to copy.
