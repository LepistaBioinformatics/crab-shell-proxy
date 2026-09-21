# The agent is told who signed in

## The request

> When a workspace starts, the agent simply knows nothing about the user — even though
> the user is signed in. I would like the user's information to be saved in some file
> similar to `USER.md`.

## What the survey found

The premise is exact, and the reason is narrower than "the agent has no memory of
people". **The proxy holds the member's identity on every turn and writes it nowhere the
prompt can see.**

### The identity is already resolved, and already discarded

`internal/identity` decodes the mycelium profile header into an `Identity` carrying
`AccID`, `Email` and the whole `*mycelium.Profile`, whose `Owners[]` have `FirstName`,
`LastName`, `Username` and `IsPrincipal`. `handlers.go:788` and `sse.go:327` have that
value in hand when they start a turn — and pass exactly one field of it onward:

```go
tgt, err := s.Mgr.EnsureRunning(turnCtx, agent, key, ident.Email)
```

The email reaches `.crab-owner.json`, a marker the admin surface reads to label a member.
Nothing else survives, and nothing at all reaches the workspace.

### There are two files called USER.md and they are not the same file

This is the distinction the whole feature turns on, and it is easy to get backwards.

| Path | Who writes it | Is it in the prompt? |
|---|---|---|
| `<workspace>/USER.md` | seeded once from the agent template (`config.WorkspaceSeed`), then owned by the agent | **no** — the ganglion reads nothing from the workspace root |
| `<workspace>/memory/USER.md` | the agent, as it learns | **yes** — both harnesses read the memory directory every turn |

`skills/prompt.go:workspaceSections()` enumerates *every* `.md` under
`<workspace>/memory/` rather than naming files, which is why grepping the harness for
`USER.md` finds nothing while a member's agent can still answer about its contents. The
harness needs no change to read one more file there; that is the seam this feature uses.

### So the gap is the content, not the plumbing

A `memory/USER.md` that exists today says this, because it is what the template says:

```
- **Nome**: A definir
- **Localização**: A definir
```

And a *fresh* ganglion workspace does not have even that: `provisionGanglion` creates
`memory/` and seeds no file into it, while `config.WorkspaceSeed` — the list that copies
`USER.md` — only runs on the picoclaw `provision` path. A member whose workspace has
always been a ganglion starts with an empty memory directory.

## FR-1 — The proxy writes the signed-in identity into the main workspace's memory

On every ensure that carries an identity, the proxy renders what it knows about the
signed-in account as Markdown at `<main workspace>/memory/SIGNED_IN_USER.md`.

The main workspace only, not each project's. `workspaceSections()` reads the main
workspace's memory on every turn *including* a project turn — stated in its own comment —
so one file covers every conversation, and a copy per project would be four files to keep
in agreement for no added reach.

## FR-2 — It is a sibling of `memory/USER.md`, never that file

`memory/USER.md` is where the agent accumulates what it learns about the person. This
file is derived from the login and rewritten whenever it changes. Those are opposite
lifecycles, and merging them destroys the first one: `projects_test.go:623` pins a
regression with the same shape — *"defect: USER.md was in the refreshed set, so every
ensure overwrote it"* — for the workspace-root `USER.md`.

The name says which is which in a directory listing. `SIGNED_IN_USER.md` also states in
its own body that it is proxy-written and that anything the agent learns belongs in
`USER.md` instead, so the agent is not left guessing which file to edit.

## FR-3 — An ensure with no identity leaves the file alone

`reconcile.go:92` restarts containers with no caller and therefore no identity. Rendering
a document from an empty profile would replace a correct file with a blank one at every
restart — the failure would look like the agent forgetting who it is talking to, which is
the bug this feature exists to fix.

No identity means no write, not an empty write. The same applies to the scheduled-turn
path, which carries an email and no profile: it writes what it has and omits the rest.

**A turn with nothing to record says so.** `principalEmail` returns `""` for a profile
with no owners, and a staff profile can pass the authorization chain carrying one — so
"there was nobody to name" is reachable on the interactive path too. It produces the same
symptom as a failed write (*the agent does not know who I am*) and is the one cause
nothing else on that path would report, so it is logged rather than skipped silently.

## FR-4 — The write is confined and readable by the agent

The proxy runs as root and `workspace/memory` is owned by the agent's uid, so a plain
`WriteFile` there follows whatever the agent may have planted as `memory`. The write uses
the same `os.Root`-anchored tree and `chownTree` as `WriteMemory`, which documents this
boundary and exists because of it.

A failure to write is logged and does not fail the turn. The member's answer is worth
more than the note about who asked for it.

## FR-5 — The document states only what the profile carries

Name, username and e-mail, each omitted when absent rather than rendered as an empty
bullet or a placeholder. An agent reading `Name: (unknown)` will repeat it back; an agent
reading nothing will ask. A profile with none of the three is not written at all.

**Not the account id, the tenant or the subscription.** Those are isolation keys, not
things to say to a person: they are in the prompt on every turn of every conversation,
and the only thing an agent can do with a UUID it was handed is repeat it.

Nothing is inferred — no timezone from a locale, no pronouns from a name, no preferences.
Preferences are what `memory/USER.md` is for, and an invented one is worse than a missing
one because the member has no way to see where it came from.

## FR-6 — Both harnesses pick it up with no harness change

The memory directory is read every turn by the ganglion (`workspaceSections()`) and by
picoclaw. This feature therefore ships in this repository alone; there is no harness
counterpart and no version coupling between the two.

The contract is already guarded on the other side:
`crab-ganglion-harness/internal/adapter/skills/project_prompt_test.go`,
`TestEveryManagedMemoryDocumentIsInjected`, seeds a `SOMETHING_NEW.md` — *"a document
nobody hardcoded"* — and asserts it reaches the prompt. That is this file, written before
it existed. Checked rather than assumed: a throwaway probe rendering `signedInDoc`'s exact
output into a workspace's `memory/` and asserting on `Prompt.System` passes unmodified.

## Deferred

- **DQ-1 — no surface to edit it.** The member cannot correct their own display name from
  the webapp, because the content is mycelium's and the fix belongs there. What a member
  *can* already write is `memory/USER.md`, through the agent.
- **DQ-2 — `.crab-owner.json` stays email-only.** Enriching the marker would let the
  admin member list show real names, which is a separate feature with its own surface.
