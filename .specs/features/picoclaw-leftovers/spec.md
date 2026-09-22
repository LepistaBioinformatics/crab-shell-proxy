# A migrated workspace stops carrying picoclaw's files

## The request

> When the ganglion is running and picoclaw leftovers exist — the native skills,
> picoclaw-exclusive files in the workspace — move them to a backup folder.

## What the survey found, and how it moves the request

### A ganglion-provisioned workspace has no leftovers at all

`EnsureRunning` returns before `provision()` for a ganglion (`manager.go:264-272`):
`provisionGanglion` creates directories and a token and copies no template. The nine
native picoclaw skills reach a workspace through `config.WorkspaceSeed`, which only the
picoclaw path runs. So a workspace created as a ganglion never had any of this.

**The workspaces that do are the MIGRATED ones** — the accounts that were picoclaw before
this deployment moved to the ganglion. That is the whole population this feature serves,
and it is finite and shrinking.

### It cannot live in the harness, which is where it was asked for

The ganglion container mounts `<userDir>/workspace` and nothing above it
(`ganglionWorkspaceBind`), and the comment there states the rule: *"anything the proxy
owns belongs above it, and is now unreachable rather than one `..` away."*

`config.json` and `.security.yml` — the two most picoclaw-exclusive files there are — sit
in `<userDir>`, on the far side of that boundary. A harness-side sweep cannot see them.

What the harness *can* see is `workspace/skills/`, and there it cannot tell a leftover
from a live mount: four managed skills are bind-mounted read-only inside that directory,
and Docker materialises a destination inode even when the bind source is missing
(`persona.go:93-96`), so an empty `skills/ganglion-workspace/` looks exactly like an
orphan. The proxy computes the bind list and therefore knows. **One sweep, proxy-side.**

## FR-1 — Only a positively identified file is moved

Never a blocklist of names. `sweepGanglionProjects` already sets this rule for the
directory it owns, and the cost of a false positive here is a member's work.

| path | why it is picoclaw's |
|---|---|
| `<ud>/config.json` | `harnessConfigFile()` returns `.ganglion-config.json` for a ganglion; this name is picoclaw's alone |
| `<ud>/.security.yml` | `provisionGanglion`'s own doc: *"the picoclaw-only native .security.yml sink is simply unused"* |
| `<ws>/.secrets/` | `ganglionWorkspaceDirs`: *"no `.secrets/`"*; only picoclaw mounts it |
| `<ws>/cron/` | `config.CronFile`: *"picoclaw owns the file; the proxy only reads it."* A ganglion's schedules are `<ud>/.schedules.json` |
| `<ws>/skills/<name>` | one of the template's own skills, **and its `SKILL.md` is byte-identical to the template's** |

### The byte comparison is the load-bearing part

`skills/github` is either template cruft or a skill this member's agent wrote — evolution
writes to exactly that path. A name match cannot tell them apart; an unmodified copy of a
file this repository ships can. **Anything edited is the member's and stays.**

### `skill-creator` is excluded by name

The template ships one and `managedSkillCreatorRel` mounts one read-only at the same path,
to both harnesses. It is the one template name that is also a live mountpoint, and a
byte comparison would not save us: the two files are different, so it would be left alone
today and moved the moment they converged.

## FR-2 — The backup is outside every bind

`<ud>/.picoclaw-backup/<timestamp>/`, mirroring the path each file had.

Above `workspace/`, so the agent cannot read it back — and specifically **not** inside
`memory/`, where `workspaceSections()` reads every `.md` and a backed-up `SOUL.md` would
land straight back in the system prompt it was removed from.

Timestamped rather than a single directory, so a second sweep after a member restores
something by hand does not have to decide which copy is current.

## FR-3 — Confined, and never overwriting

Every path resolves through an `os.Root` anchored at `<ud>`: `workspace`, `skills` and
each leaf are components the agent can replace with a symlink, and this runs as root.
`os.Root.Rename` (Go 1.25) keeps the move inside that boundary.

A destination that already exists is left alone rather than merged or replaced — the rule
`MigrateProjects` states and the reason it gives: *"Both would be guesses about which copy
is current, and the cost of guessing wrong is a member's transcripts."*

## FR-4 — A failure costs a file, never the turn

One path that cannot move does not stop the others and does not fail the ensure. The
member is trying to have a conversation; a leftover config file staying where it has been
for months is not worth refusing that.

Nothing is moved for a picoclaw agent, and an absent path is the ordinary case: a
ganglion-native workspace costs this feature a handful of `stat` calls per ensure.

## Deferred

- **DQ-1 — `logs/` and `.picoclaw.pid` are not swept.** Both are named as picoclaw runtime
  state in three comments, but **no code in either repository reads or writes either**, and
  `ganglion_test.go` actively asserts that a `logs` directory survives the existing sweep.
  Moving something a live test protects is a decision to take deliberately, not in passing.
- **DQ-2 — `SOUL.md` and `HEARTBEAT.md` stay.** The ganglion never reads them, so they are
  picoclaw concepts in a ganglion workspace — but they arrive as read-only bind mounts for
  both harnesses (`PersonaMounted`). The fix is to stop mounting them for a ganglion, not
  to move a live mountpoint out from under a running container.
