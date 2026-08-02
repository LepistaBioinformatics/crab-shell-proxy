# memory-graph-mcp — progress

## State

T1–T11 and T13 are implemented and tested. **T12 is partially done**: the whole
automated gate passes, and the transport was verified against the real picoclaw
client, but the four acceptance criteria that need a running stack were **not**
executed. See "Not verified" below — it is the honest limit of this session, not a
rounding error.

## Automated gate — measured

```
gofmt -l ./cmd ./internal
  internal/authz/authz_test.go      <- pre-existing
  internal/registry/registry.go     <- pre-existing
  (no new file)

go build ./...                      clean

go test ./...
  authz config hermes history httpapi identity mcpserver mcptoken memgraph
  pico registry restart             all ok
  docker                            FAIL — exactly 9, byte-identical to the
                                    pre-existing baseline:
    TestContinuousDoesNotArmIdle            TestEnsureRunningSingleFlight
    TestCreateAddsReadOnlySecretsBind       TestReconcileEnsuresContinuousWorkspaces
    TestEnsureRunningColdStart              TestRestartWorkspaceRestartsAndRearms
    TestEnsureRunningRecreatesOnPersonaDrift TestScaleToZeroIdleStop
    TestEnsureRunningReusesRunning
  (all nine are `lchown … operation not permitted`; they need root)

go test -race ./internal/memgraph/... ./internal/mcpserver/... ./internal/mcptoken/...
  all ok
```

**No new failure.** The nine were confirmed present on this HEAD before any code was
written, and the list after is the same list.

New test functions: `memgraph` 61, `docker/mcp_config_test.go` 20,
`mcpserver` 14, `mcptoken` 11, `httpapi/memory_graph_test.go` 10, `config` 4
— **120** total (115 before the three review defects below were fixed).

## Verified against the real picoclaw client (context.md E-9)

Not a Go test against our own handler — the actual `sipeed/picoclaw:latest` image
driving a stateless `NewStreamableHTTPHandler` from the same SDK version:

```
Connected to MCP server protocol=2025-11-25 server=memory
    serverName=crab-memory-graph serverVersion=0.1.0
Listed tools from MCP server server=memory toolCount=1
✓ MCP server "memory" reachable (1 tools).
```

`picoclaw mcp show memory` rendered the tool name, description and the parameter's
type/required/description, so a Go-derived schema arrives intact. A wrong token
produced `calling "initialize": sending "initialize": Unauthorized`.

That run also measured two things the plan had wrong: the client uses **POST and
DELETE and never GET**, and `Stateless: true` opens no standalone SSE stream.

## Not verified — needs a running stack

| AC | What | Why not |
|---|---|---|
| ~~**AC-1**~~ | — | **CLOSED against the live stack, 2026-08-01.** See below. |
| ~~**AC-2**~~ (write half) | — | **CLOSED on the live stack, 2026-08-02.** See below. Recall-after-restart still unobserved. |
| **AC-4** | `GET /v1/admin/users/config` over HTTP showing the masked block | The mask **and** its round-trip are covered at the `ReadInstanceConfig`/`WriteInstanceConfig` level, including the sibling-server case that can destroy a credential permanently. The HTTP hop is not. |
| **AC-6** | The live stack booting with `CRAB_MCP_TOKEN_SECRET` unset | Covered in unit tests at three layers (config load, `applyMCPServer`, route mounting). Not exercised against a real boot. |

### AC-1 — closed on the live stack

With `CRAB_MCP_TOKEN_SECRET` set (64 chars, confirmed inside the proxy container)
and the stack rebuilt, both spawned containers were checked directly:

```
$ docker exec crabshell-alpha-… picoclaw mcp test memory
Connected to MCP server protocol=2025-11-25 server=memory
    serverName=crab-memory-graph serverVersion=0.1.0
Listed tools from MCP server server=memory toolCount=15
✓ MCP server "memory" reachable (15 tools).
```

Identical for `crabshell-beta-…`. Fifteen tools, not the one the E-9 probe had.

Supporting evidence gathered at the same time:

- The injected block is on disk in both workspaces with the measured shape —
  `enabled: true`, `command: ""`, `type: "http"`,
  `url: http://crab-shell-proxy:8080/v1/mcp`, `headers.Authorization`.
- Restart notices were raised at `22:04:48` with `reason: "config"` for both
  workspaces, and both markers now carry a LATER `lastRestartAt` — so the notice
  mechanism fired and then cleared itself exactly as FR-5.5 describes.
- The proxy log shows the endpoint serving: `POST /v1/mcp -> 200`,
  `POST -> 202` (the initialized notification), `DELETE -> 204`.

### AC-2 (write half) and D-2 — closed on the live stack, 2026-08-02

A real chat turn, with a real model (deepseek-chat), wrote to the graph:

