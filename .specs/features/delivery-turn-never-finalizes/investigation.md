# delivery-turn-never-finalizes — Investigation

**Status:** Root cause identified and reproduced twice — once mechanically against
`RunTurn`, once live on the running stack. **Option B implemented** (2026-09-05, see
§10); the live re-check in §8 step 3 has not been run yet.
**Date:** 2026-09-05. **Successor to:** `agent-attachments` (this is the completion half
that spec's A-03 left open, and its "Not verified" section predicted would need a live
model to surface).
**Components:** `crab-shell-proxy/internal/pico/turn.go` (the defect) + upstream picoclaw
`v0.3.1` `pkg/agent/pipeline_execute.go` (what triggers it).

**Reported symptom:** the member asks the agent to produce a file. The chat shows one
line — `📎 resumo-documento.txt — public/attachments/resumo-documento.txt` — and then
keeps the caret blinking and the Stop button up, as if the agent were still working.
No error, no "connection dropped" banner. Reloading the page shows the whole turn: the
steps the agent ran and a final message *"Requested output delivered via tool
attachment."*

**Verdict: the turn completed upstream and the proxy never noticed.** picoclaw ends a
tool-delivered turn without publishing **any** plain assistant message, and this proxy's
completion heuristic can only finalize a turn *after* plain content has arrived. So
`RunTurn` sits waiting for a frame that will never come until `turnIdleTimeout` — **600s
in this deployment** — and the SSE stream stays open, heartbeating, with no terminal
marker. The member reads ten minutes of "thinking" as "forever" and reloads.

---

## 1. What the wire actually showed

The complete response body of `POST /api/chat/alpha`, captured by the member in DevTools
during a live reproduction (formatting collapsed, order exact):

| # | frame | what it is |
|---|---|---|
| 1 | `delta:{role:"assistant",content:""}` | `sse.go`'s opening chunk |
| 2 | `x_crab_progress {kind:"typing",state:"start"}` | `typing.start` |
| 3 | `x_crab_progress {kind:"typing",state:"stop"}` | `typing.stop` — **before any content** |
| 4 | `x_crab_progress {kind:"tool",tool:"write_file"}` | the agent writing the file |
| 5 | `delta:{content:"\n\n📎 resumo-curto.txt — public/attachments/resumo-curto.txt\n"}` | the proxy's own `attachmentNotice` |
| 6 | `x_crab_progress {kind:"tool",tool:"send_file"}` | the delivery tool |
| 7 | `: ping`, `: ping`, … | the heartbeat, forever |

There is **no** `finish_reason:"stop"` and **no** `data: [DONE]`. There is also no plain
`delta.content` anywhere except line 5 — and line 5 is written by this proxy, not by the
harness.

A second reproduction (01:21:21, `resumo-curto-v2.txt`) has the identical shape, with the
two tool frames in the order one would expect — `typing.start`, `typing.stop`,
`tool:write_file`, `tool:send_file`, the `📎` notice, then `: ping` with nothing after it
for as long as the member watched. The frame ordering between the notice and the
`send_file` narration is therefore incidental; the missing terminal marker is not.

Matching proxy log for the same window (`docker logs zombie-crab-project-crab-shell-proxy-1`):

```
01:03:05  chat: authorized … project="chat-ux"
01:03:28  POST /v1/chat/completions -> 200 (23s)      <- an ordinary turn, for contrast
01:03:41  chat: authorized … project="chat-ux"        <- the turn that hangs
01:03:46  stream: attachment stored at public/attachments/resumo-documento.txt (1501 bytes)
   …      (no completion line; still open 9 minutes later)
01:07:54  chat: authorized … project="chat-ux"
01:07:57  stream: attachment stored at public/attachments/resumo-curto.txt (285 bytes)
   …
01:13:41  stream: turn failed: picoclaw turn abandoned by caller: context deadline exceeded
01:13:41  POST /v1/chat/completions -> 200 (10m0.006s)
01:14:11  stream: turn failed: picoclaw turn abandoned by caller: context deadline exceeded
01:14:11  POST /v1/chat/completions -> 200 (10m0.005s)
```

The last four lines were captured after the member had already given up and reloaded:
**every hung turn ended at exactly ten minutes, reported as a failure, for work that had
finished nine and a half minutes earlier.**

