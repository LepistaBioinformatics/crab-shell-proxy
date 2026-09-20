# Admin config: tuning keys, and a preview for the keys the proxy owns

The admin console's config tab could not reach two things it should have been the
place for, and both turned out to be reachability problems rather than missing
mechanism.

## FR-1 — The harness's tuning numbers are offered by the key picker

**FR-1.1** The ganglion catalog lists `agents.defaults.max_tool_iterations`, the five
`agents.defaults.subturn.*` budgets, and `tools.subagent.enabled`.

**FR-1.2** Those rows carry `tunable: true` and no `value`. There is no default to
report: the harness reads each as a pointer, so an absent key means "the binary's
default" and a written `0` means zero.

**FR-1.3** `ganglionConfigDoc` does **not** emit them, and a test reads the generator's
output directly to keep it that way.

This is the load-bearing half. `crab-ganglion` resolves the turn's cap **file first** —
`agents.defaults.max_tool_iterations`, then `GANGLION_MAX_ITERATIONS`, then its built-in
12. A key written into every rendered document would permanently outrank the variable
this proxy sets from `config.yaml`'s per-agent `maxIterations`, for every agent, and the
operator's only lever would stop working with no error anywhere. The smaller change was
the wrong one.

**FR-1.4** Nothing else changes about the write. The apply verbs already accept any
valid non-managed dotted path, and for a ganglion workspace `WriteInstanceConfig`
records every changed leaf into `.ganglion-overlay.json`, which `renderGanglionConfig`
merges back on every ensure. The mechanism was complete; only the picker was blind.

## FR-2 — A managed key is previewed, not refused

**FR-2.1** `InspectScopeConfigKey` serves a histogram for a managed path instead of
returning `ErrManagedConfigPath`.

**FR-2.2** The response carries `managed`, computed with the same `IsManagedConfigPath`
the apply refuses with, so the two cannot drift. It is the server's answer because a
managed path can be typed by hand and need not be a catalog row — a client inferring
"absent from the catalog, therefore editable" would offer a write that can only 400.

**FR-2.3** `ApplyScopeConfigKey` refuses exactly as before. Nothing about editing is
relaxed.

**FR-2.4** **Credentials never travel.** `ReadInstanceConfig` masks
`model_list[*].api_keys` and every `tools.mcp.servers.*.headers` value before returning,
and the inspection buckets those already-masked bytes — the two managed families that
carry a secret. `TestInspectScopeConfigKeyNeverServesACredential` seeds a live-looking
key and a bearer token, reads both back through the preview, and fails if either appears
**or if the mask does not**: asserting only the absence of the secret would pass just as
well on an empty histogram.

### Why the refusal was wrong

It was justified on the grounds that a caller asking about a managed path needs to be
told the edit could not survive rather than handed a histogram it will act on. The
telling is now the `managed` flag, in the same response, and it is a stronger guard than
the refusal was. The hiding bought nothing: `GET` on an instance's configuration already
serves those same bytes to the same authority, one member per request, so an admin who
wanted to know what the proxy had written simply read them the slow way.

## Out of scope

**Scheduled-task limits.** `maxSchedulesPerWorkspace`, `agentMinEveryMs`,
`agentCreateMax`, `cron.MinEveryMs`, `maxScheduledMessage` and the scheduler's sleep
bounds are compile-time constants the proxy enforces in its own code. The ganglion
overlay cannot reach them — it feeds the *harness's* configuration document. Making them
per-instance would mean the `.crab-mode.json` / `ModeFor` pattern: a proxy-owned override
file read at each enforcement site. Most of those constants also live on an unmerged
branch, so there is nothing here to build against yet.

A cron run's **iteration depth** is not out of scope and needs nothing: a scheduled turn
is an ordinary turn of the same harness loop, bounded by the same
`agents.defaults.max_tool_iterations` FR-1 now exposes.
