package docker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// resolutionFor is a minimal resolution, enough for the renderer.
func resolutionFor(provider string) registry.Resolution {
	return registry.Resolution{Primary: registry.Model{
		ModelName: "primary", Provider: provider, Model: "m",
		APIBase: "https://example.invalid/v1",
	}}
}

func overlayManager(t *testing.T, harness string) (*Manager, WorkspaceKey, string) {
	t.Helper()
	root := t.TempDir()
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	userDir := config.UserWorkspace(root, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &Manager{
		cfg: &config.Config{
			ContainerDataRoot: root,
			Agents:            map[string]config.Agent{"alpha": {Key: "alpha", Harness: harness}},
		},
		logf: func(string, ...any) {},
	}, key, userDir
}

func readGanglionOverlay(t *testing.T, userDir string) map[string]json.RawMessage {
	t.Helper()
	got, err := readConfigOverlay(ganglionOverlayPath(userDir))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The editors were writing picoclaw's file name unconditionally, so an admin
// repairing or bulk-editing a ganglion instance edited a file that agent never
// opens -- and was told it worked.
func TestTheConfigPathFollowsTheHarness(t *testing.T) {
	for harness, want := range map[string]string{
		config.HarnessGanglion: ganglionConfigFile,
		config.HarnessPicoclaw: "config.json",
		"":                     "config.json",
	} {
		m, key, _ := overlayManager(t, harness)
		if got := filepath.Base(m.instanceConfigPath(key)); got != want {
			t.Errorf("harness %q: config file = %q, want %q", harness, got, want)
		}
	}
	// An agent removed from the config still resolves, to the older shape.
	m, key, _ := overlayManager(t, config.HarnessGanglion)
	m.cfg.Agents = map[string]config.Agent{}
	if got := filepath.Base(m.instanceConfigPath(key)); got != "config.json" {
		t.Errorf("unknown agent resolved to %q", got)
	}
}

// THE REASON THE OVERLAY EXISTS. The ganglion's config is rendered whole on every
// ensure, so an edit written into the file is gone on the next turn -- which for a
// scale-to-zero agent is immediately, and reads to the admin as the proxy
// reverting them.
func TestAnOverlaySurvivesARender(t *testing.T) {
	_, _, userDir := overlayManager(t, config.HarnessGanglion)
	path := ganglionOverlayPath(userDir)
	if err := upsertConfigOverlay(path, "agents.defaults.subturn.max_depth",
		json.RawMessage("3")); err != nil {
		t.Fatal(err)
	}

	// A freshly rendered document, as ganglionConfigDoc produces one.
	rendered := []byte(`{"model_list":[],"agents":{"defaults":{"model_name":"m"}}}`)
	merged, applied, err := applyOverlayToDoc(rendered, path)
	if err != nil {
		t.Fatalf("applyOverlayToDoc: %v", err)
	}
	if applied != 1 {
		t.Fatalf("applied = %d, want 1", applied)
	}
	var doc map[string]any
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatal(err)
	}
	sub := doc["agents"].(map[string]any)["defaults"].(map[string]any)["subturn"].(map[string]any)
	if sub["max_depth"] != float64(3) {
		t.Fatalf("max_depth = %v", sub["max_depth"])
	}
	// And what the renderer produced is still there: an overlay adds, it does not
	// replace the document.
	if doc["agents"].(map[string]any)["defaults"].(map[string]any)["model_name"] != "m" {
		t.Error("the overlay overwrote what the renderer produced")
	}
}

// An overlay must not become a way around ManagedConfigPaths: those keys are
// rewritten by the next materialization anyway, so storing one would be a promise
// the renderer breaks.
func TestAnOverlayCannotReachAManagedKey(t *testing.T) {
	_, _, userDir := overlayManager(t, config.HarnessGanglion)
	path := ganglionOverlayPath(userDir)
	if err := upsertConfigOverlay(path, "model_list", json.RawMessage("[]")); err == nil {
		t.Fatal("a managed key was accepted into the overlay")
	}
	// And even one written by hand is skipped on the way out.
	raw, _ := json.Marshal(map[string]json.RawMessage{"model_list": json.RawMessage(`["x"]`)})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	rendered := []byte(`{"model_list":[{"model_name":"real"}]}`)
	merged, applied, err := applyOverlayToDoc(rendered, path)
	if err != nil {
		t.Fatal(err)
	}
	if applied != 0 || string(merged) != string(rendered) {
		t.Fatalf("a managed key was applied: %s", merged)
	}
}

// An absent overlay is the ordinary state of almost every workspace, and must
// leave the rendered document byte-identical -- writeGanglionConfig compares it
// against the file to decide whether anything changed.
func TestNoOverlayLeavesTheDocumentAlone(t *testing.T) {
	_, _, userDir := overlayManager(t, config.HarnessGanglion)
	rendered := []byte(`{"model_list":[]}`)
	merged, applied, err := applyOverlayToDoc(rendered, ganglionOverlayPath(userDir))
	if err != nil || applied != 0 || string(merged) != string(rendered) {
		t.Fatalf("merged=%s applied=%d err=%v", merged, applied, err)
	}
}

// A whole-document edit becomes overlay entries: the instance editor submits a
// document, not a key, and for a ganglion workspace what the admin changed has to
// be recovered from it.
func TestAWriteRecordsWhatItChanged(t *testing.T) {
	m, key, userDir := overlayManager(t, config.HarnessGanglion)
	current := map[string]any{
		"agents": map[string]any{"defaults": map[string]any{
			"model_name": "m",
			"subturn":    map[string]any{"max_depth": float64(1)},
		}},
	}
	next := []byte(`{"agents":{"defaults":{"model_name":"m","subturn":{"max_depth":4,"max_concurrent":9}}}}`)

	if err := m.recordGanglionOverlay(key, current, next); err != nil {
		t.Fatalf("recordGanglionOverlay: %v", err)
	}
	got := readGanglionOverlay(t, userDir)
	if string(got["agents.defaults.subturn.max_depth"]) != "4" {
		t.Errorf("max_depth = %s", got["agents.defaults.subturn.max_depth"])
	}
	if string(got["agents.defaults.subturn.max_concurrent"]) != "9" {
		t.Errorf("max_concurrent = %s", got["agents.defaults.subturn.max_concurrent"])
	}
	// What did NOT change is not recorded: an overlay of everything would pin the
	// renderer's own output and stop it ever changing.
	if _, pinned := got["agents.defaults.model_name"]; pinned {
		t.Error("an unchanged value was pinned into the overlay")
	}
}

// A removal cannot be expressed. An overlay says what a value should be; inventing
// a way to say "and this should not exist" would let it delete what the renderer
// produces.
func TestARemovalIsNotRecorded(t *testing.T) {
	m, key, userDir := overlayManager(t, config.HarnessGanglion)
	current := map[string]any{"tools": map[string]any{"web": map[string]any{"provider": "brave"}}}
	next := []byte(`{"tools":{"web":{}}}`)

	if err := m.recordGanglionOverlay(key, current, next); err != nil {
		t.Fatal(err)
	}
	if got := readGanglionOverlay(t, userDir); len(got) != 0 {
		t.Fatalf("a removal was recorded: %v", got)
	}
}

func TestDiffReportsLeavesAndTreatsAnArrayAsOne(t *testing.T) {
	prev := map[string]any{
		"a":    map[string]any{"b": float64(1), "c": "same"},
		"list": []any{float64(1), float64(2)},
	}
	next := map[string]any{
		"a":    map[string]any{"b": float64(2), "c": "same"},
		"list": []any{float64(2), float64(1)},
		"new":  map[string]any{"deep": true},
	}
	got := diffConfigPaths(prev, next)

	if string(got["a.b"]) != "2" {
		t.Errorf("a.b = %s", got["a.b"])
	}
	if _, same := got["a.c"]; same {
		t.Error("an unchanged leaf was reported")
	}
	// A reordered array is ONE change: setPath writes whole values, so an array is
	// a value.
	if string(got["list"]) != "[2,1]" {
		t.Errorf("list = %s", got["list"])
	}
	// A new branch is recorded leaf by leaf, so a sibling the renderer adds later
	// is not overwritten by a stored parent.
	if string(got["new.deep"]) != "true" {
		t.Errorf("new.deep = %s", got["new.deep"])
	}
	if _, whole := got["new"]; whole {
		t.Error("a new branch was stored whole rather than by leaf")
	}
}

// A picoclaw workspace records nothing: its file is edited in place and the write
// is the whole story.
func TestAPicoclawWriteRecordsNoOverlay(t *testing.T) {
	m, key, userDir := overlayManager(t, config.HarnessPicoclaw)
	if m.harnessConfigFile(key) == ganglionConfigFile {
		t.Fatal("a picoclaw workspace resolved the ganglion file")
	}
	if _, err := os.Stat(ganglionOverlayPath(userDir)); !os.IsNotExist(err) {
		t.Error("a picoclaw workspace has an overlay file")
	}
}

// The WIRING, not just the merge. Nothing asserted that the render path actually
// applies the overlay, so removing that one line broke nothing -- which is the
// shape of a defect that ships.
func TestTheRenderedDocumentCarriesTheOverlay(t *testing.T) {
	m, _, userDir := overlayManager(t, config.HarnessGanglion)
	if err := upsertConfigOverlay(ganglionOverlayPath(userDir),
		"agents.defaults.subturn.max_depth", json.RawMessage("7")); err != nil {
		t.Fatal(err)
	}

	doc, err := m.renderGanglionConfig(userDir, resolutionFor("deepseek"), nil, "", "alpha")
	if err != nil {
		t.Fatalf("renderGanglionConfig: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(doc, &parsed); err != nil {
		t.Fatal(err)
	}
	defaults := parsed["agents"].(map[string]any)["defaults"].(map[string]any)
	sub, ok := defaults["subturn"].(map[string]any)
	if !ok || sub["max_depth"] != float64(7) {
		t.Fatalf("the rendered document does not carry the overlay: %s", doc)
	}
	// And the renderer's own answer survives beside it.
	if defaults["model_name"] != "primary" {
		t.Errorf("the overlay displaced the rendered model: %v", defaults["model_name"])
	}
}

// With no overlay the render is exactly what ganglionConfigDoc produces, which is
// what writeGanglionConfig compares against the file to decide whether anything
// changed. A render that differed would make every ensure look like a change.
func TestTheRenderIsUnchangedWithoutAnOverlay(t *testing.T) {
	m, _, userDir := overlayManager(t, config.HarnessGanglion)
	want, err := ganglionConfigDoc(resolutionFor("deepseek"), nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.renderGanglionConfig(userDir, resolutionFor("deepseek"), nil, "", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("the render differs with no overlay:\n%s\n---\n%s", want, got)
	}
}

// The render must be byte-stable WITH an overlay, not only without one.
//
// This is the failure a single render cannot show. writeGanglionConfig decides
// whether anything changed by comparing the rendered bytes against the file, and a
// changed file recreates the container -- so a merge that produced the same
// document with different bytes on each pass would recreate the container on every
// ensure, which for a scale-to-zero agent is every turn. The agent would appear to
// restart constantly with nothing in the logs naming a cause.
//
// It holds today because applyOverlayToDoc re-encodes through a map and
// encoding/json sorts map keys, which is determinism by a property of the encoder
// rather than by intent. This pins it: an ordered encoder, a merge that appends,
// or a switch to json.Encoder with SetEscapeHTML would each break it silently.
//
// agents.defaults.max_tool_iterations is the worked example on purpose -- it is the
// first key this whole path exists for (crab-ganglion-harness reads it as the
// per-turn tool budget) and it sits UNDER a branch the renderer also writes, which
// is the case where a merge is most likely to reorder something.
func TestTheRenderIsByteStableWithAnOverlay(t *testing.T) {
	m, _, userDir := overlayManager(t, config.HarnessGanglion)
	path := ganglionOverlayPath(userDir)
	// Two entries, and one of them nests a branch the renderer does not produce, so
	// the merge has to both descend into agents.defaults and create a subtree.
	for key, value := range map[string]json.RawMessage{
		"agents.defaults.max_tool_iterations": json.RawMessage("40"),
		"agents.defaults.subturn.max_depth":   json.RawMessage("3"),
	} {
		if err := upsertConfigOverlay(path, key, value); err != nil {
			t.Fatal(err)
		}
	}

	first, err := m.renderGanglionConfig(userDir, resolutionFor("deepseek"), nil, "", "alpha")
	if err != nil {
		t.Fatalf("renderGanglionConfig: %v", err)
	}
	// The overlay really did land, so a stable render of an UNMERGED document
	// cannot pass this test by doing nothing.
	var parsed map[string]any
	if err := json.Unmarshal(first, &parsed); err != nil {
		t.Fatal(err)
	}
	defaults := parsed["agents"].(map[string]any)["defaults"].(map[string]any)
	if defaults["max_tool_iterations"] != float64(40) {
		t.Fatalf("the overlay did not reach the rendered document: %s", first)
	}

	// Ten passes rather than two: an instability that depends on map iteration
	// order shows up probabilistically, and a single repeat would pass most runs.
	for i := 0; i < 10; i++ {
		again, err := m.renderGanglionConfig(userDir, resolutionFor("deepseek"), nil, "", "alpha")
		if err != nil {
			t.Fatalf("renderGanglionConfig on pass %d: %v", i+2, err)
		}
		if string(again) != string(first) {
			t.Fatalf("the render is not byte-stable on pass %d: every ensure would recreate the container\n%s\n---\n%s",
				i+2, first, again)
		}
	}
}