The request-log line is written when the handler returns (`handlers.go:475`). A turn that
answers normally prints it in seconds — `(23s)` above. The delivery turns never printed
one while the member was watching, which is the hang, stated in the proxy's own log.

**Verified, not assumed:** the file itself is fine. `StoreAgentAttachment` wrote it, the
member can see it in the uploads sidebar, and the bytes are correct. Nothing about the
delivery mechanism (`agent-attachments`) failed. What failed is knowing the turn was over.

## 2. The defect, in two halves

*Line numbers in this section are the **pre-fix** file (`c5aa7ad`); §10 records what
changed.*

### Half one — picoclaw never publishes a final message on this path

`pkg/agent/pipeline_execute.go:815-841` (upstream `v0.3.1`, unchanged in the local glob
image). When every tool response was `ResponseHandled` — the tool delivered its own
output, which is exactly what `send_file` does — picoclaw:

```go
summaryMsg := providers.Message{Role: "assistant", Content: handledToolResponseSummary, …}
ts.agent.Sessions.AddFullMessage(ts.sessionKey, summaryMsg)   // session only
…
ts.setPhase(TurnPhaseCompleted)
ts.setFinalContent("")                                        // nothing to publish
```

`handledToolResponseSummary` is `"Requested output delivered via tool attachment."`
(`pkg/agent/agent.go:124`). It is **appended to the session and never sent to the
channel** — which is precisely why the member sees it only after a reload, and why the
live stream and the reloaded transcript disagree. `agent-attachments`'s root-cause section
already recorded `setFinalContent("")`; what it did not follow through was that a turn
with no final content also emits no frame that this proxy's completion machine reacts to.

The Pico Protocol has no "turn over" frame to fall back on: `pkg/channels/pico/protocol.go`
defines `message.create|update|delete`, `media.create`, `typing.start|stop`, `error`,
`pong` — and nothing else. Completion has always been inferred here, which is why
`server.js`'s heuristic was ported verbatim.

### Half two — this proxy can only finalize on plain content

`internal/pico/turn.go:257`:

```go
func (p *processor) maybeArmGrace() signal {
	if !p.hasPlainContent || p.isTyping {
		return signal{}
	}
	return signal{arm: true}
}
```

`hasPlainContent` is set in exactly one place, the plain-content branch
(`turn.go:212`). The delivery branch above it (`turn.go:200-207`) deliberately returns a
zero `signal` — `agent-attachments` A-03: *"An attachment frame must not arm the finalize
grace timer by itself … an attachment must never end a turn early."* Progress frames
(`thought`, `tool_calls`, placeholders) return a zero signal too.

So for a turn whose entire output is a delivery plus tool narration, **nothing ever arms
grace**. The loop falls through to `idle.Reset(idleTimeout)` on every frame
(`turn.go:435`) and then waits. `idleTimeout` is `cfg.TurnIdleTimeout`
(`cmd/crab-shell-proxy/main.go:57`), which this deployment sets to **600s**
(`config.yaml:50`). Whichever ten-minute bound expires first — that one, or `turnCtx`
(`sse.go:20`, also 10 minutes but counted from the handler's entry rather than from the
last frame) — the turn is then reported as a **failure** even though it succeeded. On the
captured runs `turnCtx` won by about five seconds, so the log says *"picoclaw turn
abandoned by caller: context deadline exceeded"* (`turn.go:409`) rather than *"timed out
waiting for picoclaw"* (`turn.go:411`). Either message names a healthy turn.

A-03 is not wrong about the risk it names; it is incomplete. It assumed a delivery always
sits *inside* a turn that also speaks, and on this path the turn never speaks.

## 3. Why no existing safety net catches it

- **The heartbeat (`turn-stream-continuity` Group A) masks it.** `sse.go`'s `: ping` every
  10s keeps the connection healthy for the whole 600s, so nothing cuts the stream. That is
  the feature working as designed — and it is why the member sees *no* "connection
  dropped" banner. Before the heartbeat, this bug would have looked like a network cut and
  landed in the recovery path.
- **`recover()` never runs.** The webapp's recovery poll is reached only when
  `consumeStream` *returns* without a terminal marker (`turn-store.ts`, `runTurn`). Here
  the body never ends, so `consumeStream` is still awaiting `reader.read()`; `running`
  stays true, the Stop button stays up, and no code path repaints the transcript. This
  matches the member's report exactly: *"só pensando, sem faixa"*.
