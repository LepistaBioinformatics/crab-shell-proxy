# agent-rooted-workspaces — Feasibility Analysis

**Status:** analysis only. No implementation, no spec, no tasks.
**Question:** can the **agent** become the base of the workspace organization tree,
replacing the tenant?

```
today     <root>/tenants/<t>/subscriptions/<s>/agents/<role>/users/<u>/workspace
proposed  <root>/agents/<role>/tenants/<t>/subscriptions/<s>/users/<u>/workspace
```

**Verdict up front:** technically feasible, but not free and not obviously
worth it. There is one **structural blocker** (the all-agents shared scope has
no home under an agent-first root) and one **operational hazard** (a directory
move silently detaches every existing container from its data). Whether the
inversion pays depends entirely on a motivation that has not been stated —
see [Verdict](#7-verdict) and the closing question.

Scope of this doc is the **on-disk workspace tree**. A *conceptual* inversion
(making "agent" a first-class admin scope tier in the API and the webapp) is a
strictly larger, mostly-separate change — see §6.

---

## 1. What is actually rooted at `tenants/` today

The data root is not one tree. `tenants/` is one of eight top-level roots, and
only the first is tenant-first:

| Root | Keyed by | Built at |
|---|---|---|
| `tenants/<t>/…` | tenant → subscription → agent → user | `config.go:485-608`, `621-719` |
| `effective-skills/<t>/<s>/<agent>` | tenant, subscription, agent | `config.go:730` |
| `effective-persona/<t>/<s>/<agent>` | tenant, subscription, agent | `config.go:691` |
| `user-secrets/<u>/<role>` | **user, agent** — no tenant at all | `config.go:740` |
| `effective-secrets/<u>/<role>` | **user, agent** — no tenant at all | `config.go:752` |
| `restart/scopes/<t>/<s>.json`, `restart/workspaces/<t>/<s>/<role>/<u>.json` | tenant-first | `config.go:777, 788` |
| `templates/<template>` | template | `config.go:478` |
| `managed-skills/` | global | `config.go:761` |

Two observations that matter:

- **The per-(user, agent) secret stores are already agent-rooted-ish** — they
  deliberately sit outside the tenant tree (CTX-AC-03: "the same secret reaches
  every workspace of that pair"). Precedent exists in this codebase for a store
  keyed by agent rather than tenant, and it was a *correctness* decision, not a
  cosmetic one.
- **Model scope defaults already have an agent level.** `registry.ScopeSel`
  resolves `subscription → tenant → agent → global`
  (`internal/registry/resolve.go:151-155`), and `LevelAgent` is a BoltDB key
  (`agent/<slug>`), not a filesystem path. The agent already *is* a scope in the
  part of the system where scope is stored as data rather than as directory
  nesting — and it cost nothing there, precisely because it is not a path.

### Coupling surface

| Measure | Count |
|---|---|
| Non-test call sites of the tenant-first path builders | **99** |
| Test call sites of the same | 17 |
| Hard-coded `"tenants"` literals (globs / walkers) | 18 |
| Go files in `internal` + `cmd` | 135 |

The 99 call sites are mostly *mechanical* — they call a `config.*` builder and
never assemble the path themselves. The layout builders are explicitly
documented as "the single source of truth for the on-disk tree"
(`config.go:469`), and that discipline held. **Changing the builders' bodies is
a small diff.** The cost is not in the builders; it is in the 18 literal globs,
the walkers that parse a path back into a `WorkspaceKey`, and the migration.

### Path-parsing walkers (the real code cost)

These reconstruct identity *from* the directory structure and each encodes the
segment order and a hard-coded part count:

| Site | What it does | Breaks how |
|---|---|---|
| `docker/reconcile.go:96-121` | `existingWorkspaces(role)` — glob + `len(parts) != 8` + `parts[1]/parts[3]/parts[7]` | segment indices shift |
| `docker/model.go:163-182` | tenant-wide reapply glob + same index parsing | same |
| `docker/migrate_models.go:299-390` | boot migration: globs over `tenants/*/shared/model.json`, `tenants/*/subscriptions/*/shared/model.json`, and the workspace tree | same, ×3 |
| `docker/shared.go:345-355` | `ListTenants` = `dirNames(root/tenants)`; `ListTenantSubscriptions` = `dirNames(root/tenants/<t>/subscriptions)` | **loses its single-read implementation** — see §5 |
| `docker/drift.go` (`allExistingWorkspaces`) | model-drift check, disk-enumerated | index parsing |
| `restart/restart.go:308-334` | walks `restart/scopes/<t>/<s>.json` | only if the restart root is also inverted |

None of these is hard. All of them are places where a wrong segment index
produces a **silent** wrong answer (an empty list reads as "no workspaces"),
not a compile error.

---

## 2. The structural blocker: the all-agents shared scope

`docker.Scope` is `{Kind, TenantID, SubsAccID, AgentKey}` where **`AgentKey ==
""` means "every agent in this scope"** (`shared.go:28-37`). That is not a
degenerate case — it is the original shape, and `per-agent-injection-scope`
AD-2 deliberately kept it so that nothing already published needed migrating.
It is mirrored in `restart.AllAgents = "*"` (`restart.go:59`).

The stores that live at the all-agents tier have **no location** under an
agent-first root:

```
tenants/<t>/shared/{files,secrets,skills}           ← all agents under T
tenants/<t>/subscriptions/<s>/shared/{files,…}      ← all agents under T/S
tenants/<t>/shared/model.json                       ← tenant model override
tenants/<t>/subscriptions/<s>/shared/model.json     ← subscription model override
```

Under `agents/<role>/tenants/<t>/…`, "content for every agent of tenant T" is
by construction not addressable by a single path. And the merge that consumes
them is ordered:

```
EffectiveSkillsDir merge order (later wins by skill name):
  tenant all-agents → tenant this-agent → subscription all-agents → subscription this-agent
```
(`config.go:721-729`)

That cascade is the feature. There are exactly three exits, and each costs
something real:

### Exit A — Hybrid: invert only the per-user workspace tree

Leave every `shared/` store and both model-override files tenant-first; move
only `agents/<role>/tenants/<t>/subscriptions/<s>/users/<u>/`.

- **Cost:** the data root grows a second parallel top-level root, and the claim
  "the agent is the base of the tree" becomes half-true — admin content is still
  tenant-rooted, only user workspaces are not. Two mental models instead of one.
- **Benefit:** cheapest by a wide margin. The cascade, `authz`, `Scope`, and all
  admin endpoints are untouched. It buys the narrow per-agent globs of §5
  without touching the parts that resist.

### Exit B — Fan-out: materialize all-agents content per agent

Write tenant-all-agents content into every `agents/<slug>/tenants/<t>/shared/`.

- **Cost:** converts a read-time merge into a write-time N-way propagation, and
  N is `len(cfg.Agents)` and changes when config changes. Adding an agent now
  requires back-filling every tenant's shared content into the new subtree, or
  the new agent silently starts with less content than its peers. Every
  shared-content write becomes a fan-out that can partially fail. This is a
  worse consistency story than the current one and I would not recommend it.

### Exit C — Drop the all-agents tier

Require every shared store to name an agent.

- **Cost:** a **product** decision, not a refactor. It reverses
  `per-agent-injection-scope` AD-2, forces a data migration of everything
  already published at the all-agents tier, and removes an operator affordance
  ("this skill applies to all my agents") that currently costs one write instead
  of N.

### 2b. The same problem inside the part Exit A moves: `SubscriptionRoot`

The all-agents shape is not confined to `shared/`. It also sits in the node
Exit A relocates:

```go
func SubscriptionRoot(root, tenantID, subsAccID string) string {
    return filepath.Join(root, "tenants", <t>, "subscriptions", <s>, "agents")
}
```
(`config.go:485`)

This is the parent *of* the agent dirs — the scaffold `POST /v1/accounts`
creates, and it is **deliberately agent-agnostic**: CTX-TSW-04 records it as
"agent-agnostic (it scaffolds a subscription usable by any agent)". Same shape
as `AgentKey == ""`. Under `agents/<role>/tenants/<t>/subscriptions/<s>/users/`
the node has no location, and three call sites lose their meaning:

| Site | What it does today |
|---|---|
| `manager.go:206-211` | `EnsureRunning` opens with `os.Stat(subsRoot)` → `"subscription %s/%s not scaffolded (POST /v1/accounts first)"`. Nothing left to stat. |
| `manager.go:558` | `SubscriptionScaffolded(t, s)` — same stat, **no agent in hand** |
| `handlers.go:726` | the discovery endpoint annotates each licensed tuple with `scaffolded` from that call |

Creating the scaffold under every agent subtree is Exit B, already rejected.

**Resolution, and it must be part of Exit A explicitly:** keep a tenant-first
**registry root** — `tenants/<t>/subscriptions/<s>/` as a bare existence marker
with no `agents` child. `SubscriptionRoot` becomes that marker, the
`EnsureRunning` guard and `SubscriptionScaffolded` read it unchanged, and
`ListTenants` / `ListTenantSubscriptions` keep their single-`ReadDir`
implementations.

This also corrects §5 in the right direction: the `ListTenants` invisibility
problem is **only** a problem without this root. Today a scaffolded-but-unused
tenant *is* visible, because the scaffold itself creates `tenants/<t>/`. Keeping
the marker preserves that; dropping it loses tenants that no user has chatted
under yet — precisely the case CTX-TSW-06 says discovery exists to serve.

The price is that Exit A now leaves **three** parallel roots (`agents/`,
`tenants/` for shared content, `tenants/` doubling as the scaffold registry).
That sharpens the "two mental models" cost rather than removing it.

**There is also an Exit 0** worth naming: if the driver is per-agent operations
(§5), the narrow globs can be obtained *without moving anything* — by keeping
an index, or simply by accepting `filepath.Glob("tenants/*/subscriptions/*/agents/<role>/users/*")`,
which is already what `existingWorkspaces(role)` does and is already
agent-selective. It scans more directory entries; it does not scan more
workspaces.

---

## 3. Migration mechanics — the operational hazard

**This is the finding that most changes the risk profile, and it is verified,
not assumed.**

Container bind mounts are computed **at create time** from the host path:

```go
hostDir := config.UserWorkspace(m.cfg.HostDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
```
(`manager.go:370`)

And the container's **name does not encode the path**:

```go
sum := sha256.Sum256([]byte(key.TenantID + "::" + key.SubsAccID + "::" + key.UserAccID))
return fmt.Sprintf("%s-%s-%s", prefix, SanitizeID(key.Role), hex[:16])
```
(`manager.go:142-151`)

The only drift check that forces a **recreate** is `personaBindDrift`
(`manager.go:302`), which compares *persona* mounts only. Nothing compares the
workspace bind source.

Consequence of an `mv`-only migration:

1. Directories move to the new layout.
2. `Reconcile` adopts running containers by label and re-arms their timers —
   the name is unchanged, so every existing container is adopted as-is
   (`reconcile.go:40-63`).
3. `EnsureRunning` finds `st.Exists == true`, `personaBindDrift` is false
   (persona mounts come from `effective-persona/`, which need not have moved),
   so it takes the `!st.Running → Start` branch. **No recreate.**
4. The container is still bound to the old host path. Binds go out as
   `HostConfig.Binds` — a `[]string` of `"hostPath:containerPath[:ro]"`
   (`client.go:46, 140-141`), the classic form in which **the daemon creates a
   missing host source as an empty directory** rather than rejecting it.

**Net effect: users silently get empty workspaces, and the proxy reports
everything healthy.** No error, no log line, no failed health check — the
harness boots fine against an empty workspace.

So the migration is **not** "mv + restart". It is:

- move the tree (or copy-then-verify-then-delete), **and**
- force a remove+recreate of every existing container, **or**
- add a `workspaceBindDrift` check next to `personaBindDrift` so `EnsureRunning`
  self-heals — the cheaper and more robust option, and one that follows an
  existing pattern in the same switch.

The codebase already has a precedent for a one-shot on-disk migration:
`migrate_models.go` globs `tenants/*/shared/model.json`,
`tenants/*/subscriptions/*/shared/model.json` and the workspace tree, backs up
before its only destructive write, and refuses to proceed when it cannot verify
the backup (`drift.go:50-62`). A layout migration should be built to that same
standard. Note the migration itself would need to run **before** the new
builders are live, or be written against literals rather than the builders.

Secondary migration items:

- `restart/workspaces/<t>/<s>/<role>/<u>.json` — if not migrated in lockstep,
  every workspace reads as "restart pending" (a missing marker is deliberately
  the safe direction, so this is a cosmetic banner storm, not data loss).
- `effective-skills/` and `effective-persona/` are `<t>/<s>/<agent>` in their
  own roots. They are *not forced* to change, and they are regenerated on the
  next ensure, so they can simply be dropped. But leaving them tenant-first
  while the workspace tree is agent-first is exactly the two-mental-models cost
  of Exit A, now in a third place.
- `user-secrets/` and `effective-secrets/` (`<u>/<role>`) need no change under
  any exit.

---

## 4. What is genuinely unaffected

Worth stating plainly so these are not counted as risk:

- **The authorization chain.** `profile.WithWriteAccess().OnTenant(t).WithRoles([agent]).OnAccount(s).GetRelatedAccountOrError()`
  (CTX-TSW-05) is resolved entirely from the mycelium profile and reads no disk.
  `authz.CallerTier` likewise walks `LicensedResources` only
  (`authz.go:40-66`). Directory order is invisible to both.
- **Container names and the container↔identity mapping.** The name hashes the
  identity tuple, not the path; tenant/subscription/user are recovered from
  container labels and `.crab-owner.json`, never from the directory
  (`manager.go:139-151`).
- **The identity contract** `config agent key == <role_slug> == licensed_resources.role`
  (CTX-TSW-07). Unchanged — the same slug, at a different depth.
- **`internal/registry`** — keys are BoltDB strings (`WorkspaceRef.Key()` =
  `<t>/<s>/<agent>/<u>`, `resolve.go:260`), not paths. It would keep working
  untouched, though the key order would then disagree with the disk order,
  which is a small readability tax.
- **`internal/mcptoken`** — encodes the tuple into a token, order-independent
  (`token.go:111`).
- **The webapp.** It sends `tenant_id`/`acc_id` in request bodies
  (`handlers.go:480-520`); it never sees a path.

---

## 5. Which operations invert in cost

This is the substance of the trade, not the file count.

| Operation | Today | Agent-first |
|---|---|---|
| `existingWorkspaces(role)` — reconcile continuous agents | glob `tenants/*/subscriptions/*/agents/<role>/users/*` | **narrower**: `agents/<role>/tenants/*/subscriptions/*/users/*` — same depth, fewer non-matching entries walked |
| Bulk config / model reapply for **one agent** | scattered across every tenant subtree | **one subtree** |
| Delete / back up / quota **one agent** | fan-out across all tenants | **one `rm -rf`** |
| `ListTenants` (Instance-tier scope discovery, `shared.go:346`) | one `ReadDir` | **union + dedupe across every agent subtree**; a tenant with no workspace under any agent becomes **invisible** — unless the §2b registry root is kept, which restores the one-`ReadDir` form |
| `ListTenantSubscriptions` (`shared.go:353`) | one `ReadDir` | same — fan-out + dedupe, or unchanged with the §2b root |
| `PropagateScope` / `BounceScope` for a tenant or subscription (`shared.go:396+`) | single-subtree walk | fan-out across agents |
| Offboard / GC **one tenant or subscription** | one `rm -rf` | fan-out across agents |
| Per-tenant disk accounting | `du tenants/<t>` | fan-out |

**The inversion is a straight trade of tenant-wide operations for agent-wide
ones.** Everything the current layout makes cheap, it makes expensive, and vice
versa.

Two asymmetries tilt the ledger toward the status quo:

1. **The expensive side is the side the admin API is built on.** The admin scope
   model is `tenant | subscription` (`authz.Tier`, `docker.ScopeKind`) — there
   is no agent tier. Every current admin operation is tenant- or
   subscription-shaped, so agent-first makes the *existing* operations slower
   and the *hypothetical* ones faster.
2. **GC gets materially harder, and GC is already on the books.** context.md
   defers "GC on `subscriptionAccount.deleted` / `userAccount.deleted`" as
   out-of-scope-for-now. Under agent-first that deferred work stops being one
   `rm -rf` and becomes a fan-out that must be complete or it leaks a deleted
   tenant's data under some agent subtree. That is a real cost charged against
   work already planned.

---

## 6. The other reading: "agent as a scope tier" (separate question)

If the intent is not the directory tree but the *conceptual* model — an
`agent` scope alongside `tenant` and `subscription`, so an operator can say
"this applies to agent X everywhere" — that is a different and larger change:

- `docker.ScopeKind` gains a third kind; `authz.Tier` needs an answer for "who
  is authoritative over an agent across tenants?" (today, only Instance tier
  could be)
- every admin endpoint's scope parameters change shape
- `crab-exoskeleton-webapp`'s admin UI scope pickers change with them

Note that **the model registry already did exactly this** (`LevelAgent`,
`registry/resolve.go:153`) and it cost almost nothing — because scope there is a
map key, not a path. That is the cheap way to add an agent dimension, and it is
evidence that a conceptual agent scope does **not** require inverting the disk
tree. The two changes are independent; conflating them would make both harder.

---

## 7. Verdict

**Feasible: yes.** No invariant makes it impossible. The layout builders are
correctly centralized, authorization is path-independent, container identity is
path-independent, and the codebase has a working precedent for on-disk
migration.

**Advisable: only under Exit A, and only if the driver is per-agent
operations.**

| | |
|---|---|
| Recommended shape | **Exit A (hybrid)** — invert the per-user workspace tree only; leave shared stores and model overrides tenant-first |
| Required part of Exit A | keep a tenant-first **registry root** (`tenants/<t>/subscriptions/<s>/`, no `agents` child) so the scaffold, the `EnsureRunning` guard, `SubscriptionScaffolded` and both `List*` calls survive unchanged — §2b |
| Must-have safeguard | a `workspaceBindDrift` check beside `personaBindDrift` (`manager.go:302`), or the migration silently empties every live workspace |
| Migration | copy → verify → recreate/self-heal containers → delete old, to the `migrate_models.go` standard (backup, refuse-on-unverifiable) |
| Rough size | ~20 builder bodies, 6 path-parsing walkers, 18 glob literals, ~17 test call sites, 1 migration + 1 drift check + the registry-root split. Large but not Complex — the risk is in the migration, not the refactor |
| Honest cost of Exit A | **three** parallel top-level roots instead of one tree; "the agent is the base" ends up true of user workspaces only |
| Do **not** do | Exit B (fan-out of all-agents content) — it trades a read-time merge for a partially-failable write-time propagation |
| Reconsider as product work | Exit C (dropping the all-agents tier) — reverses `per-agent-injection-scope` AD-2 |
| Cheapest alternative that may already satisfy the goal | **Exit 0** — keep the layout; per-agent globs already exist (`existingWorkspaces(role)`), and a conceptual agent scope is a map key away, as the model registry shows |

### The one question that decides it

The motivation was not stated, and it flips the recommendation:

- **"I want to delete / back up / migrate / meter one agent as a unit"** →
  agent-first (Exit A) wins, and the cost is worth paying.
- **"I want per-agent isolation of shared content"** → *already delivered* by
  `per-agent-injection-scope`; the inversion buys nothing here.
- **"Tenant feels like the wrong root conceptually"** → the cheap answer is an
  agent **scope tier** (§6), not an agent **directory root**.
- **"I want to offboard tenants cleanly"** → agent-first makes this strictly
  worse; keep the current layout.

---

## Sources

All line references are to the working tree at the time of writing.

- `internal/config/config.go:469-800` — layout builders (the single source of truth)
- `internal/docker/manager.go:32-37, 139-151, 190-345, 370` — `WorkspaceKey`, `ContainerName`, `EnsureRunning`, bind assembly
- `internal/docker/shared.go:19-37, 345-355, 396+` — `Scope`, tenant/subscription enumeration, propagation
- `internal/docker/reconcile.go:66-121` — `existingWorkspaces` glob + index parsing
- `internal/docker/model.go:150-182`, `internal/docker/drift.go`, `internal/docker/migrate_models.go:299-390` — disk-enumerated workspace walks; migration precedent
- `internal/restart/restart.go:1-60, 308-334` — restart marker layout, `AllAgents`
- `internal/registry/scopes.go:20-55`, `internal/registry/resolve.go:141-160, 260` — the existing `LevelAgent` scope
- `internal/authz/authz.go:14-80` — tier model, path-independent
- `.specs/features/tenant-scoped-workspaces/context.md` — CTX-TSW-01…09, deferred GC
- `.specs/features/per-agent-injection-scope/design.md` — AD-2 (all-agents tier kept)
