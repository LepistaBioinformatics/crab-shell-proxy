# Shipped skills for the ganglion

## The request

> picoclaw has some native skills that come with the default installation. Include those
> skills in the ganglion, replacing them where necessary with skills better adapted to the
> ganglion's context.

## What the survey found

picoclaw's nine ship in this repository, in its workspace template
(`internal/docker/defaulttemplate/picoclaw/workspace/skills/`). The ganglion has **no
template at all** — `manager.go` skips `ensurePicoclawTemplate` for it deliberately, since
a picoclaw `config.json` and a `.security.yml` would be two files nothing reads. So there
was no place for a ganglion default to come from, and a ganglion agent had never been
given a shipped skill.

Held against what a ganglion container actually is — `alpine:3.21`, busybox, one static
binary, one filesystem tool — most of the nine cannot survive the move:

| picoclaw skill | Fate | Why |
|---|---|---|
| `agent-browser` | dropped | needs the CLI and a browser; the image budget is why the previous harness was replaced |
| `github` | dropped | needs `gh` |
| `tmux` | dropped | needs tmux |
| `hardware` | dropped | I2C/SPI on Sipeed boards; nothing in this stack |
| `weather` | dropped | `web_search`/`web_fetch` subsume it, and both are deployment-gated |
| `summarize` | dropped | its value is `web_fetch`, which is gated — see the gating rule below |
| `deliver-file` | **already shipped** | `managed/memory/FILE_DELIVERY.md`, bound into both harnesses and read every turn |
| `skill-creator` | **ported** | the SKILL.md format is picoclaw's unchanged, so one file serves both |
| `picoclaw-agent` | **replaced** | by `ganglion-workspace` |

So the honest deliverable is two skills and one bug fix — not nine. Reporting that is the
finding; manufacturing seven files about tools this image does not have would be the
failure `ganglion-workspace` itself warns against.

## FR-1 — The contradiction between two shipped documents

`managed/skills/shared-content/SKILL.md` had a section headed *"Giving a file to the user
(`workspace/uploads/`)"* telling the agent to `mkdir -p uploads/attachments`.
`managed/memory/FILE_DELIVERY.md` — bound into the same workspace, read every turn — says
`public/attachments/` and has a section headed *"`uploads/` is the old name"* explicitly
telling the agent **not** to create one.

`uploads` is `config.LegacyPublicDirName`: only the one-time migration should still name
it, and a file written there is invisible to the member on **both** harnesses. Corrected,
and pinned against `config.PublicDirName` rather than against the literal so a second
rename cannot leave these documents behind again.

## FR-2 — `skill-creator`, for both harnesses

Ported from picoclaw's 358-line version, cut to what is true here. Its centre is the thing
that decides whether a skill is ever used: **only the name and description reach a turn's
prompt**, the bodies stay on disk and are read with the shell. So the description is the
skill, and a long body costs nothing until it is opened.

Also states the collision rule — a name present in both the workspace and the
administrator's read-only root resolves to the administrator's — because an agent writing
a skill named after one it can see would be shadowed without being told.

Ungated: both harnesses read the same format (`crab-ganglion-harness/internal/skillfile`
says so in its own package comment) and every workspace has a skills directory.

## FR-3 — `ganglion-workspace`, replacing `picoclaw-agent`

What the runtime is, for an agent inside it: the one writable tree and the fact that its
edge is a kernel boundary rather than a check; a project as a sibling workspace it cannot
reach from here; `public/` as the only thing the member sees; and what the image holds —
busybox `ash` and not bash, busybox applets and not GNU, `wget` and not `curl`, no usable
package manager, no python, no node, no git, no jq.

It closes on the rule that makes the rest safe: **consult the tool list you were actually
given.** Reading an image, searching the web, generating one, dispatching a sub-agent and
writing to the knowledge graph are each switched on separately by an administrator.

## DEC-1 — The managed tree, not a new mechanism

`internal/docker/managed/` already ships operator content: embedded in the binary,
materialised once, bind-mounted **read-only** into `workspace/skills/<name>` and
`workspace/memory/<name>` for both harnesses. It had one skill (`shared-content`) and three
memory notes.

Rejected: a `defaulttemplate/ganglion/` (the ganglion is provisioned without a template on
purpose), and a third "builtin" root in the harness's own skill loader (a second mechanism
for what one already covers — and the proxy already ships ganglion-side guidance through
this one).

Consequence worth stating: a managed skill lands in the **workspace** layer, and the
loader resolves a collision in the administrator's favour. So an administrator can shadow a
shipped skill by publishing one with the same name. That is the wanted behaviour — an
operator's instruction outranks a default — and it is recorded here rather than discovered.

## DEC-2 — `harness` gates a mounted file, on the same principle the memory graph does

`managedContentBinds` already omits `MEMORY_ROUTING.md` unless the memory graph is on, with
the reason written beside it: with no token the agent has no `mcp_memory_*` tools, and a
file telling it to prefer them "would be actively wrong — worse than silent."

`ganglion-workspace` gates on the same principle one level up. It describes busybox, a
Landlock-confined shell and sibling project workspaces; for a picoclaw agent that is a
description of the wrong machine, and acting on it is the exact failure the file's own
closing paragraph exists to prevent.

This is also why `summarize` is not here. Its substance is `web_fetch`, which registers only
when a provider is configured — so it would need a third gate, for a skill that adds little
over the agent simply seeing the tool it has.

## What review caught, which is the point of writing these down

The first draft of `ganglion-workspace` listed `.secrets/` in the workspace tree. It does
not exist there: `ganglionWorkspaceDirs` omits it with the reason beside it — credentials
reach this harness as environment variables. The directory had been carried over from
`shared-content`, a document written for the other harness.

That is the exact failure the skill's own closing paragraph warns about, in the skill that
warns about it, and it is pinned now: a test reads `ganglionWorkspaceDirs` and fails if the
directory ever appears without the document being updated to match.

`shared-content`'s own secrets section was corrected at the same time and for the same
reason — it is mounted into both harnesses and described only one of them. It now names
both shapes and says how to tell which one you are in.

## Not changed, deliberately

`defaulttemplate/picoclaw/workspace/skills/` still holds all nine of picoclaw's skills,
untouched. They are picoclaw's, they are correct for the image picoclaw runs in, and
"replace where necessary" is about what the GANGLION gets — not about taking working
skills away from the harness they were written for.
