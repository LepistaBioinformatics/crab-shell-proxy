package docker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

func writeOverlayFile(t *testing.T, dir string, entries map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := map[string]json.RawMessage{}
	for k, v := range entries {
		raw[k] = json.RawMessage(v)
	}
	body, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, configOverlayFile)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeSeedConfig(t *testing.T, dir string, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readConfigDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parseConfigObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// The overlay is what makes "future members of THIS subscription" expressible. The
// agent template is shared by every subscription on it, so before this the only way
// to reach new members was agent-wide.
func TestApplyConfigOverlaySetsScopedKeysOnASeededConfig(t *testing.T) {
	root := t.TempDir()
	cfgPath := writeSeedConfig(t, filepath.Join(root, "user"),
		`{"evolution":{"min_task_count":2,"mode":"apply"},"heartbeat":{"interval":30}}`)
	overlay := writeOverlayFile(t, filepath.Join(root, "scope"), map[string]string{
		"evolution.min_task_count": "25",
		"tools.web.brave.enabled":  "true",
	})

	applied, err := applyConfigOverlay(cfgPath, overlay)
	if err != nil {
		t.Fatalf("applyConfigOverlay: %v", err)
	}
	if applied != 2 {
		t.Errorf("applied = %d, want 2", applied)
	}

	doc := readConfigDoc(t, cfgPath)
	evo := doc["evolution"].(map[string]any)
	if evo["min_task_count"] != float64(25) {
		t.Errorf("min_task_count = %#v, want 25", evo["min_task_count"])
	}
	// A sibling inside the same subtree survives: the overlay sets LEAVES, so
	// scoping one value must not replace the object holding it.
	if evo["mode"] != "apply" {
		t.Errorf("evolution.mode = %#v, want it untouched", evo["mode"])
	}
	if doc["heartbeat"].(map[string]any)["interval"] != float64(30) {
		t.Error("an unrelated key was disturbed")
	}
	// A branch the seed lacks is created, which is the brave case.
	brave := doc["tools"].(map[string]any)["web"].(map[string]any)["brave"].(map[string]any)
	if brave["enabled"] != true {
		t.Errorf("tools.web.brave.enabled = %#v, want true", brave["enabled"])
	}
}

func TestApplyConfigOverlayIsANoOpWithoutAnOverlayFile(t *testing.T) {
	root := t.TempDir()
	cfgPath := writeSeedConfig(t, filepath.Join(root, "user"), `{"version":3}`)
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	// Every provision of every workspace runs this, and almost none has an overlay.
	// The absent case must not rewrite the file: a needless re-marshal would churn
	// the seed's formatting for nothing.
	applied, err := applyConfigOverlay(cfgPath, filepath.Join(root, "scope", configOverlayFile))
	if err != nil {
		t.Fatalf("absent overlay must not error: %v", err)
	}
	if applied != 0 {
		t.Errorf("applied = %d, want 0", applied)
	}
	after, _ := os.ReadFile(cfgPath)
	if string(after) != string(before) {
		t.Error("the config was rewritten although no overlay existed")
	}
}

// A managed key in the overlay would be reverted by the very next materialization,
// so storing one is meaningless — and letting it through here would make the
// overlay look like a way around ManagedConfigPaths.
func TestApplyConfigOverlaySkipsAManagedKey(t *testing.T) {
	root := t.TempDir()
	cfgPath := writeSeedConfig(t, filepath.Join(root, "user"),
		`{"model_list":[],"agents":{"defaults":{"provider":"seeded"}}}`)
	overlay := writeOverlayFile(t, filepath.Join(root, "scope"), map[string]string{
		"agents.defaults.provider": `"hijacked"`,
		"heartbeat.interval":       "60",
	})

	applied, err := applyConfigOverlay(cfgPath, overlay)
	if err != nil {
		t.Fatalf("applyConfigOverlay: %v", err)
	}
	if applied != 1 {
		t.Errorf("applied = %d, want 1 (the managed key skipped)", applied)
	}
	doc := readConfigDoc(t, cfgPath)
	if got := doc["agents"].(map[string]any)["defaults"].(map[string]any)["provider"]; got != "seeded" {
		t.Errorf("provider = %#v, want the seeded value — a managed path is not overlayable", got)
	}
	if doc["heartbeat"].(map[string]any)["interval"] != float64(60) {
		t.Error("the free key in the same overlay was dropped too")
	}
}

func TestApplyConfigOverlaySkipsAKeyItCannotSet(t *testing.T) {
	root := t.TempDir()
	// tools.web holds a string, so tools.web.brave.enabled cannot be reached.
	cfgPath := writeSeedConfig(t, filepath.Join(root, "user"),
		`{"tools":{"web":"not-an-object"},"heartbeat":{"interval":30}}`)
	overlay := writeOverlayFile(t, filepath.Join(root, "scope"), map[string]string{
		"tools.web.brave.enabled": "true",
		"heartbeat.interval":      "60",
	})

	// One unusable entry must not cost the whole overlay: this runs during
	// provisioning, and failing the seed over one bad key would leave the member
	// with no workspace at all.
	applied, err := applyConfigOverlay(cfgPath, overlay)
	if err != nil {
		t.Fatalf("a conflicting entry must not fail the seed: %v", err)
	}
	if applied != 1 {
		t.Errorf("applied = %d, want 1", applied)
	}
	if readConfigDoc(t, cfgPath)["heartbeat"].(map[string]any)["interval"] != float64(60) {
		t.Error("the usable entry was not applied")
	}
}

// --- the write path ---

func TestUpsertConfigOverlayAddsAndReplacesOneKeyAtATime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configOverlayFile)

	if err := upsertConfigOverlay(path, "evolution.min_task_count", json.RawMessage("25")); err != nil {
		t.Fatal(err)
	}
	if err := upsertConfigOverlay(path, "heartbeat.interval", json.RawMessage("60")); err != nil {
		t.Fatal(err)
	}
	// Per-key upsert, deliberately not a whole-file replace: two admins setting
	// DIFFERENT keys must both land, which is also why there is no revision token
	// on this path.
	if err := upsertConfigOverlay(path, "evolution.min_task_count", json.RawMessage("40")); err != nil {
		t.Fatal(err)
	}

	got, err := readConfigOverlay(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("overlay holds %d keys, want 2: %v", len(got), got)
	}
	if string(got["evolution.min_task_count"]) != "40" {
		t.Errorf("min_task_count = %s, want the replaced 40", got["evolution.min_task_count"])
	}
	if string(got["heartbeat.interval"]) != "60" {
		t.Errorf("interval = %s, want 60 — a sibling entry was lost", got["heartbeat.interval"])
	}
}

