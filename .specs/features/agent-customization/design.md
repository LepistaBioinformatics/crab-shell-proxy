# agent-customization Design

Builds on `context.md` (CTX-AC-01..04, refined) and `spec.md` (AC-01..10).
Architecture/contracts only. Frontend excluded. All IDs → `spec.md`.

---

## 1. Part A — seed custom template workspace files (AC-01..04, TSW reuse)

`provision.go` today copies the `templateFiles` allowlist (`config.json`,
`.security.yml`) and skips `workspace/`. Extend it with a **workspace allowlist**
copied on first provision only:

```
workspaceSeed = ["AGENT.md", "SOUL.md", "USER.md", "memory/", "skills/"]   # recursive for dirs
NEVER copied: sessions/, logs/, .picoclaw.pid   (isolation + runtime state)
```

- Source: `TemplatesDir(root, agent.Template)/workspace/<entry>`.
- Dest: `UserWorkspace(...)/workspace/<entry>`.
- Absent entries are skipped (partial templates OK, AC-01.3).
- Only when the workspace is first created (the existing `config.json`-absent
  gate), so a returning user's evolved AGENT.md/memory is never clobbered
  (AC-01.4). These files are the agent's to evolve → chowned to picoclawUser
  (writable), unlike the secret sinks below.

Config: a `workspaceSeed []string` on the agent (or a package default) so the
allowlist is explicit and reviewable.

---

## 2. Part B — the per-(user, agent) secret store (AC-03)

A store OUTSIDE the tenant/subscription tree, keyed by `(userAccId, role)`:

```
StoreDir(root, userAccID, role) = <root>/user-secrets/<SanitizeID(userAccID)>/<SanitizeID(role)>/
  ├── .env             (dotenv sink:  NAME=value)
  ├── secrets.json     (json sink:    { "NAME": "value" })
  ├── secrets/<NAME>   (file sink:    one file per secret, content = value)
  └── native.yml       (native overlay: .security.yml slots to merge — see §4)
```

One store per `(user, agent)`; shared by every workspace of that pair. `userAccID
= profile.accId` (CTX-TSW-02). This single location is how the same secret
reaches every subscription's workspace (§3).

---

## 3. Read-only exposure = a RO bind mount (AC-10, and CTX-AC-03 for free)

The generic sinks (`.env`, `secrets.json`, `secrets/`) are exposed to the agent
by **bind-mounting the store read-only** into every container of that
`(user, agent)`:

```
mount (ro):  StoreDir(hostRoot, userAccID, role)  ->  <PicoclawHome>/.picoclaw/workspace/.secrets
```

- **Read-only enforced by the kernel** (`:ro`), not by convention or perms — the
  non-root agent (and even root-in-container) cannot modify/delete/replace them
  (AC-10). Skills read `workspace/.secrets/.env` etc.
- **Per-(user, agent) persistence for free (CTX-AC-03):** the same host dir is
  mounted into *every* workspace/container of that pair, so a single proxy write
  is visible everywhere — no copy/merge, no drift. Supersedes CTX-AC-03's
  "merge into each workspace" for the generic sinks.
- The store dir MUST exist before container create (empty is fine) or the mount
  source is missing → `create` fails. The manager `MkdirAll`s it at create.
- Bind mounts are live (host writes appear in-container immediately), but
  picoclaw reads secrets at **start**, so a change needs a restart to take
  effect (§5) — hence CTX-AC-04.
- Nested mount: `.secrets` sits inside the rw workspace mount; Docker applies the
  RO child mount over that subdir (the mountpoint is created if absent).

