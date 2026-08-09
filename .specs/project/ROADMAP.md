# Roadmap

**Current Milestone:** M5 — Admin controls (models & skills)
**Status:** In Progress

---

## M1: Core orchestrator

**Goal:** One isolated picoclaw container per `(agent, user)`, spun up on demand and torn down when idle.

### Features

**Container lifecycle** - COMPLETE

- Docker-socket lifecycle over raw HTTP; single-flight cold start, health-wait, reconcile-on-boot.
- Two modes per agent: `scale-to-zero` (idle-timeout stop) and `continuous` (never auto-stop).
- Non-root (uid 1000) containers with relocated `$HOME`; per-user volume chowned accordingly.

**Config-driven agent catalog** - COMPLETE

- Agents, models, and lifecycle declared in `config.yaml`; per-instance LLM keys sourced from env by name, written to each user's `.security.yml` (0600).

---

## M2: OpenAI-compatible surface

**Goal:** A drop-in OpenAI API in front of picoclaw's Pico Protocol.

### Features

**Chat + models** - COMPLETE

- `/v1/chat/completions` (SSE streaming + JSON), `/v1/models`; OpenAI HTTP bridged to the Pico Protocol WebSocket.

**Session history** - COMPLETE

- `GET /v1/sessions/history` locates a session's `.jsonl` transcript via `.meta.json` scanning; `created_at` surfaced per message.

**Media** - COMPLETE

- `POST /v1/media` stores an upload in the caller's workspace, `GET /v1/media` lists, `DELETE /v1/media` removes; widened allowlist (office, opendocument, archives).

---

## M3: Multi-tenant workspace hierarchy

**Goal:** Replace the flat per-user layout with a tenant→subscription→agent→user hierarchy.

### Features

**tenant-scoped-workspaces** - COMPLETE — spec in `.specs/features/tenant-scoped-workspaces/`

- mycelium-webhook-seeded hierarchy, gated by mycelium-SDK profile filtering.

**shared-workspaces** - COMPLETE — spec in `.specs/features/shared-workspaces/`

- Shared data surfaces alongside the isolated per-`(agent, user)` workspace.

---

## M4: Agent & workspace customization

**Goal:** Let operators tailor agents and inject shared/per-user content.

### Features

**agent-customization** - COMPLETE — spec in `.specs/features/agent-customization/`

- Seed custom agent template files into per-user workspaces; inject per-`(user, agent)` secrets across every workspace of that pair.

**admin-shared-content + managed-skills** - COMPLETE

- Admin-managed shared content delivered into workspaces; workspace memory endpoint (`MEMORY_CUSTOM.md`).

---

## M5: Admin controls — models & skills (in progress)

**Goal:** Per-scope/per-user control over which model and which skills each workspace gets.

### Features

**admin-shared-skills** - COMPLETE — reports in `.specs/features/admin-shared-skills/` (spec in parent `.specs`)

- Per-scope shared skills backend: config builders, skill store, live effective-dir mount cascading tenant→subscription, propagated via stop/start.

**admin-model-override** - COMPLETE — reports in `.specs/features/admin-model-override/` (spec in parent `.specs`)

- Per-scope/per-user model override store with user>subscription>tenant>default precedence; admin HTTP endpoints; reapply-to-scope with restart; per-agent model registry (definition + key) + per-user assignment.

**default agent template auto-bootstrap** - COMPLETE

- Provisioning auto-bootstraps the default agent template when it is missing, instead of returning a 502 on unseeded templates.

---

## M6: Projects (planned)

**Goal:** let a user carve one agent into several projects, each with its own
workspace and instructions, inheriting the parent's skills, model and secrets.

### Features

**agent-projects** - SPEC'D — spec in `.specs/features/agent-projects/`

- picoclaw `agents.list` + `agents.dispatch` driven from a proxy-owned project
  store, projected into `config.json` on every ensure.
- **Needs a patched picoclaw image, not an upstream merge:** dispatch selectors
  match exactly today, so one project would need one rule per conversation. A
  ~15-line `*` wildcard fixes it, shipped as a build-time patch overlay over a
  pinned upstream tag. Contributing it back to `sipeed/picoclaw` is a parallel,
  optional track — see the spec's DEC-2.

---

## Future Considerations

- Production hardening: TLS termination, Docker-socket privilege reduction, secret-rotation automation.
- Per-user (not just per-agent) lifecycle-mode overrides.
- Non-picoclaw harness support behind the same proxy — **deferred.** Hermes Agent was built and verified live, then withdrawn for current-infra compatibility (root `.specs/features/hermes-removal/DECISION.md`). Restarting point: `.specs/features/multi-harness-support/implementation-notes.md`.
