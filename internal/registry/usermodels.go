package registry

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
)

// A personal model lives in its OWN buckets, deliberately not in `models`.
//
// Every invariant the inventory enforces is a cross-user one: model_name unique
// instance-wide, fallbacks and replaced_by resolving inside it, the referrer
// guard on delete, position ordering. A personal model participates in none of
// them. Teaching each of those to skip owned rows would be five chances to
// forget one, and the thing forgotten would be one member's API key materialized
// into another member's workspace. Here, nothing outside the owner can even name
// a personal model: no other bucket's names resolve in this one.
var (
	bUserModels    = []byte("user_models")
	bUserSelection = []byte("user_selection")
	bScopePolicy   = []byte("scope_policy")
)

// OwnPrefix is the reserved model_name prefix a personal model is materialized
// under. The inventory refuses to create a model that starts with it (see
// requiredFields), which is what keeps the synthesized name from colliding with
// a real one inside a workspace's model_list.
const OwnPrefix = "own-"

// UserModel is one member's own model definition. APIKey is persisted here and
// never marshalled to a client: handlers convert to PublicUserModel, so leaking
// a key requires adding a field rather than forgetting one.
type UserModel struct {
	OwnerAccID string          `json:"owner_acc_id"`
	Slug       string          `json:"slug"`
	Label      string          `json:"label"`
	Provider   string          `json:"provider"`
	Model      string          `json:"model"`
	APIBase    string          `json:"api_base"`
	APIKey     string          `json:"api_key,omitempty"`
	ExtraBody  json.RawMessage `json:"extra_body,omitempty"`

	// Enabled is the administrator's switch (parent R6). A member has no control
	// over it: their own "off" is to stop selecting the model.
	Enabled bool `json:"enabled"`

	// LastTest records what the connectivity probe last said about THIS
	// definition. It is cleared on every edit that changes what would be sent,
	// because a result that survived an api_base change would assert something
	// nobody ever verified.
	LastTest *TestResult `json:"last_test,omitempty"`

	Version   uint64    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TestResult is one probe outcome. Detail is an error CLASS, never a provider
// response body — see model_probe.go.
type TestResult struct {
	OK         bool      `json:"ok"`
	StatusCode int       `json:"status_code,omitempty"`
	LatencyMS  int64     `json:"latency_ms"`
	Detail     string    `json:"detail,omitempty"`
	At         time.Time `json:"at"`
}

// PublicUserModel is the client-facing shape. No key field, by construction.
type PublicUserModel struct {
	OwnerAccID string          `json:"owner_acc_id"`
	Slug       string          `json:"slug"`
	Label      string          `json:"label"`
	Provider   string          `json:"provider"`
	Model      string          `json:"model"`
	APIBase    string          `json:"api_base"`
	ExtraBody  json.RawMessage `json:"extra_body,omitempty"`
	Enabled    bool            `json:"enabled"`
	HasKey     bool            `json:"has_key"`
	LastTest   *TestResult     `json:"last_test,omitempty"`
	Version    uint64          `json:"version"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

func PublicUser(m UserModel) PublicUserModel {
	return PublicUserModel{
		OwnerAccID: m.OwnerAccID, Slug: m.Slug, Label: m.Label,
		Provider: m.Provider, Model: m.Model, APIBase: m.APIBase,
		ExtraBody: m.ExtraBody, Enabled: m.Enabled, HasKey: m.APIKey != "",
		LastTest: m.LastTest, Version: m.Version,
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}

// UserSelection is which personal model a workspace runs. An absent record means
// the member is on whatever their administrator provides — that is the default
// and the state a "use the organisation's model" click returns to.
type UserSelection struct {
	Slug       string    `json:"slug"`
	SelectedAt time.Time `json:"selected_at"`
}

// ScopePolicy is the administrator's lock (parent R7). AllowUserModels is a
// POINTER: "not set at this level" and "explicitly allowed here" are different
// answers, and only the pointer can tell an inherited allow from a deliberate one
// when a wider level says deny.
type ScopePolicy struct {
	AllowUserModels *bool     `json:"allow_user_models,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// MaxUserModelsPerAccount bounds a member's personal list. Not a licensing knob
// — a bound so one account cannot grow the store without limit.
const MaxUserModelsPerAccount = 10

// A slug is the member's own name for the model AND half its store key, so the
// charset is the safe one and a dash may not start or end it — the key is joined
// with "/" and the materialized model_name prefixed with "own-", and neither
// should ever produce a doubled or trailing separator.
var userSlugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// userModelKey joins owner and slug. The owner id is sanitized so a separator
// inside it cannot forge another member's key; the slug charset already excludes
// the separator.
func userModelKey(ownerAccID, slug string) string {
	return identity.SanitizeID(ownerAccID) + "/" + slug
}

// MaterializedName is the model_name a personal model is written into a
// workspace's model_list under.
func (m UserModel) MaterializedName() string { return OwnPrefix + m.Slug }

// asModel renders the personal record as the inventory shape materialization
// consumes, so materializeModels needs no knowledge of personal models at all.
// Status is active because a disabled one never reaches this function.
func (m UserModel) asModel() Model {
	return Model{
		ModelName: m.MaterializedName(),
		Provider:  m.Provider,
		Model:     m.Model,
		APIBase:   m.APIBase,
		APIKey:    m.APIKey,
		ExtraBody: m.ExtraBody,
		Status:    StatusActive,
		Version:   m.Version,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}
}

func validateUserModel(m UserModel) error {
	switch {
	case m.OwnerAccID == "":
		return fmt.Errorf("%w: owner is required", ErrInvalid)
	case !userSlugRe.MatchString(m.Slug):
		return fmt.Errorf("%w: slug must be 1-40 lowercase letters, digits or dashes, not starting or ending with a dash", ErrInvalid)
	case strings.TrimSpace(m.Provider) == "":
		return fmt.Errorf("%w: provider is required", ErrInvalid)
	case strings.TrimSpace(m.Model) == "":
		return fmt.Errorf("%w: model is required", ErrInvalid)
	case strings.TrimSpace(m.APIBase) == "":
		// Unlike the inventory, api_base is ALWAYS required here: the auth_method
		// branch that makes it optional there is oauth, which a member cannot
		// complete from a drawer and whose request the probe does not represent.
		return fmt.Errorf("%w: api_base is required", ErrInvalid)
	case m.APIKey == "":
		return fmt.Errorf("%w: api_key is required", ErrInvalid)
	}
	if len(m.ExtraBody) > 0 && !json.Valid(m.ExtraBody) {
		return fmt.Errorf("%w: extra_body is not valid JSON", ErrInvalid)
	}
	return nil
}

// CreateUserModel registers one personal model. Enabled starts true: the
// administrator's switch is an intervention, not a gate to pass.
func (r *Registry) CreateUserModel(m UserModel) (UserModel, error) {
	if err := validateUserModel(m); err != nil {
		return UserModel{}, err
	}
	m.Enabled = true
	var out UserModel
	err := r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bUserModels)
		key := userModelKey(m.OwnerAccID, m.Slug)
		if b.Get([]byte(key)) != nil {
			return fmt.Errorf("%w: %q", ErrDuplicate, m.Slug)
		}
		owned, err := countUserModelsTx(b, m.OwnerAccID)
		if err != nil {
			return err
		}
		if owned >= MaxUserModelsPerAccount {
			return fmt.Errorf("%w: at most %d per account", ErrUserModelLimit, MaxUserModelsPerAccount)
		}
		at := r.now()
		m.Version = 1
		m.CreatedAt = at
		m.UpdatedAt = at
		out = m
		return putJSON(b, key, m)
	})
	if err != nil {
		return UserModel{}, err
	}
	return out, nil
}

