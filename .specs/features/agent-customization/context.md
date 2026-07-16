# agent-customization — Discussion Context (gray-area decisions)

Captured during Specify (discuss). Two related capabilities on crab-shell-proxy:
**(A) custom agent template files** and **(B) user secret injection**. Frontend
is OUT OF SCOPE for this work (the user edits it in a separate agent); we expose
endpoints + a documented contract only.

## Grounded picoclaw facts (inspected from the real template `data/templates/alpha`)

- **`.security.yml` is picoclaw's native secrets store.** Top-level keys:
  `channel_list.<channel>.settings` (e.g. `pico.settings.token`),
  `model_list.<model>:0` (per-model `api_keys`), `web.<provider>`
  (brave/tavily/kagi/gemini/perplexity/glm_search/baidu_search — web-search
  keys), `skills.registries`. The proxy already writes this file (`provision.go`
  `applyModel` sets the pico token + model api_keys).
- **`config.json`** holds non-secret config: `session, version, isolation,
  agents, evolution, channel_list, model_list, gateway, events, hooks`.
- **Workspace files** (`data/templates/alpha/workspace/`): `AGENT.md` (persona —
  frontmatter `name`/`description` + instructions), `SOUL.md`, `USER.md`,
  `memory/MEMORY.md`, `skills/<name>/SKILL.md` (a skill = a dir with `SKILL.md`).
- **Current provisioning** copies ONLY `config.json` + `.security.yml`
  (`templateFiles` allowlist); it deliberately EXCLUDES `workspace/` so sessions
  never leak — which means the template's AGENT.md/skills/memory currently never
  reach a user's workspace.

---

## CTX-AC-01 (Part A): custom templates = operator files + allowlist seed, per agent

**Decision:** the operator places customization files under
`data/templates/<agent>/workspace/`; provisioning seeds an **allowlist** of them
into a fresh user workspace: `AGENT.md`, `SOUL.md`, `USER.md`, `memory/` (incl.
`MEMORY.md`), `skills/`. It NEVER copies `sessions/` (isolation), nor runtime
state (`logs/`, `.picoclaw.pid`). Per-agent (the template is per-agent); **no
endpoint** — templates are operator config, not user-editable.

**Why:** lets the agent start customized beyond picoclaw's native default,
reusing the existing template-clone path; the sessions-exclusion invariant is
preserved.

---

## CTX-AC-02 (Part B): user-selectable secret sink format (multi-format)

**Decision (refined 2026-07-16):** the injection request carries a `format`
selector; the caller chooses where the secret lands so it matches whatever their
consuming skill reads. Supported sinks (proposed set — confirm/trim at design):
- `dotenv` → `.env` (`NAME=value` lines) in the workspace.
- `json` → `secrets.json` (`{ "NAME": "value" }`) in the workspace.
- `native` → the picoclaw `.security.yml` structured slots: `web.<provider>` +
  `model_list.<model>.api_keys` the proxy already manages. **`channel_list` slots
  are rejected (400)** — impl narrowing 2026-07-16: `channel_list.pico.settings.
  token` is the proxy↔picoclaw WS auth token, so the whole family is blocked to
  stop a user severing that connection. Non-pico channel-token injection is a
  possible follow-up (allow `channel_list.<ch>.settings.token`, `ch != pico`).
- `file` → `secrets/<NAME>` (one file per secret, content = value) — the
  mounted-secret-file convention.

There is **no single convention** — the proxy offers the sinks; the skill reads
the format the user chose. `native` is the only one with a fixed schema (the
slot must exist in the config).

**Why:** the user asked for `.env` "e outros" with the choice at injection time.

**Write-only + list-names solution:** values are NEVER returned by any endpoint.
`GET` enumerates only the **names/keys** by parsing each sink server-side (left
of `=` for dotenv, JSON top-level keys, set `.security.yml` slots, filenames
under `secrets/`). The value is necessarily readable by the proxy (to write/
apply) and picoclaw (to use), but the API's max disclosure is the name — a true
write-only-over-API store. (A model where even the proxy can't read is infeasible
— picoclaw needs the plaintext to use it.)

---

## CTX-AC-03 (Part B): persistence scoped to (user, agent), NOT the workspace tuple

**Decision:** an injected secret persists for the **(user, agent)** pair — it
follows the user across **any** subscription of the same agent (role), not just
the one workspace it was injected from. Implementation: a per-`(userAccId, role)`
secret store kept OUTSIDE the tenant/subscription tree (e.g.
`data/user-secrets/<userAccId>/<role>/`), applied (merged into `.security.yml` +
the generic store) to each `tenants/…/agents/<role>/users/<userAccId>` workspace
at provision/ensure time.

**Why:** the user's explicit choice ("disponível nas sessões futuras do usuário
desde que seja o mesmo agente"). More complex than per-tuple (needs the external
store + apply-on-ensure), accepted deliberately. `userAccId = profile.accId`
(consistent with CTX-TSW-02).

---

## CTX-AC-04 (Part B): injection restarts the agent container immediately

**Decision:** `POST` (inject/update a secret) writes the store, applies it to the
caller's current workspace, and **restarts that user's agent container** so
picoclaw re-reads it immediately (predictable "injected ⇒ live" UX). A turn in
flight is briefly interrupted. Other workspaces of the same (user, agent) pick it
up on their next provision/ensure.

**Why:** picoclaw reads `.security.yml`/workspace at start; a restart is the
deterministic way to make an injected secret take effect now.

---

## Endpoint contract (for the separate frontend agent — management UI beside chat)

- `POST /v1/secrets` — inject/update. Body:
  `{ tenant_id, subs_acc_id, format: "dotenv"|"json"|"native"|"file", name, value }`
  (`name` = the `.env`/json key, the `secrets/` filename, or — for `native` — the
  slot, e.g. `web.brave` or `model_list.<model>.api_keys`). Authorized by the same
  chat chain (`WithWriteAccess().OnTenant().WithRoles([agent]).OnAccount()`).
  Writes to the `(userAccId, role)` store under the chosen format, applies to the
  current workspace, restarts the container. Returns `200`.
- `GET /v1/secrets?tenant_id&subs_acc_id` — list set secret **names only, never
  values**, grouped by format (`{ dotenv:[names], json:[names], native:[slots],
  file:[names] }`), parsed server-side from each sink.
- `DELETE /v1/secrets?...&format=&name=` — remove one. Same authz.
- Values are write-only over the API (never returned) — the UI shows names +
  "set/replace/clear", never the secret material.

---

## Out of scope / confirmed

- **Frontend** — built separately by the user; we deliver endpoints + this
  contract only.
- Encrypting the per-user secret store at rest — deferred; the store lives in the
  per-user non-root-isolated area, same posture as today's `.security.yml`
  (plaintext token/api_keys). Flag as a follow-up if stronger at-rest protection
  is wanted.
- Editing agent templates via API/UI (CTX-AC-01 keeps them operator files).
- Changing picoclaw itself, or how it parses `.security.yml`/skills.