- **`turnTimeout` does not shorten it.** `sse.go:20` is also 10 minutes. It is the bound
  that actually fired on the captured runs, a few seconds ahead of the idle timer — which
  changes the error text and nothing else.
- **The BFF does not cut it either.** `lib/mycelium-stream.ts` sets `bodyTimeout: 0` for
  this one route (Group B), on purpose.

Net effect: **ten minutes of a spinner, then an error message for a turn that worked.**

## 4. Reproduced mechanically

Three probes against the real `RunTurn` (temporary file, removed after; kept at
`scratchpad/probe_no_plain_content_test.go`), with `IdleTimeout` shortened to 700ms so the
wait is observable in a test:

```
TestProbe_DeliveryOnlyTurn    elapsed=700ms  content=""  err=timed out waiting for picoclaw  files=1
TestProbe_AnswerThenDelivery  elapsed=500ms  content="Salvei em public/attachments/report.pdf"  err=<nil>
TestProbe_ToolCallsOnlyTurn   elapsed=700ms  content=""  err=timed out waiting for picoclaw
```

Line 1 is the reported bug in isolation: `typing.start`, a delivery frame with an empty
caption, `typing.stop` — the turn burns the whole idle budget and then reports failure.
Line 2 is the control: add one plain message and the same turn finalizes in 500ms
(`graceWindow`). Line 3 shows the trigger is **not** attachments — it is *no plain
message*: a turn whose only output is tool narration hangs identically.

**Scale factor:** in production the 700ms above is 600s.

## 5. Ruled out, with the evidence that ruled it out

| Candidate | Why not |
|---|---|
| "The frontend does best-effort file delivery and degrades" (the reported hypothesis) | The webapp has **no** handling of a delivered file at all. `attachmentNotice`'s `📎` line is plain text injected by the proxy; `grep -rn "📎" app lib components` in the webapp returns nothing. Nothing client-side fetches, waits for, or retries a file. |
| The attachment download blocking the frame loop | `storeTurnAttachment` does run inline in the read loop, but it is bounded (60s ctx, 64MiB `LimitReader`) and the log shows it completing in ~3-5s (`attachment stored at …` at 01:03:46, 5s after the turn began). The hang starts *after* it. |
| Stream cut by a hop (mycelium `gatewayTimeout=60`, Traefik, carrier NAT) | The stream is not cut. The member's capture shows `: ping` still arriving, and the request stays open in DevTools. A cut would produce the recovery banner, which is exactly what did **not** appear. |
| `project-chat-context-loss` (the routed-agent defect) | Different failure, already patched in the image. That one loses history; this one loses the turn's ending. The hung turns here ran on the `chat-ux` project agent, whose history is intact. |
| Client-side reveal driver wedging with `running` stuck true | Possible in principle (`finishIfDrained` needs `buffered === ""`), but not what happened: the response body never ended, so `runTurn`'s `finally` was never reached. |
| picoclaw crashing or restarting mid-turn | `docker logs` for the harness container shows the turn's tool activity and no exit; the transcript is complete on disk. |

## 6. Blast radius

Any turn where picoclaw publishes no plain assistant message:

- **file delivery via `send_file`** — the reported case, and the common one now that
  `agent-attachments` C-01's skill actively tells the agent to hand files over;
- **any turn ending with `allResponsesHandled == true`** — the same upstream branch,
  regardless of which tool handled the response;
- **tool-narration-only turns** (probe 3).

Both the streaming and non-streaming paths are affected: `handlers.go`'s synchronous
branch calls the same `RunTurn`, so it blocks for 600s and then answers 502 with
*"timed out waiting for picoclaw"*.

Not affected: every turn that produces a plain answer — i.e. the overwhelming majority,
which is why this survived until a model actually took the `send_file` path.

## 7. Options

### A — Arm the existing 500ms grace on a delivery (rejected)

One line in the delivery branch: `return p.maybeArmGrace()` plus a `hasDelivery` flag.
Fixes the observed trace — `isTyping` is already false there, so grace arms and the turn
ends 500ms after the file.

**Rejected because it re-creates the risk A-03 exists to prevent, and this deployment has
no guard against it.** A mid-turn delivery followed by more work would be cut 500ms later,
and nothing would cancel the timer: the pico channel here runs with `typing: {}` and
`placeholder.enabled: false` (harness `config.json`), so subsequent messages are **not**
wrapped in a fresh `typing.start` — the trace above shows exactly one typing pair for the
whole turn. `tool_calls` frames do not cancel grace either. 500ms is far shorter than one
LLM round-trip.

