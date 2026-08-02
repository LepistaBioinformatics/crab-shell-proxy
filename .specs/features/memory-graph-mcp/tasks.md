# memory-graph-mcp — Tasks (proxy)

**Spec**: `.specs/features/memory-graph-mcp/spec.md`
**Design**: `.specs/features/memory-graph-mcp/design.md`
**Context**: `.specs/features/memory-graph-mcp/context.md`
**Status**: T1–T11 + T13 done · T12 partial (automated gate green; four live-stack acceptance criteria not executed — see `progress.md`)

## Progress

| Task | Status | Tests | Note |
| --- | --- | --- | --- |
| T1 | ✅ Done | 10 funcs | Storage is JSONL with **no trailing newline** (upstream `join("\n")`). `Observation.UnmarshalJSON` decides bare-string-vs-object **per element**, where upstream decides per array by inspecting only `obs[0]` — a superset, and it also fixes a half-migrated array upstream would leave raw. An unexported `legacy` flag is what lets normalisation stamp a converted string without also overwriting a stored `confidence: 0`. |
| T2 | ✅ Done | 13 funcs | `Load` takes **no lock** — writes land by rename, so a reader sees old-or-new, never torn, and the UI never waits behind an agent's write. The per-scope lock was **mutation-checked**: removing it drops `TestConcurrentUpdatesOnOneScopeAllLand` from 24 entities to 1. `TestPackageNeverChownsTheGraph` is a source gate — the dir stays root-owned 0700, which is what makes D-2's stronger claim possible — though see the review findings below: `chownTree` had to learn to skip the directory before that claim was actually true. |
| T3 | ✅ Done | 24 funcs | **Upstream fidelity bug caught by a test**: `add_observations` indexes `confidence` by position among the observations actually ADDED, not by position in `contents` (upstream filters duplicates first, then maps). Implementation was written the intuitive way and the test failed; the port now matches upstream, with a comment saying the contract is debatable. `errNoChange` aborts `Update` without writing for the `{success:false}` refusals, so a refused archive/merge provably touches no bytes. |
| T4 | ✅ Done | 14 funcs | `threshold` is applied to a score **normalised against the best hit of the same query**, because raw BM25 is unbounded and upstream documents threshold as a 0–1 similarity. IDF uses the `log(1 + …)` form so a term present in every entity scores ~0 instead of going negative. One test assumption was wrong, not the code: two entities holding the query term once each in similar-length documents genuinely tie at 1.0, so `threshold: 1.0` keeps both — the test now asserts tie-keeping plus a strict-winner case. |
| T5 | ✅ Done | 11 funcs | `Mint` **rejects** a scope field containing `/` and returns an error, because the joined payload is otherwise not injective — two legitimately minted tokens would collide and one member's would authorise another's scope. A source gate gates `hmac.Equal` (the failure is timing-only and invisible to a behavioural test). |
| T6 | ✅ Done | 4 funcs | `mcpTokenSecret` deliberately does NOT propagate `resolve()`'s error the way `webhookSecret` does: unset means the feature is off, not that the proxy fails to boot. |
| T7 | ✅ Done | 12 funcs (with T8) | **Design changed from the plan**: ONE shared `*mcp.Server` built once, not one per request with the scope in a closure. `RequestExtra.Header` gives each tool handler the calling request's own headers, so the scope authorising a call is always that call's, never inherited session state — and 15 schemas are not re-resolved per request. Deps also gained no Docker dependency. |
| T8 | ✅ Done | (see T7) | Golden-file comparison runs against the JSON **a client actually received**, not the server-side Go value. The first version type-asserted `*jsonschema.Schema` and failed: the client decodes into a generic map. Asserting the wire form is the stronger test. `semantic_search`'s description is gated against `colbert`/`embedding`/`neural`/`vector`. |
| T9 | ✅ Done | 17 funcs (with T10) | `TestManagedConfigPathsMatchWriters` failed exactly as predicted and had to be taught about the new writer. Then it failed a second time on something real: its synthetic seed had no `tools.mcp` container, so creating one read as touching an unmanaged path — fixed by making the seed match the shape a real picoclaw `config.json` always has. Sibling-server mask round-trip asserted (the case that destroys a credential permanently). |
| T10 | ✅ Done | (see T9) | **NOT `alignWorkspace`** — see the note in the task. Writes on every ensure, `changed` gates the restart notice, and a returning member (config.json already present, `provision` short-circuits) is asserted directly. |
| T11 | ✅ Done | 10 funcs | Authorization asserted **against `/v1/memory`'s own answer** for the same caller rather than a guessed status, so the two surfaces cannot drift. FR-6.6 withdrawn: `openapi.json` is a curated discovery subset that omits every member-scoped route. `main.go` wires the store — without that the whole feature was inert in the real binary. |
| T13 | ✅ Done | manual | **Two files, not three.** `docker-compose.prod.yaml` is an overlay and inherits `environment`; rendering `-f base -f prod` confirms both variables arrive. `docker compose config` parses for base and dokploy. |
| T12 | 🟡 Partial | 120 new funcs, full gate green | Automated gate passes with **no new failure** beyond the 9 pre-existing `internal/docker` `lchown` failures; `-race` clean on all three new packages. **AC-1, AC-2, AC-4, AC-6 were NOT executed** — they need a live `docker compose` stack with real secrets. AC-3 and AC-5 are covered. See `progress.md` for exactly what is and is not proven. |

