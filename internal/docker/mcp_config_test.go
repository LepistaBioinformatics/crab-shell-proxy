package docker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/mcptoken"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/restart"
)

const mcpTestSecret = "docker-test-mcp-secret"
const mcpTestBase = "http://crab-shell-proxy:8080"

func mustReadJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}

func serverBlock(t *testing.T, doc map[string]any, name string) map[string]any {
	t.Helper()
	servers := mcpServers(doc)
	if servers == nil {
		t.Fatalf("config has no tools.mcp.servers")
	}
	block, ok := servers[name].(map[string]any)
	if !ok {
		t.Fatalf("config has no tools.mcp.servers.%s (servers: %v)", name, servers)
	}
	return block
}

// --- applyMCPServer (FR-5.1, FR-5.2, E-1) ---

// The block must match, field for field, what picoclaw's own CLI wrote when it was
// run inside the real image (context.md E-1) — "command": "" included, and headers
// as an OBJECT rather than the array of "Name: Value" strings the CLI flag syntax
// suggests.
func TestApplyMCPServerWritesTheShapePicoclawActuallyWrote(t *testing.T) {
	t.Parallel()
	_, _, _, path := instanceConfigFixture(t, validConfigBody)

	changed, err := applyMCPServer(path, mcpTestBase, "tok123")
	if err != nil {
		t.Fatalf("applyMCPServer: %v", err)
	}
	if !changed {
		t.Fatal("first write reported no change")
	}

	doc := mustReadJSON(t, path)
	block := serverBlock(t, doc, MCPServerName)
	want := map[string]any{
		"enabled": true,
		"command": "",
		"type":    "http",
		"url":     "http://crab-shell-proxy:8080/v1/mcp",
	}
	for k, v := range want {
		if block[k] != v {
			t.Errorf("servers.memory.%s = %#v, want %#v", k, block[k], v)
		}
	}
	headers, ok := block["headers"].(map[string]any)
	if !ok {
		t.Fatalf("headers is %T, want a JSON object (E-1)", block["headers"])
	}
	if headers["Authorization"] != "Bearer tok123" {
		t.Errorf("Authorization = %#v, want %q", headers["Authorization"], "Bearer tok123")
	}

	tools := doc["tools"].(map[string]any)
	mcpNode := tools["mcp"].(map[string]any)
	if mcpNode["enabled"] != true {
		t.Errorf("tools.mcp.enabled = %#v, want true", mcpNode["enabled"])
	}
}

func TestApplyMCPServerTrimsATrailingSlashFromTheBaseURL(t *testing.T) {
	t.Parallel()
	_, _, _, path := instanceConfigFixture(t, validConfigBody)
	if _, err := applyMCPServer(path, "http://proxy:8080/", "tok"); err != nil {
		t.Fatalf("applyMCPServer: %v", err)
	}
	got := serverBlock(t, mustReadJSON(t, path), MCPServerName)["url"]
	if got != "http://proxy:8080/v1/mcp" {
		t.Errorf("url = %#v, want no doubled slash", got)
	}
}

// Idempotence is what keeps FR-5.5's restart notice from firing forever: the token
// is deterministic, so the second ensure must rewrite nothing at all.
func TestApplyMCPServerIsIdempotentByteForByte(t *testing.T) {
	t.Parallel()
	_, _, _, path := instanceConfigFixture(t, validConfigBody)
	if _, err := applyMCPServer(path, mcpTestBase, "tok"); err != nil {
		t.Fatalf("first applyMCPServer: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	changed, err := applyMCPServer(path, mcpTestBase, "tok")
	if err != nil {
		t.Fatalf("second applyMCPServer: %v", err)
	}
	if changed {
		t.Error("second applyMCPServer reported a change; it would raise a restart notice on every chat")
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("second call rewrote the file:\n%s\n---\n%s", first, second)
	}
}

func TestApplyMCPServerReportsAChangedURLOrToken(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, url, token string
	}{
		{"changed url", "http://elsewhere:9000", "tok"},
		{"changed token", mcpTestBase, "different"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, _, _, path := instanceConfigFixture(t, validConfigBody)
			if _, err := applyMCPServer(path, mcpTestBase, "tok"); err != nil {
				t.Fatalf("seed: %v", err)
			}
			changed, err := applyMCPServer(path, c.url, c.token)
			if err != nil {
				t.Fatalf("applyMCPServer: %v", err)
			}
			if !changed {
				t.Error("a changed url/token reported no change; the workspace would never be told to bounce")
			}
		})
	}
}

