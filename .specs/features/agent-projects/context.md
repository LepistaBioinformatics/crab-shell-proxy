# agent-projects — Context (source verification + user decisions)

Two kinds of record. **VER-\*** are facts read out of picoclaw's own source, so a
later reader does not re-derive them or trust a secondhand summary. **DEC-\*** are
choices the user made, with the reasoning, so they are not re-litigated.

## How the picoclaw facts were established

Everything under VER-\* was read from `sipeed/picoclaw` at tag **v0.3.1**, commit
`2cf030d` — verified to be the exact commit baked into `docker.io/sipeed/picoclaw:latest`,
the image `config.yaml:26` pulls (`build_info` in
`internal/docker/defaulttemplate/picoclaw/config.json:464-468` reports
`0.3.1 / 2cf030d2`).

This matters because the feature was originally proposed from a chat summary of
the source. The summary was right about `AgentConfig`'s shape and wrong about
nothing load-bearing, but three of the four behaviors the design depends on were
not in it at all.

Where a fact also holds on upstream `main`, that is stated — it changes whether
"upgrade picoclaw" is an escape hatch.

---

## VER-1 — `agents.defaults` cascades into every `agents.list` entry

`resolveAgentWorkspace` / `resolveAgentModel` / `resolveAgentFallbacks`
(`pkg/agent/instance.go:365-400`) each fall back to `defaults` when the per-agent
field is unset.

**Consequence:** the original requirement — "configurações essenciais como
`image_model` e outros devem também ser injetadas" — is satisfied by writing
*nothing*. `image_model` is not even a per-agent field: it exists only on
`AgentDefaults` (`pkg/config/config.go:430`), so every agent in the container
shares it. `AgentConfig` (`pkg/config/config.go:314-322`) has exactly six
optional fields; there is no third place for a setting to hide.

## VER-2 — A list entry with `skills` omitted inherits every skill

`resolveAgentSkillsFilter` (`pkg/agent/instance.go:402-413`) returns `nil` when
`agentCfg.Skills == nil`, and `nil` means *no filter*, not *no skills*.

Skills reach the agent from three roots, resolved workspace → global → builtin
(`pkg/skills/loader.go:88-94`). The global root is `~/.picoclaw/skills` — which is
exactly where `manager.go:431-432` mounts the crab effective-skills directory,
**outside any workspace**. So admin shared skills already reach every agent in
the container, whatever workspace it runs in, with no change.

**Consequence:** "todas as skills do agente pai são herdadas no filho" is the
default behavior, obtained by omitting the field.

## VER-3 — Dispatch matching is exact equality, with no wildcard anywhere

`ruleMatchesView` (`pkg/routing/route.go:178-201`) performs seven literal
comparisons (`when.Chat != view.Chat`, …). No `regexp`, no `filepath.Match`, no
prefix handling exists in the package.

**This also holds on upstream `main`:** `git diff v0.3.1 origin/main -- pkg/routing/`
is empty. Routing has not changed since the deployed version, so upgrading
picoclaw does not provide wildcards. This is what forces DEC-2.

## VER-4 — The pico channel varies exactly one selector

`pkg/channels/pico/pico.go:1195-1226` builds the inbound context with
`ChatID = "pico:" + sessionID`, `ChatType = "direct"`, and a hardcoded
`SenderID = "pico-user"`. `Account`, `SpaceID` and `TopicID` are never set.

`buildDispatchView` (`pkg/routing/route.go:215-245`) then composes
`view.Chat = "direct:pico:<session_id>"`, lowercased, and normalizes the empty
account to `"default"`.

**Consequence:** of the seven selectors, only `chat` carries information the
proxy can vary. `session_dimensions` on a rule does not help — it reorders which
dimensions form the session key, but the *values* come from this same view.

## VER-5 — Agent choice and conversation identity are the same knob

`CanonicalScopeSignature` (`pkg/session/key.go:184-200`) builds the session key
from `agent | channel | account | <dimension values>`. Combined with VER-4,
`chat` is the only dimension the pico channel varies.

**Consequence:** there is no way to hold `chat` fixed per project (one static
rule) and still separate conversations. Any design that wants both must widen
what a rule can match — which is DEC-1.

## VER-6 — No other route to agent selection exists

Checked and ruled out:

- **Pico protocol** — `PicoMessage` is `{type, id, session_id, timestamp, payload}`
  (`pkg/channels/pico/protocol.go:37-43`). No agent field. Upstream `main` adds
  only `usage`.
- **Slash commands** — `cmd_switch.go` switches the *model* only. There is no
  chat→agent binding command.
