# memory-graph-mcp — spec

Give every picoclaw instance a knowledge-graph memory, served by an MCP server
the proxy hosts itself, so no extra container runs in the environment and the same
graph is readable from the user interface.

Modelled on [better-memory-mcp](https://github.com/sockeye44/better-memory-mcp)
v0.8.0. Read `context.md` first: it records the user's three scoping decisions,
eight empirically-established facts about picoclaw and the Go MCP SDK, and three
deliberate deviations from upstream. Several of the requirements below are only
justifiable in light of E-3 and E-7.

## Problem

A picoclaw instance forgets everything structural between turns. It has
`MEMORY_CUSTOM.md` (a flat document a human edits) and its session transcript,
but nothing that lets it record *that* two things relate, or accumulate
observations about an entity over months and retrieve them by meaning.

The obvious fix — run better-memory-mcp as a sidecar — costs a container per
instance, a Node runtime, a ~500 MB embedding model, and leaves the memory
invisible to the webapp. The proxy already owns every other per-member artifact
(secrets, media, skills, persona, `MEMORY_CUSTOM.md`); memory graphs belong in the
same place.

## Functional requirements

### FR-1 — The proxy hosts an MCP server over streamable HTTP

- **FR-1.1** `/v1/mcp` serves the MCP streamable-HTTP transport on **`POST`, `GET`
  and `DELETE`**, implemented with the server half of
  `github.com/modelcontextprotocol/go-sdk` pinned to **v1.6.1** (the version
  picoclaw's client ships — E-2).

  `DELETE` is not optional: picoclaw's client issues `POST` and `DELETE` and never
  `GET` (measured — E-9), and `DELETE` is how it closes a session. This
  requirement first said "POST and GET", which would have left every connection
  unable to terminate cleanly.
- **FR-1.2** The handler runs in `Stateless` mode. The tools are request/response
  with no server→client notifications, so session bookkeeping would be pure
  surface. Verified against the real client, which opens no standalone SSE stream
  (E-9).
- **FR-1.3** The server advertises `name: "crab-memory-graph"` and the tools
  capability.
- **FR-1.4** The route is registered on the same mux as the rest of `/v1/*`, and
  is the only proxy route reachable by a container on `zombie_net` without
  passing through mycelium (NFR-1 constrains what that implies).

### FR-2 — The 15 upstream tools, with upstream's schemas

Every tool below is exposed with upstream's exact name, parameter names, required
set and declared defaults. Schemas are supplied explicitly (E-4), not inferred.

| Tool | Required | Optional (default) |
|---|---|---|
| `create_entities` | `entities[]{name, entityType, observations[]}` | — |
| `create_relations` | `relations[]{from, to, relationType}` | — |
| `add_observations` | `observations[]{entityName, contents[]}` | `confidence[]` |
| `delete_entities` | `entityNames[]` | — |
| `delete_observations` | `deletions[]{entityName, observations[]}` | — |
| `delete_relations` | `relations[]{from, to, relationType}` | — |
| `read_graph` | — | `detailLevel` (`"summary"`, enum `minimal\|summary\|full`), `entityNames[]`, `includeArchived` (`false`), `includeMerged` (`false`) |
| `get_entity_details` | `entityNames[]` | — |
| `search_nodes` | `query` | `maxObservations` (`5` — E-8) |
| `semantic_search` | `query` | `k` (`10`), `threshold` (`0`) |
| `open_nodes` | `names[]` | — |
| `merge_entities` | `sourceName`, `targetName` | — |
| `archive_entity` | `entityName` | — |
| `unarchive_entity` | `entityName` | — |
| `get_recent_changes` | — | `hours` (`24`) |

- **FR-2.1** Tools are **not** `deferred`. The agent must know unconditionally
  that it has a memory; hiding memory behind tool discovery means it only
  remembers when it thinks to look.
- **FR-2.2** `semantic_search`'s description states lexical ranking. It must not
  claim embeddings (D-1).

### FR-3 — Upstream graph semantics, ported faithfully

Behaviour is ported from `index.ts`'s `KnowledgeGraphManager`, including the parts
that are easy to get subtly wrong:

- **FR-3.1** `create_entities` skips names that already exist and reports
  `{created[], skipped[]}` — it never overwrites.
- **FR-3.2** `create_relations` skips exact `(from, to, relationType)` triples that
  already exist; reports `{created, skipped}` counts.
- **FR-3.3** `add_observations` **errors** when the entity does not exist, dedupes
  new contents against existing ones, and defaults each `confidence` to `1.0`.
- **FR-3.4** `delete_entities` cascades: relations touching a deleted entity on
  either side go too. Reports `{deleted, cascadedRelations}`.
- **FR-3.5** `delete_observations` on a missing entity reports `deleted: 0` rather
  than erroring (deliberately unlike FR-3.3 — upstream differs here and the
  asymmetry is load-bearing for idempotent cleanup).
- **FR-3.6** `read_graph` excludes archived and merged entities unless asked;
  filters relations to those whose **both** endpoints survived filtering; and
  honours the three detail levels — `minimal` (name, type, observationCount),
  `summary` (adds `firstObservation` and `relationCount`, plus a graph-level
  `totalObservations`), `full` (everything). Passing `entityNames` returns full
  detail for those entities and ignores `detailLevel`.
- **FR-3.7** `merge_entities` copies non-duplicate observations from source into
  target, redirects every relation endpoint from source to target, dedupes the
  relations that redirect collapses together, then soft-deletes the source with
  `merged: true`, `mergedInto`, `mergedAt`. The source entity is retained, not
  removed.
- **FR-3.8** `archive_entity` / `unarchive_entity` return
  `{success: false, message}` — not an error — for a missing entity or a no-op
  transition.
- **FR-3.9** `get_recent_changes` reports entities and relations created within the
  window plus, per surviving entity, only the observations whose own timestamp is
  within it. Archived and merged entities are excluded.
- **FR-3.10** Legacy normalisation on read: an entity with no `createdAt` gets the
  load time; observations stored as bare strings become
  `{content, timestamp: <load time>, confidence: 1.0}`. This is what makes an
  imported upstream file work.
- **FR-3.11** `delete_relations` removes only exact `(from, to, relationType)`
  matches and reports `{deleted}`. A triple naming a nonexistent relation is not
  an error.
- **FR-3.12** Retrieval that names entities explicitly does not filter:
  `get_entity_details` returns the named entities including archived and merged
  ones, and `open_nodes` likewise, filtering relations to those whose both
  endpoints are in the returned set. Only `read_graph`, `search_nodes` and
  `semantic_search` hide archived/merged entities — asking for an entity by name
  is how you inspect one you archived.
- **FR-3.13** `search_nodes` matches case-insensitively as a substring against
  entity name, entity type and observation contents; excludes archived and merged;
  truncates each returned entity's observations to `maxObservations`; and filters
  relations to those with both endpoints in the result.

### FR-4 — Per-workspace scope, resolved from a self-authenticating token

- **FR-4.1** The MCP request carries `Authorization: Bearer <token>` where

  ```
  token   = b64url(payload) "." b64url(HMAC-SHA256(secret, payload))
  payload = tenantID "/" subsAccID "/" role "/" userAccID
  ```

  `secret` comes from `CRAB_MCP_TOKEN_SECRET` via the existing `{env: ...}` config
  convention (the same shape `webhookSecret` already uses).
- **FR-4.2** The proxy verifies the MAC with `hmac.Equal` and derives the
  `WorkspaceKey` **from the token payload**. No tool parameter names a tenant,
  subscription, role or user; there is no code path by which a caller can address
  another member's graph.

  The payload→`Scope` mapping must be **injective**, which the `/`-joined form is
  not on its own: `role="a/b", userAccID="c"` and `role="a", userAccID="b/c"`
  produce identical payloads, so one member's legitimate token would authenticate
  as another's scope — a canonicalisation collision, not a forgery, so the MAC
  check cannot catch it. Therefore `Mint` **rejects** any scope field containing
  the delimiter, and `Verify` requires exactly four fields.

  `identity.SanitizeID` maps `/` to `-` and probably makes this unreachable today,
  but that is a property of a different package. Rejecting at the boundary means
  FR-4.2 does not silently depend on it.
- **FR-4.3** The token is deterministic for a given workspace, so re-provisioning
  rewrites an identical `config.json` — no churn, no drift, no restart triggered by
  a token that changed for no reason. Rotation is rotating the secret.
- **FR-4.4** A missing, malformed, or MAC-invalid token yields `401` with no body
  detail. There is no path on which an absent token is treated as trusted.
- **FR-4.5** When `CRAB_MCP_TOKEN_SECRET` is unset the MCP route and the config
  injection are both disabled, and the proxy logs it once at startup. A deployment
  that forgot the secret must get no memory rather than an unauthenticated one.

### FR-5 — The proxy injects the MCP server into every workspace's config.json

> **Corrected during implementation.** This requirement first said `alignWorkspace`
> writes the block "on every provision". That is false about the code:
> `alignWorkspace` is called from inside `provision`'s
> `if _, statErr := os.Stat(configPath); statErr != nil` branch, so it runs **only
> on a first-ever seed**. Putting the MCP block there would mean no existing
> member ever gets memory — a permanent gap, not a delayed one. The every-ensure
> writer is `resolveAndMaterialize` (see `materialize.go`: "on every path that
> materializes — not once at first provision"), and FR-5.1 now follows it.

- **FR-5.1** A dedicated writer runs on **every** `EnsureRunning` for a picoclaw
  workspace, alongside `resolveAndMaterialize`, and writes:

  ```json
  "tools": { "mcp": { "enabled": true, "servers": { "memory": {
    "enabled": true, "command": "", "type": "http",
    "url": "<mcpBaseURL>/v1/mcp",
    "headers": { "Authorization": "Bearer <token>" } } } } }
  ```

  matching E-1's measured shape exactly, `command: ""` included. Other entries in
  `tools.mcp.servers` are preserved; only the `memory` key is owned.
- **FR-5.2** `mcpBaseURL` is configurable (`mcpBaseURL` in `config.yaml`,
  overridable by `CRAB_MCP_BASE_URL`), defaulting to
  `http://crab-shell-proxy:8080`. The proxy cannot infer the name containers reach
  it by.
- **FR-5.3** `tools.mcp.enabled` and `tools.mcp.servers.memory` join
  `ManagedConfigPaths`, so the admin config editor renders them read-only and an
  admin edit cannot survive the next provision.
  `TestManagedConfigPathsMatchWriters` is the gate.
- **FR-5.4** Redaction is extended to mask `tools.mcp.servers.*.headers.*` in the
  document `GET /v1/admin/users/config` returns, and the masked value round-trips
  on write exactly as `model_list` API keys already do — a write carrying the mask
  keeps the on-disk value instead of persisting `"***"`. Without this, every
  member's token is readable by any proxy admin (E-7).

  Masking and restoring are two separate passes, and the failure that matters is
  adding the mask while forgetting the restore. For the `memory` server that
  failure self-heals — the token is deterministic and rewritten on the next ensure
  (FR-5.1) — so it must be verified on a **hand-added sibling** server, whose
  credential the proxy does not regenerate and would therefore destroy permanently.

- **FR-5.5** When the writer actually changes the block, the workspace gets a
  restart notice (`restart.ReasonConfig`). picoclaw reads `config.json` at
  startup, and `mode: "continuous"` agents stay up across chats, so a container
  that is already running does not see the new server until it bounces — every
  existing member is in that state the day this ships.

  A notice, not a forced restart: this follows the established path for a
  proxy-side change the agent only reads at boot, and it does not interrupt a live
  conversation. Because the token is deterministic and the URL is stable
  (FR-4.3, FR-5.2), the block changes exactly once per workspace, so this raises
  one notice per member and never a restart loop.

### FR-6 — Read-only HTTP surface for the user interface

Authorized with the same chain as `GET /v1/memory` (`resolveSecretCaller` +
`authorizeSecret`), so a member reads their own graph and nothing else.

- **FR-6.1** `GET /v1/memory-graph` — `read_graph`, accepting `detail_level`,
  `include_archived`, `include_merged`.
- **FR-6.2** `GET /v1/memory-graph/nodes?names=a,b` — `open_nodes`.
- **FR-6.3** `GET /v1/memory-graph/search?query=…&k=…&threshold=…` — the same
  lexical ranking the `semantic_search` tool uses, so the UI and the bot agree
  about what matches.
- **FR-6.4** `GET /v1/memory-graph/recent?hours=…` — `get_recent_changes`.
- **FR-6.5** No write route exists in v1 (user decision). Not "not documented" —
  not registered.
- **FR-6.6** ~~All four are added to `openapi.json`.~~ **Withdrawn — the premise
  was wrong.** `internal/httpapi/openapi.json` is a curated subset served for
  mycelium's service discovery, not a full description of the surface: it lists
  `/v1/chat/completions`, `/v1/models`, `/v1/restart` and the admin model routes,
  and it does **not** list `/v1/memory`, `/v1/secrets`, `/v1/media`,
  `/v1/sessions/*` or the other member-scoped routes.

  Documenting the memory-graph routes there would make them the only
  member-scoped read routes in the document, which is inconsistent with every
  sibling. They follow the same convention their neighbours do: described in this
  spec, not in the discovery document. `/v1/mcp` stays out for a second reason —
  it is JSON-RPC and the MCP handshake describes itself.

### FR-7 — Storage

- **FR-7.1** One file per workspace at
  `<UserWorkspace>/memory-graph/memory.jsonl`, i.e. `~/.picoclaw/memory-graph/`
  inside the container — inside the member's tree, outside the agent's reachable
  workspace (E-6, D-2).
- **FR-7.2** Format is upstream's JSONL (E-5), so an existing better-memory-mcp
  `memory.json` can be dropped in and read.
- **FR-7.3** Writes are atomic: temp file in the same directory, `fsync`, rename.
  A crash mid-write must not truncate a member's memory.
- **FR-7.4** Read-modify-write is serialised per workspace by an in-process mutex
  keyed on the `WorkspaceKey`. Two concurrent turns must not lose each other's
  writes.
- **FR-7.5** The file is capped (default 4 MiB, `memoryGraphMaxBytes`). A write
  that would exceed the cap fails with a message the agent can act on, rather
  than growing until it degrades every turn's context.
- **FR-7.6** An absent file reads as an empty graph, never an error.

### FR-8 — Deployment wiring

- **FR-8.1** `CRAB_MCP_TOKEN_SECRET` reaches the `crab-shell-proxy` service in all
  three compose files — `docker-compose.yaml`, `docker-compose.prod.yaml` and
  `docker-compose.dokploy.yaml`. Parity across those three is something this repo
  tracks deliberately (see the `chore(deploy): bring prod/dokploy back in line
  with standalone` commit); a secret wired into one of them is a feature that works
  in one environment.
- **FR-8.2** `CRAB_MCP_BASE_URL` is set wherever the proxy is not reachable at the
  default `http://crab-shell-proxy:8080`.
- **FR-8.3** Because the secret is what enables the feature (FR-4.5), a deployment
  that has not set it must still boot, chat, and behave exactly as it does today.
- **FR-8.4** Every member-facing route is declared to the mycelium gateway. A route
  the proxy serves but the gateway does not know about answers
  `400 "Request path does not match any service"` — the proxy is never reached.

  Two blocks per service in each `deploy/*/config*.toml`, because mycelium's `*`
  matches a following segment and not the bare path:
  `/v1/memory-graph` and `/v1/memory-graph/*`. Gated exactly like `/v1/memory`
  (`permission = "write"`, `secretName`, `acceptInsecureRouting`), for all three
  services — `hermes-glm` included, or the drawer 400s on a hermes workspace.

  `/v1/mcp` is deliberately NOT declared: the agent reaches it directly on the
  container network, never through the gateway.

  **This requirement was missing and the gap shipped.** FR-8 covered the compose
  environment and stopped there, so the whole feature looked complete and the
  webapp failed on first use. Adding a proxy route is two changes, not one.

### FR-9 — Provenance: which conversation a fact came out of

- **FR-9.1** `Observation`, `Relation` and `Entity` each carry an optional
  `sourceSessionId` — the **raw** conversation id, not the `sessionKey` hash, because
  it is what the webapp navigates by. `omitempty`, so a graph with no provenance is
  byte-identical to what upstream writes and FR-7.2 still holds. This is the only
  divergence from upstream's record format.

- **FR-9.2** The proxy **correlates**; it is never told. The MCP endpoint is
  stateless, the bearer token carries only the workspace (a per-conversation token is
  impossible — it lives in `config.json` and is deterministic), and asking the agent
  to pass its session id would put a caller-supplied scope input on the one route
  reachable from the container network. Instead `httpapi.turnRegistry` records which
  conversation each workspace is mid-turn on, and the MCP write reads it.

- **FR-9.3** **Attribution happens only when it is unambiguous.** The registry is a
  counted set, and `Current` returns a session only when exactly one is in flight:

  - **zero** — a cron job, the heartbeat or post-turn evolution wrote it;
  - **two or more** — concurrent conversations on one workspace, which nothing
    serializes (two browser tabs).

  Both store no source. A guess would be worse than nothing: the member clicks
  through and reads a conversation that never said it. The count exists because the
  same conversation legitimately has overlapping requests, and only the last one
  ending clears it.

- **FR-9.4** Registration is `defer`red. A leaked entry does not merely lose
  attribution — it mis-attributes every later write for that workspace to a
  conversation that already ended.

- **FR-9.5** **An absent source is a normal state, not a defect**, and the interface
  says which of the reasons applies rather than rendering an empty box. Every fact
  written before this requirement existed also has none.

- **FR-9.6** `sourceSessionId` is **not** stripped from MCP tool results. Hiding
  session ids from the agent would be theatre: it already reads
  `workspace/sessions/` on its own filesystem, which is inside the workspace
  `restrict_to_workspace` allows. Stripping would cost a fragile marshalling layer
  across six read tools for no gain.

- **FR-9.7** `merge_entities` copies `Observation` structs wholesale, so provenance
  survives a merge. Pinned by a test, because a "simplification" that rebuilds the
  observations instead would keep their text and silently lose where they came from.

## Non-functional requirements

- **NFR-1 — `/v1/mcp` is exposed to the container network.** Unlike every other
  route, it does not sit behind mycelium. Consequences that are requirements:
  constant-time MAC comparison (FR-4.2); no scope input from the caller
  (FR-4.2); no token echoed in any response or log line; request body capped
  before parsing.
- **NFR-2 — The token is a credential at rest in `config.json`.** Forced by E-3.
  Mitigations are FR-5.3 (proxy-owned, admin cannot edit), FR-5.4 (redacted from
  the admin read surface), and FR-4.3 (rotation by secret change).
- **NFR-3 — No new runtime dependency in the environment.** No container, no
  Python, no model download, no outbound network call on any tool path.

  Two new *direct* Go dependencies, not one: `modelcontextprotocol/go-sdk` and
  `google/jsonschema-go`. The second is not an independent choice — `mcp.Tool`'s
  `InputSchema` is a `*jsonschema.Schema` from that module, so writing schemas
  explicitly (FR-2, E-4) requires importing it. Five further modules arrive
  transitively (`segmentio/encoding`, `segmentio/asm`, `yosida95/uritemplate/v3`,
  `golang.org/x/oauth2`, `golang.org/x/sys`).

  The SDK also raised this module's Go directive from 1.23 to 1.25 and therefore
  the Dockerfile's builder image from `golang:1.23-bookworm` to
  `golang:1.25-bookworm` (E-10). A 1.23 builder cannot compile the result at all.
- **NFR-4 — Tool-call latency.** Graphs are per-member and small. Loading,
  ranking and rewriting a few hundred entities must stay well inside picoclaw's
  turn budget; BM25 is computed per query from the loaded graph with no index
  (D-3).
- **NFR-5 — Existing behaviour is untouched.** `/v1/memory` and
  `MEMORY_CUSTOM.md` keep their current meaning. The memory graph is a separate
  surface with separate paths, deliberately not unified.

## Out of scope

- Embedding-based semantic search (D-1). The tool name is kept so the
  implementation can be swapped without a contract change.
- UI writes: curation (archive/delete/merge from the interface) and manual entity
  creation. The read surface is designed so adding them later is additive.
- A graph shared across a subscription account. The user chose per-user; a shared
  graph would need its own authorization model and its own deletion path.
- Cross-agent memory: a member's `alpha` graph and `beta` graph are distinct,
  because `Role` is part of the `WorkspaceKey`.
- Graph visualisation in the webapp. FR-6 delivers the API; rendering is a webapp
  feature with its own spec.
- Migration tooling for importing an existing upstream `memory.json`. FR-7.2 makes
  it a file copy, which needs no tool.

## Acceptance

- **AC-1** `picoclaw mcp test memory`, run inside a container the proxy actually
  spawned, against the running proxy, lists all 15 tools. This is the acceptance
  test that matters; a Go test against our own handler proves only that we agree
  with ourselves.
- **AC-2** In a live chat turn, the agent stores an entity and retrieves it on a
  later turn, after a container restart.
- **AC-3** Two members of the same subscription cannot observe each other's
  entities. Verified by calling `/v1/mcp` with member A's token and asserting
  member B's data is absent, and by asserting a forged token fails closed.
- **AC-4** `GET /v1/admin/users/config` shows the MCP block with `headers` masked,
  and re-submitting that masked document leaves the real token on disk.
- **AC-5** An upstream `memory.json` copied into `memory-graph/memory.jsonl` is
  read correctly, including bare-string observations and entities with no
  `createdAt`.
- **AC-6** With `CRAB_MCP_TOKEN_SECRET` unset, `/v1/mcp` is absent and no
  `tools.mcp.servers.memory` block is written.