func TestUpsertConfigOverlayRefusesAManagedOrMalformedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), configOverlayFile)

	if err := upsertConfigOverlay(path, "model_list", json.RawMessage("[]")); !errors.Is(err, ErrManagedConfigPath) {
		t.Errorf("managed key = %v, want ErrManagedConfigPath", err)
	}
	if err := upsertConfigOverlay(path, "a..b", json.RawMessage("1")); !errors.Is(err, ErrInvalidConfigKey) {
		t.Errorf("malformed key = %v, want ErrInvalidConfigKey", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a refused upsert created the file anyway")
	}
}

func TestReadConfigOverlayTreatsAnAbsentFileAsEmpty(t *testing.T) {
	got, err := readConfigOverlay(filepath.Join(t.TempDir(), configOverlayFile))
	if err != nil {
		t.Fatalf("absent overlay: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

// --- the seed semantics, end to end through provision ---

func overlayTemplate(t *testing.T) string {
	t.Helper()
	tmpl := t.TempDir()
	mustWrite(t, filepath.Join(tmpl, "config.json"),
		`{"agents":{"defaults":{}},"evolution":{"min_task_count":2}}`)
	mustWrite(t, filepath.Join(tmpl, ".security.yml"),
		"channel_list:\n  pico:\n    settings:\n      token: tok\n")
	return tmpl
}

// The whole point: a member created AFTER the admin scoped the key inherits it,
// without the agent template — which every subscription on that agent shares —
// having to be touched.
func TestProvisionAppliesTheSubscriptionOverlayToANewMember(t *testing.T) {
	tmpl := overlayTemplate(t)
	overlay := writeOverlayFile(t, filepath.Join(t.TempDir(), "scope"), map[string]string{
		"evolution.min_task_count": "25",
	})
	userDir := filepath.Join(t.TempDir(), "u")

	// user "" so chownTree is a no-op (this test does not run as root).
	if _, err := provision(userDir, tmpl, "", overlay, "/data", "", WorkspaceKey{}, "e@x"); err != nil {
		t.Fatalf("provision: %v", err)
	}

	doc := readConfigDoc(t, filepath.Join(userDir, "config.json"))
	if got := doc["evolution"].(map[string]any)["min_task_count"]; got != float64(25) {
		t.Errorf("min_task_count = %#v, want the scoped 25", got)
	}
	// The template itself is untouched — that is the difference from writing it.
	if got := readConfigDoc(t, filepath.Join(tmpl, "config.json"))["evolution"].(map[string]any)["min_task_count"]; got != float64(2) {
		t.Errorf("template min_task_count = %#v, want 2: the overlay must not write the template", got)
	}
}

// A SEED, not a policy. An existing workspace is never revisited, so an admin who
// tuned one instance by hand keeps that tuning and a scoped default never silently
// reverts it. This is the assertion that separates this design from the
// native-secret and persona overlays, which DO re-apply on every ensure.
func TestProvisionLeavesAReturningMemberAlone(t *testing.T) {
	tmpl := overlayTemplate(t)
	overlay := writeOverlayFile(t, filepath.Join(t.TempDir(), "scope"), map[string]string{
		"evolution.min_task_count": "25",
	})
	userDir := filepath.Join(t.TempDir(), "u")

	// First provision: the member exists and has its own hand-tuned value.
	if _, err := provision(userDir, tmpl, "", "", "/data", "", WorkspaceKey{}, "e@x"); err != nil {
		t.Fatalf("first provision: %v", err)
	}
	cfgPath := filepath.Join(userDir, "config.json")
	doc := readConfigDoc(t, cfgPath)
	if err := setPath(doc, "evolution.min_task_count", float64(7)); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	// Second provision, now WITH an overlay: a returning user must be left as-is.
	if _, err := provision(userDir, tmpl, "", overlay, "/data", "", WorkspaceKey{}, "e@x"); err != nil {
		t.Fatalf("second provision: %v", err)
	}
	if got := readConfigDoc(t, cfgPath)["evolution"].(map[string]any)["min_task_count"]; got != float64(7) {
		t.Errorf("min_task_count = %#v, want the member's own 7 — the overlay is a seed, not a policy", got)
	}
}

// The record this path writes carries the tenant AND the subscription, which is the
// difference the whole feature exists for: a template record legitimately cannot
// name a subscription, so "which subscription does this apply to" was unanswerable.
func TestApplyOverlayConfigKeyRecordsAFullyScopedMigration(t *testing.T) {
	root := t.TempDir()
	m := &Manager{cfg: &config.Config{ContainerDataRoot: root}, logf: func(string, ...any) {}}
	scope := Scope{
		Kind: ScopeSubscription, TenantID: "t-1", SubsAccID: "s-9", AgentKey: "alpha",
	}
	at := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)

	res, err := m.ApplyOverlayConfigKey(scope, "evolution.min_task_count", json.RawMessage("25"), "admin@x", at)
	if err != nil {
		t.Fatalf("ApplyOverlayConfigKey: %v", err)
	}
	if !res.OK || res.Migration == "" {
		t.Fatalf("result = %+v, want ok with a record name", res)
	}

	overlayPath := config.SubscriptionAgentConfigOverlay(root, "t-1", "s-9", "alpha")
	stored, err := readConfigOverlay(overlayPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored["evolution.min_task_count"]) != "25" {
		t.Errorf("overlay = %v, want the key stored", stored)
	}

	raw, err := os.ReadFile(filepath.Join(filepath.Dir(overlayPath), ".config-migrations", res.Migration))
	if err != nil {
		t.Fatalf("record not beside the overlay: %v", err)
	}
	var rec ConfigMigration
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Scope.TenantID != "t-1" || rec.Scope.SubsAccID != "s-9" || rec.Scope.Agent != "alpha" {
		t.Errorf("scope = %+v, want all three named", rec.Scope)
	}
	if !rec.FromAbsent {
		t.Error("a first-time scoped key must record fromAbsent")
	}
}

func TestApplyOverlayConfigKeyRecordsThePriorOverlayEntry(t *testing.T) {
	root := t.TempDir()
	m := &Manager{cfg: &config.Config{ContainerDataRoot: root}, logf: func(string, ...any) {}}
	scope := Scope{Kind: ScopeSubscription, TenantID: "t-1", SubsAccID: "s-9", AgentKey: "alpha"}
	at := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)

	if _, err := m.ApplyOverlayConfigKey(scope, "heartbeat.interval", json.RawMessage("30"), "a@x", at); err != nil {
		t.Fatal(err)
	}
	res, err := m.ApplyOverlayConfigKey(scope, "heartbeat.interval", json.RawMessage("60"), "a@x",
		at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	overlayPath := config.SubscriptionAgentConfigOverlay(root, "t-1", "s-9", "alpha")
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(overlayPath), ".config-migrations", res.Migration))
	if err != nil {
		t.Fatal(err)
	}
	var rec ConfigMigration
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	// The overlay's prior entry, not the template's value: this record describes a
	// change to the OVERLAY, and a revert puts the overlay back.
	if string(rec.From) != "30" || rec.FromAbsent {
		t.Errorf("from = %s fromAbsent = %v, want the prior overlay entry 30", rec.From, rec.FromAbsent)
	}
}
