# memory-graph-mcp — design

Implements `spec.md`. Read `context.md` for the empirical findings the shape below
depends on.

## Shape

```
picoclaw container                    crab-shell-proxy
┌──────────────────────┐              ┌──────────────────────────────────────┐
│ tools.mcp.servers    │  streamable  │ POST/GET /v1/mcp                     │
│   .memory            │──── HTTP ───▶│   mcp.StreamableHTTPHandler          │
│   type: http         │  Bearer tok  │   getServer(req):                    │
│   url: …/v1/mcp      │              │     mcptoken.Verify → memgraph.Scope │
└──────────────────────┘              │     build *mcp.Server bound to Scope │
                                      │             │                        │
webapp ──── mycelium ────────────────▶│ GET /v1/memory-graph/*   (read-only) │
            (profile hdr)             │             │                        │
                                      │      internal/memgraph.Store         │
                                      └─────────────┼────────────────────────┘
                                                    ▼
                        <userDir>/memory-graph/memory.jsonl   (root-owned 0700)
```

The load-bearing property: `getServer` resolves the scope from the request before
any `*mcp.Server` exists, and each tool handler closes over that scope. A tool
cannot receive a scope from its arguments because no tool has a scope parameter.

## Packages

Three new packages, each with one job, plus edits to two existing ones.

### `internal/memgraph` — the graph, and nothing else

No HTTP, no MCP, no Docker. This is where every behaviour in FR-3 lives and where
almost all the tests go.

```go
type Scope struct { TenantID, SubsAccID, Role, UserAccID string }

type Observation struct {
    Content    string  `json:"content"`
    Timestamp  int64   `json:"timestamp"`             // epoch ms, upstream's unit
    Confidence float64 `json:"confidence,omitempty"`
}

type Entity struct {
    Name         string        `json:"name"`
    EntityType   string        `json:"entityType"`
    Observations []Observation `json:"observations"`
    CreatedAt    int64         `json:"createdAt,omitempty"`
    Archived     bool          `json:"archived,omitempty"`
    Merged       bool          `json:"merged,omitempty"`
    MergedInto   string        `json:"mergedInto,omitempty"`
    MergedAt     int64         `json:"mergedAt,omitempty"`
}

type Relation struct {
    From, To, RelationType string
    CreatedAt              int64 `json:"createdAt,omitempty"`
}

type Graph struct { Entities []Entity; Relations []Relation }
```

Files:

- **`graph.go`** — the types above, plus `normalizeObservations`. Observations
  arrive from disk as either `["a","b"]` or `[{content,timestamp,confidence}]`
  (FR-3.10), so `Observation` carries a custom `UnmarshalJSON` that accepts a bare
  string. That is the whole of upstream compatibility, in one method, tested
  directly.
- **`store.go`** — `Store` owns the file:
  ```go
  func NewStore(containerDataRoot string, now func() time.Time) *Store
  func (s *Store) Load(Scope) (*Graph, error)                        // absent ⇒ empty
  func (s *Store) Update(Scope, func(*Graph) error) error            // lock, load, mutate, save
  ```
  `Update` is the only writer: it takes the per-scope mutex (FR-7.4), loads,
  applies the callback, enforces `memoryGraphMaxBytes` on the *encoded* result
  (FR-7.5), then writes temp→fsync→rename in the same directory (FR-7.3). Every
  mutating operation is a callback, so no operation can forget to lock or to
  write atomically.

  The mutex map is keyed on `Scope` and never evicted — one `sync.Mutex` per
  member per agent is a handful of bytes and eviction would need reference
  counting to stay correct.

  `now` is injected because FR-3.9 (`get_recent_changes`) and FR-3.10 (legacy
  timestamps) are otherwise untestable.
- **`ops.go`** — the 15 operations, one method each, returning upstream's result
  shapes (`{created, skipped}`, `{deleted, cascadedRelations}`, …). The asymmetry
  in FR-3.3 vs FR-3.5 (add-to-missing errors, delete-from-missing does not) is
  encoded here with a comment naming it as upstream's behaviour, so it survives
  the next reader's tidying instinct.
- **`search.go`** — `Rank(g *Graph, query string, k int, threshold float64) []Hit`.
  BM25 (`k1=1.2`, `b=0.75`) over one document per entity, built from name +
  entityType + observation contents. Also `SearchNodes` for the strict substring
  match FR-2's `search_nodes` needs — the two are genuinely different operations
  and share nothing but the graph.