**Test totals**: `memgraph` 61 · `docker/mcp_config_test.go` 20 · `mcpserver` 14 ·
`mcptoken` 11 · `httpapi/memory_graph_test.go` 10 · `config` 4 = **120**.

**Three defects were found by review AFTER the gate was green**, all invisible to
the tests, two of them falsifying a written claim: `chownTree` was handing the graph
directory to the agent on every ensure (D-2 was false); NFR-1's request-body cap was
specified and not implemented (mutation-checked: without it a 1 MiB+ body returns
200); and `reapplyWorkspace` did not restore the MCP block, so `ManagedConfigPaths`'
"an admin edit cannot survive" did not hold for it. All three fixed with tests. See
`progress.md`.

**Two review findings changed the plan, recorded rather than silently absorbed:**

1. **`alignWorkspace` was the wrong hook** and FR-5.1 said so. It runs only on a
   first-ever seed, so the MCP block would never reach an existing member —
   a permanent gap, not a delayed one. T10 now writes on every ensure and raises a
   restart notice (FR-5.5), because `mode: "continuous"` containers read
   `config.json` at boot and stay up across chats.
2. **The token payload was not injective.** `/`-joining four fields lets
   `role="a/b", user="c"` collide with `role="a", user="b/c"`, which the MAC cannot
   catch. T5 now rejects the delimiter at `Mint`.

---

## Test matrix and gates (derived, not assumed)

`.specs/codebase/TESTING.md` does not exist. The matrix follows the convention
every sibling feature uses: Go unit tests in the same package as the code,
table-driven, one test per requirement.

| Code layer | Required tests | Parallel-safe |
| --- | --- | --- |
| `internal/memgraph` (pure + FS) | unit, `t.TempDir()` per test | yes |
| `internal/mcptoken` (pure) | unit, table-driven | yes |
| `internal/mcpserver` (HTTP + MCP) | unit via `httptest` + the SDK's own client | yes |
| `internal/docker` | unit; pass `PicoclawUser: ""` so `chownTree` no-ops as non-root | yes |
| `internal/httpapi` | unit via `s.Handler().ServeHTTP` | yes |

| Gate | Command |
| --- | --- |
| quick | `go build ./... && go test ./internal/<pkg>/... -run <TaskPattern>` |
| full | `go test ./...` |

**Baseline — measured on this HEAD, not assumed.** `go build ./...` is clean.
`go test ./...` shows `internal/docker` FAIL with **exactly 9** failures, every one
an `lchown … operation not permitted` that needs root:

```
TestContinuousDoesNotArmIdle              TestEnsureRunningSingleFlight
TestCreateAddsReadOnlySecretsBind         TestReconcileEnsuresContinuousWorkspaces
TestEnsureRunningColdStart                TestRestartWorkspaceRestartsAndRearms
TestEnsureRunningRecreatesOnPersonaDrift  TestScaleToZeroIdleStop
TestEnsureRunningReusesRunning
```

Every other package is `ok`. **"Full gate passes" means no new failure beyond
those 9.** New tests must pass as non-root — pass `PicoclawUser: ""` the way
`TestApplyNativeSecretsPreservesSiblings` does.

**Pre-existing, do not fix here**: `gofmt -l` flags
`internal/authz/authz_test.go` and `internal/registry/registry.go`.

---

## Dependency graph

```
Phase 1 (domain, no deps on anything new)
  T1 ──→ T2 ──→ T3 [P]
   │      └────→ T4 [P]
   └──→ T5 [P]
  T6 [P]

Phase 2 (wiring, needs domain + config)
  T2,T5,T6 ──→ T7 ──→ T8
  T5,T6 ──────→ T9 [P] ──→ T10
  T2,T3,T4,T6 ─→ T11 [P]

Phase 3 (close-out)
  T8,T10,T11 ──→ T12
```

---

## Task Breakdown

### T1: Graph types and upstream JSONL codec

