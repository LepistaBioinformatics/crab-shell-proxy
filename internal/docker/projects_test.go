package docker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/projects"
)

const testHome = "/data"

var projClock = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

func proj(id, name string) projects.Project {
	return projects.Project{ID: id, Name: name, CreatedAt: projClock}
}

// wantProjection is an INDEPENDENT statement of what config.json's agents block
// must contain for a given project list — written from the spec, not by calling
// the code under test. Comparing against it is what makes the assertions below
// checks rather than tautologies.
func wantProjection(list []projects.Project) (wantList []any, wantDispatch map[string]any) {
	if len(list) == 0 {
		return nil, nil
	}
	wantList = []any{map[string]any{"id": "main", "default": true}}
	rules := []any{}
	for _, p := range list {
		wantList = append(wantList, map[string]any{
			"id":        p.ID,
			"name":      p.Name,
			"workspace": testHome + "/.picoclaw/workspace-" + p.ID,
		})
		rules = append(rules, map[string]any{
			"name":  "proj-" + p.ID,
			"agent": p.ID,
			"when": map[string]any{
				"channel": "pico",
				"chat":    "direct:pico:p." + p.ID + ".*",
			},
		})
	}
	return wantList, map[string]any{"rules": rules}
}

// roundTrip forces the config through JSON, the way it actually reaches
// picoclaw. Without it a test can pass on Go types that marshal differently —
// and picoclaw only ever sees the marshalled form.
func roundTrip(t *testing.T, cfg map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func agentsBlock(t *testing.T, cfg map[string]any) (list any, dispatch any, defaults any) {
	t.Helper()
	agents, _ := cfg["agents"].(map[string]any)
	return agents["list"], agents["dispatch"], agents["defaults"]
}

// assertProjected is the AC-4 assertion: agents.list and agents.dispatch EQUAL
// the projection of the store.
//
// Equality, not "the rule is still there". A survival check passes for three
// distinct defects — a list merged instead of rebuilt, a stale rule for a
// deleted project, and a rule written outside the read-modify-write — and DEC-6
// exists because of exactly those.
func assertProjected(t *testing.T, cfg map[string]any, list []projects.Project) {
	t.Helper()
	got := roundTrip(t, cfg)
	gotList, gotDispatch, _ := agentsBlock(t, got)

	wantList, wantDispatch := wantProjection(list)
	if wantList == nil {
		if gotList != nil {
			t.Errorf("agents.list = %#v, want absent (FR-6a: no projects, no key)", gotList)
		}
		if gotDispatch != nil {
			t.Errorf("agents.dispatch = %#v, want absent (FR-6a)", gotDispatch)
		}
		return
	}

	wantJSON := roundTrip(t, map[string]any{"list": wantList, "dispatch": wantDispatch})
	if !reflect.DeepEqual(gotList, wantJSON["list"]) {
		t.Errorf("agents.list mismatch\n got: %s\nwant: %s", prettyJSON(t, gotList), prettyJSON(t, wantJSON["list"]))
	}
	if !reflect.DeepEqual(gotDispatch, wantJSON["dispatch"]) {
		t.Errorf("agents.dispatch mismatch\n got: %s\nwant: %s", prettyJSON(t, gotDispatch), prettyJSON(t, wantJSON["dispatch"]))
	}
}

// prettyJSON is the indented sibling of the package's existing mustJSON — these
// assertions compare whole config blocks, and a one-line diff is unreadable.
func prettyJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func baseConfig() map[string]any {
	return map[string]any{
		"version": float64(3),
		"agents": map[string]any{
			"defaults": map[string]any{
				"workspace":  testHome + "/.picoclaw/workspace",
				"model_name": "deepseek-chat",
				"provider":   "deepseek",
			},
		},
	}
}

func TestProjectAgentsNoProjects(t *testing.T) {
	cfg := baseConfig()
	projectAgents(cfg, testHome, nil)
	assertProjected(t, cfg, nil)
}

func TestProjectAgentsEmitsDefaultEntryWithProjects(t *testing.T) {
	// VER-8: a non-empty agents.list removes picoclaw's implicit main agent, so
	// the moment a project exists the default must be spelled out.
	cfg := baseConfig()
	list := []projects.Project{proj("seedtrial", "Seed Trial")}
	projectAgents(cfg, testHome, list)
	assertProjected(t, cfg, list)
}

func TestProjectAgentsSeveralProjects(t *testing.T) {
	cfg := baseConfig()
	list := []projects.Project{
		proj("seedtrial", "Seed Trial"),
		proj("soil", "Soil Analysis"),
	}
	projectAgents(cfg, testHome, list)
	assertProjected(t, cfg, list)
}

// TestProjectAgentsRebuildsRatherThanMerges is the DEC-6 regression proper. The
// config starts holding a PREVIOUS projection — one project since deleted, one
// renamed — which is exactly the state an ensure finds after a store change.
func TestProjectAgentsRebuildsRatherThanMerges(t *testing.T) {
	stale := []projects.Project{
		proj("seedtrial", "Seed Trial"),
		proj("gone", "Deleted Project"),
	}
	cfg := baseConfig()
	projectAgents(cfg, testHome, stale)

	current := []projects.Project{proj("seedtrial", "Field Trial 2026")}
	projectAgents(cfg, testHome, current)

	assertProjected(t, cfg, current)
}

func TestProjectAgentsClearsWhenLastProjectDeleted(t *testing.T) {
	cfg := baseConfig()
	projectAgents(cfg, testHome, []projects.Project{proj("seedtrial", "Seed Trial")})
	projectAgents(cfg, testHome, nil)
	assertProjected(t, cfg, nil)
}

func TestProjectAgentsIsIdempotent(t *testing.T) {
	list := []projects.Project{proj("seedtrial", "Seed Trial")}

	once := baseConfig()
	projectAgents(once, testHome, list)

	twice := baseConfig()
	projectAgents(twice, testHome, list)
	projectAgents(twice, testHome, list)

	if !reflect.DeepEqual(roundTrip(t, once), roundTrip(t, twice)) {
		t.Error("projection is not idempotent")
	}
}

// TestProjectAgentsLeavesDefaultsAlone guards FR-7c. agents.defaults is owned by
// model materialization; a projection that touched it would fight the model
// registry on every ensure.
func TestProjectAgentsLeavesDefaultsAlone(t *testing.T) {
	cfg := baseConfig()
	before := prettyJSON(t, cfg["agents"].(map[string]any)["defaults"])

	projectAgents(cfg, testHome, []projects.Project{proj("seedtrial", "Seed Trial")})

	after := prettyJSON(t, cfg["agents"].(map[string]any)["defaults"])
	if before != after {
		t.Errorf("agents.defaults changed\nbefore: %s\nafter:  %s", before, after)
	}
}

// TestProjectEntriesInheritByOmission is FR-7 and AC-3. Writing `model` would pin
// the project to a name the registry may stop resolving; writing `skills` would
// freeze it against the admin shared-skills cascade. Both inheritances work only
// because the keys are ABSENT (VER-1, VER-2).
func TestProjectEntriesInheritByOmission(t *testing.T) {
	cfg := baseConfig()
	projectAgents(cfg, testHome, []projects.Project{proj("seedtrial", "Seed Trial")})

	entries := roundTrip(t, cfg)["agents"].(map[string]any)["list"].([]any)
	for _, raw := range entries {
		entry := raw.(map[string]any)
		for _, forbidden := range []string{"model", "skills", "subagents"} {
			if _, present := entry[forbidden]; present {
				t.Errorf("entry %v carries %q, which must be inherited by omission (FR-7, NFR-4a)",
					entry["id"], forbidden)
			}
		}
	}
}

// TestProjectAgentsNeverEmitsSpawnWildcard guards NFR-4a. picoclaw reads a
// literal "*" in allow_agents as "allow every agent", which would let the main
// agent delegate into any project's workspace and defeat the boundary the
// project exists to create.
func TestProjectAgentsNeverEmitsSpawnWildcard(t *testing.T) {
	cfg := baseConfig()
	projectAgents(cfg, testHome, []projects.Project{
		proj("seedtrial", "Seed Trial"),
		proj("soil", "Soil Analysis"),
	})
	if blob := prettyJSON(t, cfg["agents"]); strings.Contains(blob, "allow_agents") {
		t.Errorf("projection emitted allow_agents:\n%s", blob)
	}
}

// TestDispatchPatternMatchesPicoclawChatShape pins the one string that has to
// agree with picoclaw byte for byte. The pico channel builds chat as
// "direct:pico:<session_id>" (pkg/channels/pico/pico.go), lowercased by
// buildDispatchView, and the proxy prefixes the session id with "p.<id>.".
// If either side drifts the rule silently never matches, and the symptom is a
// project whose chats answer as the main agent.
func TestDispatchPatternMatchesPicoclawChatShape(t *testing.T) {
	cfg := baseConfig()
	projectAgents(cfg, testHome, []projects.Project{proj("seedtrial", "Seed Trial")})

	dispatch := roundTrip(t, cfg)["agents"].(map[string]any)["dispatch"].(map[string]any)
	rule := dispatch["rules"].([]any)[0].(map[string]any)
	when := rule["when"].(map[string]any)

	if got, want := when["chat"], "direct:pico:p.seedtrial.*"; got != want {
		t.Errorf("chat = %q, want %q", got, want)
	}
	if got, want := when["channel"], "pico"; got != want {
		t.Errorf("channel = %q, want %q", got, want)
	}
	// No catch-all rule: a message matching nothing falls through to the default
	// agent, which is what makes rule ordering irrelevant (FR-7a).
	if n := len(dispatch["rules"].([]any)); n != 1 {
		t.Errorf("rule count = %d, want exactly one per project", n)
	}
}

func TestSplitFrontmatter(t *testing.T) {
	tests := []struct {
		name            string
		doc             string
		wantFrontmatter string
		wantBody        string
	}{
		{
			name:            "no frontmatter",
			doc:             "# Title\n\nbody\n",
			wantFrontmatter: "",
			wantBody:        "# Title\n\nbody\n",
		},
		{
			name:            "simple block",
			doc:             "---\nname: Alpha\n---\nbody\n",
			wantFrontmatter: "---\nname: Alpha\n---\n",
			wantBody:        "body\n",
		},
		{
			name:            "body containing a horizontal rule",
			doc:             "---\nname: Alpha\n---\nintro\n\n---\n\nmore\n",
			wantFrontmatter: "---\nname: Alpha\n---\n",
			wantBody:        "intro\n\n---\n\nmore\n",
		},
		{
			// A document that merely STARTS with a rule has no frontmatter to take.
			// Swallowing it would delete the parent's first paragraph.
			name:            "unterminated block is all body",
			doc:             "---\nname: Alpha\nno closing delimiter\n",
			wantFrontmatter: "",
			wantBody:        "---\nname: Alpha\nno closing delimiter\n",
		},
		{
			name:            "empty frontmatter",
			doc:             "---\n---\nbody\n",
			wantFrontmatter: "---\n---\n",
			wantBody:        "body\n",
		},
		{
			name:            "crlf line endings",
			doc:             "---\r\nname: Alpha\r\n---\r\nbody\r\n",
			wantFrontmatter: "---\r\nname: Alpha\r\n---\r\n",
			wantBody:        "body\r\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fm, body := splitFrontmatter(tt.doc)
			if fm != tt.wantFrontmatter {
				t.Errorf("frontmatter = %q, want %q", fm, tt.wantFrontmatter)
			}
			if body != tt.wantBody {
				t.Errorf("body = %q, want %q", body, tt.wantBody)
			}
			if fm+body != tt.doc {
				t.Errorf("split is lossy: %q + %q != %q", fm, body, tt.doc)
			}
		})
	}
}

