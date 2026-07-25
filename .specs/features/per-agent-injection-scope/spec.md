# per-agent-injection-scope — Specification (crab-shell-proxy)

## Summary

Admin-published **files**, **secrets** and **skills** currently cascade to *every*
agent under the publishing scope: their stores are keyed by tenant/subscription
only (`TenantSharedFilesDir(root, tenantID)` and friends), with no agent
dimension. A tenant admin who publishes a skill for `alpha` also hands it to
`beta`.

This feature adds an **agent dimension** to all three content types, mirroring
what the admin model registry already does (per-agent catalogs, `agent.Key`-keyed
storage). The existing agent-less paths are **retained and redefined as "all
agents"**, so nothing already published moves and no migration runs.

## Context (verified in code)

- `internal/config/config.go` layout builders: `TenantSharedFilesDir`,
  `TenantSharedSecretsDir`, `TenantSharedSkillsDir` and the three
  `Subscription*` twins take no agent argument.
- `internal/docker/manager.go:264-277` mounts shared **files** as two RO binds
  (`workspace/.shared/tenant`, `workspace/.shared/subscription`) — no copy.
- `internal/docker/shared.go:311` `syncEffectiveSecrets` merges the dotenv/json
  sinks tenant → subscription → user (user wins) into `EffectiveSecretsDir`.
- `internal/docker/skills.go:354` `syncEffectiveSkills(tenantID, subsAccID)`
  copy-merges tenant → subscription into `EffectiveSkillsDir(root, t, s)`.
- `internal/httpapi/admin.go` resolves the caller's agent from the routed
  service (`resolveSecretCaller`), but the shared-content handlers ignore it; the
  webapp BFF routes every admin call through a fixed `/alpha/v1/admin` vehicle
  precisely because these endpoints are agent-agnostic.
- There is **no** agent-discovery endpoint. The webapp hardcodes
  `INSTANCES = ["alpha","beta"]` (`lib/mycelium.ts:13`), an open question left
  unresolved as `OPEN-3` in the webapp's `model-list-management` spec.

## Functional requirements

- **FR-1** Each of the three content types gains a per-agent store beside the
  existing agent-less one:
  `<scopeRoot>/shared/agents/<agentKey>/{files,secrets,skills}`. The existing
  `<scopeRoot>/shared/{files,secrets,skills}` keeps its path and now means
  **all agents**.
- **FR-2** Every admin read/write/delete on shared files, shared secrets and
  shared skills accepts an **agent target**: the sentinel `all` (default,
  preserving today's behaviour byte-for-byte) or a configured agent key. An
  unknown agent key is `400`, nothing written.
- **FR-3 (secrets cascade)** `syncEffectiveSecrets` merges, lowest precedence
  first: tenant/all → tenant/agent → subscription/all → subscription/agent →
  user. The agent used is the workspace's own (`key.Role`).
- **FR-4 (skills cascade)** the effective-skills view becomes per
  (tenant, subscription, **agent**) and merges in the same order, later winning
  by skill name.
- **FR-5 (files cascade)** the per-agent file stores are mounted as two
  additional RO binds at `workspace/.shared/tenant-agent` and
  `workspace/.shared/subscription-agent`. Files are not copy-merged (unlike
  skills) — a merge would duplicate arbitrarily large uploads on every provision.
- **FR-6** `GET /v1/admin/agents` returns the configured agent keys (from
  `Config.Agents`), so the admin UI stops hardcoding them. Authorization: any
  caller who has at least one manageable scope (same posture as
  `/v1/admin/scopes`).
- **FR-7** A write at a scope+agent re-syncs and restarts only the established
  workspaces of **that agent** under the scope; a write at `all` affects every
  agent, as today.
- **FR-8** Authorization is unchanged — `authz.AuthorizeSharedScope` on the
  caller's tier vs the target scope. The agent dimension is *not* an authority
  boundary: a Tenant-tier caller may target any agent under their tenant.

## Non-functional

- **NFR-1** No migration. Content already at the agent-less paths keeps working
  and keeps cascading to every agent.
- **NFR-2** Path segments pass through `identity.SanitizeID`, as every other
  dynamic segment does.
- **NFR-3** Adding binds changes the container spec, which Docker bakes at
  create time and the proxy never drifts-detects. Existing containers keep the
  old bind set until removed once. Documented as a deploy step, not code —
  workspaces live on the host volume, so removal loses nothing.

## Out of scope

- Per-agent **model** overrides (`TenantModelOverrideFile` stays agent-less; the
  registry already covers the per-agent case).
- Per-user shared content (the user tier is unchanged).
- Making the agent list dynamic in mycelium (the proxy config remains the source
  of truth).

## Acceptance criteria (EARS)

- **AC-1** WHEN an admin publishes a skill at subscription scope targeting agent
  `alpha` THEN only `alpha` workspaces under that subscription SHALL receive it,
  and `beta` workspaces SHALL NOT.
- **AC-2** WHEN an admin publishes at target `all` THEN every agent under the
  scope SHALL receive it (today's behaviour).
- **AC-3** WHEN both an `all` entry and an agent-specific entry carry the same
  secret name or skill name THEN the **agent-specific** one SHALL win.
- **AC-4** WHEN a request names an agent key absent from `Config.Agents` THEN the
  response SHALL be `400` and nothing SHALL be written.
- **AC-5** WHEN content already exists at the pre-feature agent-less paths THEN
  it SHALL keep cascading to every agent with no migration step.
- **AC-6** `GET /v1/admin/agents` SHALL list the configured agent keys and no
  other field of the agent config (no image, no model, no key).

## Status: implemented
