# Hermes harness — runtime findings from the live E2E

> **Status: WITHDRAWN.** The Hermes (Nous Research `hermes-agent`) harness was implemented, verified
> working end-to-end against a real z.ai/GLM deployment, and then removed for **current
> infrastructure compatibility**. See the root repo's
> `.specs/features/hermes-removal/DECISION.md` for the decision and
> `.specs/features/hermes-removal/spec.md` for what exactly was withdrawn.
>
> **Recovery:** `crab-shell-proxy` commits `d2f0a9a` (multi-harness support + Hermes),
> `748e0fe` (gate hermes agents on their secrets), `3e9e95c` (model administration rejects
> non-picoclaw agents).

This file exists because `design.md` records what was *intended* and git records what was *written*,
but neither records what was **learned by running it**. Every item below cost debugging time against
a container that booted and died, or a turn that returned nothing. If Hermes is ever re-added, start
here — not from the image's README.

---

## 1. The container will not serve its API unless you override `Cmd`

**Finding:** the image must be started with `Cmd: ["gateway", "run"]`.

The image's default command is the **interactive CLI agent**, not the API server. Under Docker there
is no TTY, so the CLI reads stdin, hits EOF immediately, prints `Goodbye!` and exits — and because
`s6-overlay` is PID 1 and that was its supervised service, **s6 tears the whole container down a few
seconds after boot**.

The symptom is therefore *not* a hang: it is a container that starts, exits on its own, and leaves a
health-wait failing against a container that is no longer running. `gateway run` is the headless
daemon that serves the OpenAI-compatible API on `:8642` and stays up.

Set the `Cmd` explicitly and treat any other value as a misconfiguration.

## 2. There is no API-only mode

Related to §1 but distinct, and it constrains the design rather than just the invocation: the image
has no "serve the API and nothing else" mode. `gateway run` brings up the whole gateway — its own
auth layer, its own user model, its own session store — and the OpenAI-compatible
`/v1/chat/completions` surface is one route on it. You are embedding a *second* multi-user
application inside a per-user container, not attaching to a library.

Every item in §3 and §4 follows from this.

## 3. `GATEWAY_ALLOW_ALL_USERS=true` is required — and the symptom without it is an EMPTY REPLY

**This is the finding most likely to burn an afternoon.** Without it the gateway logs

> `No user allowlists configured. All unauthorized users will be denied`

and then **denies every request, including API-server turns**. It does not answer `401`. The turn
completes, the stream opens, and the reply comes back **empty** — indistinguishable at the proxy from
a model that had nothing to say.

The gateway wants to authorize its own users. The proxy already did that — by the time a request
reaches a Hermes container, mycelium has authenticated the account and the proxy has resolved it to
exactly one per-user workspace. A second authorization layer inside the container has no information
the outer one lacks, and no way to obtain it. `GATEWAY_ALLOW_ALL_USERS=true` disables that inner
layer.

**Why that is not a hole:** the container is per-user. Its only network path in is the proxy, on the
compose network, and the proxy will not route a request there unless it resolved to that user's
workspace. "All users" is a set with one member. The bearer generated per user (§6) is a second
factor on top of that, not the primary one.

**Why it would become a hole:** the moment a Hermes container is shared between users — a
`continuous` instance reused across accounts, a debugging port-forward, a container reachable from
outside `zombie_net` — this flag means *anyone who reaches the port is every user*. If the
per-user-container invariant is ever relaxed, this flag must be revisited **first**.

## 4. The startup deadline is 180s, against a 35s global — and that mismatch is the real blocker

Measured cold start: well past two minutes. The image syncs 71 bundled skills and bootstraps a
Chromium instance under `s6` supervision before the gateway serves its port.

The proxy's global `startupDeadline` is **35s** — tuned for picoclaw, which is ready in a couple of
seconds. Hermes therefore needed a **per-agent** `startupDeadline: 180s` override; this is the reason
`Agent.StartupDeadline` exists at all.

