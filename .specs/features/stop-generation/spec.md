# Feature — Stop the generation for real

**Status:** implemented (2026-08-13) — two repos, not three
**Scope:** Medium — crab-shell-proxy, crab-exoskeleton-webapp

> **Correction, read this before F3/R1 below.**
>
> This spec was written believing the Pico Protocol had no way to ask for a
> cancel, and planned a picoclaw patch overlay to add one. **That was wrong.**
> picoclaw v0.3.1 — the tag `deploy/picoclaw-glob/Dockerfile` already pins —
> ships a `/stop` command that reaches the very `HardAbort` F2 identified:
>
> `pkg/agent/agent.go:191` → `tryHandleStopCommand` → `pkg/agent/agent_stop.go`
> `stopActiveTurnForSession` → `AgentLoop.HardAbort(sessionKey)`.
>
> It is dispatched from an ordinary `message.send`, and it resolves its session
> key through `resolveSteeringTarget` → `allocateRouteSession` — the *same* call
> the normal message path makes (`agent_message.go:154`). So a stop and a turn on
> one conversation always agree on the key, with nothing for this stack to
> compute or keep in sync.
>
> **F3 and R1 are therefore void**: no protocol change, no `SetAbortFunc`, no
> third patch file, and R1.4's open question does not arise. R2 and R3 stand, and
> are what was built. F1 and F2 were re-read at v0.3.1 and hold.
>
> Two details F2 did not record, both load-bearing:
>
> - `HardAbort` refuses a turn still in setup (`turnID` prefixed `pending-`).
>   `tryHandleStopCommand` covers that case by arming a pending stop, which is
>   another reason to go through the command rather than call `HardAbort` under
>   a transport of our own.
> - The rollback deletes the member's own message, confirmed rather than
>   suspected: `initialHistoryLength` is captured in `newTurnState`
>   (`turn_state.go:280`), and the user message is appended afterwards
>   (`pipeline_setup.go:124`). This settles **R-1** — see the resolution there.
>
> Neither the `/stop` nor picoclaw's reply to it is written to session history:
> the command path returns before `runAgentLoop`, and `PublishResponseIfNeeded`
> only publishes outbound. So a stopped turn leaves no residue on reload.

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

### F3 — VOID. The Pico Protocol has no client→server cancel, and the patch to add one is small

*Kept for the record; superseded by the correction at the top. The protocol
observation is accurate — `message.send`, `media.send`, `ping` is the whole
client→server vocabulary at v0.3.1 — but the conclusion drawn from it is not:
`/stop` travels inside `message.send`, so no new frame type was ever needed.*

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
webapp                 crab-shell-proxy            picoclaw v0.3.1 (STOCK)
──────                 ────────────────            ──────────────────────
[ ■ Stop ]
   │ POST /api/chat/<agent>/cancel
   ▼
 BFF route ─────────►  POST /v1/chat/cancel
                          │ resolveChatScope: same agent, authz and
                          │ session id as the turn
                          │ opens a SECOND pico WS (same auth)
                          ▼
                       {"type":"message.send",
                        "session_id":"<id>",
                        "payload":{"content":"/stop"}} ──►  tryHandleStopCommand
                                                          │
                                                          ▼
                                                     AgentLoop.HardAbort(sessionKey)
                                                          ├─ cancel provider/tool ctx
                                                          ├─ ts.Finish(true)
                                                          └─ roll history back
```

The reply travels back over **both** connections: picoclaw's `broadcastToSession`
writes to every connection registered for the session, so the running turn's
stream sees the stop and finalizes on its own rather than waiting out the idle
timeout.

### R1 — VOID (picoclaw patch)

*No patch is needed; see the correction at the top. `deploy/picoclaw-glob/` is
untouched by this feature, and R1.4's "one image, one overlay directory" question
never arises.*

### R2 — crab-shell-proxy

**R2.1** `POST /v1/chat/cancel`, registered beside `POST /v1/chat/completions` in
`internal/httpapi/handlers.go`. Same auth, agent resolution and session-key
derivation as the completions path — it must key on the identical session key or it
will abort nothing.

**R2.2** A `Cancel(ctx, sessionKey)` on the pico turner that dials the WS and sends
the frame. It does **not** reuse the turn's connection: that one is owned by an
in-flight `RunTurn` and is not safe to write from another goroutine.

**R2.3** ~~Distinguish outcomes for the caller: aborted, no active turn, agent
unreachable.~~ **Amended.** Only two outcomes are reported: reached (204) and
unreachable (502). picoclaw tells "stopped" and "nothing to stop" apart in
English prose only (`commands.FormatStopReply`), with no flag on the wire — and
the webapp acts identically either way, because it decides whether it was
mid-turn from its OWN state, not from this answer. Parsing upstream prose to
produce a distinction nobody consumes would be a coupling with no payer.

"No active turn" therefore answers 204, as R2.3 intended: it is a normal race,
not an error.

**As built:** `internal/pico/cancel.go`, and `resolveChatScope` in
`internal/httpapi/handlers.go` — extracted from `handleChatCompletions` so both
paths resolve the target through one piece of code rather than two that must be
kept in agreement. `Cancel` reads one frame before closing: the write alone only
reaches picoclaw's socket buffer, and closing on top of it can drop the command
before the read loop takes it, which would make Stop succeed and do nothing.

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

**R3.5** The stopped message goes back into the composer (R-1's resolution). The
rollback deletes it on picoclaw's side, so leaving it out would mean Stop silently
destroys what the member typed.

**R3.6** Once Stop is pressed, the rest of that turn's stream is ignored. picoclaw
answers the `/stop` with "Task stopped. …" on the same connection the turn is
streaming over, and rendering that as the assistant's reply would show a message
that the next history reload cannot produce.

## Risks

- **R-1 — The rollback is a real deletion. RESOLVED: restore the text.** Confirmed
  in source, not suspected: `initialHistoryLength` is captured in `newTurnState`
  before `pipeline_setup.go` appends the user message, so the member's own message
  goes with the turn. Product decision (2026-08-13): the webapp puts the text back
  into the composer, so stopping costs nothing that was typed. See R3.5.
- **R-2 — VOID.** No second patched-picoclaw feature exists; this one needs no
  patch at all. agent-projects remains the only overlay.
- **R-3 — Version pin.** `v0.3.1` is what `deploy/picoclaw-glob/Dockerfile` pins.
  `HardAbort`, `tryHandleStopCommand` and the rollback were read at that tag. A
  bump re-checks them — and now also re-checks that `/stop` still exists, since it
  is this feature's whole transport. It is an upstream command with no compile-time
  tie to this repo: if it were renamed, the proxy would keep answering 204 while
  stopping nothing. That is the failure to watch for on a picoclaw upgrade.

## Verification

Unit tests per repo:

- proxy — `internal/pico/cancel_test.go` asserts the frame ON THE WIRE (type,
  session id, `/stop`), because a stop carrying the wrong session id aborts
  nothing while returning success; `internal/httpapi/chat_cancel_test.go` asserts
  the cancel addresses the same session the turn ran on, in the main workspace and
  inside a project, plus the authorization cases.
- webapp — see R3.

The one that matters is end-to-end and manual, and is NOT covered by any of the
above: start a long turn, press Stop, confirm generation actually stops, then
**reload the conversation** and confirm the aborted turn is not there. F1 is
exactly the failure this catches. It needs the live stack.
