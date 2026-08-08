# agent-projects — Design

Reads with `spec.md` (requirements) and `context.md` (why picoclaw behaves as it
does, and which choices are already settled).

---

## The one hard constraint

Everything below is shaped by VER-4/VER-5: on the pico channel, `chat` is the
only dispatch selector that carries information, and the session key is derived
from that same value. **Which agent answers** and **which conversation this is**
are one knob.

Stock picoclaw therefore forces a choice between one static rule per project with
a single shared conversation, or one rule per conversation with an unbounded,
reload-hungry config. The feature buys its way out by widening what a rule can
match — one `*` — and then encoding the project into the part of the session ID
that the rule matches.

```
webapp                 crab-shell-proxy                     picoclaw container
------                 ----------------                     ------------------
POST /v1/chat          session_id =                         chat view =
  project=seedtrial ->   "p.seedtrial." + SessionKey(...) ->   "direct:pico:p.seedtrial.<hex>"
                                                                     |
                                                             rule chat="direct:pico:p.seedtrial.*"
                                                                     |
                                                             agent "seedtrial"
                                                               workspace-seedtrial/
                                                               model  <- agents.defaults
                                                               skills <- ~/.picoclaw/skills
```

A chat with no project keeps the bare 32-hex session key, matches no rule, and
lands on the default agent. That is the whole compatibility story.

---

## Part 1 — Upstream patch (`sipeed/picoclaw`)

### Change

One helper in `pkg/routing/route.go`, applied to the six string selectors inside
`ruleMatchesView` (`route.go:178-201`):

```go
// selectorMatches reports whether a dispatch selector value matches the inbound
// view. An empty pattern is an unconstrained selector.
//
// Only '*' is a metacharacter, and it matches any sequence of characters
// including the empty one. Everything else — '?', '[', '\' — is literal, so a
// pattern can never be malformed and no rule can silently widen into a
// catch-all. Patterns with no '*' take the byte-exact path, which is what keeps
// every existing configuration behaving identically.
//
// filepath.Match is deliberately not used: its '*' does not cross the path
// separator, and chat identifiers on several channels contain '/'.
func selectorMatches(pattern, value string) bool {
	if pattern == "" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	return globMatch(pattern, value)
}
```

`globMatch` splits on `*` and walks the segments: the first must be a prefix, the
last a suffix, and each middle segment is found greedily left to right. Both
sides are already lowercased upstream — `normalizeDispatchSelector`
(`route.go:247`) and `buildDispatchView` (`route.go:215`) — so the comparison
stays case-insensitive without extra work.

`Mentioned` is a `*bool` and is untouched.

### Delivery: a patch overlay, not a fork

The image is built here (DEC-2):

```dockerfile
# picoclaw with dispatch-selector globs. Delete this whole directory once the
# change is upstream and released — picoclawImage goes back to the stock tag.
FROM golang:1.25-bookworm AS build
ARG PICOCLAW_TAG=v0.3.1
RUN git clone --depth 1 --branch ${PICOCLAW_TAG} https://github.com/sipeed/picoclaw.git /src
COPY dispatch-selector-glob.patch /tmp/
RUN cd /src && git apply --verbose /tmp/dispatch-selector-glob.patch && make build
...
```

Pinned tag, never a moving branch: the patch is a diff against known bytes, and
`git apply` failing loudly on an upgrade is the desired behavior. There is no
mirrored repository to keep in sync — the maintenance surface is one `.patch`
file against one file that has not changed between v0.3.1 and upstream `main`
(VER-3).

### Upstream contribution (parallel, optional)

Not on the critical path, and deliberately not a gate. The ordering is forced the
other way round: their template requires evidence that a patched build was
exercised in a real deployment, so the image exists first regardless.

The patch is nonetheless written to be upstreamable from day one — tests, docs,
no zombie-crab content — so opening the PR is transcription rather than rework.
The issue and PR are framed in picoclaw's own interest, not this stack's:
multi-agent routing currently requires one rule per chat identifier, which does
not scale for any deployment that creates chats dynamically. A single wildcard
makes one rule cover a family of chats. Zombie-crab is at most a motivating
example.

