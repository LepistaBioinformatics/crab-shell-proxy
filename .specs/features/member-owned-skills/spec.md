# Member-owned skills

## The request

> Implement listing, viewing and editing a member's own skills from the webapp. Today
> that is impossible for a member. Remember that the skills the proxy injects and/or
> the administrator publishes are read-only.

Scope was confirmed with the owner as full CRUD over the member's own layer: create,
view, edit and delete. The read-only layers are listed but never written.

The webapp half of this feature is
`crab-exoskeleton-webapp/.specs/features/member-owned-skills/spec.md`.

## What the survey found

Three skill layers reach an agent. Two are manageable, and both are administrator-only.

| Layer | Where it lives on the proxy host | How it reaches the container | Managed by |
|---|---|---|---|
| operator-managed | `<root>/managed-skills/skills/<name>` (embedded in this binary, `//go:embed managed`) | one `:ro` bind per skill onto `<mount>/workspace/skills/<name>` | nobody — redeploy only |
| administrator cascade | four layer dirs merged into `<root>/effective-skills/<t>/<s>/<agent>` | one `:ro` bind (ganglion: `workspace/shared-skills`; picoclaw: `.picoclaw/skills`) | `GET/POST/DELETE /v1/admin/skills` |
| **the member's own** | `<UserWorkspace>/<segment>/skills/<name>` | not mounted — it is already inside the workspace | **nothing** |

The third is the gap. It is written today by exactly two things: the workspace template
seed on first provision (`config.WorkspaceSeed`, picoclaw only — the ganglion is
provisioned with no template), and the harness's own evolution in `apply` mode. A member
can neither see what their agent wrote nor correct it, and cannot write one themselves.

`handlers.go` registers **zero** member-facing skill routes. This is new proxy work, not
a webapp panel over an existing API.

## FR-1 — Four member routes

Registered in the member block of `Handler()`, beside `/v1/memory`:

| Route | Body / params | Answers |
|---|---|---|
| `GET /v1/skills` | `tenant_id`, `subs_acc_id` | `{"skills":[SkillEntry…]}` — all three layers |
| `GET /v1/skills/files` | `+ name` | `{"files":[SkillFile…],"origin"}` — everything in one skill |
| `GET /v1/skills/doc` | `+ name`, `path?` | `{"name","path","content","file","origin"}` |
| `PUT /v1/skills` | JSON `{name, path?, content, modifiedAt?}` | `{"status":"ok","name","path","file"}` |
| `DELETE /v1/skills` | `+ name` | `{"status":"deleted","name"}` |

`path` is relative to the skill's own directory and defaults to `SKILL.md`, so the two
document routes keep their original shape for a caller that does not know about
supporting files.

Authorization is the member chain, not the admin one: `resolveSecretCaller` then
`authorizeSecret`, yielding a `WorkspaceKey` whose `UserAccID` comes from the mycelium
profile and never from the request.

`authorizeSecret` — the *write* chain — on the reads too, matching `/v1/memory` beside
them rather than the read-only twin `authorizeRestartRead`. Stricter than a read needs,
and the same licence the member already needs to say anything to this agent at all. A member can only ever address their own workspace.

This is not negotiable and is why the admin handlers cannot simply be re-scoped:
`handlers.go:499-501` records that there is deliberately no admin route that reads or
writes member content (FR-7 of the admin feature). Member skills are member content.

`PUT` rather than `POST` multipart, matching `/v1/memory` rather than
`/v1/admin/skills`: the member surface writes one document, has no zip upload and no
restart policy to carry.

## FR-2 — Every entry carries its origin, and only one origin is writable

`SkillEntry` is `docker.SkillMeta` plus two fields:

```go
Origin   string `json:"origin"`             // "member" | "shared" | "managed"
Shadowed bool   `json:"shadowed,omitempty"` // see FR-3
```

