# crab-shell-proxy

**Vision:** One real container per user for your AI agents — a small Go service behind a Mycelium gateway that gives every user their own isolated picoclaw agent, spun up on demand and reachable through an OpenAI-compatible API.
**For:** Operators self-hosting picoclaw who need genuine multi-tenant/multi-team isolation without picoclaw itself having RBAC.
**Solves:** Shared-process "soft" isolation (one process, users separated by a key in a map) is a data breach waiting for a bad day — a prompt injection or path-traversal bug lets one user reach another's conversations, files, and secrets. crab-shell-proxy gives each `(agent, user)` a real boundary: separate container, separate volume, non-root, gateway-verified identity.

## Goals

- **Real per-`(agent, user)` isolation** — one container + one volume each, non-root (uid 1000), so a fully compromised agent still cannot reach another user's data.
- **OpenAI-compatible surface** — drop-in `/v1/chat/completions` (SSE streaming + JSON), `/v1/models`, `/v1/sessions/history`.
- **Scale-to-zero or continuous lifecycle** per agent, with single-flight cold start and health-gated readiness.
- **Config-driven agent catalog** — declare agents, models, and lifecycle in one YAML; per-instance LLM keys sourced from the environment by name, never in config or images.

## Tech Stack

**Core:**

- Language: Go 1.23
- Orchestration target: Docker daemon, driven over the unix socket via raw HTTP (no docker SDK)
- Runs behind: Mycelium API gateway (`standalone` mode) — verifies the JWT and injects the account profile (`x-mycelium-profile`) + service name

**Key dependencies:**

- `github.com/LepistaBioinformatics/mycelium-sdk-go` — profile parsing / tenant filtering
- `github.com/coder/websocket` — Pico Protocol WebSocket client
- `github.com/google/uuid`, `github.com/klauspost/compress`, `gopkg.in/yaml.v3`

## Scope

**Includes:**

- Per-`(agent, user)` container lifecycle: on-demand cold start, idle stop (scale-to-zero) or never-stop (continuous), reconcile-on-boot.
- OpenAI HTTP surface bridged to picoclaw's Pico Protocol; session history from on-disk transcripts.
- Tenant→subscription→agent→user workspace hierarchy (mycelium-webhook seeded, SDK profile-gated).
- Shared workspaces, agent customization (template seeding + per-`(user, agent)` secrets), managed/shared skills, admin-shared-content, media upload, per-agent model registry + per-user model override.

**Explicitly out of scope:**

- Production hardening (TLS termination, Docker-socket privilege reduction, secret-rotation automation).
- Any frontend — this service is API-only; UI lives in the sibling webapp.

## Constraints

- Always runs behind Mycelium; identity is the profile `accId`, never a client-declared field.
- The proxy holds the Docker socket and runs as root — it is the trusted control plane and the most privileged component in the stack; the agents it spawns are the sandboxed, non-root part.
- In the dev sandbox, Docker builds that touch the network need `--network=host`, and `chown`-to-uid-1000 operations fail (see STATE.md).