func countUserModelsTx(b *bolt.Bucket, ownerAccID string) (int, error) {
	prefix := []byte(identity.SanitizeID(ownerAccID) + "/")
	n := 0
	c := b.Cursor()
	for k, _ := c.Seek(prefix); k != nil && strings.HasPrefix(string(k), string(prefix)); k, _ = c.Next() {
		n++
	}
	return n, nil
}

func (r *Registry) GetUserModel(ownerAccID, slug string) (UserModel, error) {
	var m UserModel
	err := r.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bUserModels), userModelKey(ownerAccID, slug), &m)
	})
	if err != nil {
		return UserModel{}, err
	}
	return m, nil
}

// ListUserModels returns one member's own models, oldest first so the list does
// not reshuffle under them as they edit.
func (r *Registry) ListUserModels(ownerAccID string) ([]UserModel, error) {
	var out []UserModel
	prefix := identity.SanitizeID(ownerAccID) + "/"
	err := r.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bUserModels).Cursor()
		for k, raw := c.Seek([]byte(prefix)); k != nil && strings.HasPrefix(string(k), prefix); k, raw = c.Next() {
			var m UserModel
			if err := jsonUnmarshal(raw, &m); err != nil {
				return err
			}
			out = append(out, m)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// ListAllUserModels returns every personal model in the store, for the admin
// listing. Filtering to an administrator's authority is the handler's job — it is
// the only layer that holds the caller's profile.
func (r *Registry) ListAllUserModels() ([]UserModel, error) {
	var out []UserModel
	err := r.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bUserModels).ForEach(func(_, raw []byte) error {
			var m UserModel
			if err := jsonUnmarshal(raw, &m); err != nil {
				return err
			}
			out = append(out, m)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OwnerAccID != out[j].OwnerAccID {
			return out[i].OwnerAccID < out[j].OwnerAccID
		}
		return out[i].Slug < out[j].Slug
	})
	return out, nil
}

