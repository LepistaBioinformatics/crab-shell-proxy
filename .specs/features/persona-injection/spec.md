# Persona injection (delivery A — proxy)

## Problem

Picoclaw reads its identity from files at the workspace root: `AGENT.md`
(persona + instructions), `SOUL.md` (character), `HEARTBEAT.md` (the recurring
tasks the heartbeat service runs), `USER.md` (what is known about the user).

Today `provision.go:seedWorkspace` **copies** `AGENT.md`, `SOUL.md` and
`USER.md` out of the agent template into the user's workspace, and never touches
them again — `provision_test.go:196` guards a returning user's evolved
`AGENT.md` against being clobbered. The consequences:

1. A user can rewrite the agent's identity and its recurring task list. There is
   no way for an operator to fix either.
2. An operator editing the template reaches only users provisioned after the
   edit. Existing workspaces keep the old copy forever.
3. There is no per-tenant or per-subscription control at all: the template is
   the only lever, and it is global to the agent.

## Solution

An operator injects these files per scope, and the proxy delivers them as
**root-owned read-only bind mounts** over the workspace root. Where nothing is
injected, the agent template's own file is delivered the same way — so the files
are read-only for the user whether or not an admin ever touches them.

`USER.md` is deliberately excluded from the read-only set. See R4.

## Requirements

### R1 — The read-only set

**R1.1** `AGENT.md`, `SOUL.md` and `HEARTBEAT.md` are delivered as individual
read-only bind mounts at `<workspace>/<name>`. The kernel refuses the write; no
edit survives a restart, because the canonical copy is remounted every start.

**R1.2** This applies to PICOCLAW workspaces only (`m.create`). Hermes has its
own provision path and its own template (`config.yaml` + `SOUL.md`); bringing it
in is out of scope and unverified.

**R1.3** A file with no injection AND no template entry is not mounted at all. A
bind whose source is absent makes Docker invent an empty directory at the
destination, which would be worse than the file's absence.

The alternative — always materialize the three, empty where nothing provides
them, so the bind always exists — was considered and rejected. It would make
R3.2 unconditional, but it hands picoclaw an EMPTY identity file to read, which
is a worse failure than a missing one. The template ships all three, so the case
only arises with a deliberately partial custom template, which this codebase
treats as valid (AC-01.3).

### R2 — The cascade

**R2.1** Precedence, NOT merge — two `AGENT.md` files cannot be merged:

```
subscription+agent  →  tenant+agent  →  agent template
```

**R2.2** Two admin layers only, both agent-scoped. Persona is agent identity;
an agent-less layer would mean "the same persona for every agent", which is not
a thing an operator wants, and the agent-first admin console has no way to
address one.

**R2.3** The resolved set is materialized into an effective directory per
`(tenant, subscription, agent)` — the shape `EffectiveSkillsDir` already uses.
Persona does not vary per user, so the user is not part of the key.

### R3 — Update semantics

**R3.1** The proxy writes effective persona files **in place** (`O_TRUNC`), never
by rename. A file bind pins the inode: a rename would leave the container reading
the old content forever.

**R3.2** With in-place writes, editing an already-mounted file reaches the
container LIVE — no restart, no recreate. This mirrors the effective-secrets
discipline.

**R3.3** Introducing a file that was not previously mounted needs a container
recreate, because the bind did not exist. Adding `HEARTBEAT.md` to the template
(R5) makes all three exist for every picoclaw workspace, so in practice every
subsequent edit is live.

### R4 — `USER.md` is a seed, not a lock

**R4.1** `USER.md` stays WRITABLE and is not bind-mounted.

**Why:** the agent accumulates what it learns about the user there — the template
ships it as a form (Preferences / Personal Information / Learning Goals).
Mounting it read-only would silently disable that write.

**R4.2** An injection at either admin layer sets the content `USER.md` is SEEDED
from on first provision, replacing the template as the seed source.

**R4.3** After the first provision the file belongs to the agent and is never
overwritten — the existing no-clobber behaviour is preserved for `USER.md`
alone.

### R5 — `HEARTBEAT.md` joins the template

**R5.1** A default `HEARTBEAT.md` is added to
`defaulttemplate/picoclaw/workspace/`, with the content the operator supplied:
an instruction block plus an empty task list under
`Add your heartbeat tasks below this line:`.

**R5.2** It is NOT added to `WorkspaceSeed` — it is delivered by mount, like
`AGENT.md` and `SOUL.md`.

**R5.3** Note the consequence: the template invites the reader to append tasks
below that line, and under R1.1 nobody can. That is deliberate — who schedules
recurring agent work is an operator decision with direct cost implications, and
an admin can still inject a `HEARTBEAT.md` carrying the tasks they want run.

### R6 — `WorkspaceSeed` loses two entries

**R6.1** `WorkspaceSeed` becomes `["USER.md", "memory/", "skills/"]`.

**R6.2** `AGENT.md` and `SOUL.md` are no longer copied. Copying a file the mount
always shadows is dead work, and a stale copy would resurface the moment a mount
went away.

### R7 — Admin API

Registered beside the skills routes in `handlers.go`, same scope-query shape,
same restart-policy handling.

| Method | Path | Body / params | Returns |
|---|---|---|---|
| GET | `/v1/admin/persona` | scope query | `{files:[{name,size,modifiedAt}]}` — injected AT THIS SCOPE only |
| GET | `/v1/admin/persona/doc` | scope query + `name` | `{name,content}` |
| POST | `/v1/admin/persona` | form: scope, `name`, `body` | 204 |
| DELETE | `/v1/admin/persona` | scope query + `name` | 204 — drops the injection, the next layer takes over |

**R7.1** `name` must be one of the four. Anything else is a 400 — the endpoint
must never become a way to write arbitrary files into a workspace root.

**R7.2** The scope must be agent-scoped (`agent` present and not the all-agents
sentinel). An agent-less persona write is a 400, per R2.2.

**R7.3** POST and DELETE honour the restart policy exactly as the skills routes
do.

**R7.4** Both re-materialize the effective dir for every workspace the scope
reaches BEFORE firing the restart policy.

`EnsureRunning` re-resolves the cascade on every request, so a container would
pick an injection up on its own — but the handler bounces the scope immediately
after the write, and that bounce happens first. Without R7.4 picoclaw boots
reading the previous effective file and holds the stale identity until something
restarts it again.

## Out of scope

- The admin UI. That is delivery B, and it depends on this being deployed.
- Hermes persona (R1.2).
- `memory/MEMORY.md`, which the agent owns and which is not identity.
- Any change to how picoclaw itself reads these files.

## Test impact

Two existing tests assert the behaviour this feature reverses. Both are
**obsolete by design**, not broken:

- `config_test.go:248` (`TestWorkspaceSeedAllowlist`) pins `WorkspaceSeed`
  exactly; R6.1 changes it.
- `provision_test.go:196` asserts a returning user's evolved `AGENT.md` survives
  provisioning. Under R1.1 a user cannot have an evolved `AGENT.md` at all. The
  no-clobber property it guarded moves to `USER.md`, and the rewritten test
  asserts it there.

`default_template_test.go:19-21` enumerates the embedded template files and
gains `workspace/HEARTBEAT.md`.

New coverage: the cascade precedence and the bind set are both pure functions,
testable without Docker or root — the discipline `sharedFileBinds` already
follows.
