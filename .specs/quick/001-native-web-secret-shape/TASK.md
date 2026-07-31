# Quick Task 001: write native web secrets in the shape picoclaw parses

**Date:** 2026-07-31
**Status:** Done

## Description

Registering a native web-search credential (e.g. `web.brave`) took the agent's
container down: it stopped starting and returned nothing to the chat UI.

`setNativeSlot` wrote the slot as a bare string — `web: {brave: "<key>"}` — but
picoclaw types every web provider as a config struct, so its security-config
parse failed and the gateway exited before serving anything. Reproduced against
`sipeed/picoclaw:latest` (0.3.1) with a copy of a live workspace:

```
ERR gateway > Gateway startup failed
  error="error loading config: failed to load security config:
  failed to parse security config /data/.picoclaw/.security.yml: yaml: unmarshal errors:
  line 10: cannot unmarshal !!str `test-br...` into config.BraveConfig"
```

The same workspace with the nested shape starts normally (`✓ Gateway started`).

## The two shapes

From picoclaw 0.3.1's own config structs, the credential is the ONLY field
`.security.yml` carries under a provider — `enabled`, `max_results` and
`base_url` are all `yaml:"-"`, so they live in `config.json` and are untouched
here.

| Providers | Field |
|---|---|
| brave, tavily, kagi, perplexity | `api_keys` — a LIST of strings |
| gemini, glm_search, baidu_search | `api_key` — a single string |

```yaml
web:
    brave:
        api_keys:
            - <key>
```

`model_list.<model>.api_keys` was already correct (it wrote a list) and is
unchanged.

## What changed

`internal/docker/secrets.go`

- `webKeyListProviders` — the four providers whose field is the plural list.
- `setWebCredential` — get-or-create the provider block via the existing
  `childMap` idiom and set only the credential field, so a hand-set sibling
  survives. Because `childMap` replaces a non-map value, a workspace already
  poisoned by the old flat write is repaired on its next ensure — the overlay is
  re-applied every time.
- `setNativeSlot`'s web branch delegates to it.

`unsetNativeSlot` is unchanged: dropping the whole provider entry is still right,
since the credential is the only thing in it.

## Verification

- `TestNativeWebSlotShapePerProvider` — for all 7 providers, marshals and reads
  back the YAML and asserts the exact field and type picoclaw's decoder will see,
  with the plural set restated independently so a typo in the production map
  fails the test.
- `TestSetNativeWebSlotRepairsFlatLegacyValue` — a workspace left unbootable by
  the old write is repaired, not preserved.
- Three existing tests that asserted the flat string now assert the nested list
  (`secrets_test.go` ×2, `provision_model_test.go` ×1).
- `go build ./...` clean; `internal/httpapi` and `internal/registry` pass.
  `internal/docker` has 9 failures that are identical on a clean HEAD — all
  `lchown … operation not permitted`, i.e. they need root.

**Why the shape is pinned by a unit test rather than a boot check:** picoclaw
silently ignores an unrecognized field *under* a provider. `web.brave.bogus: x`
boots exactly as cleanly as the correct shape, so "the container starts" proves
only that the crash is gone — never that the credential landed.

## Still not verified

- No end-to-end run through the admin UI: the fix is in the proxy image, and the
  running stack still has the old binary until it is rebuilt.
- Whether the key alone makes the provider *usable*. `enabled` is `yaml:"-"`, so
  `.security.yml` cannot set it, and the shipped `config.json` has
  `tools.web.brave.enabled: false` with `provider: "auto"`; nothing in the proxy
  flips it. Whether `auto` requires `enabled: true` was not tested. If it does,
  an admin credential is stored correctly but inert — see below.

## Follow-up (not in this task)

Should registering a search credential also activate that provider in
`config.json`, and which provider wins when several have keys? That is a design
decision, so it is deliberately out of a quick fix.
