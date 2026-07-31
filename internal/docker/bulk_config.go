package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"
)

// One config.json key seen across every instance of one agent under a scope
// (admin-bulk-instance-config).
//
// The single-instance editor (instance_config.go) repairs one member at a time,
// which is the wrong shape for the question an admin actually arrives with:
// "which of my members have tools.web.max_results wrong?". Answering it by
// opening each document in turn does not scale past a handful, and nothing tells
// the admin how many distinct answers are out there.
//
// So the view is a HISTOGRAM, not a list of documents: instances holding the same
// value collapse into one bucket the admin can act on as a unit, and the three
// ways an instance can hold no value at all — never provisioned or unparseable,
// key missing, a non-object in the way — stay separate, because each implies a
// different decision.

// scopeInstanceEmails maps userAccId to owner email for the members of a scope.
//
// Emails come from a different source than the workspace enumeration:
// workspacesInScope returns WorkspaceKeys, which carry no email. A member whose
// .crab-owner.json is missing is reported without one — the histogram and the
// outcome list are still correct, and failing a whole batch over a cosmetic field
// would hide the answer the admin came for. The error is dropped for the same
// reason.
//
// Both the inspect and the apply view need exactly this, so it lives here rather
// than twice.
func (m *Manager) scopeInstanceEmails(scope Scope) map[string]string {
	emails := map[string]string{}
	users, _ := m.ListSubscriptionUsers(scope.TenantID, scope.SubsAccID)
	for _, u := range users {
		if u.Email != "" {
			emails[u.AccID] = u.Email
		}
	}
	return emails
}

// BucketState is why an instance is in a bucket. Only BucketValue carries Value;
// the other three carry a per-instance Detail instead.
type BucketState string

const (
	// BucketValue: the key resolved. JSON null is a value and lands here.
	BucketValue BucketState = "value"
	// BucketAbsent: the document parses and the key is simply not in it.
	BucketAbsent BucketState = "absent"
	// BucketConflict: a segment on the way to the leaf holds a non-object, so
	// setting the key would REPLACE that value rather than add a leaf.
	BucketConflict BucketState = "path_conflict"
	// BucketUnreadable: there is no document to read the key out of — the member
	// never started this agent, or the file does not parse.
	BucketUnreadable BucketState = "unreadable"
)

// ConfigKeyInstance is one member's workspace inside a bucket. Revision is the
// on-disk revision of that workspace's config.json, which a later bulk apply
// gates on: an instance the admin never saw a revision for is one it must not
// write to.
type ConfigKeyInstance struct {
	UserAccID string `json:"userAccId"`
	Email     string `json:"email,omitempty"`
	Revision  string `json:"revision"`
	Detail    string `json:"detail,omitempty"`
}

type ConfigKeyBucket struct {
	State     BucketState         `json:"state"`
	Value     json.RawMessage     `json:"value,omitempty"`
	Count     int                 `json:"count"`
	Instances []ConfigKeyInstance `json:"instances"`
}

type ScopeConfigInspection struct {
	Key     string            `json:"key"`
	Agent   string            `json:"agent"`
	Total   int               `json:"total"`
	Buckets []ConfigKeyBucket `json:"buckets"`
}