**`native` sink is the exception** — `.security.yml` is picoclaw's config file at
a fixed path merged with non-secret config, so it can't be a separate mount. The
`native.yml` overlay in the store is **merged into each workspace's
`.security.yml` at provision/ensure** (the CTX-AC-03 merge path, native-only).
Read-only for native: after merging, set that `.security.yml` `0444`.
**R-A — RESOLVED (T00, 2026-07-16):** picoclaw does NOT rewrite `.security.yml`
at runtime — its mtime stays at provision time across turns (only `workspace/
sessions/` changes), verified on two live workspaces. So native secrets can be
`0444` content-read-only safely. (Delete-resistance for `.security.yml` is
best-effort — it sits in the picoclaw-owned userDir; the strong kernel-enforced
RO applies to the generic sinks via the `:ro` mount, which is what the "env
files read-only" requirement targets.)

---

## 4. Endpoints (`internal/httpapi`) — AC-02, AC-08

All three authorize with the chat chain
(`WithWriteAccess().OnTenant(tenantID).WithRoles([agent.Key]).OnAccount(subsAccID).GetRelatedAccountOrError()`)
on `tenant_id`+`subs_acc_id` from the request; scoped to the caller's own
`(profile.accId, agent.Key)` store. Values are **write-only** — never returned.

- **`POST /v1/secrets`** — body `{ tenant_id, subs_acc_id, format, name, value }`.
  Validate `name` (safe charset) and, for `native`, that the slot/model exists in
  the workspace config (else `400`). Write to the store sink; for `native`, also
  merge into the current workspace's `.security.yml`. Restart the caller's
  container (§5). `200`.
- **`GET /v1/secrets?tenant_id&subs_acc_id`** — parse each sink server-side and
  return names only, grouped: `{ dotenv:[…], json:[…], native:[slots], file:[…] }`.
  Never a value.
- **`DELETE /v1/secrets?tenant_id&subs_acc_id&format&name`** — remove from the
  store (+ the merged `.security.yml` for native); restart.

Handlers reuse `resolveAgent` (service-name + agent token) + `Resolver.Resolve`
+ the account-switching guard, exactly like chat.

---

## 5. Restart-on-write (AC-07)

A new manager method `RestartWorkspace(key WorkspaceKey)`:
- Takes the per-container lock (`keyState.mu`) — serializes with ensure/turn/idle.
- Disarms the idle timer, `Stop`+`Start` the container (if it exists/runs), waits
  healthy, re-arms idle for scale-to-zero. If the container isn't running
  (scaled to zero / never created), it's a no-op write — the next chat cold-starts
  with the new secret (spec edge case).
- The endpoint calls it after the store write.

---

## 6. Apply at provision/ensure (AC-04)

`EnsureRunning`/`create` for a `(tenant, subs, role, user)` workspace:
1. (existing) seed config + workspace allowlist (§1).
2. **native:** merge `StoreDir(...)/native.yml` into the workspace `.security.yml`
   (then `0444` per §3 / R-A).
3. **generic:** ensure `StoreDir(...)` exists; add the RO bind mount (§3) to the
   `CreateSpec.Binds`.

So a brand-new workspace of an existing `(user, agent)` automatically gets the
already-stored secrets (generic via mount, native via merge).

---

## 7. mycelium routes (AC-09, parent repo)

Add `[[picoclaw-*.path]]` for `/v1/secrets` (methods `["GET","POST","DELETE"]`),
`group = { protectedByRoles = [{ name = "<agent>", permission = "write" }] }`
(same as chat — write required to inject), secretName per agent. Lives in the
parent repo's `config.standalone.toml`; needs a gateway rebuild.

---

## 8. Component / file map

| Concern | Location |
| --- | --- |
| `StoreDir` path builder, `workspaceSeed` allowlist | `internal/config` |
| Seed workspace allowlist on first provision | `internal/docker/provision.go` |
| Native merge into `.security.yml` + `0444` | `internal/docker/provision.go` |
| RO secret mount in `CreateSpec`; `MkdirAll` store; `RestartWorkspace` | `internal/docker/manager.go` |
| Store read/write per format + name-only listing | `internal/docker` (new `secrets.go`) or `internal/secrets` |
| `POST`/`GET`/`DELETE /v1/secrets` handlers + authz | `internal/httpapi/handlers.go` |
| mycelium `/v1/secrets` route | `mycelium/config.standalone.toml` (parent) |

---

## 9. Risks & security

- **R1 — write-only is API-level, not at-rest.** The proxy + picoclaw read
  plaintext (must, to apply/use). The store lives in the per-user non-root-
  isolated area (same posture as today's `.security.yml`). At-rest encryption is
  deferred (CTX out-of-scope). Say so; don't imply values are unrecoverable.
- **R2 — RO mount is the enforcement.** Perms alone (root-owned files in a
  1000-owned dir) would let the agent delete/replace via directory write; the
  `:ro` bind is what actually blocks it. Don't downgrade to perms-only.
- **R3 — native `.security.yml` rewrite (R-A).** Verify picoclaw doesn't rewrite
  it before relying on `0444`; otherwise native secrets aren't strictly read-only.
- **R4 — authz reuse.** The `/v1/secrets` chain is the same as chat (write +
  tenant + role + account); a caller can only write their own `(user, agent)`
  store. Staff/manager short-circuit (CTX-TSW-09) — they could write arbitrary
  stores; low-risk (their own elevated access), note it.
- **R5 — restart interrupts a live turn** (CTX-AC-04, accepted). Serialized via
  the per-container lock so it can't corrupt an in-flight ensure.
- **R6 — store dir must exist before mount** or `create` fails — the manager
  `MkdirAll`s it unconditionally at create.
