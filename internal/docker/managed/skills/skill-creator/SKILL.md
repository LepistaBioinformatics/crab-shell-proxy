---
name: skill-creator
description: Write, revise or review a SKILL.md for this workspace. Use when a workflow has been repeated enough to be worth writing down, when asked to create or edit a skill, or when a skill you consulted turned out to be wrong and should be corrected.
---

# Writing a skill

A skill is a directory holding a `SKILL.md`. It is how a workflow you had to work
out once becomes something you find again — by you on a later turn, and by every
other agent in this workspace.

This file is managed by the operator and cannot be edited; a change you make to it
is discarded on restart.

## What actually reaches you

**Only the name and the description.** Every turn is given an index of the skills
available — one line each — and nothing more. The bodies stay on disk and you read
the one you need with the shell tool.

Two consequences, and they are the whole craft of this:

- **The description is the skill.** It is the only part that decides whether the
  skill is ever opened. Write it as the situation it answers, not as a title:
  "Use when the user asks for a file exported or sent" finds its moment;
  "File utilities" does not.
- **The body can be long, and costs nothing until it is read.** Do not compress a
  procedure into hints to save room. Room is not what you are saving.

The index is bounded, so a workspace with dozens of skills will not fit all of them
— another reason a description that earns its line matters.

## Where a skill lives

```
<workspace>/skills/<name>/SKILL.md      yours, writable
<workspace>/shared-skills/<name>/       the administrator's, read-only
```

A name present in both resolves to the **administrator's** copy. That is a rule
about authority, not a merge order: the other way round would let a file you wrote
replace an instruction a manager published. So do not name a skill after one you
can see in `shared-skills/` expecting yours to win — it will be shadowed and you
will not be told at the moment it happens.

## The file

```markdown
---
name: deliver-file
description: Use when the user asks you to send, export, generate or share a FILE …
---

# Delivering a file

…the procedure…
```

- `name` must match the directory. A mismatch is what the format's own validator
  refuses, on both harnesses this stack runs.
- `description` is one sentence, written as a trigger. Naming the phrases a user
  actually types — in their language, not only in English — is what makes a skill
  findable by the person who needs it rather than by the person who wrote it.
- Everything under the frontmatter is yours: headings, steps, commands, examples.

Supporting material goes beside `SKILL.md` in the same directory — `references/`
for longer notes, `scripts/` for anything executable. Reference them by relative
path from the skill's own directory.

## Before you write one

1. **Read what is already there.** `ls skills/ shared-skills/` and read the
   descriptions. A second skill covering the same ground splits the trigger
   between two lines and makes both less likely to fire.
2. **Check the workflow actually repeats.** A thing done once is a note in
   `memory/MEMORY.md`. A thing done three times with the same shape is a skill.
3. **Write what you verified, not what you assume.** A skill that names a command
   this image does not have, or a path that does not exist, is worse than no skill:
   it is read as fact on a later turn, by an agent with no way to check it against
   the session where it was written. If you are not sure a step works, run it
   first, or say in the body that it is untested.

## Revising one

Correct the file rather than writing a second skill next to it. If a step in a
skill you consulted turned out to be wrong, fixing it is part of finishing the task
that found the error — a wrong skill will be believed again.

Keep a skill's name stable. It is what the index carries and what other skills
refer to.
