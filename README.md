# crab-shell-proxy 🦀🐚

> **One real container per user for your AI agents.**
> Not a shared process with a hope and a hash.

`crab-shell-proxy` is a small Go service that sits behind a [Mycelium](https://github.com/LepistaBioinformatics/mycelium) API gateway and gives **every user their own isolated [picoclaw](https://github.com/sipeed/picoclaw) agent** — spun up on demand, torn down when idle, and reachable through an OpenAI-compatible API. It presents `/v1/chat/completions` (streaming and not), `/v1/models`, and `/v1/sessions/history`, while transparently managing the container lifecycle underneath.

---

## Why this exists: isolation you can actually trust

Most "multi-tenant" agent setups run **one shared process** for everybody and separate users with a key in a map — a session id, an email hash, a row filter. It looks isolated. It isn't.

AI agents read and write files, run tools, execute code, keep long-lived memory, and are steered by untrusted natural language. In a shared process, a single prompt-injection, a path-traversal bug, a tool that opens the wrong file, or one leaky library is enough for **user A to read user B's conversations, files, and secrets**. "Soft" isolation is a data breach waiting for a bad day.

`crab-shell-proxy` refuses that trade-off. Each user gets a **real** boundary:

| | Shared process + hash key | **crab-shell-proxy** |
|---|---|---|
| Process isolation | ❌ same process | ✅ separate container (PID/mount/net namespaces) |
| Filesystem | ⚠️ shared, filtered by code | ✅ separate per-user volume |
| Memory / sessions / cron | ⚠️ one DB, keyed rows | ✅ per-user store on that user's volume |
| Blast radius of a compromised agent | 💥 everyone | 🛡️ that one user |
| Runs as | often root | ✅ non-root (uid 1000) |
| Identity | app-level string | ✅ gateway-verified account id (unforgeable) |

**If one user's agent is fully compromised, it still cannot reach another user's data.** Different container, different volume, non-root, no shared surface. That is the difference between *"isolated"* and isolated.

---

## How it works

```
   Client ── HTTPS + JWT ──▶  Mycelium gateway
                                 │  verifies the token, injects the account
                                 │  profile (x-mycelium-profile) + service name
                                 ▼
                        ┌──────────────────────────┐
                        │      crab-shell-proxy      │   ── Docker API (unix socket)
                        │  agent  ← service-name     │
                        │  user   ← profile accId    │
                        │  ensure container is up    │
                        │  OpenAI HTTP ⇄ Pico WS     │
                        └───────────┬────────────────┘
                                    ▼   (spawned on demand, non-root)
                        picoclaw-<agent>-<accId>
                        /data/.picoclaw  ←  per-user volume
                        (native connectors dial OUT: Telegram, Teams, …)
```

- **Identity is the account, not the email.** The user key is the Mycelium profile's `accId` (a stable, unique account id the gateway injects and the caller *cannot forge*). Emails change and are mutable; `accId` doesn't. The email is kept only as a human-readable marker (`.crab-owner.json`) so operators can find who owns a container.
- **On-demand + scale-to-zero.** A user's first request cold-starts their container; after a configurable idle window it's `docker stop`ped to free RAM, and the next request brings it back with all data intact.
- **Continuous mode.** picoclaw's native connectors (Telegram, MS Teams, …) dial *out* from inside the container and never traverse the proxy — so instances that use them run `continuous` (never auto-stopped) instead of scale-to-zero.
- **Non-root by design.** Containers run as a non-root uid with a relocated `$HOME`; the proxy chowns each user's volume accordingly.

---

## Features

- 🔒 **Real per-`(agent, user)` isolation** — one container + one volume each, non-root.
- 🔌 **OpenAI-compatible** — drop-in `/v1/chat/completions` (SSE streaming + JSON), `/v1/models`, `/v1/sessions/history`.
- ⚡ **Scale-to-zero or continuous** lifecycle, per agent, with single-flight cold start and health-gated readiness.
- 🧩 **Config-driven agent catalog** — declare agents, models, and lifecycle in one YAML.
- 🔑 **Per-instance API keys from the environment** — each agent sources its own LLM key by env-var name; keys never live in config or images.
- 🪶 **Tiny** — Go, talks to Docker over the socket via raw HTTP, three dependencies.

---

## Quick start

`crab-shell-proxy` is meant to run behind Mycelium (which verifies identity and injects the profile). Configure agents in `config.yaml`:

```yaml
listen: ":8080"
hostDataRoot: "/abs/host/path/data/agents"   # bind-mount source (host path)
containerDataRoot: "/data/agents"
network: "your_docker_network"
picoclawUser: "1000:1000"                     # non-root
picoclawHome: "/data"
agents:
  alpha:
    serviceName: "picoclaw-alpha"             # matches x-mycelium-service-name
    token: { env: "MYC_PICOCLAW_ALPHA_TOKEN" }
    template: "alpha"
    mode: "scale-to-zero"                     # or "continuous"
    idleTimeout: 15m
    model:
      provider: "deepseek"
      name: "deepseek-chat"
      apiKeyEnv: "PICOCLAW_ALPHA_API_KEY"     # key read from env, per instance
```

Build and run it as a sibling-container orchestrator (mount the Docker socket and the data root):

```bash
docker run -d --name crab-shell-proxy \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD/data/agents":/data/agents \
  -e CRAB_HOST_DATA_ROOT="$PWD/data/agents" \
  -e PICOCLAW_ALPHA_API_KEY="$YOUR_KEY" \
  crab-shell-proxy
```

See [`config.yaml`](./config.yaml) for the full set of options and env overrides
(`CRAB_HOST_DATA_ROOT`, `CRAB_NETWORK`, `CRAB_PICOCLAW_USER`, …).

---

## Security notes

- **The proxy holds the Docker socket** and runs as root — it is the most privileged component in the stack (it can control the host daemon) and must be treated as such. The *agents* it spawns are non-root and sandboxed; the proxy is the trusted control plane. Harden accordingly for production (restricted socket proxy, dedicated host, etc.).
- **Per-user keys stay in the environment**, sourced by env-var name at provisioning time; they are written into each user's `.security.yml` (0600, on that user's volume) and never committed to config, templates, or images.

---

## Status

Actively developed as part of a Mycelium + picoclaw stack. The core — per-user
isolation, lifecycle, OpenAI surface, non-root, per-instance keys — is
implemented and covered by unit tests plus real-daemon integration tests.