// InspectScopeConfigKey reports the distribution of one key across the scope's
// instances of one agent.
//
// The key is validated and refused BEFORE any filesystem access: a caller that
// asked about a managed path needs to be told the edit could not survive, not
// handed a histogram it will act on.
func (m *Manager) InspectScopeConfigKey(scope Scope, key string) (ScopeConfigInspection, error) {
	if err := ValidateConfigKey(key); err != nil {
		return ScopeConfigInspection{}, fmt.Errorf("inspect config key: %w", err)
	}
	if IsManagedConfigPath(key) {
		return ScopeConfigInspection{}, fmt.Errorf("%w: %q", ErrManagedConfigPath, key)
	}

	emails := m.scopeInstanceEmails(scope)

	// Value buckets are keyed by the ENCODED value, which is what makes the
	// grouping canonical for free: json.Unmarshal already normalised every number
	// to float64 and json.Marshal sorts object keys, so 1 and 1.0, and two objects
	// differing only in key order, collide here. A hand-written canonicaliser
	// would be a second definition of "the same value" to keep in step with the
	// encoder.
	values := map[string][]ConfigKeyInstance{}
	var absent, conflict, unreadable []ConfigKeyInstance

	keys := m.workspacesInScope(scope)
	for _, wsKey := range keys {
		inst := ConfigKeyInstance{UserAccID: wsKey.UserAccID, Email: emails[wsKey.UserAccID]}
		cfg, err := m.ReadInstanceConfig(wsKey)
		// Zero on ErrNotProvisioned, so an instance with no file carries no
		// revision and a bulk apply has nothing to match against.
		inst.Revision = cfg.Revision
		switch {
		case errors.Is(err, ErrNotProvisioned):
			inst.Detail = "not_provisioned"
			unreadable = append(unreadable, inst)
		case err != nil:
			inst.Detail = err.Error()
			unreadable = append(unreadable, inst)
		case !cfg.Valid:
			// A document that does not parse cannot be edited by path. It is
			// reported, never guessed at, and above all not counted as absent —
			// that would make a bulk apply look like it fixed an instance it
			// skipped.
			inst.Detail = cfg.ParseError
			unreadable = append(unreadable, inst)
		default:
			// cfg.Valid means ReadInstanceConfig already parsed these bytes.
			doc, _ := parseConfigObject([]byte(cfg.Raw))
			v, state := lookupPath(doc, key)
			switch state {
			case pathFound:
				// v came out of json.Unmarshal, so it always encodes.
				enc, _ := json.Marshal(v)
				values[string(enc)] = append(values[string(enc)], inst)
			case pathAbsent:
				absent = append(absent, inst)
			default:
				// lookupPath does not report WHICH segment blocked, and naming it
				// would mean walking the path a second time for a cosmetic string.
				inst.Detail = fmt.Sprintf("%v: %q", ErrPathConflict, key)
				conflict = append(conflict, inst)
			}
		}
	}

	// Bucket order has to be stable across identical calls: the admin screen is
	// a list of rows an operator clicks, and map iteration would reshuffle it on
	// every refresh. Value buckets lead, biggest first — the majority value is
	// what a bulk edit usually converges on — with the encoded value breaking
	// ties, since counts collide constantly.
	buckets := []ConfigKeyBucket{}
	for enc, insts := range values {
		sort.Slice(insts, func(i, j int) bool { return insts[i].UserAccID < insts[j].UserAccID })
		buckets = append(buckets, ConfigKeyBucket{
			State: BucketValue, Value: json.RawMessage(enc), Count: len(insts), Instances: insts,
		})
	}
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].Count != buckets[j].Count {
			return buckets[i].Count > buckets[j].Count
		}
		return string(buckets[i].Value) < string(buckets[j].Value)
	})

	// The tail states come after every value, in a fixed order, and only when
	// something is in them: an empty bucket is a row that means nothing.
	for _, tail := range []struct {
		state BucketState
		insts []ConfigKeyInstance
	}{
		{BucketAbsent, absent},
		{BucketConflict, conflict},
		{BucketUnreadable, unreadable},
	} {
		if len(tail.insts) == 0 {
			continue
		}
		insts := tail.insts
		sort.Slice(insts, func(i, j int) bool { return insts[i].UserAccID < insts[j].UserAccID })
		buckets = append(buckets, ConfigKeyBucket{
			State: tail.state, Count: len(insts), Instances: insts,
		})
	}

	return ScopeConfigInspection{
		Key: key, Agent: scope.AgentKey, Total: len(keys), Buckets: buckets,
	}, nil
}

// The apply half: one key, one value, every instance that does not already hold
// it. The inspection above is what the admin picked the value from, and its
// revisions are what this gates on.

