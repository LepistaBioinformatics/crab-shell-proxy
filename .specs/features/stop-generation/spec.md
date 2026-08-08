# Feature — Stop the generation for real

**Status:** specified, not started
**Scope:** Large — three repos (picoclaw patch overlay, crab-shell-proxy, crab-exoskeleton-webapp)

## Problem

After pressing Enter there is no way to stop a turn. The member waits, or sends
another message that queues behind the one they no longer want.

## What the investigation found

The option chosen was "real cancellation", and it turns out to be **more achievable
than the question implied**. Three findings, in the order they change the design:

### F1 — Cutting the connection cannot cancel anything

`crab-shell-proxy/internal/httpapi/sse.go:87-92` runs the turn on
`context.Background()` on purpose, with a comment recording that tying it to the
client request made picoclaw persist a truncated transcript — the "initial messages
disappear after reload" bug.

Picoclaw's side confirms why that is the wrong lever anyway:
`pkg/channels/pico/pico.go:1229` dispatches the turn with `c.HandleInboundContext(c.ctx, …)`
— **the channel's context, not the connection's**. Dropping the WebSocket does not
reach the running turn at all. So "cancel `turnCtx` in the proxy" — the shape this
feature was originally imagined as — would stop the proxy reading and nothing else,
while risking the transcript bug for no benefit. That approach is dead.

### F2 — Picoclaw already has a clean, session-safe abort. Nothing is wired to it.

`pkg/agent/steering.go:511`, `func (al *AgentLoop) HardAbort(sessionKey string) error`:

- looks the turn up by **session key** (so it cannot hit a neighbouring conversation
  in the same container — unlike `InterruptHard()`, which is deprecated for exactly
  that reason, and `InterruptGraceful(hint)`, which still uses
  `getAnyActiveTurnState()`);
- cancels the provider and tool contexts so execution stops promptly;
- calls `ts.Finish(true)` to cascade cancellation to child sub-turns **before**
  rolling back, explicitly to stop children writing during the rollback;
- **rolls session history back to `initialHistoryLength`** — the partial turn is
  removed rather than persisted half-written.

That last point is the one that matters: the mechanism answers F1's transcript
concern directly. This is a clean abort, not a severed pipe.

Nothing outside `pkg/agent` calls `HardAbort`. It is a finished capability with no
transport in front of it.

### F3 — The Pico Protocol has no client→server cancel, and the patch to add one is small

`pkg/channels/pico/protocol.go:9-23` — the client may send exactly `message.send`,
`media.send`, `ping`. (An earlier `strings` grep of the shipped binary suggested the
same, but that is weak evidence on a Go binary; this is the source at the pinned tag
`v0.3.1`, which is what the build overlay actually compiles.)

`PicoChannel` holds no `AgentLoop` reference — channels publish to a bus. But the
gateway constructs both (`pkg/gateway/gateway.go:203` and `:214`), and the codebase
already has the exact pattern for handing a control function down:
`agentLoop.SetReloadFunc(reloadTrigger)` / `HealthServer.SetReloadFunc(…)`.

So the patch is transport-only, in three small pieces, with no new abort logic.

## Design

```
webapp                 crab-shell-proxy            picoclaw (patched)
──────                 ────────────────            ──────────────────
[ ■ Stop ]
   │ POST /api/chat/<agent>/cancel
   ▼
 BFF route ─────────►  POST /v1/chat/cancel
                          │ resolves agent + session key
                          │ opens the pico WS (same auth)
                          ▼
                       {"type":"message.cancel",
                        "session_id":"<key>"}  ──────►  handleMessageCancel
                                                          │
                                                          ▼
                                                     AgentLoop.HardAbort(sessionKey)
                                                          ├─ cancel provider/tool ctx
                                                          ├─ ts.Finish(true)
                                                          └─ roll history back
```

### R1 — picoclaw patch (new overlay, alongside `deploy/picoclaw-glob/`)

**R1.1** `pkg/channels/pico/protocol.go`: add `TypeMessageCancel = "message.cancel"`
to the client→server block.

**R1.2** `pkg/channels/pico/pico.go`: route it in the WS read loop to a
`handleMessageCancel` that resolves the session id exactly as `handleMessageSend`
does (`msg.SessionID`, falling back to `pc.sessionID`) and calls the injected abort
func. Answer `error`/`no_active_turn` on the existing error frame shape rather than
silently succeeding — the webapp needs to distinguish "stopped" from "already done".

**R1.3** Wiring: a `SetAbortFunc(func(sessionKey string) error)` on `PicoChannel`,
set where the gateway already wires `SetReloadFunc`, from `agentLoop.HardAbort`.
Follow that existing pattern rather than inventing a second one.

**R1.4** Ship as a build overlay, **not** an upstream PR dependency — same reasoning
and structure as `deploy/picoclaw-glob/`: pinned `PICOCLAW_TAG`, `git apply --verbose`
with no fuzz, tests run inside the builder stage. Written to be upstreamable as-is.

**Open question for the build:** whether this becomes a second patch file in the
existing `picoclaw-glob` overlay or its own directory. One image cannot apply two
overlays, so if both patches are needed simultaneously they must live in one
directory. Resolve before writing the Dockerfile.

### R2 — crab-shell-proxy

**R2.1** `POST /v1/chat/cancel`, registered beside `POST /v1/chat/completions` in
`internal/httpapi/handlers.go`. Same auth, agent resolution and session-key
derivation as the completions path — it must key on the identical session key or it
will abort nothing.

**R2.2** A `Cancel(ctx, sessionKey)` on the pico turner that dials the WS and sends
the frame. It does **not** reuse the turn's connection: that one is owned by an
in-flight `RunTurn` and is not safe to write from another goroutine.

**R2.3** Distinguish outcomes for the caller: aborted, no active turn, agent
unreachable. "No active turn" is a normal race (the turn finished while the member
was clicking), not an error.

### R3 — crab-exoskeleton-webapp

**R3.1** A Stop control in the composer, shown while `useTurn(sid).running`.

**R3.2** It also discards `pending` and `queue` in `turn-store.ts` — otherwise
stopping the current turn just starts the next queued one, which reads as the button
not working.

**R3.3** On a successful abort the store stops the reveal and clears the in-flight
bands **without** routing through `onReplyDone`/`reloadHistory`: picoclaw has rolled
the history back, so a reload would fetch a transcript that no longer contains the
turn, and the bands must not be replaced by nothing mid-animation.

**R3.4** Copy in `en` + `pt` (`parity.test.ts` gate). The label is "Stop" / "Parar" —
with F2's rollback this is now honest, unlike the UI-only option that was rejected.

## Risks

- **R-1 — The rollback is a real deletion.** The member's own message is rolled back
  with the turn. Whether that is right (clean slate) or wrong (they lose what they
  typed) is a product call worth confirming before R3 is written. The webapp could
  restore the text into the composer.
- **R-2 — Two patched-picoclaw features now exist.** agent-projects already needs a
  patched image. See the open question in R1.4.
- **R-3 — Version pin.** `v0.3.1` is what `deploy/picoclaw-glob/Dockerfile` pins.
  `HardAbort` and its rollback were read at that tag; a bump re-checks both patches.

## Verification

Unit tests per repo. The one that matters is end-to-end and manual: start a long
turn, press Stop, confirm generation actually stops, then **reload the conversation**
and confirm the aborted turn is not there. F1 is exactly the failure this catches.
