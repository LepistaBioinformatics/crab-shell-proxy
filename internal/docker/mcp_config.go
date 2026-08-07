package docker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/mcptoken"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/memgraph"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/restart"
)

// The proxy-owned MCP server block in a workspace's config.json — the thing that
// makes the native memory graph reachable from inside a picoclaw container.
//
// Shape is not invented: it is what `picoclaw mcp add memory --transport http`
// actually wrote when run inside sipeed/picoclaw:latest, `"command": ""` and the
// headers-as-object included (see .specs/features/memory-graph-mcp/context.md E-1).

// MCPServerName is the key under tools.mcp.servers that the proxy owns. Sibling
// keys are somebody else's and are never touched.
const MCPServerName = "memory"

// GraphDirName mirrors memgraph.GraphDirName. It is redeclared here (rather than
// imported into provision.go) so chownTree can skip it without this package's
// filesystem plumbing depending on the graph package's layout — and a test asserts
// the two stay equal, so the duplication cannot drift.
const GraphDirName = memgraph.GraphDirName

// MCPRoutePath is the path the injected url points at.
const MCPRoutePath = "/v1/mcp"

// desiredMCPServer is the block the proxy writes for one workspace.
func desiredMCPServer(baseURL, token string) map[string]any {
	return map[string]any{
		"enabled": true,
		// Emitted as "" even for an HTTP server — picoclaw's own CLI writes it that
		// way, and matching it exactly is what makes the idempotence check below
		// stable against a config picoclaw itself has rewritten.
		"command": "",
		"type":    "http",
		"url":     strings.TrimSuffix(baseURL, "/") + MCPRoutePath,
		"headers": map[string]any{"Authorization": "Bearer " + token},
	}
}

// applyMCPServer writes the proxy-owned MCP server into one workspace's
// config.json and reports whether anything changed.
//
// It must be called on EVERY ensure, not at provision time. alignWorkspace — the
// obvious-looking home for this — runs inside provision's
// `if os.Stat(configPath) != nil` branch, so it fires only on a first-ever seed;
// hanging the MCP block off it would mean no existing member ever gets memory.
// resolveAndMaterialize is the established every-ensure writer and this sits
// beside it.
//
// The `changed` return is what keeps that cheap: the token is deterministic and
// the URL is configuration, so the block converges after one write and every later
// ensure is a no-op that rewrites nothing and raises no restart notice.
//
// Only tools.mcp.servers["memory"] and tools.mcp.enabled are owned. Any other
// server an operator added by hand survives untouched.
// ProjectMCPServerName is the memory server a project agent talks to. One entry
// per project, each with its own token, because tools.mcp.servers is GLOBAL to
// the container: every agent in it reads the same block, so a per-project graph
// cannot come from the token alone — it has to come from a per-project SERVER
// that only that agent is allowed to see (its AGENT.md frontmatter carries the
// mcpServers allowlist).
func ProjectMCPServerName(projectID string) string {
	return MCPServerName + "-" + projectID
}

func applyMCPServer(configPath, baseURL, token string, projectTokens map[string]string) (bool, error) {
	if token == "" {
		// No secret configured: the feature is off. Leave the file completely alone —
		// including any block a previously-configured deployment wrote, because
		// half-removing it would be a third state nobody asked for.
		return false, nil
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return false, err
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return false, fmt.Errorf("parse config.json: %w", err)
	}

	tools := childMap(cfg, "tools")
	mcpNode := childMap(tools, "mcp")
	servers := childMap(mcpNode, "servers")

	want := desiredMCPServer(baseURL, token)
	if reflect.DeepEqual(servers[MCPServerName], want) && mcpNode["enabled"] == true &&
		projectServersMatch(servers, baseURL, projectTokens) {
		return false, nil
	}
	servers[MCPServerName] = want
	// Rebuild the project entries, dropping any whose project is gone. Rebuild
	// rather than merge, for the same reason the agents.list projection does: a
	// stale server would keep a deleted project's graph reachable.
	for name := range servers {
		if strings.HasPrefix(name, MCPServerName+"-") {
			delete(servers, name)
		}
	}
	for projectID, ptoken := range projectTokens {
		servers[ProjectMCPServerName(projectID)] = desiredMCPServer(baseURL, ptoken)
	}
	// picoclaw's own CLI flips this on when a server is added; the proxy must set it
	// and must never set it back to false, or a hand-added sibling server would be
	// disabled as a side effect.
	mcpNode["enabled"] = true

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(configPath, out, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// --- redaction -----------------------------------------------------------

// mcpServers returns the servers map from a parsed config.json WITHOUT creating
// anything, so the read path cannot mutate the document it is inspecting.
func mcpServers(doc map[string]any) map[string]any {
	tools, ok := doc["tools"].(map[string]any)
	if !ok {
		return nil
	}
	mcpNode, ok := tools["mcp"].(map[string]any)
	if !ok {
		return nil
	}
	servers, _ := mcpNode["servers"].(map[string]any)
	return servers
}

// redactMCPHeaders masks every header value under tools.mcp.servers.*.headers,
// returning a copy and the dotted paths it masked.
//
// EVERY server, not just "memory": a hand-added sibling's Authorization header is
// somebody's credential too, and it is the one the proxy cannot regenerate.
//
// Without this, GET /v1/admin/users/config would hand every member's memory-graph
// bearer token to any proxy admin. redactModelKeys covers model_list only
// (context.md E-7), and nothing existing would have failed.
func redactMCPHeaders(doc map[string]any) (map[string]any, []string) {
	servers := mcpServers(doc)
	if len(servers) == 0 {
		return doc, nil
	}

	var paths []string
	// Names sorted so RedactedPaths is stable between reads — an admin UI diffing
	// the response should not see phantom changes from Go's map ordering.
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		server, ok := servers[name].(map[string]any)
		if !ok {
			continue
		}
		headers, ok := server["headers"].(map[string]any)
		if !ok || len(headers) == 0 {
			continue
		}
		keys := make([]string, 0, len(headers))
		for k := range headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			paths = append(paths, fmt.Sprintf("tools.mcp.servers.%s.headers.%s", name, k))
		}
	}
	if len(paths) == 0 {
		return doc, nil
	}

	// A deep copy of the whole document, not the surgical shallow copies
	// redactModelKeys uses. The nesting here is four levels and the cost of getting
	// a shared sub-map wrong is mutating the caller's parsed config — which on the
	// write path would mean masking the value we are about to restore FROM.
	out := deepCopyJSON(doc).(map[string]any)
	for _, server := range mcpServers(out) {
		s, ok := server.(map[string]any)
		if !ok {
			continue
		}
		headers, ok := s["headers"].(map[string]any)
		if !ok {
			continue
		}
		for k := range headers {
			headers[k] = maskPlaceholder
		}
	}
	return out, paths
}

// restoreMaskedMCPHeaders puts back every header value the read path masked,
// keyed by (server name, header name) rather than by position.
//
// The failure this prevents is asymmetric, which is why it has its own test. For
// the "memory" server a lost token self-heals: applyMCPServer rewrites a
// deterministic value on the next ensure. For a hand-added sibling nothing
// regenerates it, so persisting "***" destroys that credential permanently. Adding
// the mask (above) while forgetting the restore (here) is the natural mistake.
func restoreMaskedMCPHeaders(submitted, current map[string]any) (map[string]any, bool) {
	subServers := mcpServers(submitted)
	curServers := mcpServers(current)
	if len(subServers) == 0 || len(curServers) == 0 {
		return submitted, false
	}

	// Decide whether anything needs restoring before copying, so an unaffected
	// document is returned byte-identical by the caller.
	needed := false
	for name, s := range subServers {
		server, ok := s.(map[string]any)
		if !ok {
			continue
		}
		headers, ok := server["headers"].(map[string]any)
		if !ok {
			continue
		}
		curHeaders := headersOf(curServers, name)
		if curHeaders == nil {
			continue
		}
		for k, v := range headers {
			if holdsMask(v) && curHeaders[k] != nil {
				needed = true
				break
			}
		}
		if needed {
			break
		}
	}
	if !needed {
		return submitted, false
	}

	out := deepCopyJSON(submitted).(map[string]any)
	for name, s := range mcpServers(out) {
		server, ok := s.(map[string]any)
		if !ok {
			continue
		}
		headers, ok := server["headers"].(map[string]any)
		if !ok {
			continue
		}
		curHeaders := headersOf(curServers, name)
		if curHeaders == nil {
			continue
		}
		for k, v := range headers {
			// A mask with no counterpart on disk is left alone: it is then a literal
			// "***" somebody typed, not a hidden credential. Same rule
			// restoreMaskedModelKeys follows.
			if holdsMask(v) && curHeaders[k] != nil {
				headers[k] = curHeaders[k]
			}
		}
	}
	return out, true
}