**What**: `Scope`, `Observation`, `Entity`, `Relation`, `Graph`;
`Observation.UnmarshalJSON` accepting a bare string; `normalizeObservations`;
`encodeJSONL` / `decodeJSONL`.
**Where**: `internal/memgraph/graph.go` (new), `internal/memgraph/graph_test.go` (new)
**Depends on**: None
**Reuses**: Nothing — this package has no dependencies beyond stdlib.
**Requirement**: FR-3.10, FR-7.2, FR-7.6, AC-5

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] `decodeJSONL` reads upstream's format: one JSON object per line, dispatched
      on `"type": "entity"` / `"type": "relation"`; blank lines skipped; an
      unknown `type` is ignored, not an error (upstream's `reduce` ignores it)
- [ ] `encodeJSONL` emits all entities first, then all relations, each with the
      `type` discriminator injected — byte-comparable against an upstream fixture
- [ ] `Observation.UnmarshalJSON` accepts `"some text"` **and**
      `{"content":…,"timestamp":…,"confidence":…}`
- [ ] A bare-string observation normalizes to
      `{content, timestamp: now, confidence: 1.0}`
- [ ] An entity with no `createdAt` gets `now`; one with a `createdAt` keeps it
- [ ] Round-trip of a real upstream `memory.json` fixture (checked in under
      `testdata/upstream-memory.jsonl`) preserves every field
- [ ] `Graph` zero value is a usable empty graph
- [ ] Gate: `go build ./... && go test ./internal/memgraph/... -run 'Graph|Observation|JSONL'`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(memgraph): add knowledge-graph types and upstream JSONL codec`

---

### T2: Store — paths, per-scope locking, atomic write, size cap

**What**: `Store`, `NewStore(containerDataRoot string, now func() time.Time)`,
`Load(Scope)`, `Update(Scope, func(*Graph) error)`, `graphDir`/`graphPath`,
`memoryGraphMaxBytes`.
**Where**: `internal/memgraph/store.go` (new), `internal/memgraph/paths.go` (new),
`internal/memgraph/store_test.go` (new)
**Depends on**: T1
**Reuses**: `config.UserWorkspace` (`internal/config/config.go:456`); the
temp+rename shape and `0700` mode conventions from `internal/docker/memory.go`
**Requirement**: FR-7.1, FR-7.3, FR-7.4, FR-7.5, FR-7.6

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] `graphDir` is `filepath.Join(config.UserWorkspace(root, …), "memory-graph")`
      — **not** under `workspace/`; a test asserts the path does not contain
      `/workspace/` (this is what keeps it out of the agent's reach, E-6)
- [ ] The directory is created `0700` and is **never** chowned to `picoclawUser`
      — grep gate: no `chown`/`Lchown` in this package (D-2)
- [ ] `Load` on an absent file returns an empty graph and no error
- [ ] `Update` takes a per-`Scope` mutex; two concurrent `Update`s on one scope
      both land (run with `-race`, assert both mutations present)
- [ ] Two concurrent `Update`s on **different** scopes do not serialise against
      each other
- [ ] A callback returning an error leaves the on-disk file byte-identical
- [ ] A result exceeding `memoryGraphMaxBytes` fails with a distinct error and
      leaves the file unchanged
- [ ] The write is temp-file-in-same-dir → `fsync` → `rename`; a test asserts no
      stray temp file survives a successful write **or** a failed one
- [ ] `now` is injectable and used for every timestamp
- [ ] Gate: `go build ./... && go test -race ./internal/memgraph/... -run 'Store|Path|Update|Load'`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(memgraph): add per-workspace store with atomic writes and size cap`

---

### T3: The 15 graph operations [P]

**What**: One method per upstream tool, returning upstream's result shapes.
**Where**: `internal/memgraph/ops.go` (new), `internal/memgraph/ops_test.go` (new)
**Depends on**: T2
**Reuses**: `Store.Update` / `Store.Load` (T2)
**Requirement**: FR-3.1 – FR-3.9

**Tools**: MCP: NONE · Skill: NONE

**Done when** — one test per bullet, each named for the requirement:

- [ ] `CreateEntities` skips existing names, returns `{created[], skipped[]}`,
      never overwrites an existing entity's observations (FR-3.1)
- [ ] `CreateRelations` skips exact `(from,to,relationType)` duplicates, returns
      `{created, skipped}` counts (FR-3.2)
- [ ] `AddObservations` **errors** on a missing entity; dedupes against existing
      contents; defaults each `confidence` to `1.0`; a shorter `confidence[]` than
      `contents[]` defaults the remainder (FR-3.3)
- [ ] `DeleteEntities` cascades relations on **either** endpoint, returns
      `{deleted, cascadedRelations}` (FR-3.4)
- [ ] `DeleteObservations` on a missing entity returns `deleted: 0` and **no
      error** — the asymmetry with `AddObservations` is asserted explicitly and
      commented as upstream's behaviour, not a bug to tidy (FR-3.5)
- [ ] `DeleteRelations` removes only exact triple matches, reports `{deleted}`,
      and a triple naming no existing relation is not an error (FR-3.11)
- [ ] `ReadGraph` excludes archived and merged by default; `includeArchived` /
      `includeMerged` include them; relations are filtered to those with **both**
      endpoints surviving (FR-3.6)
- [ ] `ReadGraph` detail levels: `minimal` (name/type/observationCount),
      `summary` (adds `firstObservation`, `relationCount`, graph-level
      `totalObservations`), `full` (everything) (FR-3.6)
- [ ] `ReadGraph` with `entityNames` returns full detail and **ignores**
      `detailLevel` (FR-3.6)
- [ ] `GetEntityDetails` and `OpenNodes` return the named entities **including**
      archived and merged ones; `OpenNodes` filters relations to both-endpoints-present
      (FR-3.12 — asking by name is how you inspect something you archived)
- [ ] `SearchNodes` semantics per FR-3.13 (implemented in T4, asserted there)
- [ ] `MergeEntities`: non-duplicate observations copied; every relation endpoint
      redirected source→target; relations that collide after redirect deduped;
      source retained with `merged`/`mergedInto`/`mergedAt`; missing source or
      target and an already-merged source each return `{success:false, message}`
      (FR-3.7)
- [ ] `ArchiveEntity` / `UnarchiveEntity` return `{success:false, message}` — not
      an error — for a missing entity and for a no-op transition (FR-3.8)
- [ ] `GetRecentChanges` filters entities and relations by `createdAt`, and per
      surviving entity returns **only** observations whose own timestamp is in
      window; archived and merged excluded (FR-3.9)
- [ ] Gate: `go build ./... && go test ./internal/memgraph/... -run 'Create|Add|Delete|Read|Merge|Archive|Recent|EntityDetails'`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(memgraph): port better-memory-mcp graph operations`

---

### T4: Lexical search — strict match and BM25 [P]

**What**: `SearchNodes(g, query, maxObservations)` (substring) and
`Rank(g, query, k, threshold)` (BM25, `k1=1.2`, `b=0.75`).
**Where**: `internal/memgraph/search.go` (new), `internal/memgraph/search_test.go` (new)
**Depends on**: T2
**Reuses**: `Graph` (T1)
**Requirement**: FR-2 (`search_nodes`, `semantic_search`), D-1, NFR-4

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] `SearchNodes` matches case-insensitively on entity name, entityType **and**
      observation contents; excludes archived and merged; truncates each entity's
      observations to `maxObservations`; filters relations to both-endpoints-present
- [ ] `Rank` builds one document per entity from name + entityType + observation
      contents; excludes archived and merged
- [ ] `Rank` orders by descending score, applies `threshold` before `k`, and
      returns at most `k`
- [ ] A term appearing in every entity contributes ~nothing (IDF sanity), and a
      rare term dominates — asserted on ordering, not on absolute scores
- [ ] Empty graph and empty query each return no hits, no panic
- [ ] `Rank` does not mutate the graph
- [ ] Gate: `go build ./... && go test ./internal/memgraph/... -run 'Search|Rank|BM25'`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(memgraph): add strict and BM25 lexical search`

---

### T5: Self-authenticating workspace token [P]

**What**: `Mint(secret string, s memgraph.Scope) string`,
`Verify(secret, token string) (memgraph.Scope, bool)`.
**Where**: `internal/mcptoken/token.go` (new), `internal/mcptoken/token_test.go` (new)
**Depends on**: T1 (for `Scope`)
**Reuses**: stdlib `crypto/hmac`, `crypto/sha256`, `encoding/base64`
**Requirement**: FR-4.1, FR-4.2, FR-4.3, FR-4.4

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] `Mint` is deterministic: same secret + same scope ⇒ identical token across
      calls and across processes (FR-4.3)
