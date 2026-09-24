# A member's secrets had no path to their agent

## The request

> Review the secrets injection. Check the whole cycle — at times they seem not to be
> injected into users' environments. Injection has to let users and admins inject
> secrets and let the agent reach them. Look for faults that make users migrating from
> picoclaw not find the variables they injected into their bots.

## What the audit found

The suspicion was right and the hole is wider than "migration loses them": **there was
no agent-readable secret channel under the ganglion at all.**

| Format | picoclaw agent | ganglion agent (before) |
|---|---|---|
| `.env` | yes — file in `.secrets/` | **no** |
| `secrets.json` | yes — file in `.secrets/` | **no** |
| native `web.*` | yes | harness-internal only, never agent-readable |
| native `model_list.*.api_keys` | yes | **no** — the key comes from the registry |
| `file` | **no** | **no** |

### FR-1 — the two sinks a member writes to were never read

`createGanglion` sets `Env: ganglionEnv(...)`, whose only variable source was
`ganglionSecretEnv(res, web)`: one variable per registry model key, one per `web.`
slot. `web` came from `ganglionWebSecrets`, which keeps only the `web.` prefix. No
other producer, and no `.secrets` bind (`ganglionBinds`).

A member migrating from picoclaw saved a credential, **got a 200 back**, and their
agent could not see it. Nothing anywhere reported the gap.

### FR-2 — and injecting it would not have been enough

`crab-ganglion-harness`'s exec tool scrubs a command's environment to
`PATH/HOME/TERM/LANG/TZ`. The proxy's own comment — *"credentials arrive as
environment"* — was true of the harness **process** and false of the shell it hands
the agent.

That scrub is right and stays. It exists because `GANGLION_API_KEY` (deploy-wide) and
`GANGLION_TOKEN` (also the approver's bearer) were readable with a bare `env`, and it
is an **allowlist on purpose**: the proxy decides what enters the container, so a
denylist would have to be edited in a second repository every time the proxy adds a
variable, and forgetting would make a credential readable with no test failing.

### FR-3 — a revoked secret stayed usable

`ganglionSecretDrift` compared additions and changes only. Docker fixes environment at
container creation, so a deleted credential stayed readable by the agent for the life
of that container. The member presses *restart to apply*, the banner clears, nothing
is applied.

## What it does now

### FR-1.1 — a marked channel, not a wider allowlist

The proxy emits each member secret as `CRAB_SECRET__<NAME>`. The harness passes that
**prefix** through the scrub and strips it, so a tool looking for `DB_URL` finds
`DB_URL`.

A prefix rather than new names keeps the allowlist's principle exactly: the proxy
decides what goes under the marker, and everything outside it — `GANGLION_API_KEY`,
`GANGLION_TOKEN`, the approval endpoint, the passphrase — stays scrubbed with no edit
in the harness. `passThrough` is unchanged, so the agreement test that fails the build
when a `GANGLION_*` name joins it still holds.

Two underscores so the boundary is unmistakable: a secret called `SECRET_FOO` cannot
be mistaken for a marked one.

**AC-1.1.1** — `.env` and `secrets.json` both reach the environment.
**AC-1.1.2** — the marker is stripped before a command sees it.
**AC-1.1.3** — nothing without the marker gets through.
**AC-1.1.4** — the two repositories' constants are asserted equal, in
`TestSecretPrefixMatchesTheHarness`. A rename on either side would otherwise drop
every member secret for every agent with no test failing — the exact shape of the bug
this change exists to fix.

### FR-1.2 — a deleted secret forces a recreate

`ganglionSecretDrift` now counts **removals of `CRAB_SECRET__*`**, and only those. The
asymmetry is deliberate: a key for a model no longer in the chain is a variable nothing
reads — untidy, not worth destroying a running container over — while a revoked
credential the agent can still use is a different finding.

### What is accepted

A command can now read the member's credential, so an agent steered by untrusted text
can exfiltrate it. That is inherent — a secret the agent cannot use is not a feature —
and the owner chose it knowingly. It is the **member's own** credential rather than the
deployment's, which is the distinction the allowlist draws.

## Found and NOT fixed here

- **An admin's `model_list.<model>.api_keys` is written and never read for a ganglion.**
  Only `applyNativeSecrets` consumes those slots, into picoclaw's `.security.yml`.
  Ganglion model keys come from `registry.Model.APIKey`, so a scope admin's
  per-subscription key is silently replaced by the global inventory key — and with none
  in the inventory, every candidate is skipped for "no API key". This is a registry
  design question, not a delivery one.
- **The `file` format reaches no harness.** Accepted, stored and listed; `StoreDir` is
  never bind-mounted and the cascade copies only the other three. A member-visible dead
  end under both harnesses.
- **Stale comments** asserting the opposite of the above, at `ganglion.go:~330`,
  `shared.go:~220` and the harness's `internal/config/config.go` header.
- **Cross-sink `userWins`**: a user's `.env` `FOO` drops an admin's `secrets.json` `FOO`
  from the effective file. Documented intent, left alone.
