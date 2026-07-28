# chat-progress-events (proxy side) — Specification

**The authoritative spec, design and full task list live in the sibling repo:**
`crab-exoskeleton-webapp/.specs/features/chat-responsiveness/{spec,design,tasks}.md`.

**This repo's tasks are T10–T13** in that `tasks.md`: decode `tool_calls`
(T10), the `turn.Sink` contract (T11), emit progress in `processor.handle`
(T12), `x_crab_progress` on the wire (T13). They are `[P]` against all the
webapp work — no shared files — and the contract is additive, so an un-updated
webapp simply ignores the new field.

This file records only what the proxy owns, so a reader working in this repo does
not have to reconstruct it. Requirement IDs are the webapp spec's — do not
renumber them here.

---

## Why

picoclaw emits progress signals over the Pico Protocol and this proxy discards
every one of them, at `internal/pico/turn.go:72`:

```go
if pl.Kind == "thought" || pl.Kind == "tool_calls" || pl.Placeholder {
    return signal{}
}
```

`typing.start` / `typing.stop` are consumed by the completion state machine and
never leave it. The result is a chat that is silent for the whole turn and then
emits the full answer at once.

These frames carry human-readable text — `internal/pico/turn_test.go:60,73`
exercises them with `"calling tool"` and `"thinking..."`, and the shipped
picoclaw binary contains `Thinking...`, `Processing...`,
`channels.placeholderEntry` and `pkg/utils/visible_tool_calls.go`. So real
progress text exists; it is dropped one layer below the UI.

---

## What the proxy is responsible for

### FR-1 — Progress events

- **FR-1.1** Forward `placeholder` frames as progress events instead of
  dropping them.
- **FR-1.2** Forward `kind: "tool_calls"` frames as progress events, carrying
  the frame's text.
- **FR-1.3** Forward `typing.start` / `typing.stop` as progress events.
- **FR-1.4** Forward `kind: "thought"` frames as progress events, tagged so the
  client can distinguish internal reasoning from a tool action.
- **FR-1.5** Progress events never contribute to assistant content. The turn's
  final text — `processor.finalContent()` — is unchanged, byte for byte.
- **FR-1.6** Progress events never affect turn-completion timing.
  `maybeArmGrace` keeps its exact current semantics: only plain,
  non-placeholder content sets `hasPlainContent` and arms the finalize timer.
  The regression tests at `internal/pico/turn_test.go` must pass untouched.
- **FR-1.7** The SSE format stays consumable by a generic OpenAI-compatible
  client that knows nothing about progress events (see OQ-2 in the webapp spec).

### NFR

- **NFR-2** No change to the durable transcript. `history.SyncDurable` and the
  `.jsonl` contents are untouched — progress text is never persisted.
- **NFR-4** `go vet` and the full test suite stay green.

---

## Surfaces

| Requirement | File |
|---|---|
| FR-1.1–1.4, FR-1.6 | `internal/pico/turn.go` — `processor.handle`, the `Frame`/`Payload` types, the `onDelta` callback signature |
| FR-1.7 | `internal/httpapi/sse.go` — `writeChunk` / the SSE envelope |
| contract | `internal/turn` — the harness-agnostic turn interface, shared with `internal/hermes` |

**Note on the second harness:** `internal/hermes` implements the same turn
interface. Whatever callback shape FR-1 introduces must either be optional for
hermes or be implemented there too — decide in Design, do not let the interface
change break the hermes path silently.

---

## Measured behaviour (OQ-1 — closed 2026-07-28)

Instrumented `processor.handle` and the frame reader over two real turns against
`deepseek-chat`, then reverted. Findings:

**1. There is no token stream.** picoclaw delivers the whole answer in one
`message.create`:

```
01:43:55  typing.start
          …51s of complete silence…
01:44:46  typing.stop
01:44:46  message.create  kind=""  len=17594   ← entire answer, one frame
```

No `message.update` frames at all. `onDelta`'s suffix arithmetic
(`turn.go:79-82`) fires exactly once per turn with the full payload. Nothing on
the proxy side can change this; progressive rendering has to be a client
concern.

**2. `tool_calls` frames carry narration the current `Payload` type discards.**
`Payload` decodes 5 fields (`turn.go:37-43`) and `Content` is always `""` on
these frames — but the raw frame holds much more:

```json
{"type":"message.create","payload":{
  "content":"", "kind":"tool_calls", "message_id":"…",
  "model_name":"deepseek-chat",
  "tool_calls":[{"id":"call_…","type":"function",
    "function":{"name":"web_fetch",
                "arguments":"{\"maxChars\":15000,\"url\":\"https://github.com/…\"}"},
    "extra_content":{"tool_feedback_explanation":
      "Com certeza! Deixe-me buscar novamente as informações do projeto."}}]}}
```

`extra_content.tool_feedback_explanation` is a first-person sentence written by
the agent, in the user's own language, saying what it is about to do. **This is
the whole point of FR-1** — it exists already and only needs to stop being
dropped. See FR-1.2a–d in the webapp spec.

In a tool-using turn, 14 such frames arrived over ~30s (≈ one every 2s).

**3. No `placeholder` or `thought` frame appeared** in either turn. FR-1.1 and
FR-1.4 share the same emit path and cost nothing extra to implement, but must
not be load-bearing in the UI.

**Consequence for this repo:** `Payload` needs a typed `tool_calls` field.
`function.arguments` is deliberately left undecoded — unbounded untrusted model
output with no display role (FR-1.2d).
