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
//
// ONLY A PICOCLAW AGENT HAS ONE.
//
// The reasoning above is picoclaw's throughout, and the template file is
// picoclaw's document -- agents.defaults.allow_read_outside_workspace,
// restrict_to_workspace, steering_mode, channels, clawhub, bridge_url. A
// ganglion agent reads none of it. Offering that catalog for one was offering an
// admin a list of keys the runtime does not have, with nothing on the screen
// saying so, and the resulting write landed in a file nothing reads.
//
// So the catalog resolves by harness. For a ganglion agent it comes from
// ganglionConfigDoc (ganglion_config.go), which stands in the same relation to a
// ganglion instance as this file does to a picoclaw one: it is the only thing
// that describes every key such an instance can have. See ganglionCatalogKeys.

// TemplateKey is one dotted leaf of the template document.
type TemplateKey struct {
	Key string `json:"key"`
	// Value is the leaf as raw JSON, so a null stays a null and an array or an
	// empty object arrives at the picker in the shape it has on disk.
	//
	// ABSENT, not null, when there is no disk to read it from. A ganglion agent's
	// document is generated per member at ensure time, so the exemplar this
	// catalog is flattened from rendered a shape and not a default anybody holds
	// -- and one of its leaves is that member's own memory-graph bearer token. A
	// null here would say "this key's default is null", which is a different and
	// false claim; omitempty keeps the two apart, since a real JSON null leaf
	// encodes to four bytes and an unset one to none.
	Value json.RawMessage `json:"value,omitempty"`
	// Managed keys are INCLUDED and flagged, not filtered: the picker renders them
	// disabled and explains why. Dropping them would leave the admin hunting for a
	// key that is present in the file but simply not editable.
	//
	// It is ManagedConfigPaths for BOTH harnesses, which is a narrower claim than
	// "the proxy wrote this". Everything in a ganglion agent's document is
	// proxy-written, so the wider claim would flag every key and leave a picker
	// with nothing to pick. What this flag has to keep predicting is the refusal:
	// ValidateConfigKey/IsManagedConfigPath is what the apply verbs enforce, so a
	// flag computed any other way would disagree with the 400 the admin gets.
	// That the whole document is generated is a fact about the DOCUMENT, and it
	// belongs beside the catalog rather than on every row of it.
	Managed bool `json:"managed"`
	// Tunable marks a key the catalog OFFERS although the document it was built
	// from does not contain it.
	//
	// It exists because the ganglion's catalog is flattened from the generator's
	// own output (ganglionCatalogKeys), and the generator deliberately does not
	// emit the harness's tuning numbers -- the turn's iteration cap and the
	// sub-agent fan-out budget. Emitting them would be the smaller change and the
	// wrong one: the harness resolves max_tool_iterations FILE FIRST, so a
	// generated key would permanently outrank GANGLION_MAX_ITERATIONS and silently
	// re-cap every agent whose operator set that variable.
	//
	// So they are appended as suggestions instead, which is exactly what the
	// catalog is (handleAdminScopeConfigKeys: "a suggestion list, not a
	// whitelist"). The flag is what lets a client say the true thing about them:
	// absent means the harness's own default is in force, and writing one takes
	// effect when the workspace next starts, not on the current turn.
	Tunable bool `json:"tunable,omitempty"`
	// Harness names the runtime whose document this key came from.
	//
	// On the key and not only on the catalog. The two harnesses' documents share
	// names -- model_list, agents.defaults.model_name, tools.web -- while meaning
	// different files, so a client that labels a suggestion has to be able to
	// label it per row. A catalog-level field would be right exactly until the
	// first list that mixes them.
	Harness string `json:"harness"`
}

type TemplateCatalog struct {
	// Template is the template NAME, which config.yaml declares per agent and is
	// not the agent key: two agents may share one template, and a write here
	// reaches every agent that does.
	//
	// EMPTY for a harness with no template file, along with TemplateRevision.
	Template string        `json:"template"`
	Keys     []TemplateKey `json:"keys"`
	// TemplateRevision gates the opt-in "also write the template" apply, and has
	// no meaning without a file to write.
	TemplateRevision string `json:"templateRevision"`
	// TemplateWritable says whether that apply has a target at all.
	//
	// A field of its own rather than an empty TemplateRevision the client has to
	// interpret. The two failures an empty string invites are both silent: a
	// client that does not know the convention offers a write with nowhere to
	// land, and one that reads the emptiness cannot tell "this agent has no
	// template" from "the revision was dropped on the way here". Stated, it is
	// neither.
	TemplateWritable bool `json:"templateWritable"`
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

// TemplateConfigKeys returns every dotted leaf an instance of one agent can have.
//
// It takes the harness alongside the template name because the two harnesses
// keep that list in different places, and the template name is meaningless for
// one of them. An empty harness is picoclaw, matching config.Load's own default
// -- a config.yaml written before the field existed describes a picoclaw agent.
//
// Unlike ReadInstanceConfig, an unparseable document is a hard error here: an
// instance's broken config.json is the thing being repaired, but a broken
// template yields no catalog to offer and nothing to pick from.
func (m *Manager) TemplateConfigKeys(template, harness string) (TemplateCatalog, error) {
	if harness == config.HarnessGanglion {
		keys, err := ganglionCatalogKeys()
		if err != nil {
			return TemplateCatalog{}, err
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].Key < keys[j].Key })
		return TemplateCatalog{
			// No Template and no TemplateRevision. The agent does still declare a
			// template in config.yaml, but nothing of it ever reaches a ganglion
			// workspace: ensureTarget skips ensurePicoclawTemplate for this harness
			// precisely so a config.json and a .security.yml nothing reads are not
			// seeded beside it. Naming the template here would point the "also write
			// the template" apply at a file this agent is never provisioned from.
			Keys:             keys,
			TemplateWritable: false,
		}, nil
	}

	raw, doc, err := m.readTemplateConfig(template)
	if err != nil {
		return TemplateCatalog{}, err
	}

	keys := make([]TemplateKey, 0, len(doc))
	keys = appendTemplateLeaves(keys, doc, "", config.HarnessPicoclaw)
	sort.Slice(keys, func(i, j int) bool { return keys[i].Key < keys[j].Key })

	return TemplateCatalog{
		Template: template,
		Keys:     keys,
		// Over the bytes AS READ, so it is the same token ApplyTemplateConfigKey
		// compares the file against.
		TemplateRevision: revisionOf(raw),
		TemplateWritable: true,
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
//
// harness is stamped on every row rather than derived from the document, because
// nothing in the bytes says which runtime they describe -- the ganglion's
// generated document is deliberately picoclaw-SHAPED (ganglion_config.go, "why
// the shape is picoclaw's"), so the two are indistinguishable by inspection.
func appendTemplateLeaves(out []TemplateKey, doc map[string]any, prefix, harness string) []TemplateKey {
	for name, v := range doc {
		if !configKeySegmentRe.MatchString(name) {
			continue
		}
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		if child, ok := v.(map[string]any); ok && len(child) > 0 {
			out = appendTemplateLeaves(out, child, key, harness)
			continue
		}
		// v came out of json.Unmarshal, so it always encodes.
		enc, _ := json.Marshal(v)
		out = append(out, TemplateKey{
			Key:     key,
			Value:   enc,
			Managed: IsManagedConfigPath(key),
			Harness: harness,
		})
	}
	return out
}
