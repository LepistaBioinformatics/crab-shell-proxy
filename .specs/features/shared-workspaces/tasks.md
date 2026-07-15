# shared-workspaces Tasks

Verification gate (same as the repo): `docker build --network=host .` must pass
`go vet` + `go test ./...`; runtime checks via a hand-crafted `x-mycelium-profile`
(now including `accId`, `owners[].id`, and `licensedResources`) hitting the proxy
directly. **Planning only — no code in this doc.**

Legend: `[P]` = parallelizable with siblings.

---

## Phase 0 — Confirm the profile shape (blocking, no code)

### T00 — Capture a real injected profile & confirm fields
- **What:** obtain a real `x-mycelium-profile` for an account that is guested
  into another (so `licensedResources` is populated); confirm the JSON keys and
  the `perm` enum values and the `LicensedResources` envelope shape.
- **Done when:** the exact camelCase keys + `perm` values are written into
  design §1 (replacing the "confirm before implementing" note). Decide
  fallback-decoder vs SDK here.
- **Maps:** design §1 / R2. **Depends on:** —

---

## Phase 1 — Identity & authorization

### T01 — Extend identity to OwnerID + IsStaff + Licensed[]  — SW-06, SW-02
- **What:** parse `owners[principal].id`, `isStaff`/`isManager`, and
  `licensedResources[]` into `Identity`; add `CanReadWorkspace` /
  `CanCreateWorkspace` predicates.
- **Done when:** table tests: grant present+verified+perm ⇒ allow; missing /
  unverified / insufficient perm ⇒ deny; staff ⇒ allow; forged/empty ⇒ deny.
- **Depends on:** T00

### T02 [P] — Owner-scoped session key  — SW-09
- **What:** a `SessionKey` variant producing `sha256(accId::ownerId::session_id)`
  for shared workspaces (keep the existing 2-part key for private).
- **Done when:** unit test: shared vs private keys differ; per-owner keys differ;
  deterministic; empty inputs ⇒ empty.
- **Depends on:** T00

---

## Phase 2 — Orchestration

### T03 — "Ensure running only if exists" + shared label/marker  — SW-05, SW-07, SW-11
- **What:** manager mode that starts a stopped container but never *creates* one
  (for the shared chat path); `crab-shell.kind` label; `.crab-owner.json`
  extended with `kind/accId/createdBy*` on shared provisioning.
- **Done when:** faked-Docker tests: missing container ⇒ not-created error;
  stopped ⇒ started; label/marker present on shared create.
- **Depends on:** (existing manager) — independent of T01/T02

---

## Phase 3 — HTTP surface

### T04 — `POST /v1/workspaces` create handler  — SW-01,02,03,04
- **What:** auth (agent+token) → identity (401) → validate `acc_id` (400) →
  `CanCreateWorkspace` (403) → provision+start shared (`201`/idempotent `200`).
- **Done when:** httptest w/ fakes: 201 create, 200 idempotent, 403 no write,
  401 no profile, 400 bad body; shared label requested on the manager call.
- **Depends on:** T01, T03

### T05 — Shared routing on chat + history  — SW-05,06,07,08,09,10,12
- **What:** honor `workspace_acc_id` (chat body / history query): authorize via
  `CanReadWorkspace` (403), require-exists (409), owner-scoped session key,
  route to the shared container; omitted ⇒ unchanged private path; own-account
  edge behaves as private.
- **Done when:** httptest: authorized shared chat routes to shared key; 403
  unlicensed; 409 uncreated; omitted ⇒ private path unchanged; history scoped by
  owner.
- **Depends on:** T01, T02, T03, T04

---

## Phase 4 — Wiring & end-to-end

### T06 — mycelium route for `/v1/workspaces` (parent repo)
- **What:** add the `POST /v1/workspaces` `[[picoclaw-*.path]]` block per agent
  in `mycelium/config.standalone.toml` (monorepo), same security group/secret.
- **Done when:** `docker compose config` valid; gateway forwards the route.
- **Depends on:** T04  **Note:** lives in the parent repo, not this submodule.

### T07 — End-to-end verification (direct-to-proxy)
- **Ceiling:** the mycelium-path e2e still needs role assignment (project M3);
  achievable gate = hand-crafted profiles (with `accId`/`owners[].id`/
  `licensedResources`) hitting the proxy directly.
- **What:** owner POST creates shared container (non-root, `kind=shared`);
  two licensed members chat it → same container, isolated histories, shared
  memory/skills; unlicensed member ⇒ 403; chat to uncreated ⇒ 409; omitting
  `workspace_acc_id` ⇒ private path unchanged.
- **Done when:** each spec §Success Criteria observed and logged.
- **Depends on:** T05, T06

---

## Dependency graph

```
T00 ─┬─ T01 ─┐
     └─ T02 ─┤
        T03 ─┼─ T04 ─ T05 ─ T06 ─ T07
             ┘
```

Parallel: {T01, T02} after T00; T03 independent; T04 after T01+T03.
