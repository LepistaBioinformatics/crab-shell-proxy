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

// The AGENT TEMPLATE's config.json — <dataRoot>/templates/<agent>/config.json —
// as the bulk key editor needs it (admin-bulk-instance-config).
//
// It is read for two reasons and written for one. The key picker is populated
// from it because it is the only document that describes every key an instance of
// this agent can have; a histogram over the instances cannot, since a key no
// instance sets yet is invisible there. And the admin may opt in to applying the
// same change here, because a member's config.json is seeded from this file ONCE
// and never re-seeded (provisionUser treats an existing file as "returning user,
// leave as-is") — so without the template write the fix holds for today's members
// and silently misses tomorrow's.
//
// Writing the template reaches every subscription of the agent, which is why the
// write is opt-in, revision-gated, and recorded.

// TemplateKey is one dotted leaf of the template document.
type TemplateKey struct {
	Key string `json:"key"`
	// Value is the leaf as raw JSON, so a null stays a null and an array or an
	// empty object arrives at the picker in the shape it has on disk.
	Value json.RawMessage `json:"value"`
	// Managed keys are INCLUDED and flagged, not filtered: the picker renders them
	// disabled and explains why. Dropping them would leave the admin hunting for a
	// key that is present in the file but simply not editable.
	Managed bool `json:"managed"`
}

type TemplateCatalog struct {
	// Template is the template NAME, which config.yaml declares per agent and is
	// not the agent key: two agents may share one template, and a write here
	// reaches every agent that does.
	Template         string        `json:"template"`
	Keys             []TemplateKey `json:"keys"`
	TemplateRevision string        `json:"templateRevision"`
}

// TemplateResult reports the template write separately from an error because the
// template is the LAST step of a bulk apply: the instance writes it accompanies
// have already landed, and failing the caller's whole request over the template
// would misreport work that succeeded.
type TemplateResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	// Migration is the base name of the record written beside the template, which
	// is what a later revert is found by.
	Migration string `json:"migration,omitempty"`
}

// TemplateConfigKeys returns every dotted leaf of one agent template.
//
// Unlike ReadInstanceConfig, an unparseable document is a hard error here: an
// instance's broken config.json is the thing being repaired, but a broken
// template yields no catalog to offer and nothing to pick from.
func (m *Manager) TemplateConfigKeys(template string) (TemplateCatalog, error) {
	raw, doc, err := m.readTemplateConfig(template)
	if err != nil {
		return TemplateCatalog{}, err
	}

	keys := make([]TemplateKey, 0, len(doc))
	keys = appendTemplateLeaves(keys, doc, "")
	sort.Slice(keys, func(i, j int) bool { return keys[i].Key < keys[j].Key })

	return TemplateCatalog{
		Template: template,
		Keys:     keys,
		// Over the bytes AS READ, so it is the same token ApplyTemplateConfigKey
		// compares the file against.
		TemplateRevision: revisionOf(raw),
	}, nil
}

