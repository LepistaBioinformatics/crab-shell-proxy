# per-agent-injection-scope — Design (crab-shell-proxy)

## AD-1: agent as a *target* parameter, not a routing vehicle

The model registry expresses per-agent-ness by **routing**: the webapp calls
`/{agent}/v1/admin/registered-models` and the proxy derives the agent from the
service name. That cannot express "all agents" — there is no mycelium service
for it, and the routed agent is also the bearer-guard vehicle.

So the agent target travels as an explicit `agent` query/body parameter,
independent of the routed service. Values: `all` (default) or a key of
`Config.Agents`. This keeps the existing fixed `/alpha/v1/admin` BFF vehicle
valid and makes the default path byte-identical to today's behaviour.

## AD-2: `all` is the existing path, not a new sibling

`shared/agents/all/` was rejected: it would strand every already-published file,
secret and skill. Instead the existing `shared/{files,secrets,skills}` **is** the
all-agents layer, and per-agent content lives one level deeper:

```
tenants/<t>/shared/files                       ← all agents (unchanged)
tenants/<t>/shared/agents/<agent>/files        ← new
tenants/<t>/shared/secrets                     ← all agents (unchanged)
tenants/<t>/shared/agents/<agent>/secrets      ← new
tenants/<t>/shared/skills                      ← all agents (unchanged)
tenants/<t>/shared/agents/<agent>/skills       ← new
tenants/<t>/subscriptions/<s>/shared/…         ← same two layers
```

`shared/agents/` cannot collide with a stored file or skill name: both siblings
of `shared/` are fixed names (`files`, `secrets`, `skills`, `model.json`), and
skill/file names live *inside* those dirs.

## Scope type

`docker.Scope` gains an `AgentKey` field. Empty string == `all`, so every
existing construction site keeps compiling and keeps meaning "all agents".

```go
type Scope struct {
    Kind      ScopeKind
    TenantID  string
    SubsAccID string
    AgentKey  string // "" == all agents
}
```

The three `m.shared*Dir(scope)` helpers append
`agents/<sanitized AgentKey>` when `AgentKey != ""`. That single change carries
every existing list/write/read/delete method to the per-agent store with no
other edits.

## Path builders (config)

Six new builders mirroring the existing six, each taking `agentKey`:

```go
TenantAgentSharedFilesDir(root, tenantID, agentKey string) string
TenantAgentSharedSecretsDir(root, tenantID, agentKey string) string
TenantAgentSharedSkillsDir(root, tenantID, agentKey string) string
SubscriptionAgentSharedFilesDir(root, tenantID, subsAccID, agentKey string) string
SubscriptionAgentSharedSecretsDir(root, tenantID, subsAccID, agentKey string) string
SubscriptionAgentSharedSkillsDir(root, tenantID, subsAccID, agentKey string) string
```

`EffectiveSkillsDir` gains a fourth segment: `effective-skills/<t>/<s>/<agent>`.
`EffectiveSecretsDir` needs no change — it is already keyed by `(user, role)` and
`role` *is* the agent key.

## Cascades

**Secrets** (`syncEffectiveSecrets`): `cascadeSink` takes a `[]string` of shared
dirs instead of two positional ones, called with

```
tenant/all, tenant/<agent>, subscription/all, subscription/<agent>
```

then the user store on top. The `userWins` set is unchanged (dotenv/json only).

**Skills** (`syncEffectiveSkills`): takes `agentKey`, walks the same four dirs in
that order (later wins by name), materializes into the per-agent effective dir.
`SyncEffectiveSkillsForScope` fans out over the agents affected: the scope's
target agent, or every configured agent when the target is `all`.

**Files**: two extra RO binds, no copy:

| container path | source |
|---|---|
| `workspace/.shared/tenant` | tenant all-agents (existing) |
| `workspace/.shared/subscription` | subscription all-agents (existing) |
| `workspace/.shared/tenant-agent` | tenant, this agent (new) |
| `workspace/.shared/subscription-agent` | subscription, this agent (new) |

Sibling names, not nested mounts: nesting a bind inside an RO bind whose source
lacks the subdir is fragile across daemon versions.

## Restart fan-out

`RestartScope` already filters container labels by tenant (and subscription).
It gains an agent filter: when `scope.AgentKey != ""`, skip containers whose
`LabelAgent` differs. An `all` write keeps hitting every agent.

## `GET /v1/admin/agents`

Returns `{"agents":[{"key":"alpha"},{"key":"beta"}]}` sorted by key — keys only,
nothing else from the agent config. Gated like `/v1/admin/scopes`: the caller
must have at least one manageable scope, else `403`.

## Deploy note (NFR-3)

Binds are baked at container create and the proxy has no mount-drift detection
(`Reconcile` adopts running containers as-is). After deploying this change,
managed containers must be removed once so they are recreated with the four
shared binds; `docker rm -f $(docker ps -aq -f label=crab.managed=true)` is
enough. Workspaces, transcripts and uploads live on the host data root, so
removal loses nothing.

`EffectiveSkillsDir` gained a segment, so the pre-feature
`effective-skills/<t>/<s>/` directories are orphaned: nothing writes them and
nothing reads them once containers are recreated. They are a derived cache and
safe to delete at any time; no cleanup runs automatically.

## Test coverage note

`TestCreateAddsReadOnlySecretsBind` — the container test that would exercise the
new bind set — is in the pre-existing group that fails without root (`chown:
operation not permitted`), verified unchanged by this feature via a
stash-and-compare. The bind construction is therefore extracted as the pure
`sharedFileBinds` and asserted by `TestSharedFileBinds`, which needs neither
Docker nor root.