`origin != "member"` is refused on `PUT` and `DELETE` with 403, following the
`ErrMediaReserved` precedent (`media_folders.go:43`, "that folder is managed by the
system"). The webapp uses the same field to decide whether a row offers an edit control.

## FR-3 — A shadowed member skill must say so

The harness resolves a name collision **in the administrator's favour** and logs the
workspace copy as shadowed (`crab-ganglion-harness/internal/adapter/skills/skills.go`,
`TestBothRootsLoadAndTheAdminsWinsACollision`). A member editing a shadowed skill is
editing a file that does nothing, and today nothing would tell them.

`Shadowed` is computed the way the harness computes it — **by frontmatter name**, not by
directory name. The proxy's own cascade dedups by directory (`skills.go:451`); the two
rules differ and the harness's is the one that decides what the agent sees.

## FR-4 — Member writes are validated against what the harness will actually load

A write that stores a file the harness silently ignores is worse than a rejection.

- Name: the existing `skillNameRe` (`^[a-z0-9][a-z0-9._-]{0,63}$`).
- Frontmatter: `name` and `description`, both non-empty. **Other keys are allowed**, and
  an earlier draft of this spec forbade them. That was wrong, and checking cost one
  command: picoclaw SHIPS skills carrying `metadata:` and `homepage:` — seven of the
  nine in `defaulttemplate/picoclaw/workspace/skills/` — so its loader plainly accepts
  them. The two-key rule belongs to picoclaw's *evolution* validator
  (`validateAppliedSkillBody`), which governs what an agent may write about itself, not
  what a loader will read. Enforcing it here would have stopped a picoclaw member from
  saving any edit to the skills they were seeded with.
- The frontmatter `name` must equal the directory name. Without this, a file in `foo/`
  declaring `name: skill-creator` shadows or duplicates an operator skill, because the
  harness dedups by the declared name. It also makes FR-3 a directory lookup.
- Body cap: reuse `s.Cfg.MediaMaxBytes`, 413 past it.

## FR-5 — A skill is a directory, and all of it is reachable

A skill may carry templates, scripts and notes beside its `SKILL.md`
(`SkillMeta.HasFiles` already said so and nothing could act on it). A member who cannot
see the template an administrator's skill tells the agent to fill in cannot tell what
that skill will do.

- Listing and reading serve **all three layers**; writing serves only the member's own.
- A supporting file is **not** held to the `SKILL.md` grammar. A template has no
  frontmatter, and validating one against it would defeat the point.
- `path` is validated to a plain relative path — no absolute, no `..`, no backslash, no
  empty or `.` segment. The kernel is still the guarantee (every access goes through the
  confined root); this is the *message*, the division `media_root.go` already draws.
- A file that is not valid UTF-8, or holds a NUL, comes back **labelled** `binary` with
  its content withheld, so the panel says "not editable here" rather than offering to
  overwrite an image with a lossy decoding of itself.
- Bounded: 256 KiB per file, 500 files per listing.

## DEC-0 — The DIRECTORY is the identity; the declared name decides shadowing only

A skill's directory name and its frontmatter `name` need not agree. Writes through this
API refuse a disagreement (FR-4), but nothing else does: `skill-creator` teaches the
agent to write a skill with the shell, and picoclaw's template seeds nine of them.

- The **directory** is what a read, a write and a delete resolve, so it is what a listing
  publishes.
- The **declared name** is what the harness indexes and dedups by, so it is what
  shadowing is decided on — on *both* sides of the comparison.

The first implementation published the declared name and resolved by directory. Every
such row 404'd the moment it was opened — falling through to the administrator's layer,
where it also did not exist — and could not be deleted either. Every fixture had used the
same string for both, which is why nothing caught it. Pinned now by
`TestARowTheListPublishesCanBeOpenedAndDeleted` and
`TestShadowingComparesDeclaredNamesOnBothSides`.

## DEC-1 — No new store

The member's skills are `<UserWorkspace>/<segment>/skills/`, the directory that already
exists, is already seeded, is already written by evolution and is already the one the
harness loads. A per-user skills store beside `user-secrets/` was rejected: it would need
a bind, and the layer it would duplicate is already inside the workspace.

## DEC-2 — Managed content is read from `ManagedSkillsDir`, never from the workspace

The managed binds target `<mount>/workspace/skills/<name>`, so when a container starts,
runc creates **empty mountpoint directories under those names in the member's own
workspace on the host**. Two consequences that would each produce a silent wrong answer:

- a naive host-side listing of `workspace/skills` reports the managed skills as empty,
  descriptionless entries;
- a host-side write under one of those names lands underneath a read-only bind and is
  invisible to the agent — a save that reports success and changes nothing.

So managed entries are read from `config.ManagedSkillsDir(...)`, and the set of managed
names is derived from the same `rels` slice `managedContentBinds` builds from, so the two
cannot drift.

## DEC-3 — Shared entries come from `EffectiveSkillsDir`, not from the four layer dirs

`EffectiveSkillsDir(root, t, s, agent)` is the merged set this agent actually receives.
Reading the layer dirs instead would list skills that lost the cascade or belong to a
different agent — a catalogue of things that are not in the agent's prompt.

## DEC-4 — `os.Root` confinement, not `filepath.Join`

`internal/docker/skills.go` uses plain `filepath.Join` throughout. That is safe **only
because the admin skill dirs are proxy-owned and no agent can write them**. The member's
`workspace/skills` is agent-writable: the agent can put a symlink there, and joining a
path through it walks out of the workspace.

The pattern to copy is `internal/docker/memory.go` — `workspaceDir` + `openTreeIfExists`
+ `tree.root` + `escaped(err)` — anchored at the **segment root**, for the reason that
file already records: a named component (`memory`, and here `skills`) is itself something
the agent can replace.

**All three layers, not only the member's.** The first implementation read the
administrator's and the operator's trees with plain `os.ReadFile` on the grounds that
they are proxy-owned and nothing can plant a link in them — an admin upload is unpacked
by hardened zip code and the operator's tree is embedded in the binary. CodeQL flagged
both as `go/path-injection`, and it was right to: `name` and `path` come from the
request, `sanitizeSkillName` and `skillFileRel` are the *validators*, and `media_root.go`
states this repository's own division — the kernel is the guarantee, the validator is the
message. A defence resting on "and nobody can place a link there" is one deployment
decision away from being silently wrong. Every layer now resolves the request-supplied
name beneath an `os.Root` opened on the layer's parent directory, pinned by
`TestAReadOnlyLayerCannotBeWalkedOutOfBySymlink`.

## DEC-5 — Main workspace only; the routes take no `project`

`Loader.Workspace` in the harness is the main workspace, fixed at boot. A skill written
into `workspace-<id>/skills` is read by nothing (see the finding below). Offering a
project-scoped skills UI would let a member write inert files.

So the routes do not accept `project` and always resolve the main segment. This is a
deliberate departure from the panel convention — `lib/proxyRead.ts` forwards `project` on
every route it serves, and `workspace-panel-scope.test.ts` pins that a panel effect keyed
on the workspace is also keyed on the project. The skills panel is the exception, and the
webapp spec pins the reverse with a test of its own rather than leaving it to be noticed.

## DEC-6 — The reserved set is every managed name, unconditionally

`sanitizeSkillName` reserves only `shared-content`. Member writes additionally refuse
`skill-creator`, `ganglion-workspace` and `scheduled-tasks`.

Unconditionally — not gated on harness and not gated on the memory-graph flag that today
decides whether `scheduled-tasks` is bound. The flag can be turned on later, and the
failure it would produce then is a member's existing skill silently disappearing under a
new bind.

## DEC-7 — Conditional write, 409 on mismatch

Evolution in `apply` mode writes the same directory. `PUT` carries the `modifiedAt` the
client last read; a mismatch is 409 and the client re-reads.

Evolution is off by default (`Evolution.Enabled` zero-value, and `apply` refuses to boot
without an approver), so this is rare — but the alternative is that an agent-learned
skill vanishes with no trace and no error. `modifiedAt` is already in `SkillMeta`, so the
round trip costs nothing. A create sends no `modifiedAt` and fails if the name exists.

## DEC-8 — No restart, and no restart policy parameters

Admin skill writes carry a restart policy because the cascade is re-materialized and
bound. A member write lands in the live workspace directory, and the ganglion re-reads
its index from disk with a one-second TTL — so the change is in the next turn's system
prompt. A scale-to-zero container is created per turn anyway.

This is asserted for the ganglion and **not** for picoclaw; see DEC-9.

## DEC-9 — Gate on the harness until picoclaw is verified

Everything above about re-read cadence and collision precedence is established for
`crab-ganglion-harness`, whose source is in this chain. picoclaw's loader is not, and
neither its per-turn re-scan nor its collision precedence has been checked.

The routes register for both harnesses — picoclaw's `workspace/skills` is real and
seeded — but the claims that depend on unverified behaviour are held back:

- the `shadowed` flag is computed only for the ganglion;
- the webapp makes **no claim at all** about when a save takes effect.

The second is not what this decision first said. It asked for "takes effect on the next
message" on the ganglion and a restart hint elsewhere, which needs the harness name in
the listing response — a field `GET /v1/skills` does not carry, and whose only other
source in the webapp is a different panel's fetch. Rather than add a cross-panel request
or a field for one sentence, the save confirmation says "Saved." and stops there.

That is the weaker statement and the true one. If picoclaw's re-read cadence is verified
later, the harness field and the sentence arrive together; until then nothing tells a
member something that is right on one runtime and a guess on the other.

## DEC-10 — `ContainerDataRoot`, not `HostDataRoot`

`ganglion.go:131` builds `EffectiveSkillsDir(cfg.HostDataRoot, ...)`, and that path is
valid only inside a bind string. Reads performed by the proxy process itself use
`m.cfg.ContainerDataRoot`, as `memory.go`'s `workspaceDir` does. Copying the bind line
gives ENOENT and an empty read-only section — a wrong answer that looks like "the
administrator published nothing".

## Gateway routes — six blocks, three files

Every new member path needs its own `[[<role>.path]]` block. The gateway enumerates paths
one by one; a missing block fails in a way the panel renders as "no skills", which is
indistinguishable from an empty workspace. This stack has shipped that bug three times
(read receipts, `/v1/mangrove/read`, `/v1/sessions/tool-call`).

| File | Roles |
|---|---|
| `zombie-crab-project/deploy/standalone/config.standalone.toml` | `alpha`, `beta`, `gamma` |
| `zombie-crab-project/deploy/prod/config.base.toml` | `alpha`, `beta` |
| `mycelium-monorepo/dokploy/mycelium-api-gateway/config.toml` | `zcrab` |

Three paths (`/v1/skills`, `/v1/skills/doc`, `/v1/skills/files`), so **nine** role blocks
across the three files. `/v1/skills` lists `methods = ["GET", "PUT", "DELETE"]` and sits
in a `permission = "write"` group, like `/v1/memory` does; the other two are `["GET"]`.

The mycelium gateway reads its config at boot, so merging that repo is not enough — it
needs its own deploy.

## Out of scope, deliberately

- **Binary upload and zip download** on the member surface. Text files inside a skill are
  listed, read and written (FR-5); getting a PNG *in* still needs the admin surface or
  the agent itself.
- **Deleting a single supporting file.** Deleting the skill removes the directory.
- **Project-scoped skills.** DEC-5, and the finding below.
- **Renaming.** The directory name is the identity and the frontmatter must match it.
  Renaming is create-then-delete, done by the member.

## Findings outside this feature

**Skills are unreachable on project turns.** The proxy mounts a copy of the admin cascade
into *every* project workspace (`ganglion.go:125-132`) with a comment stating exactly why:
the agent's Landlock root is the turn's workspace, so an index pointing at the main
workspace would name files a project turn cannot open. But `GANGLION_SKILLS_ROOT` names
only the main-workspace copy and `Loader.Workspace` is the main workspace, so the harness
never reads those per-project binds. On a project turn the model is handed an index of
paths its shell cannot open — the precise failure that comment was written to prevent, one
layout later. No test covers skills on a project turn.

Not fixed here. It is a harness bug, it is what forces DEC-5, and it deserves its own
change rather than being smuggled into a webapp feature.

**`crab-exoskeleton-webapp/.specs/features/shared-skills-management/spec.md` is stale.**
It declares itself BLOCKED and says the proxy registers nothing under `/v1/admin/skills`
and that the admin tab is commented out. All five handlers exist (`admin.go:592-760`) and
the tab is live (`app/admin/tabs.ts:10`). Its own `report.md` already contradicts it.