// ErrInvalidConfigValue means the request carries no value to set, or one that is
// not JSON.
//
// The guard is on the LENGTH of the raw bytes, never on the decoded value: zero
// bytes are a field the caller omitted, while `null` is four bytes that decode to
// nil and are a perfectly good request — the inspect view already buckets an
// explicit null as BucketValue, and reverting a null is a different operation from
// reverting an absent key. Rejecting a nil target would refuse half of that pair.
var ErrInvalidConfigValue = errors.New("invalid config value")

// Per-instance outcomes. Only OutcomeApplied wrote anything.
const (
	OutcomeApplied      = "applied"
	OutcomeUnchanged    = "unchanged"
	OutcomeStale        = "stale"
	OutcomePathConflict = "path_conflict"
	OutcomeUnreadable   = "unreadable"
	OutcomeError        = "error"
)

type ScopeConfigChange struct {
	Key   string          `json:"key"`   // already validated by the caller, but re-check anyway
	Value json.RawMessage `json:"value"` // verbatim: true and "true" are different requests
	// Revisions is keyed by userAccId. Within one subscription + agent that is
	// unique, and the scope fixes both, so the rest of WorkspaceKey is not repeated.
	Revisions map[string]string `json:"revisions"`
	// AlsoTemplate and TemplateRevision are carried here for the HTTP layer, which
	// composes the template write next to this one. ApplyScopeConfigKey does not act
	// on them: a template is not a workspace, and its failure means something
	// different (future members do not inherit) than an instance failure.
	AlsoTemplate     bool   `json:"alsoTemplate,omitempty"`
	TemplateRevision string `json:"templateRevision,omitempty"`
	// AlsoSubscription scopes the change to THIS subscription's future members via
	// the seed overlay, which is the alternative to writing the agent template —
	// the template reaches every subscription on that agent, and this reaches one.
	// Carried here for the HTTP layer like the two above; ApplyScopeConfigKey does
	// not act on it.
	AlsoSubscription bool `json:"alsoSubscription,omitempty"`
	// By and AppliedAt are set by the SERVER and json:"-" is what enforces it. Both
	// are exported, so without the tag a request body carrying {"by": "someone
	// else"} would populate them — Go's field matching is case-insensitive — and a
	// caller could forge the provenance of every migration record in the batch.
	By        string    `json:"-"` // caller identity, for the migration record
	AppliedAt time.Time `json:"-"` // one timestamp for the whole batch, so records line up
}

// InstanceOutcome is what happened to one member's config.json. Reapplied is a
// pointer so that "the reapply ran and succeeded" stays distinguishable from
// "nothing was written, so no reapply happened".
type InstanceOutcome struct {
	UserAccID string         `json:"userAccId"`
	Email     string         `json:"email,omitempty"`
	Outcome   string         `json:"outcome"`
	Detail    string         `json:"detail,omitempty"`
	Migration string         `json:"migration,omitempty"`
	RecordErr string         `json:"recordError,omitempty"`
	Reapplied *ReapplyResult `json:"reapplied,omitempty"`
}

type ScopeConfigResult struct {
	Key      string            `json:"key"`
	Outcomes []InstanceOutcome `json:"outcomes"`
	Summary  map[string]int    `json:"summary"` // outcome -> count
}

