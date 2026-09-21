# Serve the harness's compaction record

The ganglion now writes one record per turn saying it shortened the context
window. This package decides what reaches the member, so the record stops here
unless it is let through.

The harness side is
`crab-ganglion-harness/.specs/features/durable-compaction/spec.md`; the render
is `crab-exoskeleton-webapp/.specs/features/durable-compaction/spec.md`.

## What the record looks like

An **events-only assistant entry** — `role: assistant`, no content, one event of
kind `compact` carrying a `count` and the summary in `detail`. That shape is not
a choice of convenience: it is the only shape in the harness's own format that
is durable and never reaches a provider, because `window.conversational` drops
exactly a tool result and a content-less entry with events.

Which means it arrives here looking **exactly like a silent tool call**.

## FR-4 — The record is served as its own kind

**FR-4.1** `Event` carries `Count`, `omitempty`.

Additive, so every line written before this is byte-identical on the wire. The
count is a number rather than a sentence for the reason the rest of `Event` is
codes: the harness has no locale, and the client renders the words.

**FR-4.2** An assistant entry whose events contain a `compact` event is served
with `Kind = KindCompact`, not `KindStep`.

The assignment runs **after** the two that set `KindStep`, so it wins. A step is
work the agent did inside a turn; this is something that happened *to* the
conversation. Marked as a step it would be folded into a run of narration by the
client's own grouping and disappear into "3 steps" — and the client's
`landingIndex` skips backwards over every step when it picks where to open a
conversation, so a trailing one would be invisible twice over.

**FR-4.3** An events-only entry that is **not** a compaction record is still
narration. The two share a shape; getting this backwards would file every silent
tool call as a shortening of the conversation.

**FR-4.4** A transcript with no such record is served exactly as before, and a
malformed line is skipped rather than failing the read — which is what
`readMessages` already does and what this changes nothing about.

## What was checked and needed no change

- **The blank-entry filter** (`history.go`, `Content == "" && reasoning == "" &&
  len(e.Events) == 0`) already lets an events-only entry through: the
  `len(e.Events)` clause was added for the silent tool call and covers this.
- **`legacyCallEvents` and `keepAnswerlessTurns`** are inert to it. Both walk
  spans between `role: "user"` messages, and the record is an assistant entry
  with no content — `speaks` is false and its `Kind` is non-empty, so neither
  pass can promote it or be confused by it. A record written with `role: "user"`
  *would* have opened a spurious turn in both; it is not.
- **The role filter** keeps `user` and `assistant` only. The record is an
  assistant entry, so it passes without widening an allowlist.

## Known, not addressed

`SyncDurable`'s `dedupKey` is `created_at` + `role`. Two assistant entries
written in the same nanosecond fold into one.

The harness stamps the marker from its own clock and writes it as the **last
transcript write of a turn** — after the answer on the ordinary exit, after the
final events entry on the exhausted-iterations one — so in production the two
differ. A test harness with an injected fixed clock could collide.

Left alone rather than given a discriminator: the fix belongs in the key, and
the key is load-bearing for the picoclaw fold this feature does not touch. Worth
re-checking if the harness ever writes anything after the marker.

## Gate

`go build ./...`, `go vet ./...`, `go test ./...`. `internal/docker` has 10
pre-existing `lchown: operation not permitted` failures on a rootless host; they
pass as root in Docker and are unrelated to this change.
