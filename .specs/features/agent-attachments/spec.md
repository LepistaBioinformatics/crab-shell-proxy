# agent-attachments — the agent's files reach the user

## Symptom

Asking the agent for a file produces `Requested output delivered via tool attachment.`
in the chat and **nothing else**. No file, no link, no error.

## Root cause (verified against upstream source, not inferred)

`Requested output delivered via tool attachment.` is **picoclaw's own** string —
`pkg/agent/agent.go:124`, emitted from `pkg/agent/pipeline_execute.go:820` when every
tool response was `ResponseHandled` (the tool delivered its own output). The same
block sets `setFinalContent("")`, so the *text* of the turn is that sentence and
nothing more; the file went out through the channel's media path.

The Pico channel **does** implement that path
(`pkg/channels/pico/pico.go:748 SendMedia`). It emits a `message.create` whose
payload carries the array this proxy never reads:

```json
{ "type": "message.create",
  "payload": { "content": "<caption, often empty>", "message_id": "<uuid>",
    "attachments": [ { "type": "file|image|audio|video",
                       "url": "/pico/media/<refID>",
                       "filename": "report.pdf",
                       "content_type": "application/pdf" } ] } }
```

and serves the bytes at `GET /pico/media/<refID>` on its own HTTP port, requiring
`Authorization: Bearer <pico token>` (upstream `pkg/channels/pico/pico_test.go:920,949`).

`internal/pico/turn.go`'s `Payload` decodes `content`, `kind`, `placeholder`,
`message` and `tool_calls` — **not `attachments`**. So the array is dropped at the
proxy and the file never leaves the container. Nothing downstream is at fault, and
no amount of frontend work alone can fix it.

## Requirements

### Proxy — receive and keep the file (A)

- **A-01** The pico `Payload` decodes `attachments` with upstream's field names
  (`type`, `url`, `filename`, `content_type`).
- **A-02** An attachment frame is a **delivery event, not the assistant's answer**.
  It must not touch the completion machine's state. Today the plain-content branch
  would set `lastPlainID` to that frame's id, and since the caption is usually
  empty, `finalContent()` would return `""` — **erasing the answer** on a
  non-streaming request. A non-empty caption is still forwarded as content.
- **A-03** An attachment frame must not arm the finalize grace timer by itself. It
  arrives inside picoclaw's own typing.start/stop pair, so the existing rules keep
  driving completion; an attachment must never end a turn early.
- **A-04** `turn.Sink` gains an `Attachment` callback, as a struct field like
  `Progress` — so every harness runner has to acknowledge it at compile time rather
  than silently ignoring a new signal.
- **A-05** The relative `/pico/media/<id>` is resolved to an absolute URL **inside
  the pico runner**, which already owns the `ws://…/pico/ws` endpoint format, and
  handed up with the token. `httpapi` never does `ws`→`http` string surgery.
- **A-06** The proxy downloads each attachment and stores it in the workspace under
  `uploads/attachments/<filename>`, chowned like every other upload. The bytes then
  live in the user's own workspace — durable, and independent of picoclaw's media
  store lifetime.
- **A-07** A failed download or store is logged and **does not fail the turn**: the
  answer the user is reading is already on its way, and a 502 in place of it would
  be a worse outcome than a missing file.
- **A-08** The filename is sanitized as a leaf (`sanitizeFilename`, the same as a
  browser upload); the `attachments/` segment is a constant, so no traversal is
  reachable. The **extension allowlist is deliberately NOT applied**: it exists to
  constrain what a browser may push into a container, whereas this file was written
  by the agent inside its own workspace — the allowlist would add no boundary and
  would silently drop legitimate deliverables.

### The user can actually get it (B)

- **B-01** No webapp change is required for delivery: `ListMedia` already walks
  nested folders and `OpenMedia` reads nested paths (upstream of this feature:
  `media_nested_test.go`), so the uploads sidebar lists `attachments/report.pdf` and
  its existing click-to-download works. This is the "same as when the user uploads"
  behaviour that was asked for.
- **B-02** (separate slice) The live turn's text gets a clickable link. This is
  convenience only: the proxy-injected text is **not** in picoclaw's transcript, so
  it does not survive a reload — the sidebar is the durable path.

### Tier 3 — a skill that needs no protocol at all (C)

- **C-01** The bundled default template ships a skill telling the agent to write
  deliverables to `uploads/attachments/` and to answer with that path in plain text.
