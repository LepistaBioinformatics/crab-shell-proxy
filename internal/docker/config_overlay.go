package docker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// A SUBSCRIPTION-SCOPED seed overlay for config.json
// (admin-bulk-instance-config, follow-up).
//
// The problem it solves: a bulk apply reaches every EXISTING instance of one agent
// in one subscription, but a workspace's config.json is seeded from the agent
// template once and never re-seeded, so members created LATER inherit the
// template's value. The only lever for those was the template itself — which is
// shared by every subscription running that agent (and by every agent declaring the
// same template), so "make this the default for my subscription's future members"
// could not be said at all. This is that sentence.
//
// It is a SEED, not a policy. It applies during provisioning of a new workspace and
// nowhere else: an existing member's config is never revisited, and an admin who
// tuned one instance by hand keeps that tuning. Native secrets and persona take the
// other route and re-apply on every ensure; the difference is deliberate, because
// this exists to scope the template's reach, and the template is a seed.
//
// Shape mirrors the native-secret overlay (secrets.go): a flat map from dotted key
// to value, merged onto the document. The file lives beside the other
// subscription+agent scope stores.

// configOverlayFile is the file name inside the scope's shared-agent dir.
const configOverlayFile = "config-overlay.json"

// readConfigOverlay reads the flat dotted-key overlay. An absent file is an empty
// overlay, not an error: almost no scope has one.
func readConfigOverlay(path string) (map[string]json.RawMessage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]json.RawMessage{}, nil
		}
		return nil, err
	}
	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", configOverlayFile, err)
	}
	return out, nil
}

// upsertConfigOverlay sets ONE key, leaving every other entry alone.
//
// Per-key rather than whole-file: two admins scoping different keys must both land.
// That is also why this path carries no revision token — the operation is an
// idempotent single-key upsert, and a whole-file gate would 409 two writes that do
// not actually conflict.
func upsertConfigOverlay(path, key string, value json.RawMessage) error {
	if err := ValidateConfigKey(key); err != nil {
		return err
	}
	// Refused here as well as at the API edge: a managed key in an overlay would be
	// reverted by the next materialization anyway, and storing one would make the
	// overlay look like a way around ManagedConfigPaths.
	if IsManagedConfigPath(key) {
		return fmt.Errorf("%w: %q", ErrManagedConfigPath, key)
	}

	current, err := readConfigOverlay(path)
	if err != nil {
		return err
	}
	current[key] = value

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

// applyConfigOverlay merges the overlay into a freshly seeded config.json and
// reports how many keys landed.
//
// Every entry it cannot apply is SKIPPED, never fatal. This runs inside
// provisioning: failing the seed over one unusable key would leave the member with
// no workspace at all, which is far worse than a workspace missing one scoped
// default. The two skip cases are a managed key (the next materialization would
// revert it) and a path blocked by a non-object.
//
// A no-op leaves the file untouched rather than re-marshalling it, so the common
// case — no overlay for this scope — does not churn the seed's formatting.
func applyConfigOverlay(configPath, overlayPath string) (int, error) {
	overlay, err := readConfigOverlay(overlayPath)
	if err != nil {
		return 0, err
	}
	if len(overlay) == 0 {
		return 0, nil
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		return 0, err
	}
	doc, err := parseConfigObject(raw)
	if err != nil {
		return 0, fmt.Errorf("parse seeded config.json: %w", err)
	}

	// Sorted so the applied order — and therefore the outcome when two entries
	// touch the same branch — is the same on every provision.
	keys := make([]string, 0, len(overlay))
	for k := range overlay {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	applied := 0
	for _, key := range keys {
		if err := ValidateConfigKey(key); err != nil || IsManagedConfigPath(key) {
			continue
		}
		var value any
		if err := json.Unmarshal(overlay[key], &value); err != nil {
			continue
		}
		if err := setPath(doc, key, value); err != nil {
			continue
		}
		applied++
	}
	if applied == 0 {
		return 0, nil
	}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(configPath, out, 0o600); err != nil {
		return 0, err
	}
	return applied, nil
}

// OverlayResult reports the scoped-seed write separately from an error, on the same
// terms as TemplateResult: it accompanies instance writes that have already landed,
// so failing the caller's whole request over it would misreport work that succeeded.
type OverlayResult struct {
	OK        bool   `json:"ok"`
	Detail    string `json:"detail,omitempty"`
	Migration string `json:"migration,omitempty"`
}

// ApplyOverlayConfigKey scopes one key to a subscription's FUTURE members.
//
// Unlike the template write, this records a fully-scoped migration: the record's
// scope carries the tenant AND the subscription, because unlike the template this
// change really does belong to one. A template record's empty tenant/subscription
// fields are accurate — there is no subscription to name — and that difference is
// the whole reason this path exists.
//
// `from` is the overlay's PRIOR entry, not the template's value: the record
// describes a change to the overlay, and reverting means putting the overlay back.
func (m *Manager) ApplyOverlayConfigKey(
	scope Scope, key string, value json.RawMessage, by string, at time.Time,
) (OverlayResult, error) {
	path := config.SubscriptionAgentConfigOverlay(
		m.cfg.ContainerDataRoot, scope.TenantID, scope.SubsAccID, scope.AgentKey)

	current, err := readConfigOverlay(path)
	if err != nil {
		return OverlayResult{}, err
	}
	prior, had := current[key]

	if err := upsertConfigOverlay(path, key, value); err != nil {
		return OverlayResult{}, err
	}

	rec := ConfigMigration{
		Key:       key,
		To:        value,
		AppliedAt: at,
		By:        by,
		Scope: ConfigMigrationScope{
			TenantID:  scope.TenantID,
			SubsAccID: scope.SubsAccID,
			Agent:     scope.AgentKey,
		},
	}
	if had {
		rec.From = prior
	} else {
		rec.FromAbsent = true
	}
	name, err := writeConfigMigration(filepath.Join(filepath.Dir(path), ".config-migrations"), rec)
	if err != nil {
		// The scoped default is stored; losing its record is the lesser harm.
		m.logf("config overlay: migration record for %s/%s failed: %v", scope.SubsAccID, key, err)
		return OverlayResult{OK: true, Detail: err.Error()}, nil
	}
	return OverlayResult{OK: true, Migration: name}, nil
}