// UpdateUserModel read-modify-writes one record under an optimistic version
// check. The mutator receives the stored record, key included, so an edit that
// does not mention the key keeps it.
func (r *Registry) UpdateUserModel(
	ownerAccID, slug string, version uint64, mutate func(*UserModel) error,
) (UserModel, error) {
	var out UserModel
	err := r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bUserModels)
		key := userModelKey(ownerAccID, slug)
		var m UserModel
		if err := getJSON(b, key, &m); err != nil {
			return err
		}
		if version != 0 && m.Version != version {
			return fmt.Errorf("%w: %q is at version %d", ErrVersionConflict, slug, m.Version)
		}
		before := m
		if err := mutate(&m); err != nil {
			return err
		}
		// Identity is not editable: the key is derived from it, so a changed
		// owner or slug would write a second record instead of updating this one.
		m.OwnerAccID, m.Slug = before.OwnerAccID, before.Slug
		if err := validateUserModel(m); err != nil {
			return err
		}
		// A test result asserts something about a specific request. Once any part
		// of that request changes, it no longer describes anything that was
		// verified — so it is dropped rather than left to make a stale promise.
		if probeIdentityChanged(before, m) {
			m.LastTest = nil
		}
		m.Version = before.Version + 1
		m.UpdatedAt = r.now()
		out = m
		return putJSON(b, key, m)
	})
	if err != nil {
		return UserModel{}, err
	}
	return out, nil
}

// probeIdentityChanged reports whether the edit changed anything the probe
// actually sends. Label and enabled are not part of it.
func probeIdentityChanged(a, b UserModel) bool {
	return a.Provider != b.Provider ||
		a.Model != b.Model ||
		a.APIBase != b.APIBase ||
		a.APIKey != b.APIKey ||
		string(a.ExtraBody) != string(b.ExtraBody)
}

// RecordUserModelTest stores a probe outcome against the saved record. It does
// not bump Version: a test is an observation about the record, not an edit of it,
// and bumping would make every concurrent editor's version stale for no reason.
func (r *Registry) RecordUserModelTest(ownerAccID, slug string, res TestResult) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bUserModels)
		key := userModelKey(ownerAccID, slug)
		var m UserModel
		if err := getJSON(b, key, &m); err != nil {
			return err
		}
		m.LastTest = &res
		return putJSON(b, key, m)
	})
}

// SetUserModelEnabled is the administrator's switch. Returns the updated record
// so a caller can report what it changed.
func (r *Registry) SetUserModelEnabled(ownerAccID, slug string, enabled bool) (UserModel, error) {
	return r.UpdateUserModel(ownerAccID, slug, 0, func(m *UserModel) error {
		m.Enabled = enabled
		return nil
	})
}

// DeleteUserModel removes a personal model. Selections naming it are dropped in
// the SAME transaction: leaving one behind would make every one of that member's
// workspaces resolve a model that no longer exists.
func (r *Registry) DeleteUserModel(ownerAccID, slug string) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bUserModels)
		key := userModelKey(ownerAccID, slug)
		if b.Get([]byte(key)) == nil {
			return fmt.Errorf("%w: personal model %q", ErrNotFound, slug)
		}
		if err := b.Delete([]byte(key)); err != nil {
			return err
		}
		return clearSelectionsForTx(tx, ownerAccID, slug)
	})
}

// clearSelectionsForTx drops every selection of one member that names slug. The
// selection bucket is keyed by workspace, so a member with workspaces under two
// agents has two records pointing at the same personal model.
func clearSelectionsForTx(tx *bolt.Tx, ownerAccID, slug string) error {
	sel := tx.Bucket(bUserSelection)
	var doomed [][]byte
	err := sel.ForEach(func(k, raw []byte) error {
		var s UserSelection
		if err := jsonUnmarshal(raw, &s); err != nil {
			return err
		}
		// The workspace key ends in the sanitized account id (WorkspaceRef.Key),
		// which is what ties a selection record to its owner.
		if s.Slug == slug && strings.HasSuffix(string(k), "/"+identity.SanitizeID(ownerAccID)) {
			doomed = append(doomed, append([]byte(nil), k...))
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, k := range doomed {
		if err := sel.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// SetUserSelection points one workspace at one of its member's own models. The
// model must exist and be enabled — selecting a disabled one would look like it
// worked and change nothing.
func (r *Registry) SetUserSelection(ref WorkspaceRef, slug string) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		var m UserModel
		if err := getJSON(tx.Bucket(bUserModels), userModelKey(ref.UserAccID, slug), &m); err != nil {
			if err == ErrNotFound {
				return fmt.Errorf("%w: no personal model %q", ErrNotFound, slug)
			}
			return err
		}
		if !m.Enabled {
			return fmt.Errorf("%w: %q", ErrUserModelDisabled, slug)
		}
		return putJSON(tx.Bucket(bUserSelection), ref.Key(), UserSelection{
			Slug: slug, SelectedAt: r.now(),
		})
	})
}

// ClearUserSelection returns a workspace to the administrator's cascade. Missing
// is a success: the caller's intent is already true.
func (r *Registry) ClearUserSelection(ref WorkspaceRef) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bUserSelection).Delete([]byte(ref.Key()))
	})
}