### B — A delivery shortens the silence budget, and its expiry means SUCCESS (recommended, IMPLEMENTED)

Two changes in `internal/pico/turn.go`, no new timer:

1. Track that the turn produced user-visible output with no plain message
   (`p.hasDelivery`, set in the delivery branch).
2. In the loop: while `hasDelivery && !hasPlainContent`, reset `idle` to a short
   `deliverySettle` window instead of `idleTimeout`, and on `<-idle.C` in that state
   return `proc.finalContent(), nil` — a completion, not an error.

Every inbound frame still resets the timer, so a turn that continues after a delivery is
safe as long as its next frame arrives inside the window; a turn that is genuinely over
finalizes in seconds instead of ten minutes. `finalContent()` is `""` for a delivery-only
turn, which the SSE path already handles (the notice is the content the member reads) and
the synchronous path already handles (notices are appended to the answer,
`handlers.go:723`).

**Two exits, one of which B does not cover.** The loop can leave through `<-idle.C`
(`turn.go:411`) *or* through `<-ctx.Done()` (`turn.go:409`), and the captured runs left
through the second — `turnCtx` beat the idle timer by ~5s because both are ten minutes
and one starts earlier. B only changes the `<-idle.C` branch. That is sufficient **because
the settle window is short**: at 20-30s the idle branch fires minutes before any caller
deadline, in every realistic case. It stops being sufficient if the window is ever raised
towards `turnTimeout`, so the two must not be allowed to converge.

**What B trades away:** `<-idle.C` stops meaning "dead connection" unconditionally. After
a delivery with no plain message it means "the turn is over"; everywhere else it keeps its
current meaning. A genuinely dead connection in that one state is reported as a completed
turn with no answer — which is what the member already sees today, minus the ten minutes.

The window is the one judgement call: it must exceed a normal LLM round-trip on this
deployment (the 23s control turn above included a `deepseek` TLS retry) and stay far below
600s. **20-30s** is the range worth measuring before pinning; the value belongs in
`config.yaml` beside `turnIdleTimeout` only if a measurement says one size does not fit.

This keeps A-02 (a delivery never becomes the answer) and the *spirit* of A-03 (a delivery
never ends a turn **early**) — it may only end one that has gone quiet.

### C — A fourth local picoclaw patch: publish the summary message (complementary, not a substitute) — **IMPLEMENTED**

Make `pipeline_execute.go`'s handled-tool branch publish `handledToolResponseSummary` to
the channel instead of only writing it to the session. That fixes the *cause*: the turn
would end with a plain assistant message, the existing rules finalize it, and the live
stream would stop disagreeing with the reloaded transcript — today the member sees `📎 …`
live and *"Requested output delivered via tool attachment."* after a reload, and never
both.

Worth doing, and **not instead of B**: the proxy must not hang for ten minutes because a
harness ended a turn quietly, whatever the harness does next. It also fits the
`deploy/picoclaw-glob/` discipline (upstreamable as-is, tests included) — but it is a
behaviour change for every channel, so it is the weaker candidate for upstream acceptance
and the stronger candidate for "fix ours first".

### Not an option

Raising or lowering `turnIdleTimeout` globally. It bounds a **dead connection**, which is
a different question from "this turn is over"; shortening it to paper over this would cut
legitimate long silences, and lengthening it makes the hang worse.

## 8. How to verify a fix

The stack the reproduction ran on is the local compose stack; the harness container is
recreated on image drift, and the proxy is `zombie-crab-project-crab-shell-proxy-1`.

1. **Unit:** the three probes in §4, promoted to real tests — a delivery-only turn and a
   tool-narration-only turn must finalize (no error) inside the settle window, and the
   `AnswerThenDelivery` control must still finalize in `graceWindow` with the answer
   intact. Keep `TestAttachmentFrameNeverErasesOrEndsTheAnswer` green: A-02 is unchanged.
2. **Regression against the risk:** a delivery followed, after a realistic gap, by a plain
   answer must still deliver that answer — that is the property option A gives up.
3. **Live:** ask the agent for a file. Pass = the caret stops, the Stop button goes away,
   and the proxy logs `POST /v1/chat/completions -> 200 (Ns)` with N in seconds. Fail =
   no completion line while the spinner runs.