- **`paths.go`** — `filepath.Join(config.UserWorkspace(root, …), "memory-graph")`.
  Deliberately **not** under `workspace/`, which is what keeps it out of the
  agent's reach (E-6).

  Directory mode `0700`, owner root, and **no `chown` to `picoclawUser`** — unlike
  `memory.go`, which chowns because the agent must read `MEMORY_CUSTOM.md`. Here
  the agent must not. The container process runs as uid 1000 and cannot traverse a
  root-owned `0700` directory, so this closes most of D-2: the shell cannot read
  the file either. Only a deployment with `picoclawUser: ""` (containers as root)
  is still exposed.

### `internal/mcptoken` — mint and verify, ~60 lines

```go
func Mint(secret string, s memgraph.Scope) string
func Verify(secret, token string) (memgraph.Scope, bool)
```

`payload = tenantID/subsAccID/role/userAccID`, token =
`b64url(payload) "." b64url(HMAC-SHA256(secret, payload))`. `Verify` splits on the
single `.`, recomputes, compares with `hmac.Equal`, and only then parses the
payload — the MAC is checked before the payload is trusted for anything, including
its own field count.

Separate from `memgraph` because it is authentication, and separate from
`mcpserver` because `internal/docker` needs `Mint` and must not import an HTTP
server. Depends only on `memgraph` (for `Scope`) and stdlib.

### `internal/mcpserver` — MCP wiring

- **`server.go`**
  ```go
  type Deps struct {
      Store  *memgraph.Store
      Secret string
      Logf   func(string, ...any)
  }
  func NewHandler(d Deps) http.Handler
  ```
  Builds `mcp.NewStreamableHTTPHandler(getServer, &mcp.StreamableHTTPOptions{Stateless: true})`.

  `getServer` reads the bearer token, calls `mcptoken.Verify`, and on success
  returns a fresh `*mcp.Server` with the tools bound to that scope. The SDK's
  `getServer` signature returns only `*Server`, so a `nil` return is how a bad
  token is rejected; `NewHandler` wraps the SDK handler in a small
  `http.HandlerFunc` that verifies first and writes `401` itself, so the failure is
  an HTTP status rather than an SDK-internal error (FR-4.4).

  Building a server per request costs 15 `AddTool` calls. With `ServerOptions.SchemaCache`
  set, schema resolution is cached across requests, so this is map insertions —
  acceptable, and it is what makes per-request scope binding safe by construction.
- **`tools.go`** — the 15 registrations. Each is
  `mcp.AddTool[In, Out](srv, &mcp.Tool{Name, Description, InputSchema: schemaX}, handler)`
  with `InputSchema` hand-written to mirror upstream (E-4), so declared defaults
  are applied by the SDK and the wire contract matches upstream's advertised one.
  Handlers are thin: unpack, call one `memgraph` method with the closed-over
  scope, marshal the result.

  A table test asserts the advertised schema of every tool against a golden
  JSON file extracted from upstream's `ListToolsRequestSchema` response, so drift
  from upstream is a test failure rather than a discovery in production.

### `internal/httpapi` — read-only UI surface

- **`memory_graph.go`** — four handlers (FR-6.1–6.4). Each one is the same six
  lines `handleMemoryGet` already is: `resolveSecretCaller`, parse `tenant_id` /
  `subs_acc_id`, `authorizeSecret` → `WorkspaceKey`, convert to `memgraph.Scope`,
  call the store, `writeJSON`. Reusing that exact chain is deliberate: the memory
  graph gets the same visibility rules as `MEMORY_CUSTOM.md`, and a future change
  to the authorization chain applies to both.
- **`handlers.go`** — register `/v1/mcp` (both methods) and the four
  `/v1/memory-graph*` routes. `/v1/mcp` is registered **only** when the secret is
  configured (FR-4.5).
- **`openapi.go` / `openapi.json`** — document the four read routes. `/v1/mcp` is
  not an OpenAPI surface; it is JSON-RPC and the MCP handshake describes itself.

### `internal/docker` — injection, managed paths, redaction

- **`instance_config.go`**
  - `ManagedConfigPaths` gains `tools.mcp.enabled` and
    `tools.mcp.servers.memory` (FR-5.3).
  - Redaction gains `redactMCPHeaders`, masking every value under
    `tools.mcp.servers.*.headers` (FR-5.4). `redactModelKeys` becomes one of two
    passes; the existing mask constant and `holdsMask` round-trip logic are reused
    unchanged, which is what makes "resubmit the masked document" keep the real
    token.
