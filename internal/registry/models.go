package registry

import (
	"fmt"
	"sort"

	bolt "go.etcd.io/bbolt"
)

// validateStatus rejects an unknown status before it reaches the store, where a
// typo would silently make a model neither offered nor retired.
func validateStatus(s Status) error {
	switch s {
	case StatusActive, StatusDisabled, StatusDeprecated:
		return nil
	}
	return fmt.Errorf("%w: unknown status %q", ErrInvalid, s)
}

// validateFallbacks enforces I8: every name exists and no model lists itself.
// Existence is checked inside the caller's transaction so a concurrent delete
// cannot slip a dangling name in.
func validateFallbacks(b *bolt.Bucket, self string, fallbacks []string) error {
	seen := map[string]bool{}
	for _, name := range fallbacks {
		if name == self {
			return fmt.Errorf("%w: model %q cannot fall back to itself", ErrInvalid, self)
		}
		if seen[name] {
			return fmt.Errorf("%w: duplicate fallback %q", ErrInvalid, name)
		}
		seen[name] = true
		if b.Get([]byte(name)) == nil {
			return fmt.Errorf("%w: fallback %q does not exist", ErrInvalid, name)
		}
	}
	return nil
}

// requiredFields rejects a record that could not produce a bootable workspace.
// api_base is optional only when auth_method is set (e.g. the antigravity oauth
// entry in the suggestion catalog ships without one).
func requiredFields(m Model) error {
	switch {
	case m.ModelName == "":
		return fmt.Errorf("%w: model_name is required", ErrInvalid)
	case m.Provider == "":
		return fmt.Errorf("%w: provider is required", ErrInvalid)
	case m.Model == "":
		return fmt.Errorf("%w: model is required", ErrInvalid)
	case m.APIBase == "" && m.AuthMethod == "":
		return fmt.Errorf("%w: api_base is required unless auth_method is set", ErrInvalid)
	}
	return nil
}

// CreateModel inserts a new model. Position defaults to the end of the list so a
// new entry never silently displaces an existing one.
func (r *Registry) CreateModel(m Model) (Model, error) {
	if err := requiredFields(m); err != nil {
		return Model{}, err
	}
	if m.Status == "" {
		m.Status = StatusActive
	}
	if err := validateStatus(m.Status); err != nil {
		return Model{}, err
	}
	if m.Status == StatusDeprecated {
		return Model{}, fmt.Errorf("%w: create a model active or disabled, then deprecate it", ErrInvalid)
	}
	var out Model
	err := r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bModels)
		if b.Get([]byte(m.ModelName)) != nil {
			return fmt.Errorf("%w: %q", ErrDuplicate, m.ModelName)
		}
		if err := validateFallbacks(b, m.ModelName, m.Fallbacks); err != nil {
			return err
		}
		if m.Position == 0 {
			m.Position = b.Stats().KeyN + 1
		}
		at := r.now()
		m.Version = 1
		m.CreatedAt = at
		m.UpdatedAt = at
		out = m
		return putJSON(b, m.ModelName, m)
	})
	if err != nil {
		return Model{}, err
	}
	return out, nil
}

// GetModel returns one record, key included. Callers that answer a client must
// convert to PublicModel first.
func (r *Registry) GetModel(name string) (Model, error) {
	var m Model
	err := r.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bModels), name, &m)
	})
	if err != nil {
		return Model{}, err
	}
	return m, nil
}

// ListModels returns every record in display order: Position, then model_name as
// a stable tiebreak so an unordered inventory still lists deterministically.
func (r *Registry) ListModels() ([]Model, error) {
	var out []Model
	err := r.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bModels).ForEach(func(_, raw []byte) error {
			var m Model
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
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		return out[i].ModelName < out[j].ModelName
	})
	return out, nil
}