```
-> TOOL: mcp_memory_create_entities
   {"entities":[{"entityType":"projeto","name":"Zombie Crab",
                 "observations":["Usa DeepSeek no alpha"]}]}
[tool] {"created":["Zombie Crab"],"skipped":[]}
```

On disk, exactly the upstream JSONL format, one line, no trailing newline:

```
{"type":"entity","name":"Zombie Crab","entityType":"projeto",
 "observations":[{"content":"Usa DeepSeek no alpha","timestamp":1785629232056,
 "confidence":1}],"createdAt":1785629232056}
```

Two things this settles that no unit test could:

- **The agent-facing tool name is `mcp_memory_<tool>`.** picoclaw prefixes MCP tools
  with `mcp_<server>_`, built from the `mcp_%s_%s` format string in its binary. Any
  instruction or documentation naming the bare `create_entities` would silently fail
  to steer the model.
- **D-2's ownership claim holds in production.** From inside the running container as
  uid 1000:

  ```
  drwx------ 2 root root 4096  memory-graph          <- entry visible
  ls: can't open '.../memory-graph/': Permission denied
  cat: can't open '.../memory-graph/memory.jsonl': Permission denied
  ```

  while the same shell reads `MEMORY_CUSTOM.md` fine — so the test discriminates
  rather than failing on everything. This is the `chownTree` skip working against the
  real `resolveAndMaterialize` path that chowns the user tree on every ensure.

Still open: **recall after a restart** (the read half of AC-2), and the webapp drawer
against real data rather than fixtures. **AC-4** and **AC-6** remain unit-tested only.

### Behavioural note, not a defect

The model had the 15 tools available for two turns before this and used
`append_file` on its own `MEMORY.md` instead — then TOLD the user it had also written
to the knowledge graph, which was false. It only used the graph after a
`MEMORY_CUSTOM.md` instruction naming the tools and forbidding unearned claims.

Worth carrying into any future default persona: shipping the tools is not shipping
the behaviour, and an agent with two memories will pick the one it already knew.
Even with the instruction it still writes to BOTH — the instruction steered it onto
the graph but did not stop the duplicate.

**AC-3** (isolation) and **AC-5** (upstream file import) ARE covered:
`TestATokenReachesOnlyItsOwnWorkspace` drives six read tools with member A's token
against member B's seeded graph plus a forged-token table, and
`TestLoadReadsAnImportedUpstreamFile` reads a real legacy fixture through the Store.

## Decisions taken during implementation, and what forced them

1. **`alignWorkspace` was the wrong hook, and the spec said it was the right one.**
   It runs only inside `provision`'s first-seed branch, so the MCP block would never
   have reached an existing member — a permanent gap, not a delayed one. The writer
   now runs on every ensure beside `resolveAndMaterialize`, and raises a
   `restart.ReasonConfig` notice when the block changes, because `continuous`
   containers read `config.json` at boot. Spec FR-5.1/FR-5.5 rewritten.
2. **The token payload was not injective.** `/`-joining four scope fields lets
   `role="a/b", user="c"` collide with `role="a", user="b/c"` — two legitimately
   minted tokens, so the MAC cannot catch it. `Mint` now rejects the delimiter.
3. **`add_observations` indexes `confidence` by position among the observations
   actually ADDED**, not by position in `contents`. The implementation was written
   the intuitive way; the test failed and upstream's source settled it. The port
   matches upstream, with a comment saying the contract is debatable.
4. **`TestManagedConfigPathsMatchWriters` caught the drift it exists for** — twice.
   First the new managed paths had no writer wired into the gate; then the gate's
   synthetic seed lacked the `tools.mcp` container every real picoclaw config has,
   so creating it read as touching an unmanaged path.
5. **FR-6.6 withdrawn.** `openapi.json` is a curated discovery subset that omits
   `/v1/memory`, `/v1/secrets`, `/v1/media` and every other member-scoped route.
   Adding the memory-graph routes would have made them the only such entries.
6. **FR-8.1's "three compose files" is two.** `docker-compose.prod.yaml` is an
   overlay and inherits `environment` from the base; rendering
   `-f base -f prod` confirms both variables arrive. Base and dokploy each got them.
7. **Two new direct Go dependencies, not one**, and the Dockerfile builder moved
   `golang:1.23` → `golang:1.25` because the SDK declares `go >= 1.25.0`. NFR-3
   amended; without the Dockerfile bump the image does not build at all.

## Three defects found by review after the gate was already green

All three were invisible to the 115 tests, and two of them made a written claim
false rather than merely incomplete.

1. **The graph directory was being handed to the agent on every ensure.**
   `internal/memgraph` chowns nothing and there is a source gate proving it — but
   `chownTree` is a `filepath.Walk` and `resolveAndMaterialize` calls
   `chownTree(userDir, picoclawUser)` on every ensure. So D-2's claim that the
   container shell cannot reach `memory.jsonl` was false from the second chat
   onward. `chownTree` now skips `GraphDirName`, with a test for the skip and a
   second test pinning the constant against `memgraph`'s.
