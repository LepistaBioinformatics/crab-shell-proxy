# agent-customization Tasks

Gate: `docker build --network=host .` (go vet + go test ./...) as today; runtime
via hand-crafted profiles hitting the proxy `:18080` directly + inspecting the
workspace/store inside the container. Frontend excluded. `[P]` = parallelizable.

---

## Phase 0 — Blocking verification

### T00 — Verify picoclaw does NOT rewrite `.security.yml` at runtime (R-A)
- **What:** confirm whether a running picoclaw ever writes back `.security.yml`.
  Inspect the image / run a turn and watch the file (mtime/owner). Decides whether
  `native` secrets can be `0444` read-only (AC-05/AC-10) or must stay writable.
- **Done when:** the answer + its consequence is written into design §3 (R-A).
- **Depends on:** —

---

## Phase 1 — Part A (independent, small)

### T01 — Seed workspace allowlist on first provision — AC-01
- **What:** in `provision.go`, after the config seed, copy `workspaceSeed`
  (`AGENT.md`, `SOUL.md`, `USER.md`, `memory/`, `skills/` — recursive) from
  `TemplatesDir/workspace/` into `UserWorkspace/workspace/`, only on first
  create, never `sessions/`/`logs/`/`.picoclaw.pid`; skip absent entries; chown
  to picoclawUser (agent-writable). Add `workspaceSeed` to `internal/config`.
- **Done when:** unit test: fresh dir gets the allowlist (dirs recursive), absent
  entries skipped, `sessions/` never copied, returning user not re-seeded.
- **Depends on:** —

---

## Phase 2 — Part B store & formats

### T02 — `StoreDir` builder + store scaffolding — AC-03
- **What:** `config.StoreDir(root, userAccID, role)` =
  `<root>/user-secrets/<u>/<role>/` (SanitizeID each). Ensure-exists helper.
- **Done when:** unit test for the path; created 0700, chowned appropriately.
- **Depends on:** —

### T03 — Secrets store module (per-format write + name-only list + delete) — AC-02,06,08
- **What:** a `secrets` unit: write/delete a `{format,name,value}` into the store
  (`dotenv` → `.env`, `json` → `secrets.json`, `file` → `secrets/<name>`,
  `native` → `native.yml` overlay); `ListNames()` returns names per format parsed
  server-side (NEVER values); name-charset validation; native slot must exist.
- **Done when:** unit tests: round-trip write→list returns names not values;
  each format's file shape correct; invalid name/unknown native slot rejected;
  delete removes the entry.
- **Depends on:** T02  (native slot-check depends on T00's conclusion)

---

## Phase 3 — Orchestration

### T04 — RO secret mount + native merge at provision + RestartWorkspace — AC-04,05,07,10
- **What:** manager `create` adds a **read-only** bind
  `StoreDir → <PicoclawHome>/.picoclaw/workspace/.secrets` (MkdirAll the store
  first); provision merges `native.yml` into the workspace `.security.yml`
  (`0444` per T00); new `RestartWorkspace(key)` (per-container lock, disarm→
  stop→start→healthy→re-arm; no-op if not running).
- **Done when:** faked-Docker tests: CreateSpec has the RO bind; store dir
  ensured; RestartWorkspace serializes + re-arms; native merged.
- **Depends on:** T00, T02  (+ existing manager from tenant-scoped-workspaces)

---

## Phase 4 — HTTP surface

### T05 — `POST`/`GET`/`DELETE /v1/secrets` — AC-02,08
- **What:** three handlers: resolveAgent + profile + account-switching guard +
  the write-access chain (403); `POST` writes store + native merge + restart;
  `GET` returns names grouped by format (no values); `DELETE` removes + restart;
  400/401/403 mapping like chat.
- **Done when:** httptest w/ fakes: 200 inject each format, 400 bad name/unknown
  native slot, 403 unlicensed/read-only, 401 no profile; GET returns names only
  (asserts no value leaks); DELETE removes; restart invoked.
- **Depends on:** T03, T04

---

## Phase 5 — Wiring & e2e

### T06 — mycelium `/v1/secrets` route (parent repo) — AC-09
- **What:** `[[picoclaw-*.path]]` for `/v1/secrets`, methods GET/POST/DELETE,
  `protectedByRoles [{name=<agent>, permission="write"}]`, secretName per agent.
- **Done when:** `docker compose config -q` valid; gateway (rebuilt) forwards it.
- **Depends on:** T05  **Note:** parent repo, needs gateway rebuild.

### T07 — End-to-end (direct-to-proxy)
- **What:** custom `AGENT.md`+skill in a template → provisioned workspace shows
  them; `POST /v1/secrets` each format → container restarts, `workspace/.secrets`
  is populated and **read-only inside the container** (agent `touch`/`rm` fails);
  a skill reads a value; `GET` lists names only; second subscription of the same
  (user,agent) sees the secret; unauthorized ⇒ 403.
- **Done when:** each spec §Success Criteria observed + logged.
- **Depends on:** T01, T05, T06

---

## Dependency graph

```
T00 ─┬─ T03 ─┐
     └─ T04 ─┼─ T05 ─ T06 ─ T07
T02 ─┴───────┘
T01 (independent) ───────────────── T07
```

Suggested order: **T00 first** (unblocks native RO), then **Part A (T01)** as a
small independent landing, then Part B (T02→T03/T04→T05→T06→T07).
