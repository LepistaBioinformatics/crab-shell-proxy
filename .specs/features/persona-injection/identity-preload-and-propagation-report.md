# persona-injection — Identity tab: empty editor + saves not reaching instances

**Date:** 2026-07-30
**Status:** Bug A **fixed** (proxy + webapp). Bug B **fixed** for candidate 1, the
missing mount — the only one of the three that code can address without evidence
from the host, and the only one consistent with "a restart does not help".

## Bug A — the editor opens empty instead of preloading the agent's template

### Symptom

Opening `AGENT.md` at `/admin?agent=alpha&tab=persona` shows an empty editor.

### Cause

Not a defect — a decision, recorded in `persona-panel.tsx`: *"opens an empty editor
when this scope inherits — deliberately NOT prefilled with the inherited content"*.
`Manager.ReadPersona` read **only** the scope's own injection dir, so the normal
state (nothing injected here) meant a blank page, and the first save silently
replaced an identity the admin had never read.

The reasoning behind the old behaviour was real: showing one layer's text as
another's starting point suggests the screen knows what a given workspace ends up
with. The fix keeps that concern instead of discarding it — by labelling the source.

### Fix

- `internal/docker/persona.go` — `ReadPersona` now resolves a **scope-level**
  cascade and returns which layer answered:
  - subscription scope: this scope → the tenant's injection → the agent template
  - tenant scope: this scope → the agent template

  Same precedence `resolvePersonaSources` applies per workspace, minus the
  workspace dimension a scope does not have. `os.ErrNotExist` only when no layer
  has the file, so the handler's 404 branch still means "there is no identity to
  show" — a stricter statement than before.
- `internal/httpapi/admin.go` — `/v1/admin/persona/doc` answers
  `{name, content, source}` with `source ∈ {scope, tenant, template}`.
- Webapp `lib/adminPersona.ts` — `readPersona` returns `{content, source}` (null
  only on a real miss), defaulting `source` to `"scope"` when an older proxy omits
  the field, since only its own injection could reach the editor there.
- Webapp `app/admin/persona-panel.tsx` — preloads the resolved text and, whenever
  it is not this scope's own, shows where it came from and that saving is what sets
  it here. Row badges still come **only** from `listPersona` (scope-only), so
  nothing turns an inherited row into an injected-looking one.