4. **The log line is the cheap oracle** for any future report of this shape: a chat that
   "hangs" with no completion line in the proxy log is this bug; one with a completion line
   is not.

## 9. Open questions

- **OQ-1 — the same file is stored two or three times per turn.** `stream: attachment
  stored at public/attachments/resumo-curto.txt (285 bytes)` appears **three** times within
  one second for a single turn, and twice for the previous one. `O_TRUNC` means the file on
  disk is correct, but each store also emits a `📎` notice, so the member would read the
  same line repeated. The likely shape is one frame carrying N copies:
  `pipeline_execute.go:816` builds the summary message with the whole accumulated
  `handledAttachments` slice, and the delivery branch loops over `pl.Attachments`, so one
  frame with three entries produces three stores and three notices.

  **The emit path is 1:1 — checked, not assumed.** A second reproduction (01:21:21,
  `resumo-curto-v2.txt`) stored the file exactly **once** and its full response body carries
  exactly **one** `📎` line before the pings, so a notice per store is the rule and the
  earlier body was a partial paste. The duplication therefore happens upstream of this
  proxy, and it varies per turn — 1, 2, 3, 1 stores across the four captured turns, always
  of the same name within a turn.

  **Whether it is independent of the hang is unverified.** Both live in the same branch of
  the same turn; treat them as one area to re-read, not as two unrelated tickets.
- **OQ-2 — the notice does not survive a reload, and the harness's sentence does not
  appear live.** By design (`attachmentNotice`'s comment: the notice is not part of the
  transcript), but the result is that the live view and the reloaded view of the same turn
  never agree. Option C would close this; `agent-attachments` C-01's skill (telling the
  agent to name the path in its own words) is the existing mitigation, and it did not fire
  here because the model used `send_file` instead of writing the path itself.
- **OQ-3 — the harness container reports `unhealthy`.** `wget --spider
  http://localhost:18790/health` gets `Connection refused` on every check while the gateway
  serves fine on `0.0.0.0:18790`. Unrelated to this bug (turns work), but it means container
  health is currently not a usable signal.

## 10. What shipped (option B)

`internal/pico/turn.go`, ~30 lines of behaviour:

- `processor.hasDelivery`, set in the delivery branch. It records that the turn produced
  its output; it still does **not** arm grace, so A-02 and the "never end a turn early"
  half of A-03 are intact.
- `processor.settling()` — `hasDelivery && !hasPlainContent && !isTyping`.
- `Client.silenceBudget()` — the idle window normally, `deliverySettleWindow` (**20s**)
  while settling, never longer than the configured idle window. Used at the loop's
  `idle.Reset`.
- `<-idle.C` while settling returns `proc.finalContent(), nil` — a completion, not the
  failure a healthy turn used to be reported as.

**`!isTyping` is not in §7-B's sketch and was added under test.** Without it, an agent that
delivers a file and then *resumes* work — announcing it with `typing.start` — would be cut
20s later, losing an answer that was still being written: the original bug's cousin, with
the transcript again ahead of the live view. The condition costs nothing on the reported
path, because upstream's `preSendMedia` stops typing before handing a file over
(`pkg/channels/manager.go`), so a delivery arrives with typing already stopped. It also
makes `settling()` refuse exactly what `maybeArmGrace` refuses.

**The twenty seconds are chosen, not measured.** The reasoning is in the const's comment;
what would revise it is a member reporting an answer that went missing right after a file
(the settle window firing over a slow round-trip) — at which point the fix is to raise it
towards, but nowhere near, the idle budget. Nothing configures it today, on purpose: a knob
here cannot be set correctly without knowing the provider's latency, and this deployment
has exactly one shape of turn that reaches it.

**The synchronous path changes with it**, since `handlers.go` calls the same `RunTurn`. A
delivery-only turn there used to be a 502 after ten minutes; it is now a 200 whose content
is the `📎` notices alone (`handlers.go:723` — the same `Attachment` sink appends them
before `RunTurn` returns, so they are present whenever the store succeeded). The one new
shape is a delivery-only turn whose *store* failed: `notices` is empty and the caller gets
a 200 with empty content instead of a late 502. Still logged (`chat: attachment %q not
stored`), and the streaming path has had that property since `agent-attachments` A-07.

