# turn-failure-visible — Spec

A turn that fails tells the member nothing. The reply area simply stays empty and
there is no way to tell "it worked and said nothing" from "it broke".

Spans two repos. Requirements here; the webapp half is listed under **Webapp** and
tracked in `crab-exoskeleton-webapp/.specs/features/turn-failure-visible/tasks.md`.

---

## Two paths lose the error, not one

### Path 1 — picoclaw sends an `error` frame

`internal/pico/turn.go:214` maps it to `signal{errMsg}`, which makes `RunTurn`
return an error. On the streaming path, `internal/httpapi/sse.go:147` **logs it and
calls `done()`** — the client receives a well-formed `finish_reason: "stop"`
carrying no information at all. There is nothing a client could match on, so this
half cannot be fixed anywhere but the proxy.

### Path 2 — picoclaw publishes the failure as ordinary assistant text

This is what the reported payload shows:

```
data: {"choices":[{"delta":{"content":"Error processing message: selected vision model
  \"glm-4.7-flash\" does not support image input; update agents.defaults.image_model
  to a multimodal model"},…}]}
data: {"choices":[{"delta":{},"finish_reason":"stop",…}]}
```

picoclaw formats it in `pkg/agent/error_format.go` (`formatProcessingError`) and
publishes it through `PublishResponseIfNeeded` — the same call an ordinary answer
takes. There is **no structural marker**: no distinct frame type, no kind, no
severity. The literal prefix `"Error processing message: "` is the only
discriminator on the wire.

The proxy forwards it as content, and the webapp then throws it away:

1. The text streams into the turn store's `revealed`.
2. The turn finishes → the completion painter runs `reloadHistory(sid)` →
   `setMessages(<durable transcript>)`.
3. **The transcript does not contain the error.** Verified on the live stack:
   `"Error processing message"` appears in *no* session `.jsonl` in either running
   agent container. picoclaw streams it and never persists it.
4. `clearCompleted(sid)` drops the bands that held it.

So the text is real, is displayed for a moment, and is then erased by the very step
whose job is to reconcile the reply with the durable transcript.

---

## Requirements

### Proxy

- **FR-1** `turn.Sink` gains `Error func(string)` + `EmitError`, so the transport
  can report a turn failure as a first-class signal instead of prose.
- **FR-2** `internal/pico/turn.go` emits that signal when a plain-content frame's
  **cumulative** `pl.Content` begins with picoclaw's error prefix.
  - **FR-2a** Matched on `pl.Content`, never on the emitted delta. The branch emits
    `pl.Content[len(prev):]` — the suffix — so the prefix is only reliably present
    on the cumulative value. Matching the delta would miss any message whose error
    text arrives as an update after a partial.
  - **FR-2b** Emitted once per message, not on every update: report only when the
    previous cumulative value did not already carry the prefix.
- **FR-3** The content is **still forwarded as content**. Not a compromise — a
  generic OpenAI client reads `delta.content` and nothing else, so suppressing it
  would leave every non-webapp consumer with an empty answer and no error at all.
  The signal is *added*, never a substitution.
- **FR-4** `internal/httpapi/sse.go` writes `x_crab_error: {"message": …}` as an
  extra top-level field on an otherwise-normal chunk with an **empty delta** — the
  exact shape and compatibility reasoning `x_crab_progress` already documents at
  `sse.go:53-58`: a client that knows nothing about it finds no content and skips
  the frame, where a named SSE event would be dropped wholesale by `data:`-only
  parsers.
- **FR-5** Path 1 is covered by the same writer: when `RunTurn` returns an error,
  the stream emits `x_crab_error` before `done()` instead of only logging.
- **FR-6** The **non-streaming** path (`handleChatCompletions`) is deliberately
  unchanged. It already returns the error text as the answer body with 200, and
  `finalContent()` derives that body from `lastPlainID` — which FR-3 keeps intact.
  Reclassifying the frame away from plain content would empty that body and report
  nothing, which is the bug, not the fix. Turning it into a 502 would change the
  contract for existing API consumers and is not what was asked.

### Webapp

- **FR-7** `consumeStream` parses `x_crab_error` and reports it, alongside the
  content and progress callbacks it already has.
- **FR-8** A reported failure sets a stable error **code** plus the harness's own
  **detail** text. Two fields, because `errorText` (`lib/i18n/errors.ts:166`) maps a
  code to localized copy and falls back to `dict.unknown` for anything else —
  putting picoclaw's sentence in `error` would render "unknown error" and discard
  precisely the actionable part ("update `agents.defaults.image_model`").
- **FR-9** The detail survives `clearCompleted` for the same reason `error` already
  does: the banner must outlive the in-flight bands.
- **FR-10** The detail is cleared where `error` is cleared on a new send
  (`turn-store.ts:351`), so a stale harness sentence cannot render underneath a
  later, unrelated error code.
- **FR-11** The chat view renders the detail beneath the localized headline, in the
  existing error `Alert` — one banner, not a second surface.
- **FR-12** The banner sits **with the message that caused it**, at the end of the
  message column and inside the scroll area, in the same 720px width as the message
  content. At the top of the view it named a problem without naming what provoked it,
  and in a scrolled conversation the two were not even on screen together.
  - **FR-12a** "The end of the column" IS the failing message: a failed turn produces
    no reply, and while the banner is up that message is necessarily the last one,
    because sending anything else clears the error (`enqueue`). No per-message
    bookkeeping is needed to anchor it.
- **FR-13** The banner is **not dismissible**. A first pass added a close control and
  it was withdrawn: the error is the only account of what happened to that message —
  it is not in the transcript and a reload loses it — so a stray click would leave the
  member with a message that has no reply and no explanation, which is the bug.

---

## Known limitation, stated rather than fixed

**A page reload loses the banner.** The error is not in the transcript (that is the
root cause of path 2, and not this stack's file to write), so a reopened
conversation shows the member's message with no reply and no error. This feature
makes the failure visible **for the session in which it happened**.

Persisting it would mean the proxy writing into picoclaw's own session store — the
thing `internal/cron/cron.go` already refuses to do for scheduled jobs, and for the
same reason: picoclaw owns that file and holds live state derived from it.

---

## Acceptance criteria

| ID | Criterion |
| --- | --- |
| AC-1 | A turn whose content begins with the error prefix emits `x_crab_error` **and** the content, in that stream |
| AC-2 | The signal is emitted once per message, not once per update frame |
| AC-3 | A cumulative content whose error prefix arrives after a partial still emits (FR-2a) |
| AC-4 | Ordinary content never emits `x_crab_error` |
| AC-5 | A `RunTurn` error (path 1) emits `x_crab_error` before `[DONE]` |
| AC-6 | The non-streaming path's body and status are byte-identical to today |
| AC-7 | The webapp shows the localized headline **and** the harness sentence, and both survive the post-turn history reload |
| AC-8 | A new send clears the previous detail |

## Tests

Proxy (`go test ./...`, nine-plus `internal/docker` chown failures are the
baseline — STATE.md L-001):

- `internal/pico`: classification table over frame sequences — prefix on first
  frame; prefix arriving on an update; ordinary content; two updates of the same
  errored message emitting once (AC-2/AC-3/AC-4).
- `internal/httpapi`: the SSE carries `x_crab_error` with the message and an empty
  delta; a `RunTurn` error emits it before `[DONE]` (AC-5); the non-streaming
  handler is unchanged (AC-6).

Webapp (`yarn test`):

- `consumeStream` surfaces `x_crab_error` and ignores a frame without it.
- `clearCompleted` preserves the detail; a new `runTurn` clears it (AC-8/FR-9/FR-10).
