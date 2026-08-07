# chat-empty-message-filter (proxy half)

The webapp renders one padded band per history message, so a blank turn served by
`/v1/sessions/history` becomes a tall empty gap in the transcript — and it also
counts as a neighbour when the client spaces the real messages around it.

Full spec, decisions and the confirmation still owed live in the webapp repo:
`crab-exoskeleton-webapp/.specs/features/chat-empty-message-filter/spec.md`.

## Change here

`internal/history/history.go` — `readMessages` already meant to exclude blank
turns, but the predicate was `e.Content != ""`. picoclaw also writes turns whose
content is nothing but whitespace (`"\n"`, `" "`), and an exact comparison serves
those. Now `strings.TrimSpace(e.Content) != ""`.

This is the root cause, so it is fixed at the source rather than only in the
client — it also keeps blank turns out of any future consumer of this API. It
takes a rebuild/redeploy to take effect, so the webapp keeps its own filter at the
BFF boundary; the two are deliberately redundant.

## Verification

`TestReadFindsAndFiltersTranscript` gained a `"   "` turn and a `"\n\n"` turn in
the fixture. The test discriminates: on the old predicate the transcript yields 4
messages against a `want 2` assertion.

`go vet ./...` clean. `go test ./internal/history/` passes. (`internal/container`
fails under this sandbox with `lchown … operation not permitted` — pre-existing,
needs root, unrelated.)