- [ ] `Verify` recomputes with `hmac.Equal` — grep gate: no `==` or
      `bytes.Equal` on the MAC
- [ ] The MAC is verified **before** the payload is parsed; a token whose payload
      has the wrong field count but a valid MAC and one with a well-formed payload
      but a bad MAC are both rejected, and a test asserts the ordering by using a
      payload that would panic a naive parser
- [ ] Rejected: `""`, no `.`, two `.`, non-base64 halves, valid token under a
      different secret, and a token whose payload was altered
- [ ] `Mint` **rejects** a scope field containing the `/` delimiter, and `Verify`
      requires exactly four fields. This is not paranoia about forgery: the
      `/`-joined payload is not injective, so `role="a/b", user="c"` and
      `role="a", user="b/c"` collide and one member's valid token authenticates as
      another's scope. A test asserts the collision is impossible rather than
      merely unlikely (FR-4.2)
- [ ] `Mint` returning an error is threaded to its caller — a workspace whose scope
      cannot be encoded gets **no** MCP block, not a block with a broken token
- [ ] `Verify` never returns a partially-populated `Scope` alongside `false`
- [ ] Gate: `go build ./... && go test ./internal/mcptoken/...`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(mcptoken): add HMAC workspace tokens for the native MCP endpoint`

---

### T6: Config — MCP base URL and token secret [P]

**What**: `mcpBaseURL` (`CRAB_MCP_BASE_URL`, default
`http://crab-shell-proxy:8080`) and `mcpTokenSecret` (`{env: CRAB_MCP_TOKEN_SECRET}`).
**Where**: `internal/config/config.go`, `internal/config/config_test.go`, `config.yaml`
**Depends on**: None
**Reuses**: `webhookSecret`'s existing `{env: …}` decoding shape
**Requirement**: FR-4.5, FR-5.2

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] `mcpTokenSecret` uses the same env-indirection type `webhookSecret` uses —
      the secret never appears literally in `config.yaml`