// ApplyScopeConfigKey sets one key to one value on every instance of one agent in
// the scope that does not already hold it.
//
// It NEVER fails wholesale: one member with a corrupt document, a scalar in the
// path or an unwritable workspace must not block a policy change for the other
// forty-nine. The returned error is only for the up-front validation, where
// nothing has been touched yet and the whole request is wrong.
//
// It knows nothing about the agent template. That write is a separate method
// composed at the HTTP layer, because it is optional and its failure means
// something different (future members do not inherit) than an instance failure.
func (m *Manager) ApplyScopeConfigKey(scope Scope, ch ScopeConfigChange) (ScopeConfigResult, error) {
	if err := ValidateConfigKey(ch.Key); err != nil {
		return ScopeConfigResult{}, fmt.Errorf("apply config key: %w", err)
	}
	if IsManagedConfigPath(ch.Key) {
		return ScopeConfigResult{}, fmt.Errorf("%w: %q", ErrManagedConfigPath, ch.Key)
	}
	if len(ch.Value) == 0 {
		return ScopeConfigResult{}, fmt.Errorf("%w: %q has no value to set", ErrInvalidConfigValue, ch.Key)
	}
	// Decoded ONCE for the whole batch, and re-encoded to get the canonical form
	// every instance is compared against. The comparison is deliberately not
	// hand-written: json.Unmarshal normalised the numbers and json.Marshal sorts
	// object keys, which is the same trick InspectScopeConfigKey buckets with.
	var target any
	if err := json.Unmarshal(ch.Value, &target); err != nil {
		return ScopeConfigResult{}, fmt.Errorf("%w: %v", ErrInvalidConfigValue, err)
	}
	// target came out of json.Unmarshal, so it always encodes.
	targetEnc, _ := json.Marshal(target)

	emails := m.scopeInstanceEmails(scope)

	keys := m.workspacesInScope(scope)
	outcomes := make([]InstanceOutcome, 0, len(keys))
	for _, wsKey := range keys {
		out := InstanceOutcome{UserAccID: wsKey.UserAccID, Email: emails[wsKey.UserAccID]}
		cfg, err := m.ReadInstanceConfig(wsKey)
		switch {
		case errors.Is(err, ErrNotProvisioned):
			out.Outcome, out.Detail = OutcomeUnreadable, "not_provisioned"
		case err != nil:
			out.Outcome, out.Detail = OutcomeUnreadable, err.Error()
		case !cfg.Valid:
			// A document that does not parse cannot be edited by path. It is skipped,
			// never repaired here — the single-instance raw editor is that tool.
			out.Outcome, out.Detail = OutcomeUnreadable, cfg.ParseError
		case ch.Revisions[wsKey.UserAccID] != cfg.Revision:
			// A MISSING revision fails the same comparison as a mismatched one, and
			// must: missing means the instance was provisioned after the admin's
			// inspect, and writing to a document the admin never saw is exactly what
			// this gate exists to prevent. cfg.Revision is never empty here.
			out.Outcome = OutcomeStale
			out.Detail = "revision missing or changed since the inspection"
		default:
			// cfg.Raw, NOT a fresh os.ReadFile: the read may have masked a legacy
			// model_list[*].api_keys, and WriteInstanceConfig's unmaskAgainst restores
			// it from disk before writing. Pairing those two halves is what keeps the
			// credential; a raw read would bypass the mask and only work by luck.
			doc, _ := parseConfigObject([]byte(cfg.Raw))
			current, state := lookupPath(doc, ch.Key)
			// current came out of json.Unmarshal, so it always encodes.
			currentEnc, _ := json.Marshal(current)
			switch {
			case state == pathConflict:
				// Same phrasing as the inspect bucket, so the two views of one
				// condition read identically.
				out.Outcome = OutcomePathConflict
				out.Detail = fmt.Sprintf("%v: %q", ErrPathConflict, ch.Key)
			case state == pathFound && string(currentEnc) == string(targetEnc):
				// No write at all, so the file's mtime is left where it was: an admin
				// looking at "last modified" must not see a change that did not happen.
				out.Outcome = OutcomeUnchanged
			default:
				m.applyOneInstance(wsKey, ch, cfg, doc, target, currentEnc, state, &out)
			}
		}
		// Every instance the batch did not change is logged, by identity and outcome
		// only. ch.Value is never a log argument: an admin may be pushing a
		// credential, and the proxy log is not the place for it (FR-5.1).
		if out.Outcome != OutcomeApplied && out.Outcome != OutcomeUnchanged {
			m.logf("bulk config %q on %s: %s: %s", ch.Key, m.ContainerName(wsKey),
				out.Outcome, out.Detail)
		}
		outcomes = append(outcomes, out)
	}

	// Deterministic order, for the same reason the inspect buckets are sorted: the
	// admin screen is a list of rows, and the enumeration is not a promise.
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].UserAccID < outcomes[j].UserAccID })
	summary := map[string]int{}
	for _, o := range outcomes {
		summary[o.Outcome]++
	}
	return ScopeConfigResult{Key: ch.Key, Outcomes: outcomes, Summary: summary}, nil
}