func (r *Registry) GetUserSelection(ref WorkspaceRef) (UserSelection, error) {
	var s UserSelection
	err := r.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bUserSelection), ref.Key(), &s)
	})
	if err != nil {
		return UserSelection{}, err
	}
	return s, nil
}

// SelectionsOf returns every workspace of one member that runs a given personal
// model, so an edit to that model can re-materialize exactly those workspaces.
func (r *Registry) SelectionsOf(ownerAccID, slug string) ([]WorkspaceRef, error) {
	var out []WorkspaceRef
	suffix := "/" + identity.SanitizeID(ownerAccID)
	err := r.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bUserSelection).ForEach(func(k, raw []byte) error {
			var s UserSelection
			if err := jsonUnmarshal(raw, &s); err != nil {
				return err
			}
			if s.Slug != slug || !strings.HasSuffix(string(k), suffix) {
				return nil
			}
			ref, err := parseWorkspaceKey(string(k))
			if err != nil {
				return nil
			}
			out = append(out, ref)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetScopePolicy writes the administrator's lock at one level.
func (r *Registry) SetScopePolicy(sel ScopeSel, allow bool) error {
	key, err := sel.Key()
	if err != nil {
		return err
	}
	return r.db.Update(func(tx *bolt.Tx) error {
		return putJSON(tx.Bucket(bScopePolicy), key, ScopePolicy{
			AllowUserModels: &allow, UpdatedAt: r.now(),
		})
	})
}

// ClearScopePolicy removes a level's policy so it inherits again.
func (r *Registry) ClearScopePolicy(sel ScopeSel) error {
	key, err := sel.Key()
	if err != nil {
		return err
	}
	return r.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bScopePolicy).Delete([]byte(key))
	})
}

// GetScopePolicy reads ONE level, without inheritance. Returns ErrNotFound when
// that level is unset, which is what lets an admin screen tell "allowed here" from
// "inherits from above".
func (r *Registry) GetScopePolicy(sel ScopeSel) (ScopePolicy, error) {
	key, err := sel.Key()
	if err != nil {
		return ScopePolicy{}, err
	}
	var p ScopePolicy
	err = r.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bScopePolicy), key, &p)
	})
	if err != nil {
		return ScopePolicy{}, err
	}
	return p, nil
}

// UserModelsAllowed answers the question the resolver and the drawer both ask:
// may this workspace run a personal model at all? The most specific level that is
// SET decides; unset everywhere means allowed, so the feature works on an
// instance whose administrator never touched the policy.
//
// It also reports the level that decided, so a screen can say "blocked by your
// tenant" instead of a bare no.
func (r *Registry) UserModelsAllowed(ref WorkspaceRef) (bool, ScopeLevel, error) {
	allowed := true
	var by ScopeLevel
	err := r.db.View(func(tx *bolt.Tx) error {
		a, l := userModelsAllowedTx(tx, ref)
		allowed, by = a, l
		return nil
	})
	return allowed, by, err
}

func userModelsAllowedTx(tx *bolt.Tx, ref WorkspaceRef) (bool, ScopeLevel) {
	b := tx.Bucket(bScopePolicy)
	sels := []ScopeSel{
		{Level: LevelSubscription, TenantID: ref.TenantID, SubsAccID: ref.SubsAccID},
		{Level: LevelTenant, TenantID: ref.TenantID},
		{Level: LevelAgent, Agent: ref.Agent},
		{Level: LevelGlobal},
	}
	for _, sel := range sels {
		key, err := sel.Key()
		if err != nil {
			continue
		}
		var p ScopePolicy
		if err := getJSON(b, key, &p); err != nil {
			continue
		}
		if p.AllowUserModels == nil {
			continue
		}
		return *p.AllowUserModels, sel.Level
	}
	return true, ""
}