- **Gateway HTTP** — the only registrations are `/health`, `/ready`, `/reload`
  (`pkg/health/server.go:49-51`) plus per-channel webhooks
  (`pkg/channels/manager.go:1137-1151`). Nothing accepts an `agent_id`.

## VER-7 — picoclaw creates a named agent's workspace, empty

`NewAgentInstance` calls `os.MkdirAll(workspace, 0o755)`
(`pkg/agent/instance.go:78`). For a named non-default agent with no explicit
`workspace`, the path is `<defaults.workspace>/../workspace-<id>`
(`pkg/agent/instance.go:374-376`).

Since crab mounts the whole per-user data dir at `<home>/.picoclaw`
(`manager.go:456`), that sibling path lands **inside the mounted volume** and
persists. But it is created bare: no `AGENT.md`, no `USER.md`, no `memory/`.
Seeding is the proxy's job (FR-8).

## VER-8 — A non-empty `agents.list` removes the implicit `main` agent

`NewAgentRegistry` (`pkg/agent/registry.go:34-42`) synthesizes
`{id: "main", default: true}` **only when the list is empty**. Otherwise the
default is whichever entry sets `default: true`, falling back to `list[0]`
(`pkg/routing/route.go:82-95`).

**Consequence:** the first project a user creates is the moment this bites. The
projection must always emit the default entry explicitly (FR-6).

## VER-9 — `/reload` ignores `hot_reload`, but restarts every service

`gateway.Gateway` always wires the manual reload channel
(`pkg/gateway/gateway.go:226-243`); `gateway.hot_reload` gates only the file
*watcher*. So `POST /reload` works on the shipped template, which sets
`hot_reload: false`. It requires the gateway pid token as a bearer
(`pkg/health/server.go:130-143`).

But `handleConfigReload` runs `stopAndCleanupServices` → `restartServices`
(`pkg/gateway/gateway.go:603,636`), which tears down the pico channel and **drops
every open WebSocket**. `config.yaml:97` already keeps the alpha agent
`continuous` specifically so picoclaw's in-memory session is not reset.

**Consequence:** reload is safe at project-create time and hostile mid-conversation.
This is what disqualified the "one rule per conversation" design, and it is why
the feature does not need `/reload` at all (DEC-3 makes create a container
recreate anyway).

## VER-10 — `delegate` registers on agent count, but authorizes fail-closed

Two separate gates, and conflating them would be a security error:

- **Registration** is count-gated: `if len(registry.ListAgentIDs()) > 1`
  (`pkg/agent/agent_init.go:395`). It does not consult `allow_agents`.
- **Authorization** is allowlist-gated at call time.
  `DelegateTool.Execute` (`pkg/tools/delegate.go:82-84`) runs `allowlistCheck` →
  `CanSpawnSubagent` → `agentAllowsSubagent` (`pkg/agent/registry.go:125-138`),
  which returns **false when `Subagents == nil` or `AllowAgents == nil`**.

**Consequence:** the main agent gains the tool on the day the first project is
created, but with `subagents` unset every call is refused with
`not allowed to delegate to agent "x"`. Inert, and inert *fail-closed* — verified
in source, not inferred from the embedded documentation, because the doc
describes only the registration gate and reads as though the tool were usable.

**Trap:** `agentAllowsSubagent` treats a literal `"*"` in `allow_agents` as
"allow every agent" (`registry.go:130`). The projection must never emit it. A
main agent with `allow_agents: ["*"]` could delegate into any project's agent,
which runs in that project's workspace — crossing the isolation boundary this
repo exists to enforce.

## VER-11 — Agent IDs are `[a-z0-9][a-z0-9_-]{0,63}`

`NormalizeAgentID` (`pkg/routing/agent_id.go:16,25-43`) lowercases, collapses
anything outside the alphabet to `-`, strips leading/trailing dashes, and
truncates at 64.

**Consequence:** `.` can never appear in an agent ID, which is what makes it a
safe separator in DEC-4.

---

## DEC-1 — Projects route through a glob dispatch rule, not a container per project

**Question:** given VER-3/4/5, how does one project hold many conversations
without the config growing per conversation?

**Options put to the user:** (a) one container per project, no dispatch at all;
(b) patch picoclaw to support globs in `DispatchSelector`; (c) one rule per
conversation, written only while the container is idle.

**Decision:** (b).

**Why:** (a) reuses every existing crab surface untouched but multiplies
containers; (c) keeps stock picoclaw but grows the config without bound, needs
pruning and a reload queue, and risks dropping other tabs' WebSockets (VER-9).
(b) is ~15 lines upstream and makes the config change **exactly once per
project**, never per conversation.

## DEC-2 — Ship a build-time patch overlay now; upstream contribution is parallel and optional

**Question:** where does the patched picoclaw come from — a submodule here, a
fork publishing an image, or upstream?

