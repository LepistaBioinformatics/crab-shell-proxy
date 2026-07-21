# State

**Last Updated:** 2026-07-21
**Current Work:** Branch `feat/admin-model-override` — admin model override + default-template auto-bootstrap landed; project docs initialized.

---

## Recent Decisions (Last 60 days)

_None recorded yet. Decision history for the wider stack (including AD-009, the crab-shell-proxy architecture decision) lives in the parent repo's `.specs/project/STATE.md`._

---

## Active Blockers

_None._

---

## Lessons Learned

### L-001: `EnsureRunning` chown tests fail in the dev sandbox (environmental, not a code defect)

**Context:** `internal/docker/manager_test.go` `TestEnsureRunning*` and other tests that chown a workspace to uid 1000.
**Problem:** The sandbox denies the chown (`operation not permitted`), so these tests fail here regardless of code correctness — the failure predates current work and is present on the base commit.
**Solution:** Verify chown-dependent behavior via the pure helper tests that don't chown (e.g. the `effectiveSkills` / model-resolution helpers). Treat these specific failures as sandbox noise, not regressions.
**Prevents:** Chasing phantom regressions when the only red tests are the known chown ones.

---

## Quick Tasks Completed

| #   | Description                                                            | Date       | Commit    | Status  |
| --- | --------------------------------------------------------------------- | ---------- | --------- | ------- |
| 001 | Move root `.sdd-*` reports into `.specs/features/`; default SDD to `.specs` (+ `.claude/CLAUDE.md`) | 2026-07-21 | `eb0c7b9` | ✅ Done |

---

## Deferred Ideas

- [ ] Per-user (not just per-agent) lifecycle-mode overrides — Captured during: project init.
- [ ] Non-picoclaw harness support (Hermes) behind the same proxy — Captured during: project init (see parent `.specs/features/multi-harness-support/`).

---

## Todos

_None._
