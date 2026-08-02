# memory-graph-mcp — context

User decisions and empirically-established facts. Everything in "Established
empirically" was measured against the real artifacts, not inferred from docs or
from a summary of docs — the distinction matters because three of these findings
contradict what the upstream README implies.

## User decisions

| Decision | Choice | Consequence |
|---|---|---|
| Graph isolation scope | **Per user** — one graph per `WorkspaceKey{TenantID, SubsAccID, Role, UserAccID}` | Mirrors `MEMORY_CUSTOM.md`, media and secrets. The graph lives inside the member's data tree, so it dies with the tree — no new deletion path to write. No cross-member visibility. |
| `semantic_search` backing | **Lexical (BM25) ranker, keeping the tool name and parameters** | No Python, no ~500 MB model, no extra container, no external embedding API. This is upstream's own documented degraded mode, made permanent. Documented deviation (see D-1). |
| UI write access | **Read-only in v1** | The bot writes via MCP; the UI reads. One read gate, reusing `/v1/memory`'s authorization chain. No admin curation surface in v1. |

## Established empirically

### E-1 — picoclaw accepts a remote HTTP MCP server; this is its exact config shape

Ran the real CLI inside the real image:

```
docker run --rm --entrypoint sh -e HOME=/tmp/h -v <dir>:/tmp/h sipeed/picoclaw:latest \
  -c 'picoclaw mcp add memory --transport http http://crab-shell-proxy:8080/v1/mcp \
      --header "Authorization: Bearer xyz"'
```

The written `config.json` holds:

```json
"tools": { "mcp": {
  "enabled": true,
  "servers": { "memory": {
    "enabled": true,
    "command": "",
    "type": "http",
    "url": "http://crab-shell-proxy:8080/v1/mcp",
    "headers": { "Authorization": "Bearer xyz" }
  } } } }
```

Three things this settles:

- `headers` is a **JSON object**, not the array of `"Name: Value"` strings the CLI
  flag syntax suggests.
- `command` is emitted as `""` even for an HTTP server.
- `tools.mcp.enabled` flips to `true` on its own — the proxy does not have to set
  it separately, but it must not set it back to `false`.

### E-2 — picoclaw's MCP client is the official Go SDK

`strings` on the shipped binary shows
`github.com/modelcontextprotocol/go-sdk@v1.6.1`, and specifically
`mcp.StreamableClientTransport` / `mcp.streamableClientConn`. Supported protocol
version strings present in the binary: `2025-11-25`, `2025-06-18`, `2025-03-26`,
`2024-11-05`.

So `type: "http"` means **streamable HTTP**, and the proxy — also Go — can use the
*server* half of the same SDK. Protocol compatibility by construction rather than
by hand-rolling a JSON-RPC-over-HTTP endpoint and hoping.

### E-3 — there is NO env indirection for the MCP token

`tools.mcp.servers` is a map, and picoclaw's env-override surface is an
enumerated per-field list. Every `PICOCLAW_TOOLS_*` key in the binary was dumped;
the MCP-related ones are exactly:

```
PICOCLAW_TOOLS_MCP_                        PICOCLAW_TOOLS_DISCOVERY_ENABLED
PICOCLAW_TOOLS_MCP_MAX_INLINE_TEXT_CHARS   PICOCLAW_TOOLS_DISCOVERY_TTL
                                           PICOCLAW_TOOLS_DISCOVERY_USE_BM
                                           PICOCLAW_TOOLS_DISCOVERY_USE_REGEX
```

No `PICOCLAW_TOOLS_MCP_SERVERS_*`. `env_file` is a stdio-only field.

**Therefore the bearer token must sit in plaintext in `config.json`.** That is not
a preference; it is the only place picoclaw will read it from. Everything in FR-4
and NFR-2 follows from this constraint.

### E-4 — the Go SDK honours an explicitly-set InputSchema, and applies its defaults

In `go-sdk`, `setSchema` infers a schema from the Go type **only** when the field
is nil (`if *sfield == nil`), and `applySchema` fills in schema `default` values
before unmarshalling into the typed input struct.

So supplying a hand-written `*jsonschema.Schema` per tool gives byte-level
fidelity to upstream's declared parameters *and* their defaults, while handlers
still receive typed Go structs. This is why FR-2 can promise the same schemas
rather than merely the same tool names.

Relevant API: `mcp.NewServer(*Implementation, *ServerOptions)`,
`mcp.AddTool[In, Out](*Server, *Tool, ToolHandlerFor[In, Out])`,
`mcp.NewStreamableHTTPHandler(getServer func(*http.Request) *Server, *StreamableHTTPOptions)`
with `StreamableHTTPOptions.Stateless bool`.

`getServer` receiving the `*http.Request` is the load-bearing detail: the
workspace is resolved from the request **before any tool exists**, so no tool can
be reached without a resolved scope.

### E-5 — upstream stores JSONL, not JSON

Despite `MEMORY_FILE_PATH` defaulting to `memory.json`, `saveGraph` writes one
JSON object per line:

```
{"type":"entity","name":...,"entityType":...,"observations":[...],"createdAt":...}
{"type":"relation","from":...,"to":...,"relationType":...,"createdAt":...}
```

`loadGraph` splits on `\n`, skips blank lines, and dispatches on `item.type`.
Matching this exactly makes import/export from an existing better-memory-mcp
deployment a file copy.

### E-6 — the whole per-user dir is mounted, but the agent's file tools are confined to `workspace/`

`Manager.create` binds `hostDir : <picoclawHome>/.picoclaw`, where `hostDir` is
`config.UserWorkspace(...)`. The agent's own workspace is
`~/.picoclaw/workspace`, with `restrict_to_workspace: true` and
`allow_read_outside_workspace: false`.