- **`provision.go`** — `alignWorkspace` gains the MCP block. It already
  read-modify-writes `config.json` and already owns
  `agents.defaults.workspace`, so this is one more managed key in a function whose
  contract is exactly "rewrite what the proxy owns". It sets only
  `tools.mcp.servers.memory`, preserving sibling servers (FR-5.1), and skips the
  whole block when the secret is unset (FR-4.5).

  `alignWorkspace` currently takes `(configPath, home)`; it grows the scope, the
  secret and the base URL. Its callers already have all three.
- **`config.go`** (in `internal/config`) — `mcpBaseURL` (`CRAB_MCP_BASE_URL`,
  default `http://crab-shell-proxy:8080`) and `mcpTokenSecret`
  (`{env: CRAB_MCP_TOKEN_SECRET}`, following `webhookSecret`'s existing shape).

## Data flow, one tool call

1. Agent decides to call `mcp_memory_create_entities`.
2. picoclaw POSTs JSON-RPC to `…/v1/mcp` with the bearer token from its
   `config.json`.
3. `NewHandler`'s wrapper verifies the MAC; `401` and stop if it fails.
4. `getServer` builds a `*mcp.Server` whose tools close over the decoded
   `memgraph.Scope`.
5. The SDK validates arguments against the explicit schema and applies declared
   defaults.
6. The handler calls `store.Update(scope, fn)`: per-scope lock → load JSONL →
   mutate → size check → temp/fsync/rename.
7. Result marshals back as the tool result; stateless mode closes the session.

## Testing

| Layer | What is tested | Where |
|---|---|---|
| `memgraph` | All of FR-3, one test per operation, including the FR-3.3/FR-3.5 asymmetry, merge's relation redirect + dedupe, and read_graph's three detail levels | `memgraph/ops_test.go` |
| `memgraph` | Upstream JSONL round-trip; bare-string observations; missing `createdAt`; a real upstream `memory.json` fixture (AC-5) | `memgraph/graph_test.go` |
| `memgraph` | Atomic write survives a failed callback; size cap; concurrent `Update` on one scope loses nothing | `memgraph/store_test.go` |
| `memgraph` | BM25 ordering and `threshold`/`k` behaviour | `memgraph/search_test.go` |
| `mcptoken` | Round-trip; forged MAC; wrong secret; malformed shapes; a payload whose field count is wrong is rejected *after* the MAC check | `mcptoken/token_test.go` |
| `mcpserver` | Advertised schemas match the upstream golden file; `401` on bad token; a valid token for member A never returns member B's data (AC-3) | `mcpserver/server_test.go` |
| `docker` | `TestManagedConfigPathsMatchWriters` (FR-5.3); headers masked and mask round-trips (AC-4); sibling MCP servers preserved; no block written without a secret (AC-6) | `docker/instance_config_test.go`, `provision_test.go` |
| `httpapi` | The four read routes' authorization, following `handleMemoryGet`'s existing tests | `httpapi/memory_graph_test.go` |
| End to end | `picoclaw mcp test memory` inside a spawned container (AC-1); store-then-recall across a restart (AC-2) | manual, recorded in the execution report |

The end-to-end check is the one that can actually falsify the design. Go tests
against our own handler only prove internal consistency; AC-1 proves picoclaw's
client and our server agree.

## Rejected alternatives

- **bbolt per workspace.** `internal/registry` uses bbolt because it is global and
  long-lived. Per-member graphs are small and numerous, so bbolt would mean an
  open-handle cache with close-on-idle — real machinery for no benefit, and it
  would give up upstream's file format.
- **One global bbolt DB.** Needs a member-deletion path that does not exist today
  (there is no wholesale user-tree delete, only per-file). Storing in the member's
  tree inherits deletion for free.
- **Storing the token hashed, mint per provision.** Every provision would write a
  new token, so `config.json` would change on every chat, defeating drift
  detection. The HMAC construction is stateless and deterministic instead.
- **stdio MCP server.** Would need a binary inside `sipeed/picoclaw`, which we do
  not build.
- **`deferred: true` tool registration.** Saves per-turn tokens but makes memory
  conditional on the agent thinking to search for it.