// ApplyTemplateConfigKey sets one dotted key in the agent template so members
// provisioned later inherit the value.
func (m *Manager) ApplyTemplateConfigKey(template, key string, value any, revision, by string, at time.Time) (TemplateResult, error) {
	if err := ValidateConfigKey(key); err != nil {
		return TemplateResult{OK: false}, fmt.Errorf("apply template config key: %w", err)
	}
	if IsManagedConfigPath(key) {
		return TemplateResult{OK: false}, fmt.Errorf("%w: %q", ErrManagedConfigPath, key)
	}

	path := m.templateConfigPath(template)
	current, doc, err := m.readTemplateConfig(template)
	if err != nil {
		return TemplateResult{OK: false}, err
	}
	// Strictly required, with no empty-token escape hatch as WriteInstanceConfig
	// allows: no screen shows this document, so a blind write is the one way two
	// admins could clobber each other with nothing to notice it by.
	if revision != revisionOf(current) {
		return TemplateResult{OK: false}, ErrStaleRevision
	}

	prior, state := lookupPath(doc, key)
	if state == pathConflict {
		return TemplateResult{OK: false}, fmt.Errorf("%w: %q", ErrPathConflict, key)
	}

	to, err := json.Marshal(value)
	if err != nil {
		return TemplateResult{OK: false}, fmt.Errorf("encode template value for %q: %w", key, err)
	}
	if err := setPath(doc, key, value); err != nil {
		return TemplateResult{OK: false}, err
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return TemplateResult{OK: false}, err
	}
	// The same cap the instance editor enforces, checked AFTER the edit because
	// the value arrives as a parsed any and the document is what has to fit. A
	// template is seeded into every future member's workspace, so bloat here is
	// bloat multiplied.
	if len(body) > maxInstanceConfigBytes {
		return TemplateResult{OK: false}, ErrConfigTooLarge
	}
	if err := writeConfigAtomic(path, body); err != nil {
		return TemplateResult{OK: false}, err
	}
	// No chown: the templates tree is NOT bind-mounted into any container — a
	// running instance mounts its own user workspace, the shared dirs and the
	// managed skills — so there is no container user to grant access to, and
	// handing one out would only widen who can read the seed.
	//
	// No re-materialization either: a template is not a workspace, so
	// reapplyWorkspace has nothing to mean here. Materialization is what happens to
	// a workspace when it is provisioned FROM this file, and that is where the
	// managed keys get their authoritative values.

	res := TemplateResult{OK: true}
	rec := ConfigMigration{
		Key:            key,
		To:             to,
		AppliedAt:      at,
		By:             by,
		Scope:          ConfigMigrationScope{Agent: template},
		RevisionBefore: revisionOf(current),
		RevisionAfter:  revisionOf(body),
	}
	if state == pathAbsent {
		rec.FromAbsent = true
	} else {
		// prior came out of json.Unmarshal, so it always encodes.
		rec.From, _ = json.Marshal(prior)
	}
	// The record goes beside the template it describes, where whoever reverts will
	// look. The scope carries no tenant or subscription: the template belongs to the
	// agent, and that is exactly why writing it reaches every subscription.
	name, err := writeConfigMigration(filepath.Join(config.TemplatesDir(m.cfg.ContainerDataRoot, template),
		".config-migrations"), rec)
	if err != nil {
		// Reported, never returned: the write landed, and telling the admin it
		// failed would invite a retry of an edit that is already on disk.
		m.logf("template config: migration record for %s/%s failed: %v", template, key, err)
		res.Detail = fmt.Sprintf("migration record not written: %v", err)
		return res, nil
	}
	res.Migration = name
	return res, nil
}

func (m *Manager) templateConfigPath(template string) string {
	return filepath.Join(config.TemplatesDir(m.cfg.ContainerDataRoot, template), "config.json")
}

// readTemplateConfig returns the bytes and the parsed document together: the
// caller needs both, and reading twice would let the revision be computed over
// bytes that are not the ones parsed.
func (m *Manager) readTemplateConfig(template string) ([]byte, map[string]any, error) {
	path := m.templateConfigPath(template)
	raw, err := os.ReadFile(path)
	if err != nil {
		// The os error travels intact rather than being translated: a missing
		// template is an operator-visible deployment fault, and errors.Is against
		// os.ErrNotExist is what the caller maps to a 404.
		return nil, nil, fmt.Errorf("agent template %q config.json: %w", template, err)
	}
	doc, err := parseConfigObject(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("agent template %q config.json: %w", template, err)
	}
	return raw, doc, nil
}

// appendTemplateLeaves flattens doc to dotted LEAF paths.
//
// What counts as a leaf is the whole substance of this function:
//
//	scalar             leaf — including a JSON null, which is a value
//	array              leaf — NOT descended and NOT indexed, because setPath
//	                   cannot write allowed_hosts.0 and offering it would hand
//	                   the admin a key no apply could honour
//	non-empty object   descended, never emitted itself
//	empty object       leaf, emitted as itself — the shipped template has several
//	                   ("isolation": {}, "turn_profile": {"history": {}}), and
//	                   descending into one emits nothing, so it would vanish from
//	                   the picker instead of showing up as the settable key it is
//
// A name no dotted path can address is skipped, along with everything under it:
// the editor addresses keys by dotted path, so a row it could never resolve is
// worse than a missing row. The check is on the RAW name and not on the joined
// path, because ValidateConfigKey splits on dots and would let a name that
// CONTAINS one through — "my.key" passes as two legal segments while describing
// a nested path the document does not have.
func appendTemplateLeaves(out []TemplateKey, doc map[string]any, prefix string) []TemplateKey {
	for name, v := range doc {
		if !configKeySegmentRe.MatchString(name) {
			continue
		}
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		if child, ok := v.(map[string]any); ok && len(child) > 0 {
			out = appendTemplateLeaves(out, child, key)
			continue
		}
		// v came out of json.Unmarshal, so it always encodes.
		enc, _ := json.Marshal(v)
		out = append(out, TemplateKey{Key: key, Value: enc, Managed: IsManagedConfigPath(key)})
	}
	return out
}