// UpdateModel read-modify-writes one record under an optimistic version check.
// The mutator receives the stored record (key included) so an edit that does not
// mention the key keeps it.
func (r *Registry) UpdateModel(name string, version uint64, mutate func(*Model) error) (Model, error) {
	var out Model
	err := r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bModels)
		var cur Model
		if err := getJSON(b, name, &cur); err != nil {
			return err
		}
		if cur.Version != version {
			return fmt.Errorf("%w: stored version %d, write carried %d", ErrVersionConflict, cur.Version, version)
		}
		if err := mutate(&cur); err != nil {
			return err
		}
		if cur.ModelName != name {
			return fmt.Errorf("%w: model_name is the key and cannot be changed", ErrInvalid)
		}
		if err := requiredFields(cur); err != nil {
			return err
		}
		if err := validateStatus(cur.Status); err != nil {
			return err
		}
		if err := validateFallbacks(b, cur.ModelName, cur.Fallbacks); err != nil {
			return err
		}
		cur.Version++
		cur.UpdatedAt = r.now()
		out = cur
		return putJSON(b, name, cur)
	})
	if err != nil {
		return Model{}, err
	}
	return out, nil
}

// SetPositions rewrites display order. It deliberately does NOT bump Version:
// position is presentation only, and bumping it would make a harmless drag
// invalidate every open edit form with a spurious 409.
func (r *Registry) SetPositions(order []string) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bModels)
		for i, name := range order {
			var m Model
			if err := getJSON(b, name, &m); err != nil {
				return fmt.Errorf("reorder %q: %w", name, err)
			}
			m.Position = i + 1
			if err := putJSON(b, name, m); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteModel removes a model, rejecting the delete while anything references it
// (I2). The rejection names the referrers so the admin knows what to detach.
func (r *Registry) DeleteModel(name string) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bModels)
		if b.Get([]byte(name)) == nil {
			return ErrNotFound
		}
		if err := guardUnreferenced(tx, name); err != nil {
			return err
		}
		return b.Delete([]byte(name))
	})
}

// SetStatus transitions a model's lifecycle state under the same optimistic
// version check as an edit.
//
//   - -> disabled carries DeleteModel's precondition (I3): a model nothing uses
//     can be shelved; one in use must be deprecated instead.
//   - -> active clears ReplacedBy and preserves Position, so reactivating
//     restores a model's place rather than appending it.
//   - -> deprecated is rejected here; T04 adds it with its replacement rules.
func (r *Registry) SetStatus(name string, version uint64, status Status, replacedBy string) (Model, error) {
	if err := validateStatus(status); err != nil {
		return Model{}, err
	}
	var out Model
	err := r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bModels)
		var cur Model
		if err := getJSON(b, name, &cur); err != nil {
			return err
		}
		if cur.Version != version {
			return fmt.Errorf("%w: stored version %d, write carried %d", ErrVersionConflict, cur.Version, version)
		}
		switch status {
		case StatusDisabled:
			if err := guardUnreferenced(tx, name); err != nil {
				return err
			}
			cur.ReplacedBy = ""
		case StatusActive:
			cur.ReplacedBy = ""
		case StatusDeprecated:
			return fmt.Errorf("%w: use Deprecate to retire a model", ErrInvalid)
		}
		cur.Status = status
		cur.Version++
		cur.UpdatedAt = r.now()
		out = cur
		return putJSON(b, name, cur)
	})
	if err != nil {
		return Model{}, err
	}
	return out, nil
}

// UpdateModelRaw mutates a record without the version check. It exists for the
// boot migration and for tests seeding states the public API refuses to create
// directly. Never call it from an HTTP handler.
func (r *Registry) UpdateModelRaw(name string, mutate func(*Model) error) (Model, error) {
	var out Model
	err := r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bModels)
		var cur Model
		if err := getJSON(b, name, &cur); err != nil {
			return err
		}
		if err := mutate(&cur); err != nil {
			return err
		}
		cur.Version++
		cur.UpdatedAt = r.now()
		out = cur
		return putJSON(b, name, cur)
	})
	if err != nil {
		return Model{}, err
	}
	return out, nil
}
