package registry

import (
	"errors"
	"fmt"
	"strings"

	bolt "go.etcd.io/bbolt"
)

// Resolution is what a workspace should have materialized.
type Resolution struct {
	// Primary is the model written to agents.defaults.provider/model_name.
	Primary Model
	// Chain is the primary's declared fallbacks, filtered to active models, in
	// declared order — written to agents.defaults.model_fallbacks.
	Chain []Model
	// Level reports which cascade rung decided the primary, for display.
	Level ScopeLevel
	// Skipped names declared fallbacks left out because they are not active, so
	// the caller can log the omission rather than silently shortening the chain.
	Skipped []string
	// SkippedLevels names cascade levels passed over because their default points
	// at a model the inventory no longer has. The registry carries no logger, so
	// the omission is reported here for the caller to log — same shape as Skipped.
	SkippedLevels []string
}

// Names returns primary + chain model names in materialization order.
func (res Resolution) Names() []string {
	out := make([]string, 0, len(res.Chain)+1)
	out = append(out, res.Primary.ModelName)
	for _, m := range res.Chain {
		out = append(out, m.ModelName)
	}
	return out
}

// ChainNames returns just the fallback names, for the Assignment record.
func (res Resolution) ChainNames() []string {
	if len(res.Chain) == 0 {
		return nil
	}
	out := make([]string, 0, len(res.Chain))
	for _, m := range res.Chain {
		out = append(out, m.ModelName)
	}
	return out
}

// Resolve is THE answer to "which model does this workspace use". Every caller
// goes through it; there is deliberately no second path, because two paths
// writing agents.defaults is the bug this package exists to remove.
//
// Cascade, most specific first:
//
//	explicit per-user assignment > subscription > tenant > agent > global
//
// A deprecated result is followed to its replacement ONLY when the workspace has
// no materialized assignment. That single condition is what produces "new users
// get the successor while existing users keep the old model" without a separate
// code path.
func (r *Registry) Resolve(ref WorkspaceRef) (Resolution, error) {
	var res Resolution
	err := r.db.View(func(tx *bolt.Tx) error {
		models := tx.Bucket(bModels)

		var existing Assignment
		hasAssignment := false
		switch err := getJSON(tx.Bucket(bAssignments), ref.Key(), &existing); {
		case err == nil:
			hasAssignment = true
		case errors.Is(err, ErrNotFound):
			// Never materialized — the condition the deprecation hop keys on.
		default:
			// A corrupt record is NOT "no assignment": reading it as absent would let
			// the cascade replace a pin this workspace may well have.
			return fmt.Errorf("read assignment %s: %w", ref.Key(), err)
		}

		candidate, level, skippedLevels, err := candidateTx(tx, ref, existing, hasAssignment)
		if err != nil {
			return err
		}

		var primary Model
		if err := getJSON(models, candidate, &primary); err != nil {
			return fmt.Errorf("cascade named model %q: %w", candidate, err)
		}

		// The hop: only an unmaterialized workspace follows the replacement.
		if primary.Status == StatusDeprecated && !hasAssignment {
			hopped, err := resolveReplacementTx(tx, candidate)
			if err != nil {
				return err
			}
			primary = hopped
		}

		chain := make([]Model, 0, len(primary.Fallbacks))
		var skipped []string
		for _, name := range primary.Fallbacks {
			var fb Model
			if err := getJSON(models, name, &fb); err != nil {
				skipped = append(skipped, name)
				continue
			}
			if fb.Status != StatusActive {
				skipped = append(skipped, name)
				continue
			}
			chain = append(chain, fb)
		}

		res = Resolution{
			Primary: primary, Chain: chain, Level: level,
			Skipped: skipped, SkippedLevels: skippedLevels,
		}
		return nil
	})
	if err != nil {
		return Resolution{}, err
	}
	return res, nil
}

// candidateTx returns the model_name the cascade selects, the level that selected
// it, and any levels skipped because their default is dangling.
//
// A level whose default names a model the inventory no longer has is SKIPPED, not
// fatal: the boot migration imports pre-existing overrides with SetScopeDefaultRaw,
// which has no active-model check, so a stale shared/model.json can name a model
// nothing declares. Failing there would refuse every inherited workspace under
// that tenant; continuing down the cascade keeps them bootable and reports the
// dangling level to the caller for logging.
//
// An EXPLICIT pin naming a missing model is deliberately NOT skipped — it stays a
// hard failure in Resolve. Falling through to a scope default would silently
// replace a deliberate per-user choice, which is the failure this package exists
// to remove; a delete cannot produce that state either (I2 blocks it).
func candidateTx(tx *bolt.Tx, ref WorkspaceRef, existing Assignment, hasAssignment bool) (string, ScopeLevel, []string, error) {
	// An EXPLICIT assignment is a pin and wins. An INHERITED one only records
	// what was materialized — treating it as an override would freeze every
	// workspace at its first model and make scope defaults inert after the first
	// provision.
	if hasAssignment && existing.Source == SourceExplicit && existing.ModelName != "" {
		return existing.ModelName, LevelUser, nil, nil
	}

	sels := []ScopeSel{
		{Level: LevelSubscription, TenantID: ref.TenantID, SubsAccID: ref.SubsAccID},
		{Level: LevelTenant, TenantID: ref.TenantID},
		{Level: LevelAgent, Agent: ref.Agent},
		{Level: LevelGlobal},
	}
	models := tx.Bucket(bModels)
	defaults := tx.Bucket(bScopeDefaults)
	var skippedLevels []string
	for _, sel := range sels {
		key, err := sel.Key()
		if err != nil {
			continue // an incomplete ref simply skips that level
		}
		var d ScopeDefault
		if err := getJSON(defaults, key, &d); err != nil {
			continue
		}
		if d.ModelName == "" {
			continue
		}
		if models.Get([]byte(d.ModelName)) == nil {
			skippedLevels = append(skippedLevels, key+" -> "+d.ModelName)
			continue
		}
		return d.ModelName, sel.Level, skippedLevels, nil
	}
	return "", "", skippedLevels, fmt.Errorf("%w: workspace %s", ErrNoModelResolvable, ref.Key())
}

// ScopeCandidate reports the model_name the SCOPE cascade selects for a
// workspace, ignoring any per-user assignment and without the deprecation hop.
//
// It answers "is this workspace's current model reproducible from the cascade?",
// which is what the boot migration needs to decide whether a captured model must
// be recorded as an explicit pin to survive. It shares candidateTx with Resolve
// on purpose: two functions answering the same question is how the answers drift.
func (r *Registry) ScopeCandidate(ref WorkspaceRef) (string, ScopeLevel, error) {
	var name string
	var level ScopeLevel
	err := r.db.View(func(tx *bolt.Tx) error {
		var err error
		name, level, _, err = candidateTx(tx, ref, Assignment{}, false)
		return err
	})
	if err != nil {
		return "", "", err
	}
	return name, level, nil
}

// resolveReplacementTx is ResolveReplacement inside an existing transaction.
func resolveReplacementTx(tx *bolt.Tx, name string) (Model, error) {
	b := tx.Bucket(bModels)
	seen := map[string]bool{}
	cursor := name
	for hop := 0; hop <= maxDeprecationHops; hop++ {
		if seen[cursor] {
			return Model{}, fmt.Errorf("%w: deprecation chain loops at %q", ErrInvalid, cursor)
		}
		seen[cursor] = true
		var m Model
		if err := getJSON(b, cursor, &m); err != nil {
			return Model{}, fmt.Errorf("deprecation chain from %q: %q: %w", name, cursor, err)
		}
		if m.Status != StatusDeprecated || m.ReplacedBy == "" {
			return m, nil
		}
		cursor = m.ReplacedBy
	}
	return Model{}, fmt.Errorf("%w: deprecation chain from %q exceeds %d hops", ErrInvalid, name, maxDeprecationHops)
}

// WorkspacesUsing lists every workspace whose materialized set contains the
// model — as primary OR as a chain member. The chain half is load-bearing: a key
// edit that reached only primaries would leave every fallback holder on a stale
// or revoked credential.
func (r *Registry) WorkspacesUsing(modelName string) ([]WorkspaceRef, error) {
	var out []WorkspaceRef
	err := r.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bAssignments).ForEach(func(k, raw []byte) error {
			var a Assignment
			if err := jsonUnmarshal(raw, &a); err != nil {
				return err
			}
			if a.ModelName != modelName && !contains(a.Chain, modelName) {
				return nil
			}
			ref, err := parseWorkspaceKey(string(k))
			if err != nil {
				return err
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

// parseWorkspaceKey inverts WorkspaceRef.Key. The ids are already sanitized, so
// the round trip yields the sanitized form — which is what every on-disk path is
// built from anyway.
func parseWorkspaceKey(key string) (WorkspaceRef, error) {
	parts := strings.Split(key, "/")
	if len(parts) != 4 {
		return WorkspaceRef{}, fmt.Errorf("malformed assignment key %q", key)
	}
	return WorkspaceRef{TenantID: parts[0], SubsAccID: parts[1], Agent: parts[2], UserAccID: parts[3]}, nil
}
