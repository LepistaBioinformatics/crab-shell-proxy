package registry

import (
	"errors"
	"sort"
	"strings"

	bolt "go.etcd.io/bbolt"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
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

// AssignmentEntry is one assignment plus the workspace it belongs to, for a
// listing that spans more than one workspace.
type AssignmentEntry struct {
	Agent     string
	UserAccID string
	Assignment
}

// AssignmentsUnder lists every recorded assignment under one (tenant,
// subscription), across agents, so an admin surface can show which users are
// pinned and to what. Ordered by key, which is the bucket's own order.
//
// It spans agents deliberately: a subscription's users may each have a workspace
// under a different agent, and a per-agent read would render exactly those users
// as unpinned. Authority over the pair is what the caller is checked for.
func (r *Registry) AssignmentsUnder(tenantID, subsAccID string) ([]AssignmentEntry, error) {
	prefix := identity.SanitizeID(tenantID) + "/" + identity.SanitizeID(subsAccID) + "/"
	var out []AssignmentEntry
	err := r.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bAssignments).Cursor()
		for k, raw := c.Seek([]byte(prefix)); k != nil && strings.HasPrefix(string(k), prefix); k, raw = c.Next() {
			var a Assignment
			if err := jsonUnmarshal(raw, &a); err != nil {
				return err
			}
			rest := strings.Split(strings.TrimPrefix(string(k), prefix), "/")
			if len(rest) != 2 {
				continue
			}
			out = append(out, AssignmentEntry{Agent: rest[0], UserAccID: rest[1], Assignment: a})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteAssignment removes a workspace's record (idempotent), for a workspace
// that has been torn down.
func (r *Registry) DeleteAssignment(ref WorkspaceRef) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bAssignments).Delete([]byte(ref.Key()))
	})
}

// RecordMaterialization writes what a workspace just materialized, preserving an
// existing EXPLICIT pin's source in the SAME transaction that reads it.
//
// Read-and-preserve cannot be split across two transactions: the source decides
// whether a later scope-default change may override this workspace, so a racing
// writer between the read and the write could silently demote a deliberate pin.
// A missing prior assignment is the normal first-materialization case; any other
// read error is returned rather than being treated as "no pin".
//
// It takes the whole Resolution rather than a name and a chain because with a
// member's own model primary the two halves diverge: ModelName/Chain keep
// describing the INVENTORY side — the cascade model, which is also the runtime
// fallback, so a key edit to it still reaches this workspace through
// WorkspacesUsing — while UserModel records what is actually primary. Writing the
// synthesized `own-<slug>` into ModelName instead would preserve it under a
// Source of "explicit" and turn an admin pin into a pin naming a model the
// inventory does not have, which candidateTx treats as a hard failure.
func (r *Registry) RecordMaterialization(ref WorkspaceRef, res Resolution) error {
	modelName, chain := res.CascadeName, res.ChainNames()
	if res.UserModel != "" {
		// The cascade model IS the chain here, so recording both would name it
		// twice. ModelName alone is what referrersTx and WorkspacesUsing scan.
		chain = nil
	}
	return r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bAssignments)
		source := SourceInherited
		var prev Assignment
		switch err := getJSON(b, ref.Key(), &prev); {
		case err == nil:
			if prev.Source == SourceExplicit {
				source = SourceExplicit
				// An explicit pin's model is what a deselect must restore, so it
				// survives a personal model taking over as primary.
				if res.UserModel != "" && prev.ModelName != "" {
					modelName, chain = prev.ModelName, prev.Chain
				}
			}
		case errors.Is(err, ErrNotFound):
			// First materialization for this workspace.
		default:
			return err
		}
		return putJSON(b, ref.Key(), Assignment{
			ModelName: modelName, Chain: chain, Source: source,
			UserModel: res.UserModel, MaterializedAt: r.now(),
		})
	})
}

// guardUnreferenced returns an *InUseError naming every referrer when anything
// still references name, so a caller can reject a delete or a disable with the
// concrete list of what to detach first. Takes the caller's transaction: the
// check must not be split from the write it guards.
func guardUnreferenced(tx *bolt.Tx, name string) error {
	refs, err := referrersTx(tx, name)
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		return &InUseError{ModelName: name, Referrers: refs}
	}
	return nil
}