- [ ] `mcpBaseURL` defaults to `http://crab-shell-proxy:8080` when neither the
      file nor `CRAB_MCP_BASE_URL` sets it; env wins over file
- [ ] An unset `CRAB_MCP_TOKEN_SECRET` yields an empty secret and **no** load
      error — a deployment without memory must still boot (FR-4.5)
- [ ] `config.yaml` documents both fields in the style of the surrounding comments
- [ ] Gate: `go build ./... && go test ./internal/config/...`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(config): add mcpBaseURL and mcpTokenSecret settings`

---

### T7: MCP streamable-HTTP handler with per-request scope binding

**What**: `mcpserver.Deps`, `NewHandler(Deps) http.Handler`; the `401` wrapper and
`getServer`.
**Where**: `internal/mcpserver/server.go` (new), `internal/mcpserver/server_test.go` (new); `go.mod`
**Depends on**: T2, T5, T6
**Reuses**: `github.com/modelcontextprotocol/go-sdk/mcp` **pinned v1.6.1** (E-2)
**Requirement**: FR-1.1, FR-1.2, FR-1.3, FR-4.2, FR-4.4, NFR-1

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] `go.mod` requires `github.com/modelcontextprotocol/go-sdk v1.6.1` — the
      version picoclaw's client ships; a comment says why the versions are tied
- [ ] The handler is `mcp.NewStreamableHTTPHandler(getServer, &mcp.StreamableHTTPOptions{Stateless: true})`
- [ ] A request with no `Authorization`, a malformed one, or a bad MAC gets `401`
      with **no** detail in the body and the token absent from every log line
      (asserted by capturing `Logf`)
- [ ] `getServer` is never reached with an unverified token
- [ ] The tool set is bound to the scope decoded from the token; **no tool schema
      contains a tenant/subscription/role/user parameter** — asserted by walking
      every advertised schema for those property names (FR-4.2)
- [ ] Member A's token cannot read member B's entities: seed two scopes, call with
      A's token, assert B's data absent (AC-3)
- [ ] `ServerOptions.SchemaCache` is set, so per-request server construction does
      not re-resolve 15 schemas
- [ ] An MCP `initialize` + `tools/list` round-trip succeeds against the handler
      using the SDK's **own client** over `httptest` — not a hand-built request
- [x] **Smoke-tested ahead of the task** — the real picoclaw client connects to a
      stateless `NewStreamableHTTPHandler`, negotiates `2025-11-25`, lists tools,
      and renders a Go-derived schema correctly; a wrong token surfaces as a clean
      `Unauthorized`. Findings in context.md E-9. The transport assumption is no
      longer an assumption.
- [ ] The route accepts `POST`, `GET` **and `DELETE`** — the client uses POST and
      DELETE and never GET (E-9), and DELETE is how it closes a session
- [ ] Gate: `go build ./... && go test ./internal/mcpserver/...`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(mcpserver): serve the memory graph over streamable HTTP MCP`

---

### T8: The 15 tool registrations with upstream schemas

**What**: `tools.go` — explicit `*jsonschema.Schema` per tool, typed handlers.
**Where**: `internal/mcpserver/tools.go` (new), `internal/mcpserver/tools_test.go` (new),
`internal/mcpserver/testdata/upstream-tools.json` (new)
**Depends on**: T3, T4, T7
**Reuses**: `memgraph` ops (T3), `memgraph.Rank`/`SearchNodes` (T4)
**Requirement**: FR-2, FR-2.1, FR-2.2, E-4, E-8

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] All 15 tools registered with upstream's exact names
- [ ] Each `mcp.Tool.InputSchema` is set explicitly (never inferred), so the SDK
      applies upstream's declared defaults (E-4)
