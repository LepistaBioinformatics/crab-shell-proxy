# admin-managed-storage-limits — Design (proxy)

**Spec:** `.specs/features/admin-managed-storage-limits/spec.md`

---

## Where each piece goes

| Concern | File | Shape |
| --- | --- | --- |
| Policy fields | `internal/registry/usermodels.go` | two `*int64` on `ScopePolicy`, two `PolicyField` constants |
| Cascade | `internal/registry/usermodels.go` | `policyCascadeTx` generalized over the field type |
| Resolution entry point | `internal/registry/media_policy.go` (new) | `MediaLimits(ref WorkspaceRef) (Limits, error)` |
| Usage accounting | `internal/docker/media.go` | `Manager.UsageBytes(key WorkspaceKey) (int64, error)`, `Manager.ExistingSize(key, project, name)` |
| Enforcement | `internal/httpapi/handlers.go` | two checks inside `handleMediaPost` |
| Admin API | `internal/httpapi/admin_media_policy.go` (new) | three handlers, registered in `handlers.go` |
| Usage in the listing | `internal/httpapi/handlers.go` | `usage` object on the `GET /v1/media` response |

`usermodels.go` is already large and now carries something that is not about
models. The two `*int64` still belong on `ScopePolicy` (DEC-1); what moves is the
*media* logic — resolution lives in its own file, and the struct stays where the
bucket plumbing is.

## The cascade, generalized

`policyCascadeTx` walks subscription → tenant → agent → global and returns the
first level that sets the field. It is written against `*bool`:

```go
func policyCascadeTx(tx *bolt.Tx, ref WorkspaceRef, pick func(ScopePolicy) *bool) (bool, ScopeLevel, bool)
```

It becomes:

```go
func policyCascadeTx[T any](tx *bolt.Tx, ref WorkspaceRef, pick func(ScopePolicy) *T) (T, ScopeLevel, bool)
```

The two existing callers are unchanged at the call site (type inference), which
is the point: the walk order and the "first level that sets it wins" rule keep
exactly one implementation. Generalize it in its own commit, with the existing
policy tests green, before any media field exists — a behavioural change hidden
inside a type change is the one thing this must not be.

## The two enforcement points

Current order in `handleMediaPost`:

```
1. MaxBytesReader / ParseMultipartForm   ← bounded by Cfg.MediaMaxBytes
2. 413 if the form was too large
3. parse tenant_id, subs_acc_id          ← the scope becomes known only HERE
4. FormFile("file")
5. ext allowlist                          ← deleted by unrestricted-upload-types
6. authorizeSecret → key
7. checkProject
8. StoreMedia
```

The new checks go **between 7 and 8**, where the workspace key and the file
header are both in hand:

```
7.  checkProject
7a. limits := Reg.MediaLimits(ref)                 // cascade, one View tx
7b. if header.Size > min(limits.MaxUpload, Cfg.MediaMaxBytes) → 413 media_too_large
7c. used := Mgr.UsageBytes(key)                    // full walk
    prior := Mgr.ExistingSize(key, project, name)  // O_TRUNC subtraction
    if limits.Quota != nil && used-prior+header.Size > *limits.Quota → 413 media_quota_exceeded
8.  StoreMedia
```

`header.Size` is authoritative here: the multipart reader has already read the
part, and step 1 has already bounded it, so this is not a client-declared number
being trusted.

**Ordering inside the two checks matters.** The per-upload cap is checked first
because it is the cheaper answer and the more specific one: a member who dropped
a 2 GiB file should be told the file is too big, not that their workspace is
full.

## Accounting

`UsageBytes` walks the **user root** — the directory holding both the main
workspace's `public/` and every project segment — not `publicRoot`, which is one
project's directory (FR-3.1). It reuses `ListMedia`'s two guards (skip symlinks,
never blank the total on an unreadable subtree) and drops the one thing that
would break it: `maxListedMedia`.

`ExistingSize` stats one file inside the destination `publicRoot`, through the
same `Root`-scoped tree `StoreMedia` writes into, so a name that resolves onto a
symlink out of the tree is an error rather than a size.

**Cost, accepted (FR-3.4):** one walk per upload. At the scale this runs — a few
hundred files in a workspace — it is microseconds against a network upload, and
it is always correct. A cached counter is not: the agent writes into this tree
from inside its own container, without going through this process at all.

## Error shape

Both refusals answer 413 with:

```json
{"error": {"code": "media_too_large", "message": "...", "limit": 52428800, "size": 81000000}}
```

`message` stays for anyone reading the proxy directly (curl, logs). `code` is
what the webapp maps; `limit`/`size` are what its copy interpolates so the
member is told the number rather than the rule. This is additive — `errBody`
gains a variant that carries extra fields; every existing error body is
untouched.

## What the admin API mirrors

`GET/PUT/DELETE /v1/admin/media-policy` are written against
`admin_user_models.go`'s policy handlers as the template: same scope parsing,
same authorization call, same "patch leaves omitted fields alone" contract, same
`?field=` on the delete. Deviating from that shape would make two admin policies
with two idioms, and the screens that drive them would diverge too.

## Migration

None. `ScopePolicy` is JSON in bbolt; two absent fields unmarshal as nil
pointers, which is exactly "unset at this level". Existing records are read and
written unchanged.