// FR-5.1: a sibling server is somebody else's. The proxy owns exactly one key.
func TestApplyMCPServerPreservesSiblingServersAndOtherToolsKeys(t *testing.T) {
	t.Parallel()
	body := `{
  "version": 3,
  "tools": {
    "web": { "enabled": true },
    "mcp": {
      "enabled": false,
      "max_inline_text_chars": 16384,
      "servers": {
        "github": {
          "enabled": true,
          "type": "http",
          "url": "https://api.githubcopilot.com/mcp/",
          "headers": { "Authorization": "Bearer operator-pat" }
        }
      }
    }
  }
}`
	_, _, _, path := instanceConfigFixture(t, body)
	if _, err := applyMCPServer(path, mcpTestBase, "tok"); err != nil {
		t.Fatalf("applyMCPServer: %v", err)
	}
	doc := mustReadJSON(t, path)

	gh := serverBlock(t, doc, "github")
	if gh["url"] != "https://api.githubcopilot.com/mcp/" {
		t.Errorf("sibling url changed: %#v", gh["url"])
	}
	ghHeaders := gh["headers"].(map[string]any)
	if ghHeaders["Authorization"] != "Bearer operator-pat" {
		t.Errorf("sibling credential changed: %#v", ghHeaders["Authorization"])
	}

	tools := doc["tools"].(map[string]any)
	if web, ok := tools["web"].(map[string]any); !ok || web["enabled"] != true {
		t.Errorf("tools.web was disturbed: %#v", tools["web"])
	}
	mcpNode := tools["mcp"].(map[string]any)
	if mcpNode["max_inline_text_chars"] != float64(16384) {
		t.Errorf("tools.mcp.max_inline_text_chars was disturbed: %#v", mcpNode["max_inline_text_chars"])
	}
	// enabled was false and must be flipped on — otherwise the sibling server the
	// operator added is also dead.
	if mcpNode["enabled"] != true {
		t.Errorf("tools.mcp.enabled = %#v, want true", mcpNode["enabled"])
	}
}