- [ ] `testdata/upstream-tools.json` holds the tool list extracted from upstream
      `index.ts` v0.8.0, and a test compares every advertised name, property name,
      `required` set, `enum` and `default` against it — drift from upstream fails
      the build
- [ ] `search_nodes`'s `maxObservations` default is **5** (the schema value), with
      a comment naming upstream's implementation/schema disagreement so nobody
      "fixes" it to 3 (E-8)
- [ ] `semantic_search`'s description states lexical/BM25 ranking and does **not**
      claim embeddings; a test greps the advertised description for `ColBERT` and
      `embedding` and fails if present (FR-2.2, D-1)
- [ ] `semantic_search` responses carry `searchType: "lexical"` (D-1)
- [ ] No tool is registered `deferred` — nothing in this package writes that field
      (FR-2.1)
- [ ] Each handler is thin: unpack, one `memgraph` call, marshal
- [ ] Gate: `go build ./... && go test ./internal/mcpserver/...`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(mcpserver): register the 15 memory-graph tools with upstream schemas`

---

### T9: Managed paths and header redaction [P]

**What**: extend `ManagedConfigPaths`; add `redactMCPHeaders` to the redaction
pass.
**Where**: `internal/docker/instance_config.go`, `internal/docker/instance_config_test.go`
**Depends on**: T5, T6
**Reuses**: `redactModelKeys`, `maskAPIKeys`, `holdsMask`,
`shallowCopyForRedaction*` (`instance_config.go`)
**Requirement**: FR-5.3, FR-5.4, NFR-2, E-7, AC-4

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] `ManagedConfigPaths` gains `tools.mcp.enabled` and
      `tools.mcp.servers.memory`; `TestManagedConfigPathsMatchWriters` passes with
      the T10 writer in place (this task and T10 are one gate together)
- [ ] Every value under `tools.mcp.servers.*.headers` is masked in the document
      `ReadInstanceConfig` returns — **all** servers, not only `memory`, so a
      hand-added sibling's credential is not leaked either
- [ ] `RedactedPaths` names the masked paths
- [ ] The revision is still computed over the **pre-redaction** on-disk bytes
      (existing invariant, asserted so redaction does not break optimistic
      concurrency)
- [ ] Resubmitting the redacted document keeps the real header on disk — the
      `holdsMask` round-trip, asserted end to end (AC-4)
- [ ] **The round-trip is asserted on a hand-added SIBLING server, not only on
      `memory`.** Mask and restore are separate passes and adding the mask while
      forgetting the restore is the natural mistake. For `memory` that mistake
      self-heals (the token is deterministic and rewritten on the next ensure), so
      testing only `memory` tests the harmless case; a sibling's credential would
      be destroyed permanently (FR-5.4)
- [ ] `IsManagedConfigPath` reports true for **prefixes** of a managed path (per
      `admin-bulk-instance-config` T1: `agents` and `agents.defaults` are both
      managed because `agents.defaults.provider` is listed). Adding
      `tools.mcp.servers.memory` therefore makes `tools`, `tools.mcp` and
      `tools.mcp.servers` un-bulk-editable as wholesale targets. Confirm that is
      acceptable and that it breaks no existing bulk-config test and no `tools.*`
      key an admin sets today — run the full `internal/docker` suite, not just this
      task's pattern
- [ ] A config with no `tools.mcp` block is unaffected
- [ ] Tests pass as non-root (`PicoclawUser: ""`)
- [ ] Gate: `go build ./... && go test ./internal/docker/... -run 'InstanceConfig|Managed|Redact'`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(docker): manage and redact the MCP server block in config.json`

---

### T10: Inject the MCP server block on every ensure, and notice the restart

**What**: `applyMCPServer(configPath, url, token) (changed bool, err error)`, called
on **every** `EnsureRunning` for a picoclaw workspace; a restart notice when it
changes something.
**Where**: `internal/docker/mcp_config.go` (new),
`internal/docker/mcp_config_test.go` (new), `internal/docker/manager.go`
**Depends on**: T9
**Reuses**: `childMap` (`secrets.go`) for nested-map creation; `mcptoken.Mint` (T5);
`RaiseWorkspaceRestartNotice` (`restart_control.go:62`), `restart.ReasonConfig`
**Requirement**: FR-5.1, FR-5.2, FR-5.5, FR-4.3, FR-4.5, AC-2, AC-6