- Webapp — **Save now requires an actual change.** This is load-bearing, not
  polish: preloading makes Edit the only way to *read* a file, so a Save on
  untouched template text would write a verbatim copy as this scope's injection —
  which then wins over the template forever. That is the exact failure the cascade
  exists to remove ("an operator editing the template reached only users
  provisioned after the edit"), one stray click away. An unchanged body cannot be
  saved, which also stops a pointless container bounce.
- Copy added in both locales (`lib/i18n/admin.ts`): `fromTemplate`, `fromTenant`,
  `emptyNothingResolves`.

### Verification

- `TestReadPersonaResolvesToTemplate` — the whole precedence: template preload at
  both scope kinds; a tenant injection reading as `tenant` from the subscription
  scope but as `scope` at the tenant; a subscription injection winning; and a file
  no layer provides still a miss.
- `TestReadPersonaWithoutConfiguredAgentHasNoTemplateLayer` — an unconfigured agent
  gets no template layer rather than a guessed path.
- `TestAdminPersonaDocReportsSource` / `TestAdminPersonaDocMissingEverywhereIs404` —
  the `{name, content, source}` contract the webapp consumes, and the 404 that must
  not degrade into a 500 or an empty 200 the editor would show as content.
- Proxy: `internal/docker` + `internal/httpapi` persona tests pass, `go vet` clean.
  Webapp: 415 tests pass, `tsc --noEmit` clean.
- **Not verified against a real stack** (none available here): that the deployed
  disk template at `data/templates/<template>/workspace/AGENT.md` is present and is
  what the admin expects to see. The bundled default template does ship all four
  files, and the template layer only exists once first provision materialized it.

## Bug B — a saved persona file does not reach the instances

### What is established (from the code)

- `WritePersona` → the scope's persona dir; `SyncEffectivePersonaForScope` →
  re-materializes `effective-persona/<t>/<s>/<agent>/`; the container reads that dir
  through a **per-file read-only bind**.
- `writeInPlace` keeps the destination inode precisely so a rewrite reaches a
  running container live — that part is designed for this.
- **`BounceScope` is stop+start, never recreate** (`shared.go:420`), and container
  binds are fixed at create time (`manager.go:401`). `personaBinds` emits **no**
  bind for a file the effective dir did not hold at that moment, because a bind
  with a missing source makes Docker invent a directory at the destination.

### The three candidates

1. **No bind.** The container was created when the effective file did not exist — or
   by an image predating persona injection (merged in `0060a16`, which is what is
   being exercised for the first time here). No mount, so no rewrite can arrive, and
   a restart cannot add one. *Fix: detect the missing bind and recreate.*
2. **The sync never ran for that workspace.** For a tenant-scope write,
   `SyncEffectivePersonaForScope` enumerates `ListTenantSubscriptions` off disk; an
   empty result means the effective file is never rewritten and the restart re-reads
   an unchanged file. The path layout in `config.go` matches the enumeration, so
   this is the least likely of the three — but it is not excluded. *Fix: the
   enumeration, nothing to do with mounts.*
3. **Delivery works; the agent's session is stale.** The file arrives, but the
   harness read its identity at session start. *Fix: neither of the above.*

### The evidence, and why the fix shipped without it

`.specs/features/persona-injection/diagnose-persona.sh <DATA_ROOT> [agent]`, run on
the container host, separates the three: the injection's mtime, the effective
file's mtime, whether any bind ends in `/workspace/AGENT.md`, and what the agent
reads inside the container. Read-only — it inspects, never changes anything.

- effective mtime NOT updating → candidate 2
- effective updated but no persona bind → candidate 1
- both healthy and the content current inside the container → candidate 3

It was never run. The fix shipped anyway because **drift-detect-and-recreate is a
no-op when there is no drift**: a container whose mounts already match is left
alone, so the change is safe whichever mechanism is live, and it is the only one of
the three that explains "even after restarting the containers" — a restart cannot
add a mount.

Candidate 2 was narrowed by test, not excluded:
`TestSyncEffectivePersonaForScopeReachesTenantWorkspaces` builds the on-disk layout
`config.go` defines and proves a tenant-scope write reaches the subscription's
effective file. That establishes **no logic defect for the layout the code
defines** — it cannot prove the deployed disk matches that layout, and an
environment mismatch would produce the same empty enumeration with the test still
green.

### Fix (candidate 1)

- `internal/docker/client.go` — `ContainerState` now carries `Binds`, decoded from
  the inspect payload's `HostConfig.Binds`. A bind set is fixed at create time, so
  this is the only way to see that a running container predates a mount it needs.
- `internal/docker/persona.go` — `personaBindDrift` compares the container's persona
  mounts against the effective set, with two deliberate choices:
  - **Destinations, never whole bind strings.** A bind embeds `HostDataRoot` and
    `PicoclawHome`; comparing strings would read an operator changing either as
    fleet-wide drift and recreate every container — mass session truncation from a
    settings edit. Covered by a test that moves the host source path and asserts no
    drift.
  - **Equality, not subset**, safe *because* the comparison is scoped to the persona
    destinations: a file no layer provides is in neither set, so it cannot false
    positive. Equality is also what catches the reverse case — see the hazard below,
    now handled rather than deferred.
- `internal/docker/manager.go` — `EnsureRunning` recreates (stop → remove → create →
  start) when it sees drift, and only there: `syncEffectivePersona` runs a few lines
  above and returns on failure, so the effective dir the comparison reads is
  current. In `RestartWorkspace` the same check would be unsafe — no sync precedes
  it, so a transient disk problem would read as "strip the mounts" and rebuild a
  healthy container with no identity files at all.
- **Hermes is excluded by harness, not by luck.** `createHermes` emits no persona
  binds at all, so a hermes workspace with an injection could never satisfy
  `expected` and would be recreated on every single request.

The healing is lazy and **per workspace, per request**: each container is rebuilt on
its own next turn, so an operator watching one user's container will not see the
others fixed until each is used. A partial recovery is the expected shape here, not
a failed fix. The recreate is logged (`persona mounts stale, recreating`) so it can
be confirmed.

The rebuild takes the `createdNow` path, which stamps the restart marker and so
clears that workspace's pending restart notice — the member's banner disappears
without them pressing anything. That is correct rather than a swallowed notice: the
marker is one reason-agnostic timestamp per workspace, and a rebuilt container has
by definition applied every pending change, because the whole pre-create pipeline
(effective secrets, materialize, native overlay, persona sync) runs above it in
`EnsureRunning` — the same reasoning the cold-start stamp already relies on.

### Verification

- `TestPersonaBindDrift` — no drift when mounts match; a missing mount; a container
  with no persona mounts at all (the pre-feature case); a stale mount whose source
  is gone; nothing-expected-nothing-mounted is not drift; and a changed host source
  path is not drift.
- `TestEnsureRunningRecreatesOnPersonaDrift` — the whole path: a running container
  with no persona mount gets rebuilt, the rebuilt spec carries the AGENT.md mount,
  **and a second request does not recreate again**. A check that never converged
  would truncate the conversation on every turn — worse than the bug.
- Bug-first: with the recreate branch disabled in a copy of the tree, that test
  fails with `removes=0 creates=0`.
- `TestSyncEffectivePersonaForScopeReachesTenantWorkspaces` — the fan-out (above).
- These are chown-dependent and cannot run in the dev sandbox (L-001), so they were
  run **as root inside the proxy image build**, which executes `go vet ./... && go
  test ./...`: `ok internal/docker` — the build is the gate, and it passed with this
  code.

### Not covered

- Candidates 2 and 3. If the symptom survives a deploy **plus one request per
  workspace**, it is one of those, and the diagnostic output is still what settles
  it.
- `docker compose up -d --build` does **not** recreate the per-user containers
  (compose does not own them). The heal happens on each workspace's next request
  after the new proxy image is running.

## Deploy order

Both halves of bug A have to ship **together**. With an old proxy, a non-injected
file still 404s, so the editor opens empty *and* the note says "nothing provides
this file yet" for a file the template does provide — a wrong statement on screen,
not just a missing feature. `docker compose up -d --build` rebuilds both images, so
the compose flow makes this automatic; a webapp-only deploy would misinform.

### The reverse hazard, now handled

Deleting an injection for a file the template also lacks makes
`syncEffectivePersona` remove the effective copy while a running container still
binds it — on its next start Docker recreates the missing source as an empty
**directory** named `AGENT.md` in the workspace root. The earlier note said a subset
comparison would not correct this; equality does, and `TestPersonaBindDrift` covers
that direction.