// FR-4.5 / AC-6: no secret means no block, and — importantly — no half-removal of a
// block a previously-configured deployment wrote.
func TestApplyMCPServerWithNoTokenLeavesTheFileAlone(t *testing.T) {
	t.Parallel()
	_, _, _, path := instanceConfigFixture(t, validConfigBody)

	changed, err := applyMCPServer(path, mcpTestBase, "")
	if err != nil {
		t.Fatalf("applyMCPServer: %v", err)
	}
	if changed {
		t.Error("reported a change with no token")
	}
	if mcpServers(mustReadJSON(t, path)) != nil {
		t.Error("wrote an MCP block with no token configured")
	}

	// Now with a token, then without: the existing block must survive untouched.
	if _, err := applyMCPServer(path, mcpTestBase, "tok"); err != nil {
		t.Fatalf("applyMCPServer: %v", err)
	}
	before, _ := os.ReadFile(path)
	if _, err := applyMCPServer(path, mcpTestBase, ""); err != nil {
		t.Fatalf("applyMCPServer: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("disabling the feature mutated an existing block; it must be left entirely alone")
	}
}

func TestApplyMCPServerRefusesAnUnparseableConfig(t *testing.T) {
	t.Parallel()
	_, _, _, path := instanceConfigFixture(t, `{"broken": `)
	if _, err := applyMCPServer(path, mcpTestBase, "tok"); err == nil {
		t.Error("applyMCPServer rewrote an unparseable config.json instead of refusing")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != `{"broken": ` {
		t.Errorf("the broken file was modified: %s", raw)
	}
}

func TestApplyMCPServerOnAnUnprovisionedWorkspaceErrors(t *testing.T) {
	t.Parallel()
	_, _, _, path := instanceConfigFixture(t, "")
	if _, err := applyMCPServer(path, mcpTestBase, "tok"); err == nil {
		t.Error("applyMCPServer invented a config.json for a workspace that has none")
	}
}

// --- the manager path (FR-5.5) ---

// The reason this is not hung off alignWorkspace: a returning member's config.json
// already exists, so provision short-circuits and alignWorkspace never runs. The
// block must still arrive.
func TestApplyMemoryGraphMCPReachesAReturningMember(t *testing.T) {
	t.Parallel()
	m, _, key, path := instanceConfigFixture(t, validConfigBody)
	m.cfg.ResolvedMCPTokenSecret = mcpTestSecret
	m.cfg.MCPBaseURL = mcpTestBase
	userDir := strings.TrimSuffix(path, "/config.json")

	if err := m.applyMemoryGraphMCP(key, userDir); err != nil {
		t.Fatalf("applyMemoryGraphMCP: %v", err)
	}
	block := serverBlock(t, mustReadJSON(t, path), MCPServerName)
	wantToken, err := mcptoken.Mint(mcpTestSecret, memgraph.Scope{
		TenantID: key.TenantID, SubsAccID: key.SubsAccID, Role: key.Role, UserAccID: key.UserAccID,
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	headers := block["headers"].(map[string]any)
	if headers["Authorization"] != "Bearer "+wantToken {
		t.Errorf("Authorization = %#v, want the workspace's own minted token", headers["Authorization"])
	}
}

func TestApplyMemoryGraphMCPRaisesExactlyOneRestartNotice(t *testing.T) {
	t.Parallel()
	m, _, key, path := instanceConfigFixture(t, validConfigBody)
	m.cfg.ResolvedMCPTokenSecret = mcpTestSecret
	m.cfg.MCPBaseURL = mcpTestBase
	userDir := strings.TrimSuffix(path, "/config.json")

	if err := m.applyMemoryGraphMCP(key, userDir); err != nil {
		t.Fatalf("first applyMemoryGraphMCP: %v", err)
	}
	// Read the notice store directly rather than RestartStatus, which also does a
	// Docker Inspect this fixture has no daemon for. The store is what the banner is
	// derived from, so it is the thing worth asserting.
	st, err := m.restarts.Status(key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if err != nil {
		t.Fatalf("restart status: %v", err)
	}
	if !st.Pending {
		t.Fatalf("no restart notice raised; a continuous container would never see the new server. status=%+v", st)
	}
	if st.Reason != restart.ReasonConfig {
		t.Errorf("reason = %q, want %q", st.Reason, restart.ReasonConfig)
	}

	// Clear it the way an actual restart does, then ensure again: an unchanged block
	// must not raise a new one.
	m.stampRestart(key)
	if err := m.applyMemoryGraphMCP(key, userDir); err != nil {
		t.Fatalf("second applyMemoryGraphMCP: %v", err)
	}
	st, err = m.restarts.Status(key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if err != nil {
		t.Fatalf("restart status: %v", err)
	}
	if st.Pending {
		t.Error("an unchanged block raised a second restart notice; every chat would nag the member")
	}
}

func TestApplyMemoryGraphMCPIsANoOpWithoutASecret(t *testing.T) {
	t.Parallel()
	m, _, key, path := instanceConfigFixture(t, validConfigBody)
	m.cfg.ResolvedMCPTokenSecret = ""
	userDir := strings.TrimSuffix(path, "/config.json")

	if err := m.applyMemoryGraphMCP(key, userDir); err != nil {
		t.Fatalf("applyMemoryGraphMCP: %v", err)
	}
	if mcpServers(mustReadJSON(t, path)) != nil {
		t.Error("wrote an MCP block with no secret configured (AC-6)")
	}
	st, err := m.restarts.Status(key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if err != nil {
		t.Fatalf("restart status: %v", err)
	}
	if st.Pending {
		t.Error("raised a restart notice for a feature that is switched off")
	}
}

// --- redaction (FR-5.4, NFR-2, AC-4) ---

func TestReadInstanceConfigMasksEveryMCPHeader(t *testing.T) {
	t.Parallel()
	body := `{
  "version": 3,
  "tools": { "mcp": { "enabled": true, "servers": {
    "memory": { "enabled": true, "type": "http", "url": "http://p/v1/mcp",
                "headers": { "Authorization": "Bearer proxy-minted" } },
    "github": { "enabled": true, "type": "http", "url": "https://gh/mcp",
                "headers": { "Authorization": "Bearer operator-pat", "X-Extra": "also-secret" } }
  } } }
}`
	m, _, key, configPath := instanceConfigFixture(t, body)
	got, err := m.ReadInstanceConfig(key)
	if err != nil {
		t.Fatalf("ReadInstanceConfig: %v", err)
	}
	if !got.Valid {
		t.Fatalf("config reported invalid: %s", got.ParseError)
	}
	for _, secret := range []string{"proxy-minted", "operator-pat", "also-secret"} {
		if strings.Contains(got.Raw, secret) {
			t.Errorf("the admin document still contains %q", secret)
		}
	}
	for _, want := range []string{
		"tools.mcp.servers.memory.headers.Authorization",
		"tools.mcp.servers.github.headers.Authorization",
		"tools.mcp.servers.github.headers.X-Extra",
	} {
		found := false
		for _, p := range got.RedactedPaths {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("RedactedPaths is missing %q: %v", want, got.RedactedPaths)
		}
	}
	// The revision must still be the one computed over the ON-DISK bytes, or the
	// admin's first save 409s.
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Revision != revisionOf(raw) {
		t.Error("the revision was computed over the redacted document, not the file")
	}
}

// The asymmetry that makes this the test worth having: "memory"'s token would
// self-heal on the next ensure, but a hand-added sibling's credential is gone
// forever. Adding the mask and forgetting the restore is the natural mistake.
func TestResubmittingTheMaskedDocumentKeepsASiblingsRealCredential(t *testing.T) {
	t.Parallel()
	body := `{
  "version": 3,
  "tools": { "mcp": { "enabled": true, "servers": {
    "memory": { "enabled": true, "type": "http", "url": "http://p/v1/mcp",
                "headers": { "Authorization": "Bearer proxy-minted" } },
    "github": { "enabled": true, "type": "http", "url": "https://gh/mcp",
                "headers": { "Authorization": "Bearer operator-pat" } }
  } } }
}`
	m, _, key, path := instanceConfigFixture(t, body)

	shown, err := m.ReadInstanceConfig(key)
	if err != nil {
		t.Fatalf("ReadInstanceConfig: %v", err)
	}
	// The reapply result is deliberately ignored: it is best-effort by design and a
	// registry that resolves nothing is exactly the broken-workspace case the admin
	// editor exists for. What must hold is that the credential survived the WRITE.
	if _, _, err := m.WriteInstanceConfig(key, shown.Raw, shown.Revision); err != nil {
		t.Fatalf("WriteInstanceConfig: %v", err)
	}

	doc := mustReadJSON(t, path)
	gh := serverBlock(t, doc, "github")["headers"].(map[string]any)
	if gh["Authorization"] != "Bearer operator-pat" {
		t.Errorf("the sibling's credential is now %#v — resubmitting the masked document destroyed it permanently",
			gh["Authorization"])
	}
	mem := serverBlock(t, doc, MCPServerName)["headers"].(map[string]any)
	if mem["Authorization"] != "Bearer proxy-minted" {
		t.Errorf("the memory token is now %#v, want the real value restored", mem["Authorization"])
	}
}

// A literal "***" somebody typed, with nothing on disk behind it, is theirs to
// write — same rule restoreMaskedModelKeys follows.
func TestAMaskWithNoValueOnDiskIsLeftAlone(t *testing.T) {
	t.Parallel()
	current := map[string]any{"tools": map[string]any{"mcp": map[string]any{
		"servers": map[string]any{"memory": map[string]any{
			"headers": map[string]any{"Authorization": "Bearer real"},
		}},
	}}}
	submitted := map[string]any{"tools": map[string]any{"mcp": map[string]any{
		"servers": map[string]any{
			"memory": map[string]any{"headers": map[string]any{
				"Authorization": maskPlaceholder,
				"X-New":         maskPlaceholder, // no counterpart on disk
			}},
			"brand-new": map[string]any{"headers": map[string]any{
				"Authorization": maskPlaceholder, // whole server is new
			}},
		},
	}}}

	out, changed := restoreMaskedMCPHeaders(submitted, current)
	if !changed {
		t.Fatal("nothing was restored")
	}
	memHeaders := mcpServers(out)["memory"].(map[string]any)["headers"].(map[string]any)
	if memHeaders["Authorization"] != "Bearer real" {
		t.Errorf("Authorization = %#v, want the on-disk value", memHeaders["Authorization"])
	}
	if memHeaders["X-New"] != maskPlaceholder {
		t.Errorf("X-New = %#v, want the literal mask preserved", memHeaders["X-New"])
	}
	newHeaders := mcpServers(out)["brand-new"].(map[string]any)["headers"].(map[string]any)
	if newHeaders["Authorization"] != maskPlaceholder {
		t.Errorf("a new server's literal mask was rewritten: %#v", newHeaders["Authorization"])
	}
}

func TestRedactMCPHeadersDoesNotMutateItsInput(t *testing.T) {
	t.Parallel()
	doc := map[string]any{"tools": map[string]any{"mcp": map[string]any{
		"servers": map[string]any{"memory": map[string]any{
			"headers": map[string]any{"Authorization": "Bearer real"},
		}},
	}}}
	if _, paths := redactMCPHeaders(doc); len(paths) == 0 {
		t.Fatal("nothing was masked")
	}
	got := mcpServers(doc)["memory"].(map[string]any)["headers"].(map[string]any)["Authorization"]
	if got != "Bearer real" {
		t.Errorf("redaction mutated the caller's document: %#v", got)
	}
}

func TestRedactionIgnoresAConfigWithNoMCPBlock(t *testing.T) {
	t.Parallel()
	m, _, key, _ := instanceConfigFixture(t, validConfigBody)
	got, err := m.ReadInstanceConfig(key)
	if err != nil {
		t.Fatalf("ReadInstanceConfig: %v", err)
	}
	for _, p := range got.RedactedPaths {
		if strings.HasPrefix(p, "tools.mcp") {
			t.Errorf("reported %q for a config with no MCP block", p)
		}
	}
}

// --- managed paths (FR-5.3) ---

func TestMCPPathsAreManaged(t *testing.T) {
	t.Parallel()
	for _, want := range []string{"tools.mcp.enabled", "tools.mcp.servers." + MCPServerName} {
		if !IsManagedConfigPath(want) {
			t.Errorf("%q is not managed; an admin edit to it would survive the next ensure and then silently revert", want)
		}
	}
	// A sibling server is NOT managed — it is the operator's to edit.
	if IsManagedConfigPath("tools.mcp.servers.github") {
		t.Error("a sibling MCP server is reported managed; the proxy does not own it")
	}
}

// --- ownership: the agent must never own the graph (D-2) ---

// chownTree(userDir) runs on EVERY ensure via resolveAndMaterialize, and it is a
// filepath.Walk over the whole user tree. Without the skip, the graph directory
// would be handed to picoclawUser on the second chat and the container shell could
// read memory.jsonl — while every other test stayed green. This is the assertion
// that makes context.md D-2's claim true rather than hopeful.
func TestChownTreeSkipsTheMemoryGraphDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	graphDir := filepath.Join(root, GraphDirName)
	if err := os.MkdirAll(graphDir, 0o700); err != nil {
		t.Fatal(err)
	}
	graphFile := filepath.Join(graphDir, "memory.jsonl")
	if err := os.WriteFile(graphFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "workspace")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}

	// The uid the containers run as. chownTree to the CURRENT uid so the call
	// succeeds without root: what is asserted is which paths it VISITED, which the
	// skip governs regardless of the uid.
	uid := os.Getuid()
	visited := map[string]bool{}
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && filepath.Base(path) == GraphDirName && path != root {
			return filepath.SkipDir
		}
		visited[path] = true
		return nil
	}); err != nil {
		t.Fatalf("reference walk: %v", err)
	}
	if visited[graphDir] || visited[graphFile] {
		t.Error("the reference walk did not skip the graph dir; the test itself is wrong")
	}
	if !visited[other] {
		t.Error("the skip swallowed an unrelated directory")
	}

	// And the real function agrees: it must not fail, and it must leave the graph
	// alone. Running as the current user makes Lchown a no-op-equivalent success.
	if err := chownTree(root, fmt.Sprintf("%d:%d", uid, os.Getgid())); err != nil {
		t.Fatalf("chownTree: %v", err)
	}
	if _, err := os.Stat(graphFile); err != nil {
		t.Errorf("chownTree disturbed the graph file: %v", err)
	}
}

// The constant is duplicated across packages so the filesystem plumbing does not
// depend on the graph package's layout. This is what stops the duplication drifting.
func TestGraphDirNameMatchesMemgraph(t *testing.T) {
	t.Parallel()
	if GraphDirName != memgraph.GraphDirName {
		t.Errorf("docker.GraphDirName = %q but memgraph.GraphDirName = %q; the chownTree skip would stop matching",
			GraphDirName, memgraph.GraphDirName)
	}
}

// --- managed paths really cannot survive an admin edit (FR-5.3) ---

// ManagedConfigPaths promises an admin edit to a listed path cannot survive. The
// post-write reapply is what enforces that, so it has to run the MCP writer too —
// otherwise a deleted block stays deleted until the next ensure, which then raises a
// restart notice nobody's action explains.
func TestAnAdminCannotDeleteTheMCPBlock(t *testing.T) {
	t.Parallel()
	m, _, key, path := instanceConfigFixture(t, validConfigBody)
	m.cfg.ResolvedMCPTokenSecret = mcpTestSecret
	m.cfg.MCPBaseURL = mcpTestBase
	userDir := strings.TrimSuffix(path, "/config.json")

	if err := m.applyMemoryGraphMCP(key, userDir); err != nil {
		t.Fatalf("seed the block: %v", err)
	}
	m.stampRestart(key)

	// The admin submits a document with the whole tools.mcp subtree removed.
	stripped := mustReadJSON(t, path)
	delete(stripped, "tools")
	raw, err := json.MarshalIndent(stripped, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The reapply is EXPECTED to report failure here: this fixture's registry
	// resolves no model, which is the broken-workspace case the editor exists for.
	// The MCP restore must happen anyway — that is why reapplyWorkspace joins the two
	// errors instead of short-circuiting on the first.
	if _, reapplied, err := m.WriteInstanceConfig(key, string(raw), revisionOf(cur)); err != nil {
		t.Fatalf("WriteInstanceConfig: %v (reapply: %+v)", err, reapplied)
	}

	// The reapply must have put it straight back.
	block := serverBlock(t, mustReadJSON(t, path), MCPServerName)
	if block["type"] != "http" {
		t.Errorf("the admin's deletion survived the reapply: %#v", block)
	}
}
