# Design — Persona injection (delivery A)

## Data layout

Two admin stores, beside the existing `files`/`secrets`/`skills` ones under each
agent-scoped shared root:

```
tenants/<t>/shared/agents/<agent>/persona/{AGENT.md,SOUL.md,HEARTBEAT.md,USER.md}
tenants/<t>/subscriptions/<s>/shared/agents/<agent>/persona/…
```

New `config` helpers, mirroring the `…SharedFilesDir` family, which already
compose from `tenantAgentSharedRoot` / `subscriptionAgentSharedRoot`:

```go
func TenantAgentPersonaDir(root, tenantID, agentKey string) string
func SubscriptionAgentPersonaDir(root, tenantID, subsAccID, agentKey string) string
func EffectivePersonaDir(root, tenantID, subsAccID, agentKey string) string  // effective-persona/<t>/<s>/<agent>
```

## New module: `internal/docker/persona.go`

Kept out of `shared.go`, which is already the shared-files/skills cascade. Three
pieces, the first two React-free-equivalent (no Docker, no root):

```go
// The four files the feature knows about, and the three that are mounted.
var PersonaFiles     = []string{"AGENT.md", "SOUL.md", "HEARTBEAT.md", "USER.md"}
var PersonaMounted   = []string{"AGENT.md", "SOUL.md", "HEARTBEAT.md"}

func IsPersonaFile(name string) bool

// resolvePersonaSources returns, per file, the first path that exists in
// precedence order — or "" when neither layer nor template has it.
func resolvePersonaSources(cfg, key, templateDir string) map[string]string

// personaBinds is the read-only bind set, one entry per file actually present.
func personaBinds(cfg *config.Config, key WorkspaceKey, mountDest string) []personaBind

// syncEffectivePersona materializes the resolved set into EffectivePersonaDir,
// writing IN PLACE so an already-mounted file updates live (R3.1).
func (m *Manager) syncEffectivePersona(key WorkspaceKey, templateDir string) error
```

`personaBinds` is pure and takes only what it needs, so the bind strings are
testable the way `sharedFileBinds` already is (`shared_test.go:322`).

### Why in-place, concretely

```go
// NOT os.WriteFile-to-temp + os.Rename. A file bind mount pins the inode it was
// created against; a rename gives the host a new inode and leaves the container
// reading the old one for the life of the container.
f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
```

The cost is that a reader can observe a partial file mid-write. For persona
markdown read at agent start that is acceptable, and any write that matters is
followed by the caller's restart policy anyway.

## Wiring into `m.create`

`manager.go` already builds `secretsMount`, `sharedMounts`, `managedSkillMount`,
`managedMemoryMount` and `skillsMount`. Persona joins them:

```go
if err := m.syncEffectivePersona(key, templateDir); err != nil { … }
for _, pb := range personaBinds(m.cfg, key, mountDest) {
    binds = append(binds, pb.bind)
}
```

`m.create` is the picoclaw branch (`manager.go:253` picks `createHermes` for
hermes), so R1.2 holds without a harness check of its own.

`syncEffectivePersona` needs `templateDir`, which `EnsureRunning` already has in
scope where it calls `provision`.

## Provision changes

`config.WorkspaceSeed` → `["USER.md", "memory/", "skills/"]`.

`USER.md`'s seed source changes from "the template" to "the resolved persona
cascade". `seedWorkspace` copies from the template dir; the cleanest seam is to
materialize the effective persona set BEFORE provisioning (it has to happen
before create anyway) and let `seedWorkspace` read `USER.md` from the effective
dir while everything else still comes from the template.

That keeps `seedWorkspace`'s no-clobber logic untouched: it already skips an
entry whose destination exists, which is exactly R4.3.

## Admin API

`internal/httpapi/admin.go` gains four handlers modelled on the skills ones
(`handleAdminSkillsList` / `Doc` / `Post` / `Delete`), registered at
`handlers.go:249-253`'s neighbours:

```go
mux.HandleFunc("GET /v1/admin/persona", s.handleAdminPersonaList)
mux.HandleFunc("GET /v1/admin/persona/doc", s.handleAdminPersonaDoc)
mux.HandleFunc("POST /v1/admin/persona", s.handleAdminPersonaPost)
mux.HandleFunc("DELETE /v1/admin/persona", s.handleAdminPersonaDelete)
```

Two guards the skills routes do not need:

- `IsPersonaFile(name)` — the endpoint writes into a workspace ROOT, so an
  unconstrained `name` would be an arbitrary-write primitive. 400 otherwise.
- agent-scoped only. `scope.AgentKey == ""` or the all-agents sentinel → 400.

The `Manager` methods behind them (`ListPersona`, `ReadPersona`, `WritePersona`,
`DeletePersona`) live in `persona.go` and follow the shared-skills shape:
sanitize, then act on `personaDir(scope)`.

## Files

| File | Change |
|---|---|
| `internal/config/config.go` | three path helpers; `WorkspaceSeed` shrinks |
| `internal/config/config_test.go` | seed assertion updated |
| `internal/docker/persona.go` | new — cascade, binds, effective sync, CRUD |
| `internal/docker/persona_test.go` | new — precedence + bind set, no Docker |
| `internal/docker/manager.go` | sync + binds in `m.create` |
| `internal/docker/provision.go` | `USER.md` seeded from the effective dir |
| `internal/docker/provision_test.go` | no-clobber assertion moves to `USER.md` |
| `internal/docker/defaulttemplate/picoclaw/workspace/HEARTBEAT.md` | new |
| `internal/docker/default_template_test.go` | expects the new entry |
| `internal/httpapi/admin.go` | four handlers |
| `internal/httpapi/handlers.go` | four routes + interface methods |
| `internal/httpapi/handlers_test.go` | fake orchestrator gains the methods |

## Risks

**An absent bind source.** Docker creates a directory at the destination when the
source does not exist, which would put a *directory* named `AGENT.md` in the
workspace. `personaBinds` therefore emits nothing for a file with no source
(R1.3), and the effective sync creates only what it resolved.

**A stale copy resurfacing.** Dropping `AGENT.md`/`SOUL.md` from the seed matters
for workspaces provisioned BEFORE this change: they already have real files
there, which the mount then shadows. Shadowed, not deleted — if the feature were
ever reverted, the old content would come back. Worth knowing; not worth a
migration that deletes user data.

**`chown` on the effective dir.** The bind source must be readable by the
non-root agent, so the effective dir gets `chownTree` like every other bind
source. The FILES stay root-owned and `0o600`-ish on the host side — that is what
makes the read-only mount unforgeable, and `:ro` is what actually blocks the
write.