func TestComposeProjectAgentMDKeepsParentFrontmatter(t *testing.T) {
	// The frontmatter IS the inheritance: picoclaw reads the agent's declared
	// tools, skills and model override from it.
	dir := t.TempDir()
	parent := filepath.Join(dir, "AGENT.md")
	if err := os.WriteFile(parent,
		[]byte("---\nname: Alpha\ntools: [web_search, exec]\nskills: [weather]\n---\nParent body, discarded.\n"),
		0o600); err != nil {
		t.Fatal(err)
	}

	got, err := composeProjectAgentMD(parent, projects.Project{
		ID: "seedtrial", Name: "Seed Trial", Instructions: "Always cite the protocol.",
	})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}

	// Preserved verbatim EXCEPT for the one deliberate addition: the project's own
	// mcpServers allowlist, which is what locks it to its own knowledge graph.
	for _, want := range []string{"name: Alpha", "tools: [web_search, exec]", "skills: [weather]"} {
		if !strings.Contains(got, want) {
			t.Errorf("inherited frontmatter line %q missing:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "mcpServers: [memory-seedtrial]") {
		t.Errorf("project mcp allowlist not injected:\n%s", got)
	}
	if !strings.Contains(got, "Always cite the protocol.") {
		t.Errorf("instructions missing:\n%s", got)
	}
	if strings.Contains(got, "Parent body, discarded.") {
		t.Errorf("parent body leaked into the project:\n%s", got)
	}
}

func TestComposeProjectAgentMDWithoutParent(t *testing.T) {
	// No AGENT.md anywhere in the cascade is a valid state, not an error.
	got, err := composeProjectAgentMD(filepath.Join(t.TempDir(), "absent.md"),
		projects.Project{ID: "seedtrial", Name: "Seed Trial", Instructions: "Do the thing."})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	// A parent with no frontmatter still yields one: the allowlist has to live
	// somewhere, and without it the project agent would see every graph server.
	if !strings.Contains(got, "mcpServers: [memory-seedtrial]") {
		t.Errorf("project mcp allowlist not injected:\n%s", got)
	}
	if !strings.Contains(got, "Do the thing.") {
		t.Errorf("instructions missing:\n%s", got)
	}
}

func TestComposeProjectAgentMDInstructionsContainingDelimiter(t *testing.T) {
	// A user writing "---" in their instructions must not be able to forge or
	// truncate the frontmatter block.
	dir := t.TempDir()
	parent := filepath.Join(dir, "AGENT.md")
	if err := os.WriteFile(parent, []byte("---\nname: Alpha\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := composeProjectAgentMD(parent, projects.Project{
		ID: "x", Name: "X", Instructions: "First rule.\n\n---\n\nname: Injected\n",
	})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}

	// The instructions must not be able to forge or truncate the block: whatever
	// the user wrote lands in the BODY, and the frontmatter still carries exactly
	// the parent's fields plus our allowlist.
	fm, body := splitFrontmatter(got)
	if !strings.Contains(fm, "name: Alpha") || !strings.Contains(fm, "mcpServers: [memory-x]") {
		t.Errorf("frontmatter altered by instruction content: %q", fm)
	}
	if strings.Contains(fm, "name: Injected") {
		t.Errorf("instruction content leaked into the frontmatter: %q", fm)
	}
	if !strings.Contains(body, "name: Injected") {
		t.Errorf("instruction content missing from the body: %q", body)
	}
}