Raising it past mycelium's 60s `gatewayTimeout` is *structurally* fine — chat is streamed, so the
`200` is flushed before the cold start begins and the gateway is not waiting on it. **But see §9:**
the streaming argument saves the cold start and does not save the turn.

## 5. `model.base_url` is mandatory for any OpenAI-compatible provider

The profile's `model` section needs `base_url` explicitly (e.g.
`https://api.z.ai/api/paas/v4` for z.ai). There is no provider registry mapping a provider name to
its endpoint — an unset `base_url` silently falls back to a default the provider does not serve, and
the failure surfaces as an auth or 404 error from the *provider*, not as a config error.

This is why `config.ModelConfig` grew a `BaseURL` field. **That field still exists** after the
removal, because the boot model-registry migration imports it as the registry's `APIBase`
(`migrate_models.go`) — it is empty for every shipped picoclaw agent, which is why the migration
falls back to the template's `model_list` definition.

## 6. The in-container key env name is provider-specific and NOT derivable

The harness reads the provider API key from an environment variable **inside** its container, and the
variable's name depends on the provider — `GLM_API_KEY` for provider `zai`. It is not
`<PROVIDER>_API_KEY`, not `OPENAI_API_KEY`, and not configurable from the profile.

This is the entire reason `ModelConfig.KeyEnvName` existed: the proxy had to be *told* the name,
because there is no rule to compute it from `Provider`. A re-add needs this field back, and needs a
per-provider lookup table that is maintained by hand.

**Also:** the per-user API-server bearer was generated by the proxy and persisted to
`.crab-hermes.json` in the user's data dir (a dotfile the harness ignores, beside its `config.yaml`
under `/opt/data`), so a returning user reuses the same bearer instead of invalidating their session.
It was injected as container env, never written into the profile on disk.

## 7. Everything lives in one flat `/opt/data`

The image keeps config, keys, sessions, skills and memories under a single flat `/opt/data`. The
per-user profile was bind-mounted there wholesale.

This is **structurally incompatible** with picoclaw's layout, and it is why Hermes was excluded from
several cross-cutting features rather than merely unimplemented in them:

- **Persona injection:** `createHermes` emitted **no persona binds at all**. Picoclaw's persona
  cascade mounts read-only identity files at `.picoclaw/workspace`-relative paths that have no
  equivalent in a flat `/opt/data`. The bind-drift recreate check had to exclude Hermes explicitly —
  otherwise `expected` could never be satisfied and the container would be **recreated on every
  single request**.
- **Projects** and **personal models:** both are picoclaw `config.json` constructs (`agents.list` +
  a dispatch rule; `model_list` + `agents.defaults`). Hermes never reads `config.json`, so a project
  or a model selection would be stored, reported as active, and change nothing. Both features gated
  themselves on the harness and answered `501`.

A re-add does not get these features for free. Each one needs a Hermes-native mechanism or an
explicit, documented `501`.

## 8. History: `state.db`, and it was never wired up

Hermes keeps its own transcripts in a **SQLite** `state.db` under `/opt/data`.

**The schema is NOT recorded here, and this file will not guess at it.** The proxy never opened the
file (see below), so nothing in this codebase ever depended on its shape. Inspect it directly on a
re-add — `sqlite3 <userDir>/state.db .schema` inside a provisioned workspace — rather than trusting a
second-hand description.

Session scoping over the wire used two headers, which is why `turn.Request` has both fields:

| Header | `turn.Request` field | Meaning |
|---|---|---|
| `X-Hermes-Session-Id` | `SessionID` | the conversation transcript |
| `X-Hermes-Session-Key` | `SessionKey` | stable per-(user, agent) long-term memory scope |

**The proxy never read `state.db`.** Chat history for a Hermes agent was a known gap (MHS-18), with
two candidate designs: a SQLite reader (needs a driver dependency the proxy did not otherwise have)
or deriving durable history from the passthrough stream. Neither shipped.

