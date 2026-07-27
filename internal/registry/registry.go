// Package registry owns the proxy-level model inventory: the single source of
// truth for which model a workspace uses. It replaces the config.yaml-backed
// override cascade and the on-disk registered-models store, which wrote the same
// workspace fields without knowing about each other.
package registry

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
)

// Status is a model's catalog lifecycle state. The two inactive states have
// opposite preconditions, which is why this is an enum and not a bool: disabled
// demands zero usage, deprecated exists precisely because usage persists.
type Status string

const (
	// StatusActive: offered; may be a scope default, an assignment or a fallback.
	StatusActive Status = "active"
	// StatusDisabled: not offered, and nothing may reference it. Reversible.
	StatusDisabled Status = "disabled"
	// StatusDeprecated: not offered to new users, but existing users keep it.
	// Requires ReplacedBy.
	StatusDeprecated Status = "deprecated"
)

// Source records why a workspace has the model it has.
type Source string

const (
	SourceExplicit  Source = "explicit"
	SourceInherited Source = "inherited"
)

// Model is one inventory record. APIKey is persisted here and never marshalled
// to a client: handlers convert to PublicModel (see public.go), so leaking a key
// requires adding a field rather than forgetting one.
type Model struct {
	ModelName  string          `json:"model_name"`
	Provider   string          `json:"provider"`
	Model      string          `json:"model"`
	APIBase    string          `json:"api_base"`
	APIKey     string          `json:"api_key,omitempty"`
	AuthMethod string          `json:"auth_method,omitempty"`
	ExtraBody  json.RawMessage `json:"extra_body,omitempty"`

	Status     Status   `json:"status"`
	ReplacedBy string   `json:"replaced_by,omitempty"`
	// Fallbacks is this model's own ordered fallback chain, by model_name. It is
	// expanded one level only when materialized, matching picoclaw's flat
	// agents.defaults.model_fallbacks.
	Fallbacks []string `json:"fallbacks,omitempty"`
	// Position orders the active list in the admin UI. It has NO functional
	// effect: reordering never re-materializes and never restarts a workspace.
	Position int `json:"position"`

	Version   uint64    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// ImportedOrphan marks a record the boot migration recovered from a live
	// workspace because no other source declared it, for admin review.
	ImportedOrphan bool `json:"imported_orphan,omitempty"`
}

// Assignment is what a workspace actually has materialized. Chain is recorded
// alongside the primary so a key edit reaches every workspace holding the model
// as a fallback, not only those where it is primary.
type Assignment struct {
	ModelName      string    `json:"model_name"`
	Chain          []string  `json:"chain,omitempty"`
	Source         Source    `json:"source"`
	MaterializedAt time.Time `json:"materialized_at"`
}

// ScopeDefault is the model chosen at one cascade level.
type ScopeDefault struct {
	ModelName string    `json:"model_name"`
	UpdatedAt time.Time `json:"updated_at"`
}

// WorkspaceRef identifies one per-user agent workspace. The registry defines its
// own ref rather than importing docker.WorkspaceKey, which would be an import
// cycle (docker depends on registry).
type WorkspaceRef struct {
	TenantID  string
	SubsAccID string
	Agent     string
	UserAccID string
}

// Key is the assignments bucket key. Each segment is sanitized before joining so
// a separator inside an id cannot forge another workspace's key.
func (w WorkspaceRef) Key() string {
	return strings.Join([]string{
		identity.SanitizeID(w.TenantID),
		identity.SanitizeID(w.SubsAccID),
		identity.SanitizeID(w.Agent),
		identity.SanitizeID(w.UserAccID),
	}, "/")
}

var (
	// ErrNotFound: no such model / assignment / scope default.
	ErrNotFound = errors.New("registry: not found")
	// ErrDuplicate: a model with that model_name already exists.
	ErrDuplicate = errors.New("registry: model_name already exists")
	// ErrVersionConflict: the write carried a stale version.
	ErrVersionConflict = errors.New("registry: version conflict")
	// ErrInvalid: malformed input (bad status, self-referential fallback, a
	// deprecation without a valid replacement, a cycle).
	ErrInvalid = errors.New("registry: invalid")
	// ErrNoModelResolvable: no cascade level yields a model. Provisioning must
	// refuse rather than write a workspace picoclaw cannot boot.
	ErrNoModelResolvable = errors.New("registry: no model resolvable for this workspace")
)

// Referrer names one thing that keeps a model alive.
type Referrer struct {
	// Kind is "workspace" | "scope_default" | "replaced_by" | "fallback".
	Kind string `json:"kind"`
	// ID is the workspace key, scope key, or referring model_name.
	ID string `json:"id"`
}

// InUseError rejects a delete or disable and names what to detach first, so the
// admin has a concrete next action rather than a bare conflict.
type InUseError struct {
	ModelName string
	Referrers []Referrer
}

func (e *InUseError) Error() string {
	parts := make([]string, 0, len(e.Referrers))
	for _, r := range e.Referrers {
		parts = append(parts, r.Kind+":"+r.ID)
	}
	return fmt.Sprintf("registry: model %q is in use by %s", e.ModelName, strings.Join(parts, ", "))
}

var (
	bModels        = []byte("models")
	bAssignments   = []byte("assignments")
	bScopeDefaults = []byte("scope_defaults")
	bMeta          = []byte("meta")

	kSchemaVersion = []byte("schema_version")
)

// Registry is the inventory store. Every mutation runs in one bolt write
// transaction, so a check-then-write (e.g. "nobody uses this" then delete)
// cannot interleave with another admin's write.
type Registry struct {
	db  *bolt.DB
	now func() time.Time
}

// Open opens (creating if absent) the inventory database and ensures every
// bucket exists. now is injectable so tests get deterministic timestamps.
func Open(path string, now func() time.Time) (*Registry, error) {
	if now == nil {
		now = time.Now
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open model registry %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bModels, bAssignments, bScopeDefaults, bMeta} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init model registry buckets: %w", err)
	}
	return &Registry{db: db, now: now}, nil
}

func (r *Registry) Close() error { return r.db.Close() }

// SchemaVersion reports the migration marker; 0 means the boot migration has not
// run yet.
func (r *Registry) SchemaVersion() (int, error) {
	var v int
	err := r.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bMeta).Get(kSchemaVersion)
		if raw == nil {
			return nil
		}
		if len(raw) != 8 {
			return fmt.Errorf("corrupt schema_version (%d bytes)", len(raw))
		}
		v = int(binary.BigEndian.Uint64(raw))
		return nil
	})
	return v, err
}

func (r *Registry) SetSchemaVersion(v int) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		buf := make([]byte, 8)
		binary.BigEndian.PutUint64(buf, uint64(v))
		return tx.Bucket(bMeta).Put(kSchemaVersion, buf)
	})
}

// --- internal codecs, shared by every operation below ---

func putJSON(b *bolt.Bucket, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return b.Put([]byte(key), raw)
}

// getJSON returns ErrNotFound when the key is absent, so callers can branch on
// it instead of on a nil check they might forget.
func getJSON(b *bolt.Bucket, key string, v any) error {
	raw := b.Get([]byte(key))
	if raw == nil {
		return ErrNotFound
	}
	return json.Unmarshal(raw, v)
}

// jsonUnmarshal exists so bucket scans (which hold raw bytes, not a key) share
// one decode path with getJSON.
func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }
