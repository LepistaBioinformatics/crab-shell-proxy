# harness-aware-config-catalog — Specification

**Status:** IMPLEMENTED (proxy + webapp).
**Size:** Small — one resolver branch, one new field on `TemplateKey`, one on
`TemplateCatalog`, and the picker copy that reads them.
**Repos touched:**

| Repo | Role |
| --- | --- |
| `crab-shell-proxy` | `GET /v1/admin/scope/config/keys` resolves its catalog by the agent's harness |
| `crab-exoskeleton-webapp` | The key picker labels each suggestion's harness and stops offering a template write that has no target |

---

## Problem Statement

The bulk config screen (`admin-bulk-instance-config`) offers key suggestions in a
searchable `<input list=…>`. The suggestions come from `TemplateCatalog`, which
the proxy builds by reading `<dataRoot>/templates/<agent>/config.json`.

**That file is picoclaw's config shape.** Confirmed on the live volume: it carries
`agents.defaults.allow_read_outside_workspace`, `restrict_to_workspace`,
`steering_mode`, `channels`, `clawhub` and `bridge_url`, among ~470 lines of
others.

The `alpha` agent now runs the **ganglion** (`config.HarnessGanglion`), which reads
a completely different document — the one `ganglionConfigDoc`
(`internal/docker/ganglion_config.go:95`) generates as `.ganglion-config.json`.
None of the keys above appear in it.

So the screen offered an admin a list of keys the runtime does not have. Picking
any of them wrote a field nothing reads, and **nothing on the screen said so**: the
suggestion looked exactly like a suggestion that works, the apply returned 200, and
the setting simply had no effect.

The proxy already knew which runtime each agent orchestrates —
`config.Agent.Harness` (`internal/config/config.go:46-55, 114-118`) — and the
handler that builds the catalog already had the whole `config.Agent` in hand. It
passed only `agent.Template`.

## Decision

**The catalog resolves by harness.** `TemplateConfigKeys` now takes
`(template, harness)`.

- **picoclaw — unchanged.** The template file is still the source, still the
  revision the template write is gated on, and still the only document that
  describes every key such an instance can have.
- **ganglion — the generated document is the source.** There is no template file
  for a ganglion agent and never was, so the catalog is flattened from an exemplar
  rendered by `ganglionConfigDoc` itself. `ganglionCatalogKeys` lights every
  optional branch of that function — a fallback in the chain, every provider in
  `webProviders`, an MCP endpoint — because each of them is omitted from the
  document when its input is empty, and an exemplar built from a bare resolution
  would describe an instance that *has* none of them rather than one that *can*.

Deriving it from the generator rather than from a list maintained beside it is the
point: a hand-written list is a second thing to keep in step, and the first key
added to `ganglionConfigDoc` and forgotten would put the picker straight back to
describing a document the runtime does not have.

### DEC-1 — the flattening is the template path's, unchanged

`appendTemplateLeaves` gained a `harness` parameter and nothing else. The rules it
encodes — arrays are leaves and are never indexed, an empty object is a leaf, a
name no dotted path can address is skipped along with everything under it — are
rules about what the *editor* can address, not about picoclaw, so they hold
identically for the other document. A second flattener would be a second place for
`allowed_hosts.0` to start being offered.

One consequence worth stating: `model_list` is an array, so it flattens to one
managed leaf and the per-model fields under it (`api_base`, `thinking_level`,
`extra_body`) are deliberately not offered. `setPath` could not write them.

### DEC-2 — `TemplateRevision` has no meaning without a template file, and the catalog says so explicitly

The revision exists for exactly one job: gating the opt-in "also write the
template" apply. A ganglion agent has no template for that apply to land in, so the
option must not be offered at all.

The catalog reports this as a field of its own — `templateWritable` — rather than
leaving the client to infer it from an empty `TemplateRevision`. Both failures an
empty string invites are silent ones: a client that does not know the convention
offers a write with nowhere to go, and a client that does read the emptiness cannot
tell "this agent has no template" from "the revision was dropped on the way here".

