# T1 report — Config builders + skill store (admin-shared-skills)

Branch: `feat/admin-shared-skills` (crab-shell-proxy). Scope: T1 only — no
mounts/handlers touched.

## What was built

### `internal/config/config.go`
- `TenantSharedSkillsDir(root, tenantID string) string` —
  `<root>/tenants/<t>/shared/skills`.
- `SubscriptionSharedSkillsDir(root, tenantID, subsAccID string) string` —
  `<root>/tenants/<t>/subscriptions/<s>/shared/skills`.
- Both mirror `TenantSharedFilesDir`/`SubscriptionSharedFilesDir` exactly
  (same `identity.SanitizeID` per dynamic segment), placed right after the
  existing `*SharedSecretsDir` builders.

### `internal/docker/skills.go` (new, package `docker`)
- `SkillMeta{Name, Description, Size, ModifiedAt, HasFiles}` with the
  requested json tags.
- `(m *Manager) sharedSkillsDir(scope Scope) string` — mirrors
  `sharedFilesDir` in `shared.go`.
- `sanitizeSkillName(raw string) (string, error)` — trims whitespace first,
  then requires the trimmed value to already be lowercase and match
  `^[a-z0-9][a-z0-9._-]{0,63}$`; rejects empty and the reserved name
  `shared-content` (the operator-managed skill's dir name in
  `managed_skills.go`).
- `parseSkillFrontmatter(skillMD string) (name, description string, err error)`
  — line-scans the leading `---`…`---` fence for `name:`/`description:`,
  stripping one matching pair of quotes; errors on missing fence or empty
  field. No YAML dependency.
- `(m *Manager) ListSharedSkills(scope Scope) ([]SkillMeta, error)` — lists
  immediate subdirs (skipping dot-prefixed dirs — see Deviations), building
  each `SkillMeta` via a `skillMeta(dir, dirName)` helper: frontmatter name/
  description with fallback to `dirName`/`""` when SKILL.md is unreadable or
  unparsable; `Size` = sum of all file sizes under the dir; `ModifiedAt` =
  newest mtime via the same `modTime()` helper `listFileMeta` uses (RFC3339);
  `HasFiles` = true if anything besides `SKILL.md` exists (files or dirs).
  Sorted by name; absent dir → `[]SkillMeta{}`, no error.
- `(m *Manager) ReadSharedSkillDoc` / `WriteSharedSkillDoc` — read/write
  `SKILL.md`; write validates frontmatter first, `MkdirAll(0700)`,
  `WriteFile(0600)` (truncating), `chownTree`. Leaves other files intact.
- `(m *Manager) WriteSharedSkillZip` — reads `r` into a buffer capped at
  `m.cfg.MediaMaxBytes` (`readAllCapped`), opens with `archive/zip`. **All**
  entries are validated (`validateZipEntry`) and size-capped **before**
  anything is extracted: reject absolute paths, any `..` segment (checked
  both on the cleaned path and per-segment), symlinks
  (`Mode()&os.ModeSymlink`), irregular modes (not regular, not dir), depth >
  8 segments, per-entry size > `MediaMaxBytes`, running total > `MediaMaxBytes`,
  and entry count > 200. Requires a top-level `SKILL.md` entry. Only after
  full validation does it `MkdirTemp` a sibling `.{name}.tmp-XXXX`, extract,
  re-validate the extracted `SKILL.md`'s frontmatter, then atomically
  `RemoveAll` + `Rename` into the final `dir/<name>`, then `chownTree`. Any
  failure returns before the temp dir is created, or the deferred
  `os.RemoveAll(tmp)` cleans it up — nothing is ever written to the final
  path on error.