func headersOf(servers map[string]any, name string) map[string]any {
	server, ok := servers[name].(map[string]any)
	if !ok {
		return nil
	}
	headers, _ := server["headers"].(map[string]any)
	return headers
}

// deepCopyJSON copies a value decoded from JSON. Only the three container shapes
// encoding/json produces need handling; scalars are immutable and shared safely.
func deepCopyJSON(v any) any {
	switch node := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for k, child := range node {
			out[k] = deepCopyJSON(child)
		}
		return out
	case []any:
		out := make([]any, len(node))
		for i, child := range node {
			out[i] = deepCopyJSON(child)
		}
		return out
	default:
		return v
	}
}

// applyMemoryGraphMCP injects the memory-graph MCP server into one workspace and,
// when the block actually changed, tells that workspace it needs a bounce.
//
// The restart notice is the point. picoclaw reads config.json at startup, and
// `mode: "continuous"` agents stay up across chats — so a container that is already
// running does not see the new server until it restarts, and on the day this ships
// EVERY existing member is in that state. Without the notice the feature would look
// broken to everyone who already had a workspace.
//
// A notice rather than a forced restart: this follows the path every other
// proxy-side change the agent only reads at boot already uses, and it does not
// interrupt a live conversation. Because the block converges after one write, this
// raises exactly one notice per member and never loops.
//
// A notice failure is logged, not returned: the config is already correct on disk,
// and refusing the chat because we could not write a banner would be worse than the
// banner being missing.
func (m *Manager) applyMemoryGraphMCP(key WorkspaceKey, userDir string) error {
	token, err := m.memoryGraphToken(key)
	if err != nil {
		return err
	}
	if token == "" {
		return nil // feature disabled: no secret configured
	}
	// One token per project, each encoding that project in its scope, so the
	// proxy's MCP endpoint resolves a different graph directory for each.
	projects, err := m.projectStore(key).List()
	if err != nil {
		return fmt.Errorf("read projects: %w", err)
	}
	projectTokens := make(map[string]string, len(projects))
	for _, p := range projects {
		ptoken, terr := m.memoryGraphTokenFor(key, p.ID)
		if terr != nil {
			return terr
		}
		projectTokens[p.ID] = ptoken
	}

	changed, err := applyMCPServer(
		filepath.Join(userDir, "config.json"), m.cfg.MCPBaseURL, token, projectTokens)
	if err != nil {
		return fmt.Errorf("apply memory-graph mcp server: %w", err)
	}
	if !changed {
		return nil
	}
	m.logf("memory graph: mcp server written for %s/%s/%s/%s, restart notice raised",
		key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if nerr := m.RaiseWorkspaceRestartNotice(key, restart.ReasonConfig); nerr != nil {
		m.logf("memory graph: could not raise restart notice: %v", nerr)
	}
	return nil
}

// memoryGraphToken mints the workspace's bearer token, or "" when the feature is
// disabled.
//
// A scope that cannot be encoded is an ERROR, not an empty token: writing a block
// with a broken token would give the agent a memory server that always 401s, which
// is harder to diagnose than no memory server at all.
func (m *Manager) memoryGraphToken(key WorkspaceKey) (string, error) {
	return m.memoryGraphTokenFor(key, "")
}

// memoryGraphTokenFor mints the token for one workspace, optionally scoped to a
// project. An empty projectID yields the pre-projects token byte for byte.
func (m *Manager) memoryGraphTokenFor(key WorkspaceKey, projectID string) (string, error) {
	if m.cfg.ResolvedMCPTokenSecret == "" {
		return "", nil
	}
	token, err := mcptoken.Mint(m.cfg.ResolvedMCPTokenSecret, memgraph.Scope{
		TenantID:  key.TenantID,
		SubsAccID: key.SubsAccID,
		Role:      key.Role,
		UserAccID: key.UserAccID,
		Project:   projectID,
	})
	if err != nil {
		return "", fmt.Errorf("mint memory-graph token: %w", err)
	}
	return token, nil
}

// projectServersMatch reports whether the config already holds exactly the
// project memory servers it should — no more (a deleted project's entry) and no
// fewer (a project created since the last write).
func projectServersMatch(servers map[string]any, baseURL string, tokens map[string]string) bool {
	found := 0
	for name := range servers {
		if strings.HasPrefix(name, MCPServerName+"-") {
			found++
		}
	}
	if found != len(tokens) {
		return false
	}
	for projectID, token := range tokens {
		if !reflect.DeepEqual(servers[ProjectMCPServerName(projectID)], desiredMCPServer(baseURL, token)) {
			return false
		}
	}
	return true
}
