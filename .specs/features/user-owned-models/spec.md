# user-owned-models — crab-shell-proxy scope

Proxy slice of the cross-cutting feature. The authoritative requirements live in
the parent repo at `.specs/features/user-owned-models/spec.md`; this file records
what changes **here** and the invariants this repo upholds.

## What this repo owns

The personal-model store, the new cascade rung, the connectivity probe, and the
HTTP surface for both the member and the administrator.

## Storage: new buckets, not the `models` bucket

`internal/registry` gains three buckets:

| Bucket | Key | Value |
|---|---|---|
| `user_models` | `<accID>/<slug>` | `UserModel` (definition + key + last test) |
| `user_selection` | `WorkspaceRef.Key()` | `UserSelection{Slug}` |
| `scope_policy` | the `ScopeSel` key already used by `scope_defaults` | `ScopePolicy{AllowUserModels *bool}` |

A personal model is NOT a row in `models`. Every invariant that bucket enforces is
a cross-user invariant — `model_name` unique instance-wide, `fallbacks` and
`replaced_by` resolve inside it, referrer-guarded delete, `position` ordering —
and a personal model participates in none of them. Teaching each of those to skip
owned rows would be five chances to forget one, and the thing forgotten would be a
member's API key materialized into somebody else's workspace. The separate bucket
makes S3 structural: nothing outside the owner can name a personal model, because
no other bucket's names resolve there.

## The cascade rung

`Resolve` consults, before the assignment record:

1. `user_selection[ref]` — the member's choice for this workspace.
2. The model it names, in `user_models[owner/slug]`, `Enabled`.
3. `scope_policy` — most specific of subscription / tenant / agent / global that
   is set; unset everywhere means allowed.

When all three hold, the resolution is:

- `Primary` — a synthesized `Model` whose `ModelName` is `own-<slug>`.
- `Chain` — the model the ordinary cascade resolves, if any (parent R5). A
  cascade that resolves nothing is NOT fatal here: a member with their own key
  can run on an instance whose defaults are unset, which is the one case the
  `ErrNoModelResolvable` refusal cannot help with anyway.
- `Level` — `LevelUserModel` (`"user_model"`), a new level. Reusing `LevelUser`
  would make the ladder in the admin UI report a personal model as an admin pin.

`own-` is a **reserved prefix**: `CreateModel`/`UpdateModel` reject an inventory
model whose name starts with it, so the synthesized name can never collide with a
real one inside a materialized `model_list`.

## The assignment record must not be clobbered

`RecordMaterialization` used to receive `(ref, modelName, chain)`. With a personal
model resolved, writing `own-<slug>` into `Assignment.ModelName` would be a live
bug in two directions:

- The record preserves `Source: explicit`. An admin pin would come back as a pin
  naming a model the inventory does not have, and `candidateTx` treats that as a
  **hard failure** — the member's next deselect would leave an unbootable
  workspace.
- The pinned model name itself would be lost, so deselecting could not restore it.

So `Assignment` gains `UserModel string` (`<owner>/<slug>`), and
`RecordMaterialization` takes the whole `Resolution`: `ModelName`/`Chain` keep
describing the **inventory** side (the cascade model, which is also the runtime
fallback, so `WorkspacesUsing` still reaches this workspace on a key edit), while
`UserModel` records what is actually primary.

## HTTP surface

Member routes (role-gated; write enforced in-proxy, mirroring `/v1/projects`):

| Method | Route | Notes |
|---|---|---|
| `GET` | `/v1/models/mine` | own models (no keys) + selection + effective policy + the provider picker |
| `POST` | `/v1/models/mine` | register |
| `PUT` | `/v1/models/mine?slug=` | edit; absent `api_key` keeps the stored one |
| `DELETE` | `/v1/models/mine?slug=` | delete; clears the selection if it pointed here |
| `POST` | `/v1/models/mine/test` | probe an unsaved draft |
| `PUT` | `/v1/models/mine/selection` | select `own` or the organisation's model |
| `DELETE` | `/v1/models/mine/selection` | back to the cascade |

Admin routes (under the existing `/v1/admin/*` gateway block, `HasAdminPrivileges`
for the policy, authority-over-scope for the listing):

| Method | Route | Notes |
|---|---|---|
| `GET` | `/v1/admin/user-models` | every personal model under a subscription |
| `PUT` | `/v1/admin/user-models/status` | enable / disable one |
| `GET` `PUT` `DELETE` | `/v1/admin/model-policy?scope=…` | the R7 lock |