- **C-02** Shipped **preemptively**, not as a discovered failure: whether tier 2
  works end-to-end cannot be observed without the user's stack and a model that
  actually takes the attachment path. Its independent value is real — a path written
  as text is part of the transcript and survives a reload, which B-02's injected
  link does not.

## Out of scope

- Inline rendering of images in the chat (the `type` field distinguishes them, but
  showing them is a design change, not a bug fix).
- Deleting the attachment from picoclaw's media store after copying it.
- The `web/backend/api/pico.go` route upstream serves — that is picoclaw's own web
  UI, not this stack.

## What shipped

- `internal/pico/turn.go` — `Payload.Attachments`; the delivery branch **before**
  the plain-content branch (A-02/A-03); `mediaBaseFrom` resolving `ws://host/pico/ws`
  → `http://host` so the sink hands up an absolute URL plus the turn's bearer (A-05).
- `internal/turn/turn.go` — `Attachment` + the `Sink.Attachment` field (A-04).
- `internal/httpapi/attachments.go` — fetch (bearer, 60s cap, 64MiB `LimitReader`)
  and store; failures logged, never surfaced (A-07). `attachmentName` prefers the
  frame's filename, then `Content-Disposition`, then the media ref id.
- `internal/httpapi/sse.go` and `handlers.go` — the sink wiring on **both** turn
  paths. A delivery dropped on `stream:false` would be the reported bug again with
  no stream to notice it in, so it stores there too and appends the notices after
  the answer. Two positions on purpose: in the stream the notice lands wherever the
  frame arrived (upstream sends the file during tool execution, so it usually reads
  BEFORE the "delivered via tool attachment" sentence — harmless, and not something
  observable from here); in the non-streaming answer the order is ours and the
  notice is a footnote.
- `internal/docker/media.go` — `StoreAgentAttachment` → `uploads/attachments/<name>`,
  sanitized leaf, constant subdir, no extension allowlist (A-06/A-08).
- Skill (C-01), in **two** places for a reason: the default template's
  `workspace/skills/deliver-file/` reaches new workspaces only (`WorkspaceSeed` runs
  at first provision), so the same rule was also added to the **managed**
  `shared-content` skill — that tree is re-materialized on every proxy start and is
  already bind-mounted into every existing container, so it reaches workspaces that
  already exist without a new mount or a recreate.
- `internal/docker/manager.go` + `reconcile.go` — the managed tree's materialization
  moved out of `create` into `ensureManagedContent`, now also called from `Reconcile`
  at startup. Inside `create` behind a per-process `sync.Once`, a fully warm
  deployment would never write the updated guidance and it would reach nobody; the
  skill dir is a directory bind, so writing it at startup reaches every running
  container live.

## Verification results

- `TestAttachmentFrameDecodesUpstreamShape` — the fixture is copied verbatim from
  upstream's `client_test.go`, so a misread shape fails here.
- `TestAttachmentIsEmittedAbsoluteWithToken` — absolute URL, bearer, metadata, the
  caption still forwarded as content, and no timer change.
- `TestAttachmentFrameNeverErasesOrEndsTheAnswer` — delivery before AND after the
  answer: file emitted once, `finalContent()` intact, grace still arms on
  `typing.stop`. This is the ordering that would have reproduced the original
  symptom.
- `TestStoreAgentAttachmentLandsWhereTheSidebarLooks` — the stored path is listed by
  `ListMedia` and read back by `OpenMedia`, i.e. the sidebar's own two operations.
- `TestStoreAgentAttachmentSanitizesAndOverwrites` — `../../etc/passwd` collapses to
  a leaf; a re-delivery overwrites instead of piling up.
- Whole suite green as root inside the proxy image build (`go vet ./... && go test
  ./...`), which is also the build gate.

## Not verified

The end-to-end path — agent produces a file → user sees and downloads it — needs the
running stack and a model that actually takes the attachment path. Everything above
is unit-level plus upstream-fixture-level. If the file still does not appear after a
deploy, the next thing to read is the proxy log: `attachment stored at …` means the
proxy did its half.

## Verification plan (original)

- Decode: fixtures copied **verbatim** from upstream's own tests
  (`client_test.go:464`, `pico_test.go:920`) so the test fails if the shape was
  misread, and documents where the shape came from.
- Ordering: attachment frame before AND after the answer text — the attachment is
  emitted once, `finalContent()` is untouched, and no premature finalize.
- Store: the file lands at `uploads/attachments/<name>`, is listed by `ListMedia`
  and readable by `OpenMedia` (the sidebar's own path).
- End-to-end (agent → chat) **cannot be verified here**: it needs the running stack
  and a model that takes the attachment path. Stated, not claimed.
