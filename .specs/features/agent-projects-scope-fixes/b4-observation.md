# B4 — observed 2026-08-10, on the live stack

**There is no restart. The container never bounced.** The reported "reinicia o bot e
perde a memória" is a text-only model rejecting an image, and the rejection
poisoning the conversation permanently.

---

## The restart question, answered

Every restart path in the stack was checked, in code and against the running
containers:

| Path | Reachable from an upload? |
| --- | --- |
| `EnsureRunning` recreate on `personaBindDrift \|\| projectBindDrift` (`manager.go:289`) | **No** — `EnsureRunning` is called only from the chat path (`handlers.go:631`, `sse.go:97`). `handleMediaPost` → `StoreMedia` touches no container |
| `RaiseWorkspaceRestartNotice` | **No** — raised only by secrets writes (`handlers.go:1032,1106`) and admin config, never by media |
| `POST /v1/restart` | **No** — user-initiated only; the webapp calls it from the restart banner, and `onRestartNeeded` is wired to the secrets drawer alone |
| Scale-to-zero idle stop | Exists, but unrelated to an upload |
| picoclaw filesystem watcher | **Does not exist.** `fsnotify` appears only in `go.sum` (indirect, unused); no `NewWatcher` anywhere in `pkg/` or `cmd/`. The template sets `gateway.hot_reload: false` |

Empirical confirmation, `crabshell-alpha-e5bc87e15c74fc6c`:

```
started=2026-08-10T18:48:57  restarts=0
boot banners, all time: 2026-08-09 01:24, 01:25, 12:21, 2026-08-10 18:48:57
```

The last boot is when the whole compose stack came up (postgres started 18:48:56).
No boot during any of the uploads. **`app/chat/turn-store.ts:68`'s claim that
"picoclaw reloads to pick up the new workspace file" is folklore** — delete it, and
`UPLOAD_SETTLE_MS` with it.

## The filesystem question, answered

Injecting into the container's filesystem over the API without a restart is not
merely possible — **it is already what happens, and always has.** `StoreMedia`
writes into the host directory that is bind-mounted at `<mountDest>` in the
container, so the file is visible to the agent the instant the write returns.
Nothing needs to be reloaded for it to appear. Verified from inside the container:
the uploaded PNGs are readable at
`/data/.picoclaw/workspace-primeiro-projeto/uploads/`.

---

## What actually happens

Reproduced live in project `primeiro-projeto` while this was being written
(`turn-9`, then `turn-17`):

```
ERR agent pipeline_llm.go:463 > LLM call failed
  error="API request failed:\n  Status: 400\n  Body: {\"error\":{\"code\":\"1210\",
         \"message\":\"messages.content.type is invalid, allowed values: ['text']\"}}"
  agent_id=primeiro-projeto iteration=2 model=glm-4.7-flash
ERR events > agent.turn.end ... status=error final_len=0 iterations_total=2
```

The chain:

1. The member uploads a PNG. It lands correctly in the project's `uploads/`.
2. The agent calls `load_image`, which returns a tool message carrying a
   `media://<uuid>` ref (`pkg/tools/fs/load_image.go:150`).
3. `toolloop.go:73-84` resolves that ref to a `data:image/...` URL **before the
   next** LLM call — its own comment notes iteration 1 usually has no refs, which
   is exactly why every failure is logged at `iteration=2`.
4. `SerializeMessages` turns it into `{"type":"image_url", …}`
   (`pkg/providers/common/common.go:122`).
5. `glm-4.7-flash` is **text-only** and rejects the whole request with 400/1210.
6. picoclaw *has* a graceful fallback for this — `isVisionUnsupportedError`
   (`pkg/agent/llm_media.go:40-72`) — but **none of its patterns match GLM's
   wording**. The closest requires the literal string `image_url` to appear next to
   "invalid"; GLM says `messages.content.type`. So there is no fallback: the turn
   dies with no answer at all.
7. **The tool message with the media ref is persisted in the session JSONL.**
   Confirmed: lines 3 and 16 of
   `workspace-primeiro-projeto/sessions/sk_v1_46e3…d231.jsonl` both hold
   `"media":["media://…"]`. Every subsequent turn in that conversation re-resolves
   the image, re-sends `image_url`, and gets the same 400.

Step 7 is why it reads as amnesia. The conversation is not truncated and nothing
restarted — it is **permanently unable to answer**, from the upload onward. An
agent that replies to nothing is indistinguishable from an agent that forgot
everything.