The single argument for bothering: a private patch must be re-applied and
re-tested on every picoclaw upgrade, forever.

### `CONTRIBUTING.md` checklist — if and when the PR is opened

Their rules, not this repo's, and they differ in several places.

| Step | Requirement |
| --- | --- |
| Before code | Feature-request issue first — *"For substantial new features, please open an issue first to discuss the design"*. This is also the DEC-2 decision point |
| Toolchain | Go **1.25+** (this repo is on 1.23; the patch needs its own toolchain) |
| Gate | `make check` green — deps, fmt, vet, test, `lint-docs` |
| Branch | Off `main`, targeting `main`. `feat/dispatch-selector-glob` |
| Commits | English, imperative, Conventional Commits |
| Tests | Extend `pkg/routing/route_test.go`; existing cases stay unmodified (FR-2/AC-2) |
| Docs | Update `docs/guides/routing-guide.md`. Its `.zh.md` sibling can be left stale: `scripts/lint-docs.sh:208` only requires a translation to *have* an English source, never the reverse — checked, so English-only will not fail their CI |
| PR template | Every section, including **🤖 AI Code Generation → 🛠️ Mostly AI-generated** and **🧪 Test Environment** |
| Evidence | A patched build exercised in a real deployment. Channel: pico. Not optional — *"PRs where it is clear the contributor has not read or tested the AI-generated code will be closed without review"* |
| Scope | Nothing crab-specific in the PR |
| Review | No force-push once review starts; add commits, maintainer squashes |
| Reviewers | Agent area: @lxowalle / @Zhaoyikaiii |

### Reverting to stock

`picoclawImage` (`config.yaml:26`) is a plain config value. When the change is
upstream and released, point it back at `sipeed/picoclaw:<tag>` and delete the
overlay directory. Nothing else in this feature knows the difference.

---

## Part 2 — Proxy

### Components

| Component | Responsibility |
| --- | --- |
| `internal/projects` (new) | The `.projects.json` store: load, create, rename, delete, ID generation and uniqueness (FR-5) |
| `internal/docker/projects.go` (new) | Projection of the store into `agents.list` + `agents.dispatch.rules`; project workspace seeding |
| `internal/docker/materialize.go` | Calls the projection inside its existing read-modify-write (FR-7b) |
| `internal/docker/manager.go` | One extra `.secrets` bind per project; recreate on project-set drift |
| `internal/config/config.go` | `SessionsDir` / `CronFile` / `UploadsDir` take a workspace segment (FR-12a) |
| `internal/httpapi/projects.go` (new) | The CRUD surface |
| `internal/httpapi/handlers.go` | Optional `project` on chat, history, media, cron |

### Store and projection

`.projects.json` lives in the user data dir beside `config.json` and
`.crab-owner.json` — above `workspace/`, so `restrict_to_workspace` keeps the
agent out of it.

The projection is a pure function of the store, applied to the in-memory
`config.json` map that `materializeModels` is already holding:

```
materializeModels(configPath, secPath, resolution, projects)
  read config.json
  ... existing model_list / agents.defaults work ...
  projectAgents(cfg, projects)      // rebuilds agents.list + agents.dispatch
  write config.json
```

Rebuild, never merge. A merge would accumulate rules for deleted projects and
leave the config drifting from the store, which is exactly the class of bug
DEC-6 exists to prevent. `agents.defaults` is out of the projection's reach.

### Workspace seeding

On create (host side, before the container comes back up):

```
workspace-<id>/
  AGENT.md          parent frontmatter + project instructions   (FR-9a)
  USER.md           copy of resolved persona                    (FR-9b)
  SOUL.md           copy                                        (FR-9b)
  HEARTBEAT.md      copy                                        (FR-9b)
  memory/  sessions/  uploads/                                  (FR-9)
  .secrets/         read-only bind of the effective secrets dir (FR-9c)
```

