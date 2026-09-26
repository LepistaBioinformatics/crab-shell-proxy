# Tasks — member-owned skills (proxy + gateway)

Spec: `./spec.md`. Webapp tasks:
`crab-exoskeleton-webapp/.specs/features/member-owned-skills/tasks.md`.

## T1 — The member skill store

**Where** `internal/docker/memberskills.go` (new), `internal/docker/memberskills_test.go`.

**What** `ListMemberSkills(key, harness) ([]MemberSkill, error)`,
`ReadMemberSkill(key, harness, name) (string, SkillMeta, error)`,
`WriteMemberSkill(key, harness, name, content, ifModifiedAt string) (SkillMeta, error)`,
`DeleteMemberSkill(key, harness, name) error`.

**Reuses** `memory.go`'s `workspaceDir` / `openTreeIfExists` / `escaped` (DEC-4),
`skills.go`'s `SkillMeta`, `skillNameRe`, `parseSkillFrontmatter`,
`config.ManagedSkillsDir`, `config.EffectiveSkillsDir`.

**Done when**
- the three layers are listed with `Origin` set, member's first (FR-2);
- managed entries are read from `ManagedSkillsDir` and the managed-name set is derived
  from the same `rels` `managedContentBinds` uses (DEC-2);
- shared entries come from `EffectiveSkillsDir` built on `ContainerDataRoot` (DEC-3,
  DEC-10);
- `Shadowed` is computed by frontmatter name, ganglion only (FR-3, DEC-9);
- writes reject: bad name, any managed name plus `shared-content`, frontmatter with a
  third key or a `name` that disagrees with the directory, oversized bodies (FR-4,
  DEC-6);
- `ifModifiedAt` mismatch returns a conflict error (DEC-7);
- every path goes through the `os.Root` tree anchored at the segment root (DEC-4).

**Tests** a symlink in `workspace/skills` pointing outside is refused; a managed
mountpoint stub is not reported as a member skill; a write to a managed name is refused
with the flag both on and off; frontmatter with an extra key is refused; a
name/directory disagreement is refused; a stale `modifiedAt` conflicts; shadowing is
detected by declared name, not directory.

**Gate** `gofmt -l .` empty, `go build ./...`, `go test ./internal/docker/ -count=1`.

## T2 — The four routes

**Where** `internal/httpapi/memberskills.go` (new), registration in `handlers.go`'s
member block beside `/v1/memory`, the `Mgr` interface at `handlers.go:238-248`, and the
test fakes that implement it.

**Depends on** T1.

**Done when** the four routes of FR-1 exist; reads use `authorizeRestartRead` and writes
`authorizeSecret`; no route accepts `project` (DEC-5); errors map to 400 / 403 / 404 /
409 / 413 / 502 with the `errBody` envelope; the 403 for a non-member origin reads like
`ErrMediaReserved`.

**Gate** `go build ./...`, `go test ./internal/httpapi/ -count=1`.

## T3 — Gateway routes

**Where** `deploy/standalone/config.standalone.toml` (roles `alpha`, `beta`, `gamma`),
`deploy/prod/config.base.toml` (roles `alpha`, `beta`).

**Done when** each role has a `[[<role>.path]]` block for `/v1/skills`
(`methods = ["GET","PUT","DELETE"]`, `permission = "write"` group) and one for
`/v1/skills/doc` (`["GET"]`), placed beside the existing `/v1/memory` block; both files
still parse as TOML and the per-role block count rises from 33 to 35.

**Not here** `mycelium-monorepo/dokploy/mycelium-api-gateway/config.toml` — a separate
repo with its own PR, and the gateway reads it at boot so it needs its own deploy.
