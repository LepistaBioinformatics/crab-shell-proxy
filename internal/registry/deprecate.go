package registry

import (
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// maxDeprecationHops bounds a replacement walk. A chain longer than this is a
// configuration mistake, and an unbounded walk over corrupt data would hang the
// resolver on every provision.
const maxDeprecationHops = 8

// Deprecate retires a model that may still be in use: existing workspaces keep
// it, while new ones get replacedBy. That is the only way to retire something in
// use — disable requires zero usage (I3).
//
// Enforces I4 (the replacement must exist and be active) and I5 (no cycles).
func (r *Registry) Deprecate(name string, version uint64, replacedBy string) (Model, error) {
	if replacedBy == "" {
		return Model{}, fmt.Errorf("%w: deprecating %q requires a replacement so new users have somewhere to go", ErrInvalid, name)
	}
	if replacedBy == name {
		return Model{}, fmt.Errorf("%w: %q cannot replace itself", ErrInvalid, name)
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
		var repl Model
		if err := getJSON(b, replacedBy, &repl); err != nil {
			if err == ErrNotFound {
				return fmt.Errorf("%w: replacement %q does not exist", ErrInvalid, replacedBy)
			}
			return err
		}
		if repl.Status == StatusDisabled {
			return fmt.Errorf("%w: replacement %q is disabled, so it could not serve new users", ErrInvalid, replacedBy)
		}
		// Walk forward from the replacement: if it leads back to name, this write
		// would close a loop and strand every workspace that resolves through it.
		if err := assertNoCycleTx(tx, replacedBy, name); err != nil {
			return err
		}
		cur.Status = StatusDeprecated
		cur.ReplacedBy = replacedBy
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

// assertNoCycleTx walks replaced_by from start and fails if it reaches forbidden
// or revisits a node.
func assertNoCycleTx(tx *bolt.Tx, start, forbidden string) error {
	b := tx.Bucket(bModels)
	seen := map[string]bool{}
	cursor := start
	for hop := 0; hop < maxDeprecationHops; hop++ {
		if cursor == forbidden {
			return fmt.Errorf("%w: that would create a deprecation cycle through %q", ErrInvalid, forbidden)
		}
		if seen[cursor] {
			return fmt.Errorf("%w: deprecation chain already loops at %q", ErrInvalid, cursor)
		}
		seen[cursor] = true
		var m Model
		if err := getJSON(b, cursor, &m); err != nil {
			return nil // a dangling tail cannot form a cycle
		}
		if m.Status != StatusDeprecated || m.ReplacedBy == "" {
			return nil
		}
		cursor = m.ReplacedBy
	}
	return fmt.Errorf("%w: deprecation chain from %q exceeds %d hops", ErrInvalid, start, maxDeprecationHops)
}

// ResolveReplacement follows replaced_by until it reaches a model that is not
// deprecated, and returns that. A model that is not deprecated resolves to
// itself, so callers can call this unconditionally.
func (r *Registry) ResolveReplacement(name string) (Model, error) {
	var out Model
	err := r.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bModels)
		seen := map[string]bool{}
		cursor := name
		for hop := 0; hop <= maxDeprecationHops; hop++ {
			if seen[cursor] {
				return fmt.Errorf("%w: deprecation chain loops at %q", ErrInvalid, cursor)
			}
			seen[cursor] = true
			var m Model
			if err := getJSON(b, cursor, &m); err != nil {
				return fmt.Errorf("deprecation chain from %q: %q: %w", name, cursor, err)
			}
			if m.Status != StatusDeprecated || m.ReplacedBy == "" {
				out = m
				return nil
			}
			cursor = m.ReplacedBy
		}
		return fmt.Errorf("%w: deprecation chain from %q exceeds %d hops", ErrInvalid, name, maxDeprecationHops)
	})
	if err != nil {
		return Model{}, err
	}
	return out, nil
}
