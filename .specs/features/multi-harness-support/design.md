# multi-harness-support Design (crab-shell-proxy)

Implements the P1 "chat works with a Hermes-backed agent" slice from the parent
spec (`../../../../.specs/features/multi-harness-support/spec.md`, MHS-01,02,05,
09,10,12,13,14,15). Branch-at-seams — no grand harness interface — matching the
grain the repo already set (`defaulttemplate/<harness>/`, `materializeDefaultTemplate(harness)`).

## Scope of THIS slice (advisor-gated)

**In:** `harness` selector + `model.baseUrl`; a Hermes container (image, `/opt/data`
mount, `API_SERVER_*` env, provider key, non-root UID); an OpenAI-passthrough turner
to `:8642`; Hermes provisioning (`config.yaml`); turner selection by harness; picoclaw
path byte-for-byte unchanged.

**Out (named follow-ups):** Hermes history (`state.db` SQLite reader — needs a driver,
build-system change; P2/MHS-18) — for now `history.SyncDurable`/`Read` no-op harmlessly
for Hermes; shared secrets/skills/files cascade; managed content; native secrets;
model-override admin path; `continuous`/scale-to-zero tuning for Hermes; any webapp change.

## Hermes container — what it gets, and explicitly NOT

Gets: `image=nousresearch/hermes-agent:latest`, bind `<userDir>:/opt/data`, env
`API_SERVER_ENABLED=true`, `API_SERVER_HOST=0.0.0.0`, `API_SERVER_PORT=8642`,
`API_SERVER_KEY=<generated>`, the provider key under its in-container env name
(e.g. `GLM_API_KEY`), `PUID`/`PGID` from the configured user; labels + network as today.

Does NOT get (all picoclaw-`.picoclaw/workspace`-relative, wrong for flat `/opt/data`):
`workspace/.secrets`, `.shared/tenant|subscription`, managed skill/memory mounts,
`/skills` effective-skills mount, `.security.yml` native secrets.

## Changes

### 1. `internal/config/config.go`
- `Agent.Harness string` (yaml `harness`), default `"picoclaw"` in a new
  `applyDefaults` agent loop; validate ∈ {`picoclaw`,`hermes`} (unknown → load error).
- `ModelConfig.BaseURL string` (yaml `baseUrl`) + `ModelConfig.KeyEnvName string`
  (yaml `keyEnvName`) — the in-container env var Hermes reads the key under
  (provider-specific, e.g. `GLM_API_KEY`; not derivable from provider).

### 2. `internal/turn/turn.go` (new leaf package)
```go
type Request struct { Endpoint, AuthToken, SessionID, SessionKey, Model, Content string }
```
Shared by pico, hermes, httpapi — avoids an import cycle (httpapi defines the
consumer interface referencing `turn.Request`).

### 3. `internal/pico/turn.go`
- `RunTurn(ctx, turn.Request, onDelta)` — read Endpoint/AuthToken/SessionID/Content
  from the struct (SessionKey unused). Behavior identical. Update `turn_test.go`.

### 4. `internal/hermes/turn.go` (new) + `turn_test.go`
- `Client{TurnTimeout}` implementing the same signature. POSTs
  `Endpoint+"/v1/chat/completions"`, `Authorization: Bearer AuthToken`, headers
  `X-Hermes-Session-Id: SessionID`, `X-Hermes-Session-Key: SessionKey`, body
  `{model, messages:[{role:"user",content}], stream:true}`. Parse SSE
  `chat.completion.chunk`, emit `choices[0].delta.content` via onDelta, accumulate,
  stop on `[DONE]`. Ignore `hermes.tool.progress` and role-only deltas. Test with
  `httptest` serving OpenAI SSE.

### 5. `internal/docker/manager.go`
- `Target{ Name, Endpoint, AuthToken, Harness string }` (rename WSEndpoint→Endpoint,
  PicoToken→AuthToken, add Harness).
- `harnessPort(agent)` → 8642 for hermes else PicoclawPort; `endpoint(agent,name)` →
  `http://name:8642` for hermes else `ws://name:PicoclawPort/pico/ws`;
  `waitHealthy`/`httpHealth` use `harnessPort`.
- `EnsureRunning`: if `agent.Harness=="hermes"` → `provisionHermes(...)` (returns the
  generated API_SERVER_KEY as authToken) + `createHermes(...)`; else the existing
  picoclaw flow verbatim. Return `Target{Harness: agent.Harness, ...}`.
- `createHermes`: minimal spec per the "gets" list above.

### 6. `internal/docker/provision_hermes.go` (new)
- `provisionHermes(userDir, templateDir, home, user, model) (apiServerKey string, err)`:
  first-provision seed of `config.yaml` (from `defaulttemplate/hermes`, self-healing via
  `materializeDefaultTemplate(templateDir,"hermes",user)`), patch `model.{default,provider,base_url}`,
  chown; generate + return an `API_SERVER_KEY`. Provider key + API_SERVER_KEY are injected
  as container env (not written to disk) by `createHermes`.

### 7. `internal/httpapi/handlers.go` + `sse.go`
- `Turner.RunTurn(ctx, turn.Request, onDelta)`; `Server.Pico`→ keep, add `Server.Hermes Turner`;
  `turnerFor(harness)` selects. Call sites build `turn.Request{Endpoint:tgt.Endpoint,
  AuthToken:tgt.AuthToken, SessionID:sessionKey, SessionKey:key.UserAccID+":"+key.Role,
  Model:model, Content:userContent}`. History sync stays (no-op for hermes).

### 8. `internal/docker/defaulttemplate/hermes/config.yaml` + `cmd/.../main.go`
- Minimal Hermes `config.yaml` (model section patched at provision). Wire
  `Hermes: &hermes.Client{TurnTimeout: ...}` in main.go. Document `harness`/`baseUrl`/
  `keyEnvName` + a commented hermes agent in `config.yaml`.

## Gate
`go build ./... && go vet ./... && go test ./...` green. Live E2E (real container +
z.ai key + published 8642) operator-gated.
