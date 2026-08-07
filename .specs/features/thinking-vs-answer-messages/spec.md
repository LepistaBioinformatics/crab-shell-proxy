# thinking-vs-answer-messages (proxy half)

The transcript served by `/v1/sessions/history` did not distinguish the agent
narrating its work from the agent answering, so the client rendered both as
ordinary messages. Measured on this deployment's own durable transcripts, that is
200 of 304 assistant messages.

Full evidence, decisions and the client half live in the webapp repo:
`crab-exoskeleton-webapp/.specs/features/thinking-vs-answer-messages/spec.md`.

## Why the proxy is where this belongs

The live stream already draws the line: `internal/pico/turn.go:172` skips
`kind == "thought"` and `kind == "tool_calls"` frames from content and re-emits
them as `x_crab_progress`. History did not, so live and reload disagreed —
against the principle `history.go` already states, *"History should match what
the user actually saw."*

## Changes — `internal/history/history.go`

- `jsonlEntry` reads `tool_calls` (presence only; kept `[]json.RawMessage`
  because the calls themselves are picoclaw's business) and `reasoning_content`.
- `Message` gains `Kind` (`"step"` on narration, omitted on an answer) and
  `Reasoning`. Both `omitempty`, so an older client sees the response it always
  saw.
- An entry now survives when it has content **or** reasoning. Over half the
  entries carrying `reasoning_content` (52 of 82) have empty content and were
  being dropped whole, taking the reasoning with them.
- `keepAnswerlessTurns` is the safety floor. Demoting every tool-call frame is
  unsafe: a model can deliver its whole reply in the same frame as a trailing
  call, and **7 of 112 sampled turns did exactly that**. Narration stays demoted
  only while its turn still has a plain assistant message with text.

## Verification

`TestReadSeparatesNarrationAndReasoning` — narration is marked, reasoning is
trimmed and kept, and a reasoning-only entry with empty content survives.
`TestReadKeepsAnswerWhenTurnIsAllNarration` — the floor: a turn whose only texty
entry carried a tool call keeps it as the answer, while a turn that has a real
answer keeps its narration demoted.

`go build ./...` and `go vet ./internal/history/` clean; `go test
./internal/history/` passes. (`internal/container` fails under this sandbox with
`lchown … operation not permitted` — pre-existing, needs root, unrelated.)

Takes effect only after a rebuild/redeploy.