**NOT `alignWorkspace`.** `alignWorkspace` is called from inside `provision`'s
`if os.Stat(configPath) != nil` branch, so it runs only on a **first-ever seed** —
putting the block there would mean no existing member ever gets memory. The
every-ensure writer to follow is `resolveAndMaterialize`, whose own doc comment
says "on every path that materializes — not once at first provision". Call the new
writer next to it in `EnsureRunning`'s picoclaw branch, not inside it: model
resolution and MCP injection have nothing to do with each other.

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] The written block matches E-1's measured shape **exactly**, `command: ""`
      included, `type: "http"`, `headers` as a JSON **object**
- [ ] `url` is `<mcpBaseURL>/v1/mcp`
- [ ] `tools.mcp.enabled` is set `true` and never set back to `false`
- [ ] Sibling entries in `tools.mcp.servers` survive untouched (FR-5.1)
- [ ] Re-running the writer produces a **byte-identical** file and reports
      `changed: false` — the token is deterministic, so a second ensure causes no
      drift, no rewrite and no second restart notice (FR-4.3)
- [ ] A workspace that already has the correct block gets **no** restart notice; a
      workspace seeing the block for the first time, or seeing a changed URL, gets
      exactly one with `restart.ReasonConfig` (FR-5.5)
- [ ] A **returning** member — a workspace whose `config.json` already exists, so
      `provision` short-circuits and `alignWorkspace` never runs — still gets the
      block. This is the whole point of not using `alignWorkspace`; assert it
      directly by seeding a config.json first
- [ ] With an empty secret, **no** `tools.mcp` block is written and any previously
      written `memory` server is left alone rather than half-removed (FR-4.5, AC-6)
- [ ] An existing `config.json` that is not a JSON object still returns the current
      error rather than being silently rewritten
- [ ] `agents.defaults.workspace` alignment still happens (regression)
- [ ] Tests pass as non-root (`PicoclawUser: ""`)
- [ ] Gate: `go build ./... && go test ./internal/docker/... -run 'AlignWorkspace|Provision|Managed'`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(docker): inject the native memory-graph MCP server into each workspace`

---

### T11: Read-only HTTP surface for the UI [P]

**What**: four handlers, route registration (including the conditional `/v1/mcp`),
OpenAPI entries.
**Where**: `internal/httpapi/memory_graph.go` (new),
`internal/httpapi/memory_graph_test.go` (new), `internal/httpapi/handlers.go`,
`internal/httpapi/openapi.go`, `internal/httpapi/openapi.json`
**Depends on**: T2, T3, T4, T6 (and T7 for the `/v1/mcp` registration)
**Reuses**: `resolveSecretCaller`, `authorizeSecret` (`handlers.go`) — the exact
chain `handleMemoryGet` uses; `writeJSON`, `errBody`
**Requirement**: FR-6.1 – FR-6.6, FR-1.4, NFR-5

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] `GET /v1/memory-graph`, `/nodes`, `/search`, `/recent` registered and
      authorized with `resolveSecretCaller` + `authorizeSecret`, requiring
      `tenant_id` and `subs_acc_id` as UUIDs exactly as `handleMemoryGet` does
- [ ] A caller who fails `authorizeSecret` gets the same status
      `handleMemoryGet` gives — asserted against the existing test's expectation,
      not a fresh guess
- [ ] `/search` uses the **same** ranking as the `semantic_search` tool, so UI and
      bot agree (FR-6.3)
- [ ] **No write route exists** — a table test asserts `PUT`/`POST`/`DELETE` on all
      four paths return 405/404, and a grep gate asserts this file registers no
      mutating method (FR-6.5)
- [ ] `/v1/memory` and its handler are untouched — grep gate on
      `handleMemoryGet`/`handleMemoryPut` (NFR-5)
- [ ] `/v1/mcp` (`POST` and `GET`) is registered **only** when the token secret is
      non-empty; a test asserts 404 for both methods when it is empty (FR-4.5)
- [ ] Malformed `hours`, `k`, `threshold`, `detail_level` yield 400, not a panic
      and not a silent default
- [ ] The four read routes appear in `openapi.json`; `/v1/mcp` does not (FR-6.6)
- [ ] Gate: `go build ./... && go test ./internal/httpapi/...`

**Tests**: unit · **Gate**: quick
**Commit**: `feat(httpapi): expose the memory graph read-only and mount the MCP endpoint`

---

### T13: Wire the secret into all three compose files [P]

**What**: `CRAB_MCP_TOKEN_SECRET` (and `CRAB_MCP_BASE_URL` where needed) on the
`crab-shell-proxy` service.
**Where**: `docker-compose.yaml`, `docker-compose.prod.yaml`,
`docker-compose.dokploy.yaml` (repo root — **outside this submodule**)
**Depends on**: T6
**Reuses**: the existing `CRAB_WEBHOOK_SECRET` env wiring as the pattern to copy
**Requirement**: FR-8.1, FR-8.2, FR-8.3

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] The variable is present on the proxy service in **all three** files. Parity
      across them is tracked in this repo on purpose (`chore(deploy): bring
      prod/dokploy back in line with standalone`); wiring one is a feature that
      works in one environment
- [ ] It is sourced from the environment / `.env`, never a literal in a committed
      compose file
- [ ] `CRAB_MCP_BASE_URL` is set wherever the proxy is not at the default
      `http://crab-shell-proxy:8080`
