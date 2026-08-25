# Quick Task 002: stop classifying uploads by extension

**Date:** 2026-08-24
**Status:** Done (build + tests green; runtime-unverified until the image is rebuilt)

## Description

`POST /v1/media` refuses any filename whose extension is not in
`Cfg.MediaAllowedExts`. Members hit this with ordinary working files the list
does not name — `.parquet`, `.fasta`, `.dcm` — and with files that have no
extension at all, since `filepath.Ext("Makefile")` is `""` and `""` is not in the
list.

The workaround they found is worse than the refusal: rename the file so the
picker will show it, or zip it. A renamed file's extension now **lies about its
bytes**, and the agent opens it as the type the name claims.

The webapp half (the picker's `accept`, which is what hides the file in the OS
dialog in the first place) is specified in `crab-exoskeleton-webapp`,
`.specs/features/unrestricted-upload-types/spec.md`. This task is the server
gate only. **Order matters:** if the client ships first, the dialog offers a file
the proxy then rejects, which is worse than today.

## What changes

`internal/config/config.go`

- Delete the `MediaAllowedExts` field and its default block
  (`applyDefaults`, currently ~line 396). `MediaMaxBytes` is untouched — the size
  cap is a separate rule with a separate reason.

`internal/httpapi/handlers.go`

- Delete `mediaExtAllowed` (~line 1491) and the 400 in `handleMediaPost` that
  calls it (~line 1238).

`internal/httpapi/projects_test.go`

- Line ~256 sets `s.Cfg.MediaAllowedExts = []string{"zip"}`. The field is gone;
  the line goes with it, and whatever the test asserted about a rejected type is
  either dropped or restated against a rule that still exists.

**Not changed:** `sanitizeFilename` already reduces any name to a safe base name
(collapses unsafe characters, strips leading dots, rejects traversal and empty),
and it needs no help — an extensionless name survives it intact. `StoreMedia`,
the listing, the download route and the folder operations never looked at the
extension.

## Why this is safe to remove

The download route is what makes an arbitrary upload harmless, and it is
unchanged:

```go
w.Header().Set("Content-Type", "application/octet-stream")
w.Header().Set("Content-Disposition", `attachment; filename="`+display+`"`)
```

Bytes never render inline from this origin, whatever they are. The webapp's
preview is a strictly smaller allowlist of its own (`PREVIEW_KINDS`) and is
deliberately **not** widened alongside this change. Nothing in the proxy executes
an uploaded file; it is written `0600`, chowned to the picoclaw user, and read as
data.

## Verification

- `go build ./...` and `go test ./internal/...` clean, with `projects_test.go`
  updated rather than skipped.
- A new case in the media handler tests: an upload named `Makefile` (no
  extension) and one named `sample.parquet` both store and appear in the listing.
  Both are rejected on a clean HEAD, so the tests fail before the change and pass
  after.
- `grep -rn "MediaAllowedExts\|mediaExtAllowed"` returns nothing outside this
  task's diff.

## What actually happened

As written, plus one thing the tests found:

- `TestMediaUploadStillHonoursTheSizeCap` first failed for the wrong reason. The
  body bound is `MaxBytesReader(r.Body, MediaMaxBytes + 1<<20)` — a **megabyte of
  slack** over the configured cap, for the multipart envelope. So a file between
  `MediaMaxBytes` and `MediaMaxBytes + 1 MiB` is accepted today, and the
  configured number is not the enforced one. Left alone here (it is the size
  rule, not the type rule) and written into
  `admin-managed-storage-limits`, whose per-scope cap is checked against
  `header.Size` after the parse and is therefore exact.
- `mediaUploadReq` grew two variants (`…Named`, `…Sized`) so the filename and the
  payload size are the caller's; the original helper delegates and every existing
  call site is unchanged.
- Baseline is **10** pre-existing `lchown` failures in `internal/docker`, not the
  9 the older task notes record. Verified by `git stash` + re-run: the same 10
  before and after this change.

## Follow-up (not in this task)

The 413 message for an oversized upload is a prose sentence
(`"file exceeds the %d-byte limit"`), which the webapp cannot map to a
translatable code — it currently surfaces as a generic error. Making the cap
administrable, and reporting it legibly, is
`.specs/features/admin-managed-storage-limits/`.