`SessionID` and `SessionKey` **remain** in `turn.Request` after the removal, populated but unread —
removing them means removing their populators and is seam surgery, not profile removal.

## 9. The unresolved problem: turn latency

**This is what actually killed it, and it was never solved.**

Turns were slow enough to sit **near mycelium's 60s `gatewayTimeout`**. Not reliably over it — near
it. That is worse than a clean failure: it means a fraction of real turns time out at the gateway
while the container is still working, and the user sees a failed request for a turn that succeeded.

The cold-start argument from §4 does **not** apply here. Streaming flushes the `200` early, which
protects the *cold start*, because nothing needs to arrive before the stream opens. It does not
protect a turn whose **first content token** is late — the gateway is waiting on bytes, and there are
none yet.

Mitigations considered and not implemented:
- Emit a keep-alive/typing frame before the first real token, so the stream is never silent long
  enough to trip the timeout. (`turn.Progress` with `Kind: "typing"` now exists and would be the
  natural vehicle — it was built later, for picoclaw, by `chat-progress-events`.)
- Raise `gatewayTimeout` in mycelium. Rejected: it is global, so it degrades failure detection for
  every other service to accommodate one.
- Map the harness's own `hermes.tool.progress` SSE event to `turn.Progress`. The runner **parsed and
  discarded** this event; wiring it through is the obvious first attempt, and would double as the
  keep-alive above.

**Anyone re-adding Hermes should treat this as the first problem to solve, not the last.** The rest
of this document is configuration that can be copied. This one is a design question.

## 10. Smaller things, in one place

- **API server port is `8642`** (`API_SERVER_PORT`). Unlike picoclaw's, it was hardcoded, not
  configurable — the proxy carried a `hermesAPIPort` constant.
- **The entrypoint starts as root and drops to a non-root `hermes` user via s6**, mapping host
  ownership from `PUID`/`PGID`. Two consequences that are easy to get wrong:
  - **Do NOT set the container's `User`** — that skips s6's `setuidgid` step and breaks the drop.
    Pass `PUID`/`PGID` as env instead.
  - **Do NOT enable docker `--init`** — `s6-overlay` is already PID 1.

  The profile still had to be chowned to that uid at provision time, or the agent could not write its
  own sessions.
- **`applyHermesModel` patched `model.{default,provider,base_url}` in the profile's `config.yaml`**
  while preserving every other key — a YAML round-trip, not a template render, so an operator's own
  edits to the profile survived a re-provision.
- **SSE framing:** only `data:` lines matter (`event:`/`id:`/comment lines are skipped); a line that
  does not parse as an OpenAI chunk is skipped rather than treated as an error, which is how
  `hermes.tool.progress` was discarded. The scanner buffer had to be lifted to 8 MiB — assistant
  answers exceeded `bufio.Scanner`'s default token size.
- **The image is heavy:** 71 skills plus Chromium under `s6`, pulled **per user**. On a
  scale-to-zero deployment this is a per-user cold-start cost, not a one-off.
- **SSE parsing:** deltas arrive as OpenAI-style chunks, terminated by `[DONE]`. Role-only deltas
  (the first frame, carrying `role: "assistant"` and no content) and `hermes.tool.progress` events
  must be **ignored** — treating them as content injected empty strings and, for the progress event,
  raw JSON into the user's answer.
- **`DisabledAgents` was a genuinely good idea worth keeping.** A Hermes agent whose token or
  provider key was unset removed itself from `cfg.Agents` at `Load` rather than failing the whole
  proxy — so a config declaring an agent this deployment is not provisioned for degraded to "that
  agent does not exist" instead of "the proxy will not boot". Both append sites were inside
  `Harness == HarnessHermes` branches, so the mechanism died with the removal. **A re-add will want
  it back.**