`memory/jsonl.go`'s `TruncateHistory` (reached via `summarizeSession`, which does
`SetSummary` + `TruncateHistory`) was investigated as an alternative explanation
and is **not** implicated here: no summarize event appears in the logs for these
turns.

## Contributing configuration

`config.json` in the affected workspace holds:

```json
"image_model": "glm-4.7-flash"
```

The vision model is set to a model that has no vision. This is **not** written by
the proxy — `image_model` appears nowhere in `internal/`, and not in the default
template; it is an operator value living in the per-user `config.json`, reachable
from the admin instance-config editor. picoclaw's own error text for the graceful
path says to "update `agents.defaults.image_model` to a multimodal model", which is
precisely what is wrong.

## Separate finding: the agent container is permanently unhealthy

`crabshell-alpha` reports `Status: unhealthy, FailingStreak: 85`, every probe
`wget: can't connect to remote host: Connection refused`. The gateway is fine —
from inside the container, `wget -O- http://127.0.0.1:18790/health` returns
`{"status":"ok"}` and `netstat` shows `0.0.0.0:18790 LISTEN 7/picoclaw`.

The probe is ours: `deploy/picoclaw-glob/Dockerfile:67-68` (copied from upstream)
uses `http://localhost:18790/health`. In alpine `localhost` resolves to `::1`
first, and picoclaw binds IPv4 `0.0.0.0` only — so the probe never connects. One
word (`127.0.0.1`) fixes it. The proxy is unaffected because `waitHealthy` runs its
own HTTP probe rather than reading Docker's health status, but anything that reads
container health (Dokploy, `docker ps`) sees a permanently sick container.

---

## What was done about it (2026-08-10)

Decided by the user: patch the detector, leave `image_model` alone for now, and
start fresh conversations rather than editing transcripts.

### The patch

`deploy/picoclaw-glob/vision-unsupported-glm.patch` — a second, independent patch
in the existing overlay. It touches no file the dispatch patch touches, so the two
apply in either order; verified against a clean `v0.3.1` clone.

- `pkg/agent/llm_media.go`: `isVisionUnsupportedError` recognises the ZhipuAI/GLM
  shape. The enumeration is matched **whole** (`allowed values: ['text']`) rather
  than by looking for `text` inside it — a provider answering
  `['text','input_audio']` accepts more than text and is complaining about
  something else.
- `pkg/agent/llm_media_test.go`: new file, 13 cases. It covers the existing
  patterns as well as the new one, so the patch cannot silently narrow what
  upstream already caught.

The Dockerfile applies both patches and runs both tests as build steps, so a build
cannot produce an image that fails to recognise this deployment's own model.
`zombie-crab/picoclaw:0.3.1-glob` rebuilt; both `globMatch` and
`allowed values: ['text']` confirmed present in the shipped binary.

**Two things the test run taught, worth keeping:**

1. The first version of the pattern (`contains "'text'"`) also matched
   `['text','image_url']`. The test caught it before the patch was written.
2. `['text','image_url']` is nonetheless classified as vision-unsupported — by an
   **older, broader upstream rule** (`image_url` beside `invalid`), not by the new
   one. That is asserted in the test rather than corrected, so a future tightening
   of either rule has to decide about the case deliberately.

### What the patch does and does not buy

It makes the failure **legible**, not survivable. `pipeline_llm.go:282` returns the
actionable "configure a multimodal image_model" error instead of retrying; it does
not strip the image and try again (only `askSideQuestion` does that,
`turn_coord.go:565`). So:

- a conversation that already carries a media reference **stays broken**;
- uploading an image to a chat will still break that chat, and now says why;
- images will not work at all until `image_model` names a multimodal model.

Making the main turn path strip media and retry — mirroring what `askSideQuestion`
already does — is the change that would stop the symptom rather than explain it. It
is a real behaviour change (the agent would answer without having seen the image)
and was left for a deliberate decision.

### Affected conversations

Scanned every session transcript in both running agent containers for a persisted
media reference. **Exactly one:**

```
crabshell-alpha  workspace-primeiro-projeto/sessions/sk_v1_46e3…d231.jsonl   2 refs
```

That is the conversation used to reproduce this. Nothing else is poisoned — the
earlier `marco-dos-biol-gicos` report involved a zip, which produces no media ref.
Starting a new chat in `primeiro-projeto` is the whole recovery.

