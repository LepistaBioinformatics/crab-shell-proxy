---
name: scheduled-tasks
description: How to schedule work for later — what a scheduled run is and is not, why the member has to approve each one, the limits you will be refused by, and how to list and remove what you have scheduled. Read before calling schedule_create, and whenever a member asks for something recurring, a reminder, or work at a particular time.
---

# Scheduling work for later

You can ask to be given a message again at a time you choose, or repeatedly. The
proxy holds the schedule and starts a turn when it is due — waking your container
if it had stopped.

Three tools: `schedule_create`, `schedule_list`, `schedule_delete`.

## What a scheduled run actually is

**It is a fresh turn with the message you stored, and it reaches nobody.** Nothing
you write during it appears in any conversation. The member reads it in the Tasks
panel, if they go there.

That changes what a useful message looks like. Write the message as an instruction
to yourself, with everything the future turn needs in it — it will not have this
conversation's context. "Summarise the week's notes and write it to
`public/weekly.md`" works; "do that thing we discussed" does not.

It also means a scheduled task is a poor way to tell someone something. If the
member wants to be told, the task has to put it somewhere they will look.

## The member approves each one

`schedule_create` stops and asks. The member sees what would run and when, and
allows or refuses it. Until they answer, nothing happens; if they are not there,
it is refused after a few minutes and you are told nobody answered.

So call it when a member has asked you for something recurring — not on your own
initiative, and not to give yourself a reminder they did not request. A refusal
comes back with their reason when they gave one; relay it rather than trying again
with different arguments.

An approval buys exactly one task. Two tasks means asking twice.

`schedule_list` and `schedule_delete` do not ask. Reading your own schedule grants
nothing, and removing a task you no longer need is the direction that reduces
standing work.

## What you will be refused for

- **Shorter than fifteen minutes.** Every fire may start a stopped container, so a
  short interval is a way of keeping yourself running. If a member genuinely needs
  something more frequent, say so and let them arrange it.
- **More than twenty tasks in this workspace.** Remove one first — `schedule_list`
  shows what is there.
- **More than five new tasks in an hour.**
- **A one-off in the past**, and an invalid cron expression.

Each refusal comes back as text you can read and explain. None of them is an error
you should retry.

## The arguments

`kind` picks how `when` is read:

| `kind` | you also send | example |
|---|---|---|
| `cron` | `expr`, a 5-field expression, and optionally `tz` | `"0 7 * * 1"` with `tz: "America/Sao_Paulo"` |
| `every` | `everyMs` | `3600000` for hourly |
| `at` | `atMs`, Unix epoch milliseconds, in the future | one-off |

`message` is required. `name` is a short label the member sees in the panel; it is
taken from the message's first line when you omit it. `deleteAfterRun` removes the
task once it has run — usually what you want for a one-off.

Without a timezone a cron expression is UTC. If a member says "every Monday at
7am" they mean their own morning, so ask which zone or use one you already know
from the conversation.

## Removing

`schedule_list` gives you the ids. `schedule_delete` takes one. Past run
transcripts stay where they are — a task that ran and was then removed is an
ordinary end state, not a gap.