**Decision:** a **patch overlay** — a small Dockerfile that clones upstream at a
pinned tag, applies a `.patch`, and builds. This is the critical path and nothing
waits on it. The upstream issue and PR are a parallel, lower-priority track;
whether to open the PR is decided once the patch is proven in production.

**Why:** the first framing of this decision ("upstream PR first, fork if
refused") conflated two separable things. What the feature requires is a
*binary*; where the patch lives is distribution. And the ordering is forced
anyway: `CONTRIBUTING.md` demands evidence that a patched build was exercised in
a real environment, so **the image necessarily exists before the PR can be
opened**. Treating the PR as the gate had the dependency backwards.

The overlay is also much cheaper than the fork it was cast as a fallback to:
there is no mirrored repository to keep in sync, only a diff against a tag. If
the PR lands, the file is deleted and the stock image returns.

**Timeline evidence that this is not merely impatience:** `:latest` on Docker Hub
is published only from a **release tag** — `.github/workflows/docker-build.yml`
is a `workflow_call` taking a required release tag, not a push-to-`main` trigger.
So an upstream merge does not produce an image. Twelve releases exist
(v0.2.1 → v0.3.1), so the cadence is active, but v0.3.1 dates from 2026-07-03 and
is still `latest`. Best case is issue → review → merge → next release.

**Consequence:** `picoclawImage` (`config.yaml:26`) points at the overlay build.
Adopting a future upstream release is a one-line change. The residual cost of the
private patch is re-applying and re-testing ~15 lines on each picoclaw upgrade —
low, because `pkg/routing/` did not change at all between v0.3.1 and current
`main` (VER-3), but not zero. That residual cost is the only argument for the PR.

## DEC-3 — A project workspace inherits leanly, plus `.secrets`

**Question:** what does `workspace-<proj>` get from the main workspace?

**Options put to the user:** full parity (per-project binds for `.secrets`, the
four shared-files layers, and persona); lean (own `AGENT.md`, own files, global
skills, inherited model); lean plus `.secrets`.

**Decision:** lean plus `.secrets`.

**Why:** an agent with no credentials is usually a useless agent, so `.secrets`
earns its one bind. The four shared-files layers do not earn five more binds per
project in a first cut. Persona files are cheap enough to copy rather than mount.

## DEC-4 — The project prefix is `p.<id>.`, separated by a dot

**Question:** how is the project encoded into the session ID the rule matches?

**Decision:** `session_id = "p." + projectID + "." + SessionKey(accID, clientSessionID)`,
matched by `chat: "direct:pico:p.<projectid>.*"`.

**Why:** a `-` separator collides. Project IDs may contain `-` (VER-11), so
projects `my` and `my-proj` would both be matched by the pattern `p-my-*` — one
user's conversation silently routed into another project's agent and workspace.
`.` cannot occur in an agent ID, so the prefix is unambiguous.

## DEC-5 — The glob supports `*` only

**Question:** which metacharacters does the upstream patch accept?

**Decision:** `*` (any sequence, including empty). No `?`, no `[...]` classes, no
escaping. A pattern containing no `*` keeps today's byte-exact comparison.

**Why:** `filepath.Match` was the obvious reach and is wrong twice — its `*` does
not cross `/`, and chat IDs on other channels contain `/`; and a malformed `[`
returns `ErrBadPattern`, where the tempting "treat as no-match / treat as match"
handling turns a typo into either a dead rule or a silent catch-all that reroutes
a user's chats. With `*` alone no pattern can be invalid, and every existing
config behaves identically.

## DEC-6 — Projects are stored proxy-side and *projected* into `config.json`

**Question:** where is the list of projects the source of truth?

**Decision:** a proxy-owned `.projects.json` in the user data dir.
`agents.list` and `agents.dispatch.rules` are derived from it on every ensure,
inside the same read-modify-write as model materialization.

**Why:** `materializeModels` (`internal/docker/materialize.go:31-111`) rewrites
the whole `config.json` on **every** ensure. A rule written directly into
`config.json` would be silently erased on the user's next chat — the most likely
failure mode in the whole feature, and one that presents as "the project stopped
working" rather than as an error. Deriving from a store makes it self-healing,
and mirrors how the model registry already works.

The store sits **above** `workspace/`, next to `.crab-owner.json`, so
`restrict_to_workspace` keeps the agent from reading or editing the list of
projects.

## DEC-7 — Conversation in Portuguese; every artifact in English

The upstream issue, PR, commits and code comments are English, and follow
`sipeed/picoclaw`'s `CONTRIBUTING.md` rather than this repo's conventions where
the two differ. See design.md § Upstream contribution for the checklist.