So a path at `~/.picoclaw/memory-graph/` is inside the member's tree (deleted with
it) but outside the agent's reachable workspace. See D-2 for what this does and
does not protect against.

### E-9 — picoclaw's real client works against a stateless handler, and it issues POST + DELETE (never GET)

The one transport assumption everything else rests on, tested rather than reasoned
about. A throwaway Go server was built against `go-sdk v1.6.1`'s
`NewStreamableHTTPHandler(getServer, &StreamableHTTPOptions{Stateless: true})`
with a bearer check in front, and the real `sipeed/picoclaw:latest` image was
pointed at it over `--network host`:

```
Connected to MCP server protocol=2025-11-25 server=memory
    serverName=crab-memory-graph serverVersion=0.1.0
Listed tools from MCP server server=memory toolCount=1
✓ MCP server "memory" reachable (1 tools).
```

`picoclaw mcp show memory` rendered the tool's name, description, and the
parameter's type/required flag/description — so a schema derived from a Go struct
with `jsonschema:"…"` tags reaches the agent intact.

Two findings:

- **Stateless mode is fine.** The concern was that `Stateless: true` changes GET
  handling while the client has a `connectStandaloneSSE` path. It never opens one.
- **The client uses `POST` and `DELETE`, never `GET`.** The server log for one
  `mcp test` run:

  ```
  POST /v1/mcp   DELETE /v1/mcp   POST /v1/mcp   POST /v1/mcp   POST /v1/mcp   DELETE /v1/mcp
  ```

  `DELETE` is session termination. FR-1.1 originally said "POST and GET"; routing
  only those would have left every connection unable to close cleanly. The route
  must accept all three.

The rejection path was verified too — a wrong bearer token yields:

```
Error: failed to reach MCP server "memory": failed to connect: calling
"initialize": sending "initialize": Unauthorized
```

so a `401` from the wrapper surfaces as a clean, diagnosable client-side failure
rather than a hang or a partially-initialised session.

### E-10 — the SDK forces this module from Go 1.23 to 1.25

`github.com/modelcontextprotocol/go-sdk v1.6.1` declares `go >= 1.25.0`, so adding
it raised this module's own directive to `go 1.25.0`. The Dockerfile pinned
`golang:1.23-bookworm`, which cannot compile the result at all — bumped to
`golang:1.25-bookworm`. Nothing in CI pins a Go version, so there was nothing else
to change.

### E-7 — `redactModelKeys` covers `model_list` only

`internal/docker/instance_config.go`'s redaction walks `model_list` entries and
masks API keys. Nothing else. `tools.mcp.servers.*.headers` is not covered, and
`GET /v1/admin/users/config` returns the redacted document to admins.

Adding the MCP block without extending redaction would publish every member's
memory-graph bearer token to any proxy admin. `TestManagedConfigPathsMatchWriters`
catches an unlisted *managed path*; nothing existing catches an unredacted
credential. That gap is why FR-4 carries its own test.

### E-8 — upstream's `search_nodes` default disagrees with itself

The advertised `inputSchema` says `maxObservations` defaults to `5`; the
implementation signature is `searchNodes(query, maxObservations = 3)`. We follow
the **schema** (5), because that is the contract the model reads and the one an
MCP client can observe. Noted so a future reader does not "fix" it toward 3.

## Deviations from upstream

### D-1 — `semantic_search` is lexical

Same tool name, same parameters (`query`, `k`, `threshold`), same response shape.
Ranking is BM25 over entity name, entity type and observation contents instead of
ModernColBERT embeddings. Upstream falls back to keyword search whenever
Python/model are unavailable and reports `searchType: "keyword"`; we always
report `"lexical"` so the distinction is visible in the response rather than
implied.

The tool description must not claim embeddings. A description promising semantic
understanding that the implementation cannot deliver is a prompt-level lie that
degrades the agent's tool choice.

### D-2 — the graph file is reachable from the container shell only if containers run as root

`tools.exec` is enabled, so a shell inside the container reaches paths the agent's
file tools cannot — the exposure `config.json` and `.security.yml` already have at
that level.

For the graph this is closed by ownership rather than by path. `memory.go` chowns
the memory dir to `picoclawUser` because the agent must *read*
`MEMORY_CUSTOM.md`; the memory-graph dir is written only by the proxy, so it stays
root-owned at `0700`. Containers run as `picoclawUser` (`1000:1000` by default) and
cannot traverse it — not via file tools, not via a shell.

**This claim was false when first written, and required a code change to make
true.** `internal/memgraph` chowns nothing (there is a source gate for that), but
`chownTree` is a `filepath.Walk`, and `resolveAndMaterialize` calls
`chownTree(userDir, picoclawUser)` on **every** ensure — so the graph directory
would have been handed to the agent on the second chat, with all tests still green.
`chownTree` now skips `GraphDirName`, gated by
`TestChownTreeSkipsTheMemoryGraphDirectory` and by
`TestGraphDirNameMatchesMemgraph` (the constant is duplicated across the two
packages, so it needs its own anti-drift check).

Residual exposure: a deployment that sets `picoclawUser: ""` runs containers as
root, and then the shell can read the file. Accepted for that configuration, which
is already the one where every other per-user credential is readable too.

### D-3 — no semantic-search indexing lifecycle

Upstream starts a background bridge, indexes on startup and re-indexes on write.
A BM25 ranker over a few hundred entities is computed per query from the loaded
graph, so there is no index to build, warm, invalidate or corrupt.
