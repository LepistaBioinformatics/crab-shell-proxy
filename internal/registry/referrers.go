package registry

import (
	"sort"

	bolt "go.etcd.io/bbolt"
)

// referrersTx collects everything keeping name alive. It runs inside the
// caller's transaction so the check cannot be split from the write it guards.
//
// A full scan of three buckets beats maintaining reverse indexes here: the
// inventory holds tens of models and hundreds of workspaces, and a scan cannot
// drift out of sync with the data it derives from.
func referrersTx(tx *bolt.Tx, name string) ([]Referrer, error) {
	var out []Referrer

	err := tx.Bucket(bAssignments).ForEach(func(k, raw []byte) error {
		var a Assignment
		if err := jsonUnmarshal(raw, &a); err != nil {
			return err
		}
		// Chain membership counts: that workspace has this model's key on disk
		// and names it in agents.defaults.model_fallbacks.
		if a.ModelName == name || contains(a.Chain, name) {
			out = append(out, Referrer{Kind: "workspace", ID: string(k)})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	err = tx.Bucket(bScopeDefaults).ForEach(func(k, raw []byte) error {
		var d ScopeDefault
		if err := jsonUnmarshal(raw, &d); err != nil {
			return err
		}
		if d.ModelName == name {
			out = append(out, Referrer{Kind: "scope_default", ID: string(k)})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	err = tx.Bucket(bModels).ForEach(func(k, raw []byte) error {
		if string(k) == name {
			return nil
		}
		var m Model
		if err := jsonUnmarshal(raw, &m); err != nil {
			return err
		}
		if m.ReplacedBy == name {
			out = append(out, Referrer{Kind: "replaced_by", ID: m.ModelName})
		}
		if contains(m.Fallbacks, name) {
			out = append(out, Referrer{Kind: "fallback", ID: m.ModelName})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// Referrers reports everything that keeps a model alive, for the admin UI's
// usage count and for the detail an in-use rejection carries.
func (r *Registry) Referrers(name string) ([]Referrer, error) {
	var out []Referrer
	err := r.db.View(func(tx *bolt.Tx) error {
		var err error
		out, err = referrersTx(tx, name)
		return err
	})
	return out, err
}

// PutAssignment records what a workspace has materialized.
func (r *Registry) PutAssignment(ref WorkspaceRef, a Assignment) error {
	if a.MaterializedAt.IsZero() {
		a.MaterializedAt = r.now()
	}
	return r.db.Update(func(tx *bolt.Tx) error {
		return putJSON(tx.Bucket(bAssignments), ref.Key(), a)
	})
}

// GetAssignment returns ErrNotFound when the workspace has never been
// materialized — which is exactly the condition the deprecation hop keys on.
func (r *Registry) GetAssignment(ref WorkspaceRef) (Assignment, error) {
	var a Assignment
	err := r.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bAssignments), ref.Key(), &a)
	})
	if err != nil {
		return Assignment{}, err
	}
	return a, nil
}

// DeleteAssignment removes a workspace's record (idempotent), for a workspace
// that has been torn down.
func (r *Registry) DeleteAssignment(ref WorkspaceRef) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bAssignments).Delete([]byte(ref.Key()))
	})
}

// PutScopeDefault sets the model chosen at one cascade level. The caller
// validates the model exists and is active.
func (r *Registry) PutScopeDefault(scopeKey string, d ScopeDefault) error {
	if d.UpdatedAt.IsZero() {
		d.UpdatedAt = r.now()
	}
	return r.db.Update(func(tx *bolt.Tx) error {
		return putJSON(tx.Bucket(bScopeDefaults), scopeKey, d)
	})
}