func TestComposeProjectAgentMDEmptyInstructions(t *testing.T) {
	got, err := composeProjectAgentMD(filepath.Join(t.TempDir(), "absent.md"),
		projects.Project{ID: "x", Name: "Empty Project"})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if !strings.Contains(got, "Empty Project") {
		t.Errorf("project name missing:\n%s", got)
	}
	if strings.TrimSpace(got) == "# Empty Project" {
		t.Errorf("agent gets a bare heading with no explanation:\n%s", got)
	}
}

func TestSeedProjectWorkspaceIsIdempotentAndDerived(t *testing.T) {
	userDir := t.TempDir()
	effPersona := t.TempDir()
	for name, body := range map[string]string{
		"AGENT.md": "---\nname: Alpha\n---\nparent\n",
		"SOUL.md":  "soul v1\n",
		"USER.md":  "user notes\n",
	} {
		if err := os.WriteFile(filepath.Join(effPersona, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	p := projects.Project{ID: "seedtrial", Name: "Seed Trial", Instructions: "v1 instructions"}
	// user "" so the test does not need root: chownTree is a no-op then, which is
	// what keeps this out of the STATE.md L-001 sandbox-failure class.
	if err := seedProjectWorkspace(userDir, effPersona, p, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}

	root := filepath.Join(userDir, "workspace-seedtrial")
	for _, sub := range []string{"sessions", "memory", "uploads"} {
		if info, err := os.Stat(filepath.Join(root, sub)); err != nil || !info.IsDir() {
			t.Errorf("missing dir %s: %v", sub, err)
		}
	}
	for _, name := range []string{"SOUL.md", "USER.md", "AGENT.md"} {
		if !fileExists(filepath.Join(root, name)) {
			t.Errorf("missing %s", name)
		}
	}
	// HEARTBEAT.md had no source; absent is correct, not an empty file.
	if fileExists(filepath.Join(root, "HEARTBEAT.md")) {
		t.Error("HEARTBEAT.md invented from nothing")
	}

	// A second ensure with changed inputs must converge, not accumulate.
	if err := os.WriteFile(filepath.Join(effPersona, "SOUL.md"), []byte("soul v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Instructions = "v2 instructions"
	if err := seedProjectWorkspace(userDir, effPersona, p, ""); err != nil {
		t.Fatalf("reseed: %v", err)
	}

	soul, _ := os.ReadFile(filepath.Join(root, "SOUL.md"))
	if string(soul) != "soul v2\n" {
		t.Errorf("persona copy not refreshed: %q", soul)
	}
	agentMD, _ := os.ReadFile(filepath.Join(root, "AGENT.md"))
	if !strings.Contains(string(agentMD), "v2 instructions") {
		t.Errorf("AGENT.md not recomposed: %q", agentMD)
	}
	if strings.Contains(string(agentMD), "v1 instructions") {
		t.Errorf("stale instructions survived: %q", agentMD)
	}
}

func TestRemoveProjectWorkspace(t *testing.T) {
	userDir := t.TempDir()
	p := projects.Project{ID: "seedtrial", Name: "Seed Trial"}
	if err := seedProjectWorkspace(userDir, t.TempDir(), p, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := removeProjectWorkspace(userDir, p.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(userDir, "workspace-seedtrial")); !os.IsNotExist(err) {
		t.Errorf("workspace survived deletion: %v", err)
	}
	// Removing what is already gone is not an error — delete must be retryable.
	if err := removeProjectWorkspace(userDir, p.ID); err != nil {
		t.Errorf("second remove: %v", err)
	}
}

const testMountDest = "/data/.picoclaw"

func TestProjectSecretsBinds(t *testing.T) {
	const effHost = "/host/effective-secrets/u1/alpha"
	got := projectSecretsBinds(effHost, testMountDest, []projects.Project{
		proj("seedtrial", "Seed Trial"),
		proj("soil", "Soil Analysis"),
	})
	want := []string{
		effHost + ":/data/.picoclaw/workspace-seedtrial/.secrets:ro",
		effHost + ":/data/.picoclaw/workspace-soil/.secrets:ro",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("binds =\n%v\nwant\n%v", got, want)
	}

	// All three point at the SAME source: a project inherits the parent's
	// credentials, so there is nothing per-project to sync or to leak between
	// projects.
	for _, b := range got {
		if !strings.HasPrefix(b, effHost+":") {
			t.Errorf("bind %q does not read the shared effective secrets dir", b)
		}
		if !strings.HasSuffix(b, ":ro") {
			t.Errorf("bind %q is not read-only", b)
		}
	}
}

func TestProjectSecretsBindsEmptyWithoutProjects(t *testing.T) {
	if got := projectSecretsBinds("/host/eff", testMountDest, nil); len(got) != 0 {
		t.Errorf("binds = %v, want none", got)
	}
}

func TestProjectBindDrift(t *testing.T) {
	bind := func(id string) string {
		return "/host/eff:" + testMountDest + "/workspace-" + id + "/.secrets:ro"
	}
	// Binds a real container always carries, none of which are project mounts.
	base := []string{
		"/host/user:" + testMountDest,
		"/host/eff:" + testMountDest + "/workspace/.secrets:ro",
		"/host/skills:" + testMountDest + "/skills:ro",
		"/host/persona/AGENT.md:" + testMountDest + "/workspace/AGENT.md:ro",
	}

	tests := []struct {
		name   string
		list   []projects.Project
		actual []string
		want   bool
	}{
		{"no projects, no project binds", nil, base, false},
		{
			"project created after the container was built",
			[]projects.Project{proj("seedtrial", "Seed Trial")},
			base,
			true,
		},
		{
			"matching set",
			[]projects.Project{proj("seedtrial", "Seed Trial")},
			append(append([]string{}, base...), bind("seedtrial")),
			false,
		},
		{
			"project deleted but its bind survives",
			nil,
			append(append([]string{}, base...), bind("seedtrial")),
			true,
		},
		{
			"one of two projects missing its bind",
			[]projects.Project{proj("seedtrial", "Seed Trial"), proj("soil", "Soil")},
			append(append([]string{}, base...), bind("seedtrial")),
			true,
		},
		{
			"same count, different projects",
			[]projects.Project{proj("soil", "Soil")},
			append(append([]string{}, base...), bind("seedtrial")),
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := projectBindDrift(testMountDest, tt.list, tt.actual); got != tt.want {
				t.Errorf("projectBindDrift = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestProjectBindDriftIgnoresHostPath is the fleet-safety property. The bind
// string embeds HostDataRoot; comparing whole strings would read an operator
// moving the data root as drift on every workspace at once and recreate the
// entire fleet, truncating every live conversation.
func TestProjectBindDriftIgnoresHostPath(t *testing.T) {
	list := []projects.Project{proj("seedtrial", "Seed Trial")}
	moved := []string{"/some/other/host/root:" + testMountDest + "/workspace-seedtrial/.secrets:ro"}
	if projectBindDrift(testMountDest, list, moved) {
		t.Error("a changed host path was read as drift")
	}
}

// TestProjectBindDriftIgnoresMainWorkspace guards the prefix. "workspace-" must
// not be confused with "workspace", or the main .secrets mount would be counted
// as a project's and every container would look drifted.
func TestProjectBindDriftIgnoresMainWorkspace(t *testing.T) {
	actual := []string{"/host/eff:" + testMountDest + "/workspace/.secrets:ro"}
	if projectBindDrift(testMountDest, nil, actual) {
		t.Error("the main workspace .secrets mount was counted as a project bind")
	}
}

// TestSeedProjectWorkspaceKeepsAgentWrittenUserFile is the regression for a real
// defect: USER.md was in the refreshed set, so every ensure overwrote it.
//
// USER.md is the one identity file the main workspace leaves writable — it is
// excluded from PersonaMounted precisely because the agent accumulates what it
// learns about the user there, and provision.go seeds it once so a returning
// user's evolved file is never clobbered. A project agent has to get the same
// treatment, or everything it learns is erased on the next message.
func TestSeedProjectWorkspaceKeepsAgentWrittenUserFile(t *testing.T) {
	userDir := t.TempDir()
	effPersona := t.TempDir()
	for name, body := range map[string]string{
		"USER.md": "seeded starting point\n",
		"SOUL.md": "soul v1\n",
	} {
		if err := os.WriteFile(filepath.Join(effPersona, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	p := projects.Project{ID: "seedtrial", Name: "Seed Trial"}
	if err := seedProjectWorkspace(userDir, effPersona, p, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	root := filepath.Join(userDir, "workspace-seedtrial")

	// First seed takes the cascade's copy.
	if got, _ := os.ReadFile(filepath.Join(root, "USER.md")); string(got) != "seeded starting point\n" {
		t.Fatalf("USER.md = %q, want the seeded content", got)
	}

	// The agent then writes what it learned.
	learned := "Prefers metric units. Works the north plots.\n"
	if err := os.WriteFile(filepath.Join(root, "USER.md"), []byte(learned), 0o600); err != nil {
		t.Fatal(err)
	}

	// A later ensure must NOT undo that — even though the cascade's own copy changed.
	if err := os.WriteFile(filepath.Join(effPersona, "USER.md"), []byte("different starting point\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := seedProjectWorkspace(userDir, effPersona, p, ""); err != nil {
		t.Fatalf("reseed: %v", err)
	}

	if got, _ := os.ReadFile(filepath.Join(root, "USER.md")); string(got) != learned {
		t.Errorf("USER.md = %q, want what the agent wrote (%q)", got, learned)
	}
	// SOUL.md keeps refreshing: it mirrors a read-only bind in the main workspace,
	// so an admin change to it must still reach the project.
	if got, _ := os.ReadFile(filepath.Join(root, "SOUL.md")); string(got) != "soul v1\n" {
		t.Errorf("SOUL.md = %q", got)
	}
}

// TestWithProjectMCPAllowlistReplacesAnInheritedOne is the leak this function
// exists to close. A parent that already restricts its MCP servers would
// otherwise hand the project a list naming those servers PLUS its own graph —
// and the project would read the parent's memory through them.
func TestWithProjectMCPAllowlistReplacesAnInheritedOne(t *testing.T) {
	tests := []struct {
		name        string
		frontmatter string
		wantHas     []string
		wantLacks   []string
	}{
		{
			name:        "no frontmatter at all",
			frontmatter: "",
			wantHas:     []string{"---", "mcpServers: [memory-p1]"},
		},
		{
			name:        "frontmatter without the field",
			frontmatter: "---\nname: Alpha\n---\n",
			wantHas:     []string{"name: Alpha", "mcpServers: [memory-p1]"},
		},
		{
			name:        "parent already restricts its servers",
			frontmatter: "---\nname: Alpha\nmcpServers: [memory, github]\n---\n",
			wantHas:     []string{"name: Alpha", "mcpServers: [memory-p1]"},
			// The parent's own graph server must NOT survive into the project.
			wantLacks: []string{"[memory, github]"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withProjectMCPAllowlist(tt.frontmatter, "p1")
			for _, want := range tt.wantHas {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in:\n%s", want, got)
				}
			}
			for _, bad := range tt.wantLacks {
				if strings.Contains(got, bad) {
					t.Errorf("still contains %q in:\n%s", bad, got)
				}
			}
			// Whatever we produce has to parse back as a frontmatter block, or
			// picoclaw reads the whole thing as body and the allowlist does nothing.
			if fm, _ := splitFrontmatter(got + "body\n"); fm == "" {
				t.Errorf("result is not a parseable frontmatter block:\n%s", got)
			}
		})
	}
}