`Template` is empty alongside it. The agent does still *declare* a template in
`config.yaml`, but **nothing of it ever reaches a ganglion workspace**:
`ensureTarget` skips `ensurePicoclawTemplate` for this harness on purpose
(`internal/docker/manager.go:245`), so that a `config.json` and a `.security.yml`
nothing reads are not seeded beside it. Naming the template here would point the
write at a file this agent is never provisioned from.

The webapp reads an **absent** `templateWritable` as `true`. The direction matters:
every proxy from before this field existed had a template for every agent it knew
about, so reading the omission as "no template" would take the option away from the
picoclaw agents it works for.

### DEC-3 — `Managed` stays `ManagedConfigPaths`, for both harnesses

The ganglion's document is generated in full and rewritten on every ensure, so "the
proxy wrote this" is true of every key in it. Flagging on that reading would mark
every row managed and leave a picker with nothing pickable.

The narrower reading is the one the flag has to keep: `Managed` is what predicts the
**refusal**. `ValidateConfigKey`/`IsManagedConfigPath` is what the apply verbs
enforce, so a flag computed any other way would disagree with the 400 the admin
actually gets — the catalog would be lying about the API in one direction or the
other.

Computed that way, the ganglion catalog splits as follows:

| Key | `managed` | Why |
| --- | --- | --- |
| `model_list` | yes | Registry-resolved; replaced outright on every ensure |
| `agents.defaults.model_name`, `…model_fallbacks` | yes | Same cascade, same writer |
| `tools.mcp.enabled`, `tools.mcp.servers.memory.*` | yes | The proxy's own memory-graph block |
| `tools.web.<provider>.enabled` (×7) | no | Follows the native `web.<provider>` secret slots an admin fills |

Seven pickable rows out of sixteen. That the whole document is nevertheless
generated is a fact about the DOCUMENT rather than about any one row, so it is
disclosed once beside the list (`bulkConfig.generatedDoc`) instead of being smeared
across every row's flag. Same shape as DEC-4 of `admin-bulk-instance-config`: the
sentence changes what the admin knows, not what the screen lets them do.

### DEC-4 — a ganglion key carries no value

`TemplateKey.Value` is documented as the leaf "in the shape it has on disk". For a
ganglion agent nothing is on disk until a container is ensured, so the exemplar
rendered a *shape* and not a default anybody holds. One of its leaves is also the
member's own memory-graph bearer token, which must not be published as a stand-in.

The field is `omitempty` and left nil for ganglion keys. A real JSON `null` leaf
encodes to four bytes and an unset one to none, so the picoclaw path — where `null`
is a value and not an absence — is unaffected.

### DEC-5 — `harness` is on the KEY, not only on the catalog

The two documents share names: `model_list`, `agents.defaults.model_name` and
`tools.web` exist in both. A client labelling a suggestion therefore cannot
attribute it from the key, and a catalog-level field would be right exactly until
the first list that mixes them. It is stamped rather than inferred, because the
ganglion's document is deliberately picoclaw-*shaped* and the two are
indistinguishable by inspection.

The webapp reads an absent `harness` as `"picoclaw"` — the same back-compat reading
`picoclawAgentKeys` already uses in `lib/admin.ts`.

### An admin edit to a ganglion agent DOES survive — the overlay already carries it

Stated here because an earlier draft of this spec claimed the opposite, and the
claim was wrong.