// applyOneInstance writes the edited document and records it. It is split out of
// the loop above only because the loop is already two switches deep; it is one
// instance's write path and nothing else calls it.
func (m *Manager) applyOneInstance(wsKey WorkspaceKey, ch ScopeConfigChange,
	cfg InstanceConfig, doc map[string]any, target any, currentEnc []byte,
	state pathState, out *InstanceOutcome) {

	if err := setPath(doc, ch.Key, target); err != nil {
		// Unreachable after a pathFound/pathAbsent lookup — setPath and lookupPath
		// agree on every shape — but the error is not dropped.
		out.Outcome, out.Detail = OutcomePathConflict, err.Error()
		return
	}
	// Two-space indent, matching what materializeModels already writes, so the file
	// stays in the shape the proxy produces everywhere else.
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		out.Outcome, out.Detail = OutcomeError, err.Error()
		return
	}
	// cfg.Revision is passed along: the admin's own gate above compared against the
	// inspection, and this one closes the window between that read and this write.
	written, reapplied, err := m.WriteInstanceConfig(wsKey, string(raw), cfg.Revision)
	if err != nil {
		// ErrStaleRevision here is the SAME condition the gate above reports, caught
		// one window later: a second writer landed between this instance's read and
		// its write. Reporting it as stale rather than error keeps one condition to
		// one outcome, so an admin re-inspecting and retrying is the answer in both
		// cases.
		out.Outcome, out.Detail = OutcomeError, err.Error()
		if errors.Is(err, ErrStaleRevision) {
			out.Outcome = OutcomeStale
		}
		return
	}
	out.Outcome = OutcomeApplied
	// A failing reapply leaves the outcome applied: the write landed, and undoing
	// it would throw away the change the admin asked for.
	out.Reapplied = &reapplied

	rec := ConfigMigration{
		Key: ch.Key,
		// The submitted bytes, not the re-encoded target: a round trip through
		// json.Unmarshal turns a large integer into 1.2345678901234567e+19, and a
		// recovery aid that cannot restore the value it recorded is not one.
		To:        ch.Value,
		AppliedAt: ch.AppliedAt,
		By:        ch.By,
		// From the WORKSPACE key, not from scope: a tenant-wide scope carries no
		// SubsAccID and may carry no agent, and a record found on its own has to say
		// which instance it belongs to.
		Scope: ConfigMigrationScope{
			TenantID: wsKey.TenantID, SubsAccID: wsKey.SubsAccID, Agent: wsKey.Role,
		},
		RevisionBefore: cfg.Revision,
		RevisionAfter:  written.Revision,
	}
	if state == pathAbsent {
		// Reverting an absent key means DELETING it, which "from": null cannot say.
		rec.FromAbsent = true
	} else {
		rec.From = json.RawMessage(currentEnc)
	}
	// Derived from the config path rather than restating the workspace layout.
	dir := filepath.Join(filepath.Dir(m.instanceConfigPath(wsKey)), ".config-migrations")
	name, err := writeConfigMigration(dir, rec)
	if err != nil {
		// The change landed; losing the recovery copy is the lesser harm, so it is
		// reported and logged rather than turned into a failure.
		m.logf("bulk config %q on %s: migration record failed: %v",
			ch.Key, m.ContainerName(wsKey), err)
		out.RecordErr = err.Error()
		return
	}
	out.Migration = name
}
