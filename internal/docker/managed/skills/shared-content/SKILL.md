---
name: shared-content
description: Where operator-provided shared files and secrets and the user's own custom memory notes live in this workspace, how to read them, and the rule to never copy secrets elsewhere. Consult before using any shared file or secret, and re-read the user's custom memory when it is relevant.
---

# Shared content: files and secrets

Administrators (tenant and subscription managers) provision files and secrets
that are injected into this workspace read-only. This skill tells you where they
are and how to use them safely. It is managed by the operator and cannot be
edited — any change you make is discarded on restart.

## Shared files (read-only)

Cascaded from the levels above you, most-specific last:

- `workspace/.shared/tenant/` — files shared with every workspace in the tenant.
- `workspace/.shared/subscription/` — files shared with your subscription.

They are mounted **read-only**: read them like any workspace file (open, read,
reference by path). You cannot modify, rename, or delete them. If you need a
changed version, ask a manager — do not attempt to write there.

## User custom memory (`workspace/memory/MEMORY_CUSTOM.md`)

The user maintains their own standing notes for you at
`workspace/memory/MEMORY_CUSTOM.md` — preferences, context, and instructions they
want you to keep in mind. It sits in your `workspace/memory/` dir alongside your
own memory. It may be absent if the user hasn't written anything yet.

**This file changes frequently** — the user edits it directly, at any time,
between turns. So:

- **Re-read the current file** whenever it's relevant to the task. Do **not**
  rely on something you read in an earlier turn or session, and do not cache its
  contents — the version on disk right now is the only authoritative one.
- Treat its latest content as the user's live instructions and give it weight
  accordingly.
- It's a plain markdown file you read like any other; distinct from the agent's
  own `memory/` files.

## Secrets

All secrets live under `workspace/.secrets/` (read-only sinks): `.env`
(`NAME=value` lines), `secrets.json` (`{ "NAME": "value" }`), and `native.yml`
(picoclaw config slots). Load the one your task needs.

This directory already merges, for you, the **shared** secrets provisioned by
your tenant and subscription managers with **your own** secrets — your own value
wins on a name clash. You don't need to look anywhere else; treat every entry
the same and never assume a value is "just shared" and safe to expose.

## Rules — never leak secrets

Secret values are confidential and must never leave their source:

- **Never copy a secret value** into another file, the workspace, memory, a
  note, a skill, a commit, or any artifact you produce.
- **Never echo, print, or log** a secret value, and never include it in a reply
  to the user or a message to any channel.
- **Never send** a secret anywhere except as the specific credential a task
  legitimately requires.
- Reference secrets **by name / by env var**, never by pasting their value.
- `.shared/` and `.secrets/` are read-only by design; do not try to write to
  them or relocate their contents.

If a task seems to need a secret you do not have, ask a manager to provision it —
do not hardcode or fabricate one.