`.ganglion-config.json` really is rendered whole on every ensure, with nothing
merged into it — but that is the reason the **overlay** exists, not a limit that
survived it. `internal/docker/ganglion_overlay.go` (#53) stores an admin's edit as a
flat dotted map in `.ganglion-overlay.json` beside the workspace and above every
bind, and `renderGanglionConfig` (`internal/docker/ganglion.go:495`) applies it over
the generated document on every render. So the generated document stays
authoritative, the admin's delta is explicit and inspectable, and the edit is
re-applied rather than preserved-by-accident.

Both routes reach it: `WriteInstanceConfig` calls `recordGanglionOverlay`
(`internal/docker/instance_config.go:215`), and the bulk apply writes through that
same function (`internal/docker/bulk_config.go:390`) — so "individually and in bulk"
is one code path, not two.

`applyOverlayToDoc` skips managed and invalid keys exactly as `applyConfigOverlay`
does, so the overlay cannot become a way around `ManagedConfigPaths` and the render
never reintroduces a path the apply verbs return 400 for.

The worked example is `agents.defaults.max_tool_iterations` — the per-turn tool
budget `crab-ganglion-harness` reads. It is not managed, so it overlays; it sits
under a branch the renderer also writes, so the merge has to descend rather than
replace; and it reaches the running agent by the ordinary route — changed bytes make
`writeGanglionConfig` report a change, which recreates the container, which re-reads
the file.

### DEC-6 — the route and its auth are untouched

`GET /v1/admin/scope/config/keys` keeps `AuthorizeUserManagement`, its subscription
ceiling (DEC-1 of `admin-bulk-instance-config`) and its audit lines. It is also
deliberately **not** added to `picoclawOnly` in `harness_gate.go`: both harnesses
have a configuration document, so the honest answer for a ganglion agent is a real
catalog rather than a 501.

---

## Out of Scope

| Deferred | Reason |
| --- | --- |
| **Keys the harness reads but the proxy does not generate are not SUGGESTED** | The catalog is derived from `ganglionConfigDoc` on purpose, so it cannot drift from what the proxy writes. But the harness reads keys the proxy never emits — `agents.defaults.max_tool_iterations` is the first — so those are typeable and fully functional (see the requirement above) while being absent from the dropdown. Closing this properly means the harness publishing its own readable-key set as a cross-repo contract, not a second hand-maintained list in the proxy. That is the next slice. |
| Gating `alsoTemplate` server-side for a ganglion agent | The client no longer offers it, so refusing it here would be a new 400 for a request nothing sends. The write is also not a pure no-op at the proxy: a template is named per agent and two agents may share one, so writing it still reaches any picoclaw agent declaring the same name. A refusal would have to be about the CALLER's agent, not the document. |
| Per-model keys under `model_list` | DEC-1. `setPath` cannot address an array index, and offering `model_list.0.api_base` would hand the admin a key no apply could honour. The model inventory is where those are edited. |
| A third harness | `TemplateConfigKeys` branches on `HarnessGanglion` and everything else falls through to the template path, which is the same default `config.Load` applies to an empty `harness`. A harness with neither a template file nor a generator would need a branch of its own. |

---

## Verification

**Proxy.** `go build ./...`, `go vet ./...` clean.

`internal/docker` gained seven tests: that a ganglion catalog ignores a template file
that IS on disk and carries the generated document's keys instead; that it reports
no template to write to while picoclaw still does; that an absent harness reads as
picoclaw; that every key is labelled with its harness; that the exemplar lights
every optional branch of `ganglionConfigDoc` and offers every native web slot; that
`Managed` matches `IsManagedConfigPath` with the list neither wholly managed nor
wholly free; and that no ganglion key carries a value or leaks the exemplar's
endpoint onto the wire.

`internal/httpapi` gained a ganglion agent to the bulk-config fixture and a test
that the handler forwards the agent's harness alongside its template.

`internal/docker/ganglion_overlay_test.go` gained
`TestTheRenderIsByteStableWithAnOverlay`: it renders eleven times with an overlay
carrying `agents.defaults.max_tool_iterations` and asserts identical bytes every
time. A render that is stable without an overlay was already covered; this is the
merged path, where instability would make `writeGanglionConfig` report a change on
every ensure and recreate the container every turn. Verified non-vacuous by
mutation — injecting a per-call value into the merged document fails it.

`go test ./...` passes except `internal/docker`, which has 16 pre-existing
`lchown … operation not permitted` failures across 10 tests. They reproduce
unchanged on `main` and need a privileged runner; the count is the same before and
after this change.

**Webapp.** `./node_modules/.bin/vitest run` — 122 files / 1570 tests, green
(baseline 121 / 1561). `npx next build` succeeds. `lib/i18n/parity.test.ts` is
green with the new `en`/`pt` copy and needed no `SHARED` entry.