`AGENT.md` is the feature's substance. Splitting frontmatter from body and
re-emitting the parent's frontmatter verbatim is what makes the child inherit the
parent's declared tools and skills — picoclaw reads that frontmatter through
`loadAgentDefinition(workspace)`, and it overrides the config-level skills filter
when present.

The three persona copies are refreshed on every ensure, so an admin persona
change propagates. `AGENT.md` is **not** refreshed: it holds user-authored
instructions.

A symlink from a project workspace to the main one was considered and rejected —
`restrict_to_workspace` will either resolve it and refuse, or not resolve it and
leave a hole in the sandbox. Neither outcome is worth the saved copy.

### Lifecycle

| Event | Effect |
| --- | --- |
| Create | Write store → seed workspace → recreate container (new bind) |
| Rename | Write store; projection updates `name` on next ensure. No bounce |
| Edit instructions | Rewrite `AGENT.md` body in place. No bounce |
| Delete | Remove from store → remove workspace → recreate container |

Recreate, not restart, because a bind mount cannot be added to a running
container. Project events are deliberate and rare, and `EnsureRunning` already
recreates on persona drift — the path is well travelled. Detection reuses that
mechanism: the project set becomes part of what drift compares.

### Routing and endpoints

`identity.SessionKey` is unchanged. A thin wrapper prefixes when a project is
named, keeping the hash function in one place:

```go
func ProjectSessionID(projectID, sessionKey string) string {
	if projectID == "" {
		return sessionKey
	}
	return "p." + projectID + "." + sessionKey
}
```

An unknown project is a 404 before any container work (FR-8a). Falling through
to the default agent would silently write the user's conversation into the wrong
workspace — the failure would look like lost history rather than a bad request.

---

## What could still go wrong

**The upstream PR stalls or is refused.** Mitigated by DEC-2's exit: fork,
publish a tag, change one config line. The crab work is unaffected either way.
This is the only genuinely external risk.

**A project entry acquires a `model` key.** Then the project is pinned to a name
the model registry may later stop resolving, while `materializeModels` keeps
rewriting `model_list` around it — the project silently diverges from the model
its parent actually runs, or names an entry that is no longer there. How
picoclaw reacts to a per-agent `model.primary` absent from `model_list` was not
verified; the documented failure is for `agents.defaults.model_name`
(`internal/docker/materialize.go:177-181`). Either way the pin is wrong. FR-7
forbids it; AC-3 tests for its absence.

**The projection lands outside `materializeModels`.** The rule survives creation,
works once, and vanishes on the user's next chat on any workspace. AC-4 is the
regression test, and it is worth writing before the implementation.

**Bind count.** One `.secrets` bind per project. A user with thirty projects has
thirty extra binds. Acceptable at the expected scale; if it stops being so, the
answer is a single bind of a parent directory, not a redesign.

**Container recreate loses in-flight turns.** Same exposure `restart-control`
already manages, and project events are user-initiated. The webapp is expected to
say so before it happens.

---

## Testing

Unit, and runnable in the sandbox:

- `globMatch` table test: prefix, suffix, middle, bare `*`, no `*`, empty
  pattern, literal `?`/`[` (upstream).
- Projection: zero projects still emits the explicit `main` entry (VER-8); N
  projects emit N rules and no catch-all; deleted project leaves nothing behind.
- Projection is idempotent, and re-running `materializeModels` preserves it
  (AC-4).
- ID generation: slug normalization, uniqueness, `main` rejected, no `.` or `*`
  reachable (NFR-3).
- `AGENT.md` composition: frontmatter preserved byte-for-byte, body replaced.

Chown-dependent paths follow STATE.md L-001 — verified through the pure helpers,
or run as root inside `golang:1.23-bookworm`, never treated as sandbox failures.

End-to-end (needs the stack, and doubles as the upstream PR's evidence): create a
project, chat in it, confirm the transcript lands in `workspace-<id>/sessions/`,
confirm a tenant-scope skill is visible to the project agent, chat on the main
agent, confirm the project still routes.
