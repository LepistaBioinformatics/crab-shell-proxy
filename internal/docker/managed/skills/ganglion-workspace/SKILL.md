---
name: ganglion-workspace
description: How this runtime works around you — the one directory you can write in, where a project's files are, what the shell can and cannot reach, and which commands the image actually has. Consult before assuming a path exists, before installing anything, and when a command fails with "permission denied" or "not found".
---

# This workspace, and its edges

You run in the ganglion harness: one small Go process in an Alpine container, with
one tool that touches the filesystem — the shell. Most of what you might assume
from other environments is not here, and the two paragraphs below are the reason.

This file is managed by the operator and cannot be edited; a change you make to it
is discarded on restart.

## The one tree you can write in

Every command runs with the turn's workspace as its working directory, so a
relative path lands where you expect. That directory is also the **outer edge**:
the kernel confines each command to it, so a path above it does not fail a check —
it does not exist as far as the command is concerned.

```
AGENT.md SOUL.md HEARTBEAT.md   who you are (read-only)
USER.md                         who the member is
memory/MEMORY.md                what you have learned, and what you write to
memory/                         other standing notes, including the operator's
skills/                         your skills — see the skill-creator skill
shared-skills/                  the administrator's, read-only
public/                         THE MEMBER SEES THIS. Nothing else.
.shared/tenant/ …               files an administrator published (read-only)
sessions/  windows/             your own transcripts and context; leave them alone
.tool-output/                   large tool output, parked; a message points at a file here
state/                          the harness's own; leave it alone
.tmp/                           scratch; TMPDIR already points here
```

When a command produces more output than fits in the conversation, the result you
see keeps its first and last lines and names a file under `.tool-output/`. The
whole of it is in that file — read it with the shell when you need the middle.
Several turns later the inline part is dropped too and only the path is left; the
file has not moved.

The conversation itself is shortened the same way once it grows: the oldest
messages stop being sent with each turn. They are not lost — `search_history`
searches everything that was SAID earlier in this conversation, including what is
no longer in front of you. It does not cover tool output; that is what the files
above are for.

**There is no `.secrets/` here.** Credentials reach this harness as environment
variables, not as files — so read them from the environment and do not go looking
for a secrets directory. (The `shared-content` skill describes one; that part of it
is about the other harness this platform runs.)

There is no `cron/` either. Your scheduled work is held outside this tree and you
cannot read or write it as a file. The store staying out of reach is the point:
creating a task asks the member first, and a file you could write would not.

**Whether you can schedule anything at all depends on the deployment**, like every
other tool below. If `schedule_create` is in the tool set you were given this turn,
the `scheduled-tasks` skill is there too and explains when to use it — what a
scheduled run reaches (nobody), why each one is approved, and the limits. If it is
not in your tool set, this deployment has no scheduling and saying otherwise to a
member is the failure the last section of this file is about.

**A project is a workspace of its own, beside this one, not inside it.** A turn in
a project starts in that project's directory and cannot reach the main workspace or
any other project — again as a kernel boundary rather than a convention. So do not
look for a project's files under this tree, and do not try to copy something from
one project to another; ask the member to hand it over through their own interface.

A project's own directory holds the same shape, plus `PROJECT.md` — the instructions
the member wrote for that project — and its own `memory/MEMORY.md`, separate from
this one.

## Files left over from the other harness

Some workspaces were run by **picoclaw** before this platform moved to the
ganglion, and a migrated one can still carry that harness's files: a `config.json`
and a `.security.yml` above this tree, a `.secrets/` directory, a `cron/`
directory, and skills describing a browser, `gh`, `tmux` or a Raspberry Pi.

**All of it is deprecated and none of it applies to you.** Those skills describe a
machine that is not this one — see the last section — and the directories are read
by nothing here. The orchestrator moves them into a backup outside this tree as it
finds them, so what you see is transitional.

So: do not act on a skill that assumes tools this image does not have, do not go
looking for `.secrets/` or `cron/`, and do not recreate any of them. If a member
asks about something that only existed under the old harness, say it is not part of
this runtime rather than improvising an equivalent.

## Handing a file to the member

`public/attachments/`, and nowhere else. `workspace/memory/FILE_DELIVERY.md` is the
rule and it is read on every turn; the `shared-content` skill repeats it. Mentioned
here only so this file is not read as a complete map that left it out.

## What the image has

Alpine with busybox, and the harness binary. That is the whole of it.

- **`sh`, not bash.** Busybox `ash`. No arrays, no `[[ ]]`, no process substitution.
- **Busybox applets, not GNU.** `sed`, `awk`, `grep`, `find`, `tar` are the
  smaller implementations; a long GNU-only flag will not be recognised.
- **`wget`, not `curl`.**
- **No package manager you can use.** You are not root and there is no writable
  system prefix, so `apk add` fails. No python, no node, no git, no jq.

So: **check before you depend.** `command -v <name>` costs one call and is the
difference between a plan that runs and a turn spent discovering it cannot. When a
tool you need is genuinely absent, say so plainly and propose what the image can
do instead — do not fake the result, and do not report a step as done because the
command exited quietly.

Anything you fetch, unpack or build lives in the workspace and survives restarts
only if you leave it there; `.tmp/` is scratch and is not a place to keep things.

## Tools other than the shell

Which ones you have **depends on the deployment**, and you can see the real list —
it is the tool set you were given this turn. Reading an image, searching the web,
generating an image, dispatching a sub-agent and writing to the knowledge graph
are each switched on separately by an administrator.

Consult the list you actually have rather than this file. A capability named in a
document is not a capability you possess, and telling a member you searched the web
in a deployment with no search configured is the specific failure this paragraph
exists to prevent.
