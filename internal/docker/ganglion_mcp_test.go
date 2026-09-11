package docker

import (
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

func mcpDoc(t *testing.T, baseURL, token string, web map[string]string) map[string]any {
	t.Helper()
	b, err := ganglionConfigDoc(registry.Resolution{
		Primary: model("primary", "deepseek", "deepseek-chat", "https://api.deepseek.com/v1", "sk-1"),
	}, web, baseURL, token)
	if err != nil {
		t.Fatal(err)
	}
	return decodeDoc(t, b)
}

// serversIn returns tools.mcp.servers, or nil.
func serversIn(doc map[string]any) map[string]any {
	tools, ok := doc["tools"].(map[string]any)
	if !ok {
		return nil
	}
	mcp, ok := tools["mcp"].(map[string]any)
	if !ok {
		return nil
	}
	servers, _ := mcp["servers"].(map[string]any)
	return servers
}

// The block is picoclaw's record, because that is the shape the harness reads
// (config.loadMCP) -- `command: ""` included, which is the field most likely to
// be "tidied" away by someone who notices it is empty for an HTTP server.
func TestGanglionConfigCarriesTheMemoryServer(t *testing.T) {
	doc := mcpDoc(t, "http://crab-shell-proxy:8080", "tok-1", nil)

	servers := serversIn(doc)
	if len(servers) != 1 {
		t.Fatalf("servers = %+v, want exactly one", servers)
	}
	srv, ok := servers[MCPServerName].(map[string]any)
	if !ok {
		t.Fatalf("no %q server: %+v", MCPServerName, servers)
	}
	for k, want := range map[string]any{
		"enabled": true,
		"command": "",
		"type":    "http",
		"url":     "http://crab-shell-proxy:8080" + MCPRoutePath,
	} {
		if srv[k] != want {
			t.Errorf("%s = %v, want %v", k, srv[k], want)
		}
	}
	headers, _ := srv["headers"].(map[string]any)
	if headers["Authorization"] != "Bearer tok-1" {
		t.Errorf("headers = %+v", headers)
	}
	if enabled := doc["tools"].(map[string]any)["mcp"].(map[string]any)["enabled"]; enabled != true {
		t.Errorf("tools.mcp.enabled = %v", enabled)
	}
}

// EXACTLY ONE SERVER, and this is the test that says why rather than a comment
// that does.
//
// picoclaw gets one entry per project, because each project there is a separate
// AGENT sharing one global servers map. The ganglion has one agent and takes the
// project as a header, and its harness registers a remote server's tools under
// their OWN names -- so two servers both offering memory_search collide, and the
// harness refuses to boot on exactly that. Writing picoclaw's shape here would
// stop a ganglion container from starting the moment its member made a project.
func TestGanglionConfigWritesNoPerProjectMemoryServers(t *testing.T) {
	servers := serversIn(mcpDoc(t, "http://crab-shell-proxy:8080", "tok-1", nil))
	for name := range servers {
		if name != MCPServerName {
			t.Fatalf("server %q would collide with %q inside the harness", name, MCPServerName)
		}
	}
}

// NFR-1's shape for this slice: with the feature off the file is what it was
// before the feature existed.
func TestGanglionConfigOmitsTheMemoryServerWhenOff(t *testing.T) {
	for name, tc := range map[string]struct{ baseURL, token string }{
		"no secret configured":  {"http://crab-shell-proxy:8080", ""},
		"no reachable base url": {"", "tok-1"},
	} {
		t.Run(name, func(t *testing.T) {
			doc := mcpDoc(t, tc.baseURL, tc.token, nil)
			if _, present := doc["tools"]; present {
				t.Fatalf("tools block written with the feature off: %+v", doc["tools"])
			}
		})
	}
}

// The two tools blocks are siblings. Writing one used to replace the whole
// "tools" key, so adding the graph would have silently removed web search.
func TestGanglionConfigKeepsWebAndMemoryTogether(t *testing.T) {
	doc := mcpDoc(t, "http://crab-shell-proxy:8080", "tok-1", map[string]string{"brave": "k"})
	tools, ok := doc["tools"].(map[string]any)
	if !ok {
		t.Fatalf("no tools block: %+v", doc)
	}
	if _, present := tools["web"]; !present {
		t.Error("the web block was lost when the memory server was added")
	}
	if _, present := tools["mcp"]; !present {
		t.Error("the mcp block is missing")
	}
}

// The render must be stable, or the harness re-reads the file on every ensure
// and a diff is useless. Same rule ganglionWebBlock already follows.
func TestGanglionConfigRendersTheMemoryServerStably(t *testing.T) {
	first, err := ganglionConfigDoc(registry.Resolution{
		Primary: model("primary", "deepseek", "deepseek-chat", "https://api.deepseek.com/v1", "sk-1"),
	}, map[string]string{"brave": "k"}, "http://p:8080", "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		again, err := ganglionConfigDoc(registry.Resolution{
			Primary: model("primary", "deepseek", "deepseek-chat", "https://api.deepseek.com/v1", "sk-1"),
		}, map[string]string{"brave": "k"}, "http://p:8080", "tok-1")
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("render %d differs:\n%s\n---\n%s", i, first, again)
		}
	}
}