- [ ] `docker compose config` parses for each file
- [ ] The stack still boots with the variable **unset** (FR-8.3)

**Tests**: manual · **Gate**: `docker compose -f <each> config`
**Commit**: `chore(deploy): wire CRAB_MCP_TOKEN_SECRET into all three compose files`

---

### T12: Full gate and end-to-end verification

**What**: run the full suite against the baseline, then verify against a real
picoclaw container. Record results in `progress.md`.
**Where**: `.specs/features/memory-graph-mcp/progress.md` (new)
**Depends on**: T8, T10, T11, T13
**Reuses**: `docker compose` stack at the repo root
**Requirement**: AC-1 – AC-6

**Tools**: MCP: NONE · Skill: NONE

**Done when**:

- [ ] `go build ./...` clean; `gofmt -l` adds no new file beyond the two
      pre-existing ones
- [ ] `go test ./...` shows **no new failure** beyond the 9 baseline
      `internal/docker` `lchown` failures — verified by `git stash` + re-run, not
      by assumption
- [ ] `go test -race ./internal/memgraph/... ./internal/mcpserver/...` clean
- [ ] **AC-1**: `picoclaw mcp test memory` inside a container the proxy actually
      spawned lists all 15 tools. Output pasted into `progress.md`. This is the
      only check that proves picoclaw's client and our server agree — a Go test
      against our own handler does not.
- [ ] **AC-2**: a live chat turn stores an entity; a later turn retrieves it after
      a container restart
- [ ] **AC-3**: a forged token fails closed; member A cannot read member B
- [ ] **AC-4**: `GET /v1/admin/users/config` shows the block with `headers`
      masked; resubmitting keeps the real token
- [ ] **AC-5**: an upstream `memory.json` copied in is read correctly
- [ ] **AC-6**: with `CRAB_MCP_TOKEN_SECRET` unset, `/v1/mcp` 404s and no
      `tools.mcp.servers.memory` is written
- [ ] Any AC that fails is recorded as failed in `progress.md` with its output —
      not quietly dropped
- [ ] Gate: full

**Tests**: integration + manual · **Gate**: full
**Commit**: `test(memory-graph-mcp): record full-gate and end-to-end verification`

---

## Traceability

| Requirement | Task | Verified by |
| --- | --- | --- |
| FR-1.1 – FR-1.4 | T7, T11 | `mcpserver/server_test.go`, `httpapi/memory_graph_test.go` |
| FR-2, FR-2.1, FR-2.2 | T8 | `mcpserver/tools_test.go` + `testdata/upstream-tools.json` |
| FR-3.1 – FR-3.9 | T3 | `memgraph/ops_test.go` |
| FR-3.10 | T1 | `memgraph/graph_test.go` |
| FR-3.11, FR-3.12 | T3 | `memgraph/ops_test.go` |
| FR-3.13 | T4 | `memgraph/search_test.go` |
| FR-4.1 – FR-4.4 | T5, T7 | `mcptoken/token_test.go`, `mcpserver/server_test.go` |
| FR-4.5 | T6, T10, T11 | `config_test.go`, `provision_test.go`, `memory_graph_test.go` |
| FR-5.1, FR-5.2 | T6, T10 | `provision_test.go` |
| FR-5.3, FR-5.4 | T9 | `instance_config_test.go` |
| FR-6.1 – FR-6.6 | T11 | `httpapi/memory_graph_test.go` |
| FR-7.1 – FR-7.6 | T2 | `memgraph/store_test.go` |
| NFR-1 | T7 | `mcpserver/server_test.go` (401, no-scope-params, no token in logs) |
| NFR-2 | T9 | `instance_config_test.go` (mask + round-trip) |
| NFR-3 | T7 | `go.mod` diff is one module; no container/Python added |
| NFR-4 | T4 | `memgraph/search_test.go` |
| NFR-5 | T11 | grep gate on `handleMemoryGet`/`handleMemoryPut` |
| AC-1 – AC-6 | T12 | `progress.md` |
