# crab-shell-proxy 🦀🐚

The orchestrator of the zombie-crab stack: a small Go service that sits behind
the [Mycelium](https://github.com/LepistaBioinformatics/mycelium) gateway, reads
which agent a request is for and which member made it, and makes sure that
member's own agent container is running before relaying the conversation to it.

It is the component that holds the Docker socket, and it runs as root. The
`Dockerfile`'s runtime stage says why in as many words: it has to reach the
socket (`root:docker`, mode 0660), read root-owned template files, and write the
per-user data directories a container then reads. That makes it the trusted
control plane of the stack, and the agents it spawns the sandboxed part.

The unit it orchestrates is one container per `(tenant, subscription, agent,
user)`. The agent comes from the `x-mycelium-service-name` header the gateway
injects (`config.AgentByServiceName`); the member comes from the profile header's
`accId`, never the e-mail, because an e-mail is mutable and an account id is not.
The container is named `<containerPrefix>-<role>-<hash>` — `crabshell` by default,
the agent key, and sixteen hex characters of a SHA-256 over
`tenant::subscription::user`, since the full tuple is two UUIDs and would break
the 63-character DNS label limit that lets a container resolve itself on the
Docker network (`internal/docker/manager.go`). Identity is therefore *not* in the
name: it is in the `crab-shell.*` labels and the `.crab-owner.json` marker.

Each agent declares which runtime answers for it, and one that declares no
`harness:` key gets the **ganglion** — `config.DefaultHarness` is
`config.HarnessGanglion`, filled in before validation runs. picoclaw is the one
being deprecated: still fully served, no longer what an omitted key means. An
agent inheriting the default needs `CRAB_GANGLION_IMAGE`, which is why every
agent in this repository's `config.yaml` declares its harness explicitly.

## What it is not responsible for

It does not authenticate anyone. Identity arrives already verified from the
gateway, and the job here is to trust that header, not to reproduce the check.

It does not run the agent loop. Which tool to call, when to stop, and what the
answer is belong to the harness —
[crab-ganglion-harness](https://github.com/LepistaBioinformatics/crab-ganglion-harness)
or [picoclaw](https://github.com/sipeed/picoclaw). Where a harness genuinely
cannot serve a feature, `internal/httpapi/harness_gate.go` answers `501` naming
the harness rather than quietly succeeding; the file records the incident that
produced the rule.

It does not render anything. The member-facing UI is
[crab-exoskeleton-webapp](https://github.com/LepistaBioinformatics/crab-exoskeleton-webapp),
which reaches this service through the gateway and never talks to an agent
container directly.

## Building and testing

**The build is the test gate.** The `Dockerfile`'s build stage runs `go mod tidy`,
`go vet ./...` and `go test ./...` before it links the binary, so a failing test
means no image. But **no workflow runs on a pull request**:
`.github/workflows/release-image.yml` is the only workflow here, and it triggers
on a push to `main`, a `v*` tag, or `workflow_dispatch`. The gate therefore fires
*after* a merge, which makes running it yourself the whole pre-merge check:

```bash
go vet ./... && go test ./...
```

A second suite talks to a real Docker daemon — it creates and destroys a
throwaway `alpine` container to prove the hand-written Engine API encoding
round-trips, and needs no LLM key. It is behind `//go:build integration`, so it
runs in neither the command above nor the image build, and must be asked for:

```bash
go test -tags integration ./internal/docker -run TestIntegration -v
```

The binary reads `CRAB_CONFIG` (default `/etc/crab-shell-proxy/config.yaml`,
where the image bakes it) and `DOCKER_SOCKET` (default `/var/run/docker.sock`).
[`config.yaml`](./config.yaml) documents every field and its `CRAB_*` override,
and holds environment-variable *names* rather than values so one image serves
every deployment. Deploying the stack is the book's job, not this file's.

## How the code is laid out

`cmd/crab-shell-proxy/main.go` is the entry point; everything else is under
`internal/`.

| Package | What lives there |
|---|---|
| `config` | the agent catalog, defaults and validation, the harness constants, and the path helpers for the data root (`templates/<template>` and `tenants/<t>/subscriptions/<s>/agents/<role>/users/<u>`) |
| `httpapi` | every route — the OpenAI-shaped member surface, `/v1/admin/...`, `GET /healthz`, `GET /doc/openapi.json` — plus the harness feature gate |
| `docker` | the hand-written Docker Engine API client spoken as raw HTTP over the socket, and everything done *to* a container or its volume |
| `pico`, `ganglion`, `turn` | running one turn against picoclaw (its WebSocket protocol) or against crab-ganglion-harness (HTTP with SSE), plus the harness-neutral request shape both share |
| `registry` | the proxy-level model inventory: the single source of truth for which model a workspace uses |
| `history` | reading a conversation's transcript back out of a member's directory |
| `memgraph`, `mcpserver`, `mcptoken` | the knowledge-graph memory, the MCP endpoint a container reaches it through, and the bearer token that scopes that access |
| `cron`, `projects`, `restart`, `authz`, `identity` | scheduled tasks, a member's projects, restart-notice state, the caller's administrative tier, and resolving the account from the profile header |

`internal/docker` is by a wide margin the largest package, and that is a fair
signal of where the work is. Specs live under [`.specs/`](./.specs), one folder
per feature.

## Security notes

- **The proxy holds the Docker socket and runs as root.** It can control the host
  daemon, which makes it the most privileged component in the stack and the one
  worth hardening first — a restricted socket proxy, a dedicated host.
- **`telemetryToken` is not an agent token.** `GET /v1/instances` is the read-only
  inventory a watcher reads to attribute a container to its tenant, and it takes a
  credential of its own: the profile header is decoded and never verified, so an
  agent token is what stops a caller on the container network from asserting any
  `accId` it likes. It gates chatting as any member, and monitoring must not hold
  it (`internal/httpapi/instances.go`).
- **Spawned containers run as `picoclawUser`**, shipped as `1000:1000`, with a
  relocated `$HOME`; the proxy chowns each per-user directory to that uid. Setting
  the field to `""` runs them as root instead.
- **Secrets stay in the environment.** An agent's LLM key is named by `apiKeyEnv`
  and resolved from this process's environment at provisioning time, then written
  into that member's own store (mode 0600, on their volume) — never into
  `config.yaml`, a template, or an image.
- **Two secrets are optional and fail closed when unset.** No `mcpTokenSecret`
  disables the memory graph and unregisters `/v1/mcp`; no `telemetryToken`
  unregisters `GET /v1/instances` entirely — a 404, not a 401. A deployment that
  forgot one gets no endpoint rather than an unauthenticated one.

## Documentation

The book at <https://lepistabioinformatics.github.io/zombie-crab-project/> is
canonical for everything about the stack beyond this checkout:

- [crab-shell-proxy](https://lepistabioinformatics.github.io/zombie-crab-project/50-crab-shell-proxy.html)
  — this component in the context of the stack.
- [Harnesses](https://lepistabioinformatics.github.io/zombie-crab-project/11-harnesses.html)
  — the two runtimes, and how one is chosen.
- [Agents, workspaces and projects](https://lepistabioinformatics.github.io/zombie-crab-project/12-agents-and-workspaces.html)
  — the directory layout this service reads and writes.
- [Deployment](https://lepistabioinformatics.github.io/zombie-crab-project/40-deployment.html)
  — running the stack, which this file deliberately does not cover.

## License

Licensed under either of

- Apache License, Version 2.0 ([`LICENSE-APACHE`](./LICENSE-APACHE) or
  <http://www.apache.org/licenses/LICENSE-2.0>)
- MIT license ([`LICENSE-MIT`](./LICENSE-MIT) or
  <http://opensource.org/licenses/MIT>)

at your option.

Unless you explicitly state otherwise, any contribution intentionally submitted
for inclusion in this project by you, as defined in the Apache-2.0 license,
shall be dual licensed as above, without any additional terms or conditions.