- `(m *Manager) ArchiveSharedSkill` — walks the skill dir and streams a zip
  (entries relative to the skill dir, `filepath.ToSlash`'d) to `w`.
- `(m *Manager) DeleteSharedSkill` — `os.RemoveAll(dir/<name>)`, naturally
  idempotent.

### `internal/docker/skills_test.go` (new)
19 tests (see Test list below), reusing `sharedManager(t)` /
`tenantScope()` from `shared_test.go` (same package) for `*Manager`
construction.

## Test list + results

```
go test ./internal/docker/... -run 'Skill' -v
```
All 19 pass:

- `TestSanitizeSkillNameValid`
- `TestSanitizeSkillNameInvalid`
- `TestSanitizeSkillNameReserved`
- `TestSanitizeSkillNameChanged`
- `TestParseSkillFrontmatterMissingBlock`
- `TestParseSkillFrontmatterMissingName`
- `TestParseSkillFrontmatterMissingDescription`
- `TestParseSkillFrontmatterQuotedValues`
- `TestParseSkillFrontmatterUnquoted`
- `TestWriteReadSharedSkillDocRoundTrip`
- `TestWriteSharedSkillDocRejectsInvalidFrontmatter`
- `TestWriteSharedSkillZipExtractsGoodZip` (SKILL.md + references/x.md)
- `TestWriteSharedSkillZipRejectsTraversal` (`../escape` entry)
- `TestWriteSharedSkillZipRejectsSymlink`
- `TestWriteSharedSkillZipRejectsOversizeEntry`
- `TestWriteSharedSkillZipRejectsTooManyEntries` (201 entries, cap 200)
- `TestWriteSharedSkillZipRejectsMissingSkillMD`
- `TestArchiveSharedSkillProducesReadableZip`
- `TestDeleteSharedSkillIdempotent`

Each rejection test asserts nothing was written to the final skill dir path
(`assertNothingWritten`).

## Gate output

- `gofmt -l internal/config/config.go internal/docker/skills.go internal/docker/skills_test.go`
  → no output (clean).
- `go build ./...` → clean.
- `go vet ./internal/config/... ./internal/docker/...` → clean.
- `go test ./internal/config/...` → **pass**.
- `go test ./internal/docker/...` (whole package) → **FAILS**, but only on
  **pre-existing** tests unrelated to T1: `manager_test.go`'s
  `TestEnsureRunningColdStart`, `TestCreateAddsReadOnlySecretsBind`,
  `TestRestartWorkspaceRestartsAndRearms`, `TestEnsureRunningSingleFlight`,
  `TestEnsureRunningReusesRunning`, `TestScaleToZeroIdleStop`,
  `TestContinuousDoesNotArmIdle`, `TestReconcileEnsuresContinuousWorkspaces`
  all fail with `chown ... operation not permitted` — they hard-code
  `PicoclawUser: "1000:1000"` and this sandbox can't chown to uid 1000 as a
  non-root user. Verified pre-existing (not introduced by T1) via
  `git stash -u` + rerun on the unmodified branch tip — same failure.
  All new `*Skill*` tests pass in isolation (`-run 'Skill'`), and the whole
  package's only *new* content (skills.go/skills_test.go) is otherwise
  gofmt-clean/vet-clean.
- `gofmt -l .` (repo-wide) → **not clean**, but only pre-existing files
  outside T1 scope: `internal/authz/authz_test.go`, `internal/docker/client.go`,
  `internal/docker/managed_skills.go`, `internal/httpapi/admin.go`,
  `internal/httpapi/handlers.go`, `internal/httpapi/handlers_test.go`,
  `internal/pico/turn_test.go`. None of these were touched by T1.

## Deviations / notes

1. **Manager construction in tests**: reused the existing `sharedManager(t)`
   helper from `shared_test.go` (same package) instead of writing a new one
   — it already builds a `*Manager` over `t.TempDir()` with
   `PicoclawUser` empty, which makes `chownTree` a no-op (it returns `nil`
   immediately when `user == ""`), so the zip/doc-write paths that call
   `chownTree` run unprivileged in tests without needing a real uid.
2. **`ListSharedSkills` skips dot-prefixed dirs.** Not explicitly asked for
   in the task text, but added after review: `WriteSharedSkillZip` creates a
   sibling temp dir `.{name}.tmp-XXXX` inside the same `sharedSkillsDir`
   before the atomic rename; if the process crashed between `MkdirTemp` and
   the deferred cleanup (e.g. killed mid-extract), a leftover dot-dir would
   otherwise surface as a phantom skill in listings (and later, in T2, as a
   phantom mount). One-line guard, no behavior change for the documented
   test matrix.
3. **Fallback breadth in `ListSharedSkills`**: the design says fall back to
   `Name=dirName, Description=""` "if unreadable" — implemented that
   fallback for both an unreadable `SKILL.md` *and* one that reads but fails
   frontmatter parsing (e.g. a hand-edited malformed file), so one broken
   skill can never fail the whole listing.
4. Did not add a `config_test.go` test for the two new builders — the
   existing style tests `Tenant/SubscriptionSharedFilesDir` etc. only
   indirectly (via `internal/docker` tests use them directly), and the task's
   test list doesn't call for a distinct config-level test; `skills_test.go`
   exercises both builders transitively through `sharedSkillsDir`/every
   store method.
