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

**Context:** `internal/docker/manager_test.go` `TestEnsureRunning*` (including `TestEnsureRunningRecreatesOnPersonaDrift`) and other tests that chown a workspace to uid 1000.
**Problem:** The sandbox denies the chown (`operation not permitted`), so these tests fail here regardless of code correctness — the failure predates current work and is present on the base commit.
**Solution:** Verify chown-dependent behavior via the pure helper tests that don't chown (e.g. the `effectiveSkills` / model-resolution helpers). Treat these specific failures as sandbox noise, not regressions. When a chown-dependent test IS the verification, run it as root: `docker build ./crab/crab-shell-proxy` executes `go vet ./... && go test ./...` inside the build, or mount the tree into `golang:1.23-bookworm` and run it there.
**Prevents:** Chasing phantom regressions when the only red tests are the known chown ones.

---

## Quick Tasks Completed

| #   | Description                                                            | Date       | Commit    | Status  |
| --- | --------------------------------------------------------------------- | ---------- | --------- | ------- |
| 001 | Move root `.sdd-*` reports into `.specs/features/`; default SDD to `.specs` (+ `.claude/CLAUDE.md`) | 2026-07-21 | `eb0c7b9` | ✅ Done |
| 002 | Identity (persona) saves 400'd on `tenant_id`: `handleAdminPersonaPost` used `ParseForm` on a multipart body. Now accepts multipart **and** urlencoded (the webapp moved to urlencoded so Identity works pre-deploy). See `.specs/features/persona-injection/multipart-parse-fix-report.md` | 2026-07-30 | uncommitted | ✅ Fixed |
| 003 | Persona: Identity editor preloads the agent template (+`source` label, Save requires a change); a container whose persona mounts are stale is now rebuilt on its next request, so a saved identity actually reaches the instances. See `.specs/features/persona-injection/identity-preload-and-propagation-report.md` | 2026-07-30 | uncommitted | ✅ Fixed (candidate 1) |
| 004 | Agent attachments: `Requested output delivered via tool attachment.` arrived with no file because the pico `Payload` dropped upstream's `attachments` array. Proxy now decodes it, fetches from the harness media route and stores under `uploads/attachments/` (visible in the uploads sidebar); plus a default skill telling the agent to write deliverables there. See `.specs/features/agent-attachments/spec.md` | 2026-07-30 | uncommitted | ✅ Fixed (unit-level; e2e needs the stack) |

---

## Deferred Ideas

- [ ] Per-user (not just per-agent) lifecycle-mode overrides — Captured during: project init.
- [ ] Non-picoclaw harness support (Hermes) behind the same proxy — Captured during: project init (see parent `.specs/features/multi-harness-support/`).

---

## Todos

_None._
