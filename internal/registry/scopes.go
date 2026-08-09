package registry

import (
	"fmt"

	bolt "go.etcd.io/bbolt"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
)

// ScopeLevel names one rung of the resolution cascade.
type ScopeLevel string

const (
	LevelGlobal       ScopeLevel = "global"
	LevelAgent        ScopeLevel = "agent"
	LevelTenant       ScopeLevel = "tenant"
	LevelSubscription ScopeLevel = "subscription"
	// LevelUser is reported by Resolve as the winning level. It is never a
	// scope-defaults key: a per-user choice is an assignment, not a default.
	LevelUser ScopeLevel = "user"
	// LevelUserModel is reported when the member's OWN registered model won.
	// Distinct from LevelUser, which means an administrator pinned them: an admin
	// screen that showed a personal model as a pin would offer to unpin something
	// no administrator set.
	LevelUserModel ScopeLevel = "user_model"
)

// ScopeSel identifies one scope-defaults entry.
type ScopeSel struct {
	Level     ScopeLevel
	Agent     string
	TenantID  string
	SubsAccID string
}

// Key is the scope_defaults bucket key. Ids are sanitized so they cannot contain
// the separator and forge another scope's key.
func (s ScopeSel) Key() (string, error) {
	switch s.Level {
	case LevelGlobal:
		return "global", nil
	case LevelAgent:
		if s.Agent == "" {
			return "", fmt.Errorf("%w: an agent scope needs an agent", ErrInvalid)
		}
		return "agent/" + identity.SanitizeID(s.Agent), nil
	case LevelTenant:
		if s.TenantID == "" {
			return "", fmt.Errorf("%w: a tenant scope needs a tenant id", ErrInvalid)
		}
		return "tenant/" + identity.SanitizeID(s.TenantID), nil
	case LevelSubscription:
		if s.TenantID == "" || s.SubsAccID == "" {
			return "", fmt.Errorf("%w: a subscription scope needs a tenant and a subscription id", ErrInvalid)
		}
		return "subs/" + identity.SanitizeID(s.TenantID) + "/" + identity.SanitizeID(s.SubsAccID), nil
	}
	return "", fmt.Errorf("%w: unknown scope level %q", ErrInvalid, s.Level)
}

// SetScopeDefault points one cascade level at a model.
//
// The model must be ACTIVE: a scope default is what new workspaces land on, and
// pointing it at a disabled model would produce workspaces picoclaw refuses to
// boot. A model that is already a scope default may still be DEPRECATED later —
// that is the normal retirement path, and Resolve hops to the replacement for
// new users without the admin re-pointing every scope first.
func (r *Registry) SetScopeDefault(sel ScopeSel, modelName string) error {
	key, err := sel.Key()
	if err != nil {
		return err
	}
	return r.db.Update(func(tx *bolt.Tx) error {
		var m Model
		if err := getJSON(tx.Bucket(bModels), modelName, &m); err != nil {
			if err == ErrNotFound {
				return fmt.Errorf("%w: model %q does not exist", ErrInvalid, modelName)
			}
			return err
		}
		if m.Status != StatusActive {
			return fmt.Errorf("%w: model %q is %s and cannot be a scope default", ErrInvalid, modelName, m.Status)
		}
		return putJSON(tx.Bucket(bScopeDefaults), key, ScopeDefault{
			ModelName: modelName, UpdatedAt: r.now(),
		})
	})
}

func (r *Registry) GetScopeDefault(sel ScopeSel) (ScopeDefault, error) {
	key, err := sel.Key()
	if err != nil {
		return ScopeDefault{}, err
	}
	var d ScopeDefault
	err = r.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bScopeDefaults), key, &d)
	})
	if err != nil {
		return ScopeDefault{}, err
	}
	return d, nil
}

// ClearScopeDefault removes a level's default. Missing is a success: the admin's
// intent is already true.
func (r *Registry) ClearScopeDefault(sel ScopeSel) error {
	key, err := sel.Key()
	if err != nil {
		return err
	}
	return r.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bScopeDefaults).Delete([]byte(key))
	})
}

// ListScopeDefaults returns every level's default keyed by scope key, for the
// admin UI and for the migration's drift reporting.
func (r *Registry) ListScopeDefaults() (map[string]ScopeDefault, error) {
	out := map[string]ScopeDefault{}
	err := r.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bScopeDefaults).ForEach(func(k, raw []byte) error {
			var d ScopeDefault
			if err := jsonUnmarshal(raw, &d); err != nil {
				return err
			}
			out[string(k)] = d
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetScopeDefaultRaw writes a scope default WITHOUT the active-model check. It
// exists for the boot migration, which imports pre-existing overrides whose model
// may already be retired. Never call it from an HTTP handler.
func (r *Registry) SetScopeDefaultRaw(key, modelName string) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		return putJSON(tx.Bucket(bScopeDefaults), key, ScopeDefault{
			ModelName: modelName, UpdatedAt: r.now(),
		})
	})
}
