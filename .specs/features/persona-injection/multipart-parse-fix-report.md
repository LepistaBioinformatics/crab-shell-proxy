# persona-injection — fix report: Identity saves rejected with a tenant_id error

**Date:** 2026-07-30
**Status:** Fixed, tests green. **Not yet deployed** — the admin UI stays broken
until this proxy is rebuilt and redeployed.

## Symptom

Saving any of the four identity `.md` files from the admin screen's **Identity**
tab (the `persona` panel in the webapp) failed with:

```json
{ "error": "\"tenant_id\" is required and must be a UUID", "status": 400 }
```

POST only. Listing and deleting were unaffected.

## Root cause

`handleAdminPersonaPost` called `r.ParseForm()`, while every client posts these
writes as `multipart/form-data` (the webapp's `savePersona` builds a `FormData`,
and its BFF at `app/api/admin/persona/route.ts` re-emits one).

For a multipart body, Go's `ParseForm` populates `r.Form` from the **query string
only** — and leaves it non-nil. `r.FormValue` parses a multipart body lazily, but
only `if r.Form == nil`, so the guard was already satisfied and the body was never
read. Every field came back `""`, and `parseAdminScope("", "", "")` returned
exactly the message above (`internal/httpapi/admin.go:40`).

The wording in the user's report is the proxy's own, arriving through the webapp's
`upstreamError` passthrough — i.e. the request had already cleared the BFF's
`typeof tenantId !== "string"` guard, which is what placed the defect upstream.

The two sibling handlers were already correct — `/v1/admin/shared` (files) and
`/v1/admin/shared-skills` both use `ParseMultipartForm(1 << 20)`. Persona was the
only outlier: `grep -rn "ParseForm()" internal/` matched one line in the whole
tree, and it was this one.

## Fix

`internal/httpapi/admin.go` — `handleAdminPersonaPost` now reads the fields from
**either** encoding:

```go
if err := r.ParseMultipartForm(4 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
```

`ParseMultipartForm` calls `ParseForm` **first** — which parses a urlencoded body —
and only then fails with `ErrNotMultipart`. Tolerating that one error therefore
accepts both encodings in a single call, with no Content-Type sniffing, and
`?restart=` / `?restart_at=` / `?restart_note=` keep resolving from the query
either way.

Accepting both is not indecision. The webapp had to switch its upstream leg to
urlencoded to work against the proxy **already deployed** (see the deploy note
below), so both encodings have live clients and neither repo's deploy order can
break Identity again. A body that is neither — JSON, say — still gets a 400 with no
write.

The budget is `4 << 20`, not the siblings' `1 << 20`, and the difference is
deliberate: `maxMemory` bounds non-file parts *in total* and only file parts spill
to temp storage. `/v1/admin/shared` and `/v1/admin/shared-skills` upload their
payload as a **file** part, so their limit never bounds the content; persona sends
the document as the `body` **field**, so this number is the effective maximum size
of an identity file. Over it, the parse fails and the caller sees "could not read
the form body" rather than a size complaint — hence the headroom.

`docker.WritePersona` does **not** branch on the file name (verified): all four
files, `USER.md` included, take the same write path, and the seed-only distinction
lives in the mount/provision logic. So the two tests below cover the write for
every name the endpoint accepts.

## Deploy note

This proxy fix only reaches the user once the image is rebuilt and redeployed. To
unblock Identity before that, the webapp's BFF now sends the upstream leg as
urlencoded — which the currently deployed handler reads correctly — recorded in
`crab-exoskeleton-webapp/.specs/quick/003-persona-urlencoded-upstream/`. That is
why this handler accepts both encodings rather than only multipart: with two live
client encodings, every deploy order works.

## Verification

- `internal/httpapi/admin_test.go` — four new tests. The first two were written
  before the fix and confirmed to fail with the user's exact 400 body:
  - `TestAdminPersonaPostMultipart` — a multipart write returns 200 **and** the
    recorded `WritePersona` call carries the posted scope, agent, name and body.
    Asserting the recorded write (not just the status) is deliberate: the fields
    arriving empty is how this bug presented.
  - `TestAdminPersonaPostMultipartHonoursQueryRestartPolicy` — `?restart=notice`
    on a multipart request still parses, and raises a notice instead of bouncing.
    This pins the `ParseForm`-inside-`ParseMultipartForm` behaviour the fix leans on.
  - `TestAdminPersonaPostUrlencoded` — the encoding the webapp now sends is
    accepted, fields and all.
  - `TestAdminPersonaPostRejectsNonFormBody` — a JSON body still gets a 400 and
    writes nothing, so tolerating `ErrNotMultipart` did not become "accept
    anything".
- `internal/httpapi/handlers_test.go` — `fakeOrch.WritePersona` now records its
  arguments (`personaWrites`), previously a bare `return nil`.
- `go test ./internal/httpapi/` passes; `go vet ./...` clean; `gofmt` clean on the
  files touched.
- `internal/docker` still fails its chown tests — L-001 in `.specs/project/STATE.md`,
  reproduced here with these changes stashed, so it is sandbox noise and unrelated.

## Behaviour change worth knowing

An identity file larger than 4MiB now fails the parse (see the size note above),
surfacing as `400 "could not read the form body"` rather than as a size complaint.

Nothing else narrows: before the fix, urlencoded was the *only* encoding that could
work, and it still works. The persona endpoints are not in `openapi.json` (which
documents chat, models and restart only), so no published contract had to move.