Every mutation ends in `ReapplyModelUser(key, bounce=false)`: re-materialize, then
raise the per-workspace restart notice. No forced bounce (parent R8).

## The provider picker carries the endpoint

`GET /v1/models/mine` returns each registerable provider with the `api_base` and
model names the embedded catalog knows (`docker.SuggestionCatalog`), so the
member's form can fill the endpoint in rather than asking them to know it.

This is not convenience. The catalog exists because free-text definition fields
made typos the normal failure mode for the ADMIN form; the member's form was
shipped with the same free-text fields and reproduced it immediately. A base URL
missing its version path (`https://integrate.api.nvidia.com` instead of
`…/v1`) reaches a real host and answers 404, which the probe can only report as
"no chat endpoint there" — indistinguishable, to the member, from having picked
the wrong provider.

A catalog read failure degrades to bare provider names rather than failing the
request: the member can still type an endpoint, which is what they did before.

## The probe

`internal/httpapi/model_probe.go`. One `POST {api_base}/chat/completions`,
`max_tokens: 16`, matching picoclaw's `openai_compat` provider.

Guards (parent S1/S2), all in the same place:

- scheme must be `https`;
- redirects ARE followed, up to 5 hops, because picoclaw follows them (its client
  sets no `CheckRedirect`, so Go's default applies). Refusing them made the probe
  fail for endpoints the container handles fine — a bare provider domain
  redirecting to its API host, e.g. nvidia's — and a probe that disagrees with the
  real request is worse than no probe. A hop that leaves `https` is still refused:
  the key rides on the request. The `Authorization` header is not re-attached
  across hosts, matching what Go (and therefore picoclaw) does;
- a `DialContext` hook re-checks **every** resolved address at connect time and
  refuses loopback / private / link-local / unique-local / metadata. This — not
  the redirect rule — is the SSRF boundary: it runs on every connection the client
  makes, redirect hops included, so a permitted host that redirects inward is
  refused when the hop is dialled. Checking at dial rather than before it is also
  what closes the DNS-rebinding window;
- the address check also refuses the forms that smuggle an IPv4 address inside an
  IPv6 one — 6to4, Teredo, NAT64 — which `net.IP`'s own predicates do not unwrap,
  plus CGNAT. `2002:7f00:0001::1` is 127.0.0.1 and `64:ff9b::a9fe:a9fe` is the
  metadata address; both passed until CodeQL's SSRF alert sent us back to look.
  Refused outright rather than unwrapped: no provider is reachable only through a
  transition mechanism, so there is nothing to lose and no arithmetic to get
  wrong;
- both outbound requests (the completion and the model listing) build their URL
  through the same validator. The listing used to be string concatenation, valid
  only because its single caller happened to validate first;
- 15s timeout, 8 KiB read cap, no body echoed;
- one probe at a time per account, and a minimum interval between probes.

## A 404 has two causes, and the endpoint is asked which

A chat completion answering 404 means either "no such route" or "no such model",
and providers answer both identically — NVIDIA does. Reporting the first for the
second is what shipped: a member with a correct URL and a stale model id was told
to check their version path, the one part that was right.

So on a 404 the probe reads `GET {api_base}/models` — the OpenAI-compatible
contract's own listing, the same call `picoclaw model add` makes — and reports
`bad_model` only when the list is readable, non-empty, and does not contain the
id. An unreadable or empty list claims NOTHING and the original message stands:
guessing "your model is wrong" from a gateway that would not answer is the same
mistake in the other direction. The extra request happens only on that path.

Related: the embedded catalog's nvidia entry named `nemotron-4-340b-instruct`,
which that API does not have (its ids are namespaced). It was harmless while the
catalog only prefilled the admin's form; surfacing it to members as a suggestion
made it produce exactly the 404 above. Corrected against the live listing.

## Member-facing errors are CODES, not prose

`userModelErrStatus` (not `registryErrStatus`) answers every member route, and it
emits `api_base_not_https`, `user_models_cap`, `provider_not_allowed`… which
`lib/i18n/errors.ts` resolves into the member's language.

The inventory's admin routes keep returning prose, deliberately: an administrator
reads this proxy's logs, and a code there would be worse. A member does not —
they get "Something went wrong" for every rejection unless the wire carries
something the interface can look up, which is exactly what happens if the
validation prose is left in place. `ErrUserModelLimit` and `ErrUserModelDisabled`
are their own sentinels for the same reason: "you already have 10" and "an
administrator switched this off" need different next actions.

## Not covered by automated tests

The probe against a real provider, and the gateway → proxy round trip. The address
guard, the cascade rung, the store invariants and the reserved prefix are unit
tested.
