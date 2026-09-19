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
.tmp/                           scratch; TMPDIR already points here
```

**There is no `.secrets/` here.** Credentials reach this harness as environment
variables, not as files — so read them from the environment and do not go looking
for a secrets directory. (The `shared-content` skill describes one; that part of it
is about the other harness this platform runs.)

There is no `cron/` either. Your scheduled work is held outside this tree and you
cannot read or write it as a file — but you can manage it through the
`schedule_create`, `schedule_list` and `schedule_delete` tools, and the
`scheduled-tasks` skill explains when to. The store staying out of reach is the
point: creating one asks the member first, and a file you could write would not.

**A project is a workspace of its own, beside this one, not inside it.** A turn in
a project starts in that project's directory and cannot reach the main workspace or
any other project — again as a kernel boundary rather than a convention. So do not
look for a project's files under this tree, and do not try to copy something from
one project to another; ask the member to hand it over through their own interface.

A project's own directory holds the same shape, plus `PROJECT.md` — the instructions
the member wrote for that project — and its own `memory/MEMORY.md`, separate from
this one.

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
