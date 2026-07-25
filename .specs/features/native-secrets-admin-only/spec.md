# native-secrets-admin-only — Specification (crab-shell-proxy)

## Summary

The `native` secret format — picoclaw's own `.security.yml` slots
(`web.<provider>` search-provider keys and `model_list.<model>.api_keys`) —
**moves** from the end-user surface to the admin surface. End users keep
`dotenv`, `json` and `file`; only scope administrators inject native credentials,
and they do so per agent, through the shared-secrets cascade added by
[`per-agent-injection-scope`](../per-agent-injection-scope/spec.md).

"Moves", not "blocks": the admin path does not exist today
(`WriteSharedSecret` rejects everything but dotenv/json), so gating the user path
alone would leave `web.<provider>` keys unsettable by anyone.

## History — why the previous attempt was reverted

A webapp-only version of this gate shipped and was reverted
(`crab-exoskeleton-webapp/.specs/features/native-secrets-scope-gate/spec.md`):
native secrets never cascaded from a scope, so blocking non-admins removed the
only way to set or rotate a per-user model API key, stranding users on an invalid
key. This feature is the version that closes that hole — it builds the cascade
first and enforces in the proxy rather than only in the BFF.

Two admin paths now cover what users lose:

| Slot family | Admin path | Reaches a user who never chatted? |
|---|---|---|
| `model_list.<model>.api_keys` | this feature's scope cascade, **or** the existing registered-models apply | cascade: yes (applied at provision). registry apply: no — it needs a provisioned `config.json` |
| `web.<provider>` | this feature's scope cascade | yes |

**Accepted trade-off, stated explicitly:** rotating a personal model key now
requires an administrator. That is the point of the change, and it is the user's
decision; the cascade is what makes it safe.

## Functional requirements

- **FR-1** `POST /v1/secrets` rejects `format=native` with `403`, naming the admin
  surface in the message. The gate lives in the **proxy**, not only in the webapp
  BFF — the previous attempt gated the BFF and left the Go layer open.
- **FR-2** `GET /v1/secrets` keeps reporting the `native` names already present
  in a user's legacy store, and `DELETE /v1/secrets` keeps accepting them.
  **Only writes move to admins.** A delete cannot inject a credential, and
  blocking it would leave a user permanently unable to remove their own stored
  data — the drawer hides the group, so no other affordance would exist.
  Deleting a legacy entry re-applies the remaining cascade, so an admin value for
  the same slot survives.
- **FR-3** `WriteSharedSecret` / `DeleteSharedSecret` accept `format=native`,
  storing a `native.yml` overlay in the scope's secret store (all-agents or
  per-agent, per `per-agent-injection-scope`).
- **FR-4 (slot validation at scope level)**
  - `web.<provider>` is validated against the fixed provider enum. It needs no
    model list, so it is allowed at **any** target including all-agents.
  - `model_list.<model>.api_keys` is validated against the **agent template's**
    `.security.yml` model list, so it **requires an explicit agent target**
    (`400` when the target is all-agents — there is no single model list to
    validate against).
  - `channel_list.*` stays rejected at every tier (the proxy↔picoclaw token).
- **FR-5 (cascade)** `syncEffectiveSecrets` merges the native overlays instead of
  passing the user's through untouched. Precedence, lowest first: user legacy →
  tenant all-agents → tenant this-agent → subscription all-agents →
  subscription this-agent. **Admin values win over the user's** — the inverse of
  the dotenv/json rule, because native is now an admin-owned surface.
- **FR-6** A native write/delete at a scope re-syncs the effective view and
  restarts the affected running containers, via the existing `RestartScope`
  (which the per-agent feature already narrowed to the targeted agent).

## Non-functional

- **NFR-1** Write-only is preserved: no endpoint ever returns a native value.
- **NFR-2** No migration and no silent breakage: a user's existing `native.yml`
  keeps applying at the lowest precedence. It is no longer writable by that user,
  and the drawer stops offering it.
- **NFR-3** `applyNativeSecrets` keeps merging from the **effective** view, so a
  scope-level native secret reaches a workspace on its next provision or
  stop/start with no recreate.

## Out of scope

- An admin endpoint to enumerate or purge a user's legacy `native.yml` entries.
  They are inert once an admin sets the same slot; a cleanup affordance is a
  follow-up, recorded here so it is not mistaken for done.
- Per-user native writes by an admin (the scope cascade covers the need, and a
  per-user store would reintroduce the pre-provision hole).

## Acceptance criteria (EARS)

- **AC-1** WHEN any caller posts `format=native` to `/v1/secrets` THEN the proxy
  SHALL respond `403` and write nothing, regardless of the caller's tier; a
  `DELETE` of the same format SHALL still succeed.
- **AC-2** WHEN a subscription admin sets `web.brave` at subscription scope for
  agent `alpha` THEN every established `alpha` workspace under that subscription
  SHALL receive it, and `beta` workspaces SHALL NOT.
- **AC-3** WHEN an admin sets `model_list.<model>.api_keys` with an all-agents
  target THEN the proxy SHALL respond `400` and write nothing.
- **AC-4** WHEN a model slot names a model absent from the target agent's
  template model list THEN the proxy SHALL respond `400` and write nothing.
- **AC-5** WHEN both a user's legacy native entry and an admin scope entry set
  the same slot THEN the **admin** value SHALL be the one merged into
  `.security.yml`.
- **AC-6** WHEN a user has a legacy native entry and no admin entry sets that
  slot THEN the legacy value SHALL keep applying.
- **AC-6.1** WHEN an admin deletes a native secret at a narrow scope while a
  broader scope still provides the same slot THEN the covered workspaces SHALL
  end up holding the broader value — including **stopped** workspaces, which
  `RestartScope` never visits. `UnsetNativeSlotForScope` therefore unsets and
  re-applies per workspace rather than relying on the restart pass.
- **AC-7** WHEN any caller targets a `channel_list.*` slot at any tier THEN the
  request SHALL be rejected.

## Status: implemented