2. **NFR-1's body cap was written into the spec and not implemented.** `/v1/mcp` is
   the only proxy route reachable from `zombie_net` without mycelium in front, and
   the wrapper delegated straight to the SDK. Now capped at 1 MiB before anything
   parses. **Mutation-checked**: without the cap, a 1 MiB+ body returns `200`.
3. **`ManagedConfigPaths`' promise did not hold for the MCP block.**
   `reapplyWorkspace` only ran `resolveAndMaterialize`, so an admin submitting a
   document with `tools.mcp.servers.memory` deleted left it deleted until the next
   ensure — which would then raise a restart notice nobody's action explained.
   Writing the test surfaced a second problem: the two writers were chained with
   `if err != nil { return }`, so a workspace whose registry resolves no model — the
   exact broken case the admin editor exists to repair — would skip the MCP restore.
   They are now `errors.Join`ed and both always run.

## Cold start needs no special handling — worth not "fixing"

`applyMemoryGraphMCP` raises the notice, then `EnsureRunning`'s `!st.Exists` branch
creates the container and `createdNow` → `stampRestart` clears it, because a
container created just now already booted with the new config. Only
already-running `continuous` workspaces keep a pending notice, which is precisely
the set that needs one.

## A fourth defect, found only by a member actually using it

The webapp's drawer failed on first use with
`{"error":"Request path does not match any service","status":400}` — from mycelium,
not the proxy.

**A proxy route is not reachable until the gateway is told about it.** Every
member-facing path needs a `[[<service>.path]]` block in `deploy/*/config*.toml`.
FR-8 covered the compose environment and stopped there, so nothing in the spec, the
120 proxy tests, the 534 webapp tests or the production build could have caught it:
every one of them exercises the proxy directly or mocks the fetch.

Fixed with two blocks per service (`/v1/memory-graph` and `/v1/memory-graph/*` —
mycelium's `*` matches a following segment, not the bare path) across all three
config files, `hermes-glm` included. Verified by probing the gateway unauthenticated:

```
/alpha/v1/memory              401   <- the working reference route
/alpha/v1/memory-graph        401   <- now resolves, needs auth
/alpha/v1/memory-graph/search 401   <- the wildcard block works
/alpha/v1/nao-existe          400   <- control: still "does not match any service"
```

Recorded as FR-8.4. The general lesson for this stack: **adding a member-facing proxy
route is two changes, not one**, and only a live request finds the missing half.

## Provenance (FR-9) — added after the feature shipped

`sourceSessionId` on observations, relations and entities, captured by
`httpapi.turnRegistry` and rendered as a clickable conversation list in the webapp's
graph pane.

The whole design is in one decision: **attribute only when exactly one turn is in
flight.** Nothing serializes turns per workspace, so two tabs on two conversations are
concurrent, and a single-value map would silently attribute a fact from chat A to chat
B — worse than no link, because the member clicks through and reads a conversation
that never said it. Zero in flight (cron, heartbeat, post-turn evolution) and two or
more both store nothing.

New tests: `httpapi/turn_registry_test.go` 9 (including a `-race` concurrency case and
a leak check on the internal map), plus 6 provenance cases in `memgraph/ops_test.go` —
one of which pins that an **unattributed** write emits no `sourceSessionId` key at all,
which is what keeps FR-7.2's byte-for-byte upstream compatibility true.

## Mutation checks

Ordering was not TDD throughout, so the load-bearing assertions were checked by
breaking the code instead of by claiming a RED phase that did not happen:

- Removing the per-scope lock from `Store.Update` drops
  `TestConcurrentUpdatesOnOneScopeAllLand` from 24 surviving entities to 1.
- The `confidence`-indexing fidelity bug (item 3) was found *by* a failing test, not
  written to match the code.
- `TestManagedConfigPathsMatchWriters` failed twice on real gaps before passing.

## Follow-ups

- Run T12's four remaining acceptance criteria against a live stack.
- ~~Webapp rendering of the graph.~~ **Done** — see
  `crab-exoskeleton-webapp/.specs/features/memory-graph-mcp/`. A right-hand drawer
  entered from the workspace panel: Entities / Search / Recent, with a detail pane
  per entity. 30 new tests; webapp suite at 534 passing and `next build` clean.

  Building it found a real defect in this feature's API shape, worth knowing about
  here: a single-entity `open_nodes` returns **no relations**, because
  `relationsAmong` requires both endpoints to be among the requested names. That is
  correct for the tool contract (it is upstream's behaviour) but it means there is
  no way to ask this proxy for one entity's neighbourhood. The webapp works around
  it by filtering the relations the browse response already carries. If a future
  version wants a proper per-entity read, that is a new proxy route, not a fix.
- UI curation (archive / delete / merge). The read surface was shaped so adding it
  is additive.