### Deployment step still required

The tag is unchanged (`0.3.1-glob`), so `picoclawImage` needs no edit — but the
**running agent containers still run the previous image id**. They pick the new
binary up only when recreated, which the proxy does on persona/project bind drift
or when the container is absent. Removing the `crabshell-*` containers while the
stack is up is the direct way; it was NOT done here.

### Untouched, and separate

The `localhost` healthcheck finding above is a one-word fix in the same Dockerfile
and was deliberately left alone — it is not part of what was asked, and changing a
container's health probe changes what the orchestrator does with it.

### Why the first deploy did not take effect (2026-08-10, after the patch)

The image was rebuilt and the stack redeployed, and the bug reproduced identically.
Cause: **a compose deploy does not reach the agent containers.**

```
crabshell-alpha  image=sha256:2dee40ea…  (the PREVIOUS glob build)
crabshell-beta   image=sipeed/picoclaw:latest  (stock — created before the override existed)
both: allowed values: ['text'] MISSING from /usr/local/bin/picoclaw
both: started unchanged across the deploy
```

`crabshell-*` containers are created by the proxy at runtime, not by compose. They
outlive a redeploy and keep running whatever image they were created from —
`EnsureRunning` reuses an existing container regardless of its image, recreating
only on persona/project bind drift. So the new binary is picked up only when the
container is **removed**; the next chat request recreates it.

Checked before recommending that, because getting it wrong would have been worse
than the bug:

- The effective image is right. `CRAB_PICOCLAW_IMAGE=zombie-crab/picoclaw:0.3.1-glob`
  is set on the proxy and overrides `config.yaml`'s stock value
  (`internal/config/config.go:323-324`) — so a recreate yields the patched image,
  **not** stock. A recreate against stock would have silently removed the dispatch
  glob and broken project routing entirely.
- `EnsureImage` (`internal/docker/client.go:217-228`) fast-paths on a locally
  present image and does not attempt a pull, so a local-only tag is fine.
- Nothing durable lives in the container: workspaces, sessions and uploads are on
  the host bind mount.

`hasMediaRefs` was also checked as a possible second cause and is not one — it is
`len(msg.Media) > 0` (`pkg/agent/agent_utils.go:551`), so the guard in front of the
detector holds once the patched binary is actually running.

**Worth its own fix:** nothing in the deploy path invalidates agent containers when
`picoclawImage` changes. Image identity is not part of what `EnsureRunning`'s drift
check compares, though persona and project binds are. Adding it would make a
harness upgrade land by itself instead of requiring a manual `docker rm`.

### Fixed: a harness-image upgrade now lands by itself

`imageDrift` (`internal/docker/projects.go`) joins `personaBindDrift` and
`projectBindDrift` in what `EnsureRunning` compares, so a rebuilt harness image
recreates the container on the next request instead of waiting for a manual
`docker rm`.

It compares **resolved image IDs, not the tag**, and that is the whole point: the
tag does not change when the image behind it is rebuilt, and a fixed tag is exactly
how this stack ships its harness. A name comparison would have reported "no drift"
for the very upgrade this exists to catch. `Inspect` now carries the top-level
`Image` (the resolved id) rather than `Config.Image` (the tag), and a new
`ImageID(ctx, ref)` resolves what a container created right now would run — without
pulling, because that question has no answer for a reference absent locally.

Every "I don't know" answers **false**, never drift: an older daemon that reports no
image, an image not present locally, a daemon error. Recreating on uncertainty would
destroy a live conversation to install nothing — the container is already running a
working image, and `create()` calls `EnsureImage`, which is where a genuinely
missing image is pulled or fails loudly.

Tests: `TestImageDrift` covers the five outcomes and asserts **how many times
`ImageID` was called**, so the unknown-container case is shown to short-circuit
rather than merely agree by accident. `TestEnsureRunningRecreatesOnImageDrift`
covers the wiring — verified to discriminate by removing `m.imageDrift` from the
`||` and watching it fail — and pins convergence: once rebuilt, a second request
must not recreate again, because a check that never settles would truncate the
conversation on every turn.

The sandbox `internal/docker` failure baseline moves from 9 to 10 (the new
EnsureRunning test is chown-dependent, STATE.md L-001). Both it and the
pre-existing persona-drift test pass as root in `golang:1.25-bookworm`.