**Not covered, deliberately:** a turn whose only output is tool narration (probe 3) still
waits out the idle budget. Narration is what a *working* agent emits, and a long LLM call
after it is normal — there is nothing there to distinguish "over" from "thinking". A
delivery is different: upstream's own branch ends the turn right after it.

### Verification results

- `TestDeliveryOnlyTurnFinalizes` — the reported shape (`typing.start`, `typing.stop`,
  delivery with an empty caption, silence). Finalizes on the settle window with the file
  emitted and no error. **Watched it fail first**, with the bug's exact signature:
  `RunTurn failed on a delivery-only turn: timed out waiting for picoclaw: no frame for 10s`.
- `TestAnswerAfterADeliveryIsNotCut` — an answer arriving inside the window still wins;
  this is the property option A gives up.
- `TestDeliveryFollowedByTypingWaitsForTheAnswer` — a delivery, then `typing.start`, then
  an answer three settle-windows later. **Watched it fail** (`final = ""`) before
  `!isTyping` existed.
- `TestAttachmentFrameNeverErasesOrEndsTheAnswer` and the rest of `internal/pico` — green.
- `go vet ./...` clean; `go test ./...` green except the ten pre-existing
  `internal/docker` failures, which are `lchown … operation not permitted` (that suite
  needs root, as it gets in the image build) and fail identically with the change stashed.

### Still to do

- **Live re-check** (§8 step 3) on the stack the reproduction ran on: ask for a file, watch
  for `POST /v1/chat/completions -> 200 (Ns)` with N in seconds.
- **Option C** remains open on its own merits — the member still never sees
  *"Requested output delivered via tool attachment."* live, only after a reload.
- **OQ-1** (the same file stored 2-3× in one turn) is untouched by this change.

## 11. What shipped for option C (2026-09-05)

`deploy/picoclaw-glob/handled-tool-summary.patch`, the fourth local patch, wired into the
Dockerfile after the other three with a test gate of its own.

- `AgentLoop.publishHandledToolSummary` (new, in `pkg/agent/agent_outbound.go`) publishes
  `handledToolResponseSummary` as an ordinary outbound assistant message, marked final,
  gated `!SendResponse && AllowInterimPicoPublish` — the same condition `Finalize` already
  uses for *"the final answer must still be delivered outside normal SendResponse"*.
- The `allResponsesHandled` branch of `pipeline_execute.go` calls it, right after
  `setFinalContent("")` and before `DismissToolFeedback`. `finalContent` stays empty on
  purpose: the answer really was the tool's output, and this is the only place the turn
  can speak from.

**Effect:** the delivery turn now ends with plain content, so the proxy's ordinary rules
finalize it after `graceWindow` — **500ms instead of the 20s settle window**, and instead
of the ten minutes it started at. The member also reads live the same sentence the
transcript has always held, which closes OQ-2's second half.

**§7-B's settle window stays.** It is now the fallback, not the mechanism: it is what
still bounds a turn if a `PICOCLAW_TAG` bump drops this patch, if the harness is not
picoclaw, or if any other path ends a turn without speaking.

### Verification

| Test | Where | Pins |
|---|---|---|
| `TestHandledToolSummaryReachesTheChannel` | new, in the patch | the summary reaches the channel. **Watched it fail first:** *"the handled-tool summary never reached the channel; it sent []"* |
| `TestProcessMessage_MediaToolHandledSkipsFollowUpLLMAndFinalText` | upstream's own | still green: the turn still skips the follow-up LLM call and still returns no final response |

The whole upstream `./pkg/agent/...` suite passes with all four patches applied (75s),
`go vet` clean, and `docker compose build picoclaw-image` completes — which is the real
gate, since the Dockerfile runs both tests above and fails the build on `no tests to run`.

**Ordering note for the next tag bump:** this patch and `context-routed-agent.patch` both
touch `pkg/agent/pipeline_execute.go`, a dozen lines apart. This one is generated against a
tree with the other three already applied and is applied last; the Dockerfile says so.

**Deployment:** the image is rebuilt (`zombie-crab/picoclaw:0.3.1-glob`). `imageDrift`
compares resolved image IDs rather than tags (`internal/docker/projects.go:402`), so the
harness container is recreated on the next message with no `docker rm` — and because the
fix is upstream of the proxy, it takes effect even against a proxy binary that predates
§10.
