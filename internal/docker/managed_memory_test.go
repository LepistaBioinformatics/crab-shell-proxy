package docker

import (
	"strings"
	"testing"
)

// The operator-managed content the platform mounts read-only into every picoclaw
// container. Asserted through the pure bind builder rather than through create(),
// because create() chowns and cannot run without privileges here — that is the whole
// reason the list is built in a function a test can reach.

func TestManagedContentBindsAreReadOnly(t *testing.T) {
	t.Parallel()
	for _, b := range managedContentBinds("/host/managed", "/data/.picoclaw", true) {
		if !strings.HasSuffix(b, ":ro") {
			t.Errorf("bind %q is not read-only; the agent could alter operator content", b)
		}
		if strings.Count(b, ":") != 2 {
			t.Errorf("bind %q is not host:container:ro", b)
		}
	}
}

func TestManagedContentBindsPlaceEachFileInTheWorkspace(t *testing.T) {
	t.Parallel()
	binds := managedContentBinds("/host/managed", "/data/.picoclaw", true)
	want := map[string]string{
		managedSkillRel:   "/host/managed/skills/shared-content:/data/.picoclaw/workspace/skills/shared-content:ro",
		managedMemoryRel:  "/host/managed/memory/CONTEXT_RECOVERY.md:/data/.picoclaw/workspace/memory/CONTEXT_RECOVERY.md:ro",
		managedRoutingRel: "/host/managed/memory/MEMORY_ROUTING.md:/data/.picoclaw/workspace/memory/MEMORY_ROUTING.md:ro",
	}
	for rel, spec := range want {
		found := false
		for _, b := range binds {
			if b == spec {
				found = true
			}
		}
		if !found {
			t.Errorf("no bind for %s; wanted %q in %v", rel, spec, binds)
		}
	}
}

// With no CRAB_MCP_TOKEN_SECRET the agent has no mcp_memory_* tools, so a file telling
// it to prefer them would be actively wrong. The routing note is the ONLY managed file
// that is conditional; the other two apply regardless.
func TestTheRoutingNoteIsMountedOnlyWhenTheMemoryGraphIsOn(t *testing.T) {
	t.Parallel()
	off := managedContentBinds("/host/managed", "/data/.picoclaw", false)
	for _, b := range off {
		if strings.Contains(b, "MEMORY_ROUTING.md") {
			t.Errorf("the routing note was mounted with the memory graph switched off: %q", b)
		}
	}
	if len(off) != 2 {
		t.Errorf("binds with the graph off = %d, want 2 (skill + context recovery)", len(off))
	}

	if on := managedContentBinds("/host/managed", "/data/.picoclaw", true); len(on) != 3 {
		t.Errorf("binds with the graph on = %d, want 3", len(on))
	}
}

// Stable order, so a container's bind list does not churn between starts — anything
// comparing them would otherwise read that as drift.
func TestManagedContentBindOrderIsStable(t *testing.T) {
	t.Parallel()
	first := managedContentBinds("/m", "/d", true)
	for i := 0; i < 5; i++ {
		again := managedContentBinds("/m", "/d", true)
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("bind order changed between calls: %v vs %v", first, again)
			}
		}
	}
}

// A file that is mounted but not embedded produces a bind with a missing source — and
// Docker invents an empty DIRECTORY at the destination, which the agent would read as
// an empty memory note rather than as an error.
func TestEveryManagedRelExistsInTheEmbeddedTree(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{managedSkillRel, managedMemoryRel, managedRoutingRel} {
		if _, err := managedFS.ReadDir("managed/" + rel); err == nil {
			continue // a directory, fine
		}
		if _, err := managedFS.ReadFile("managed/" + rel); err != nil {
			t.Errorf("managed/%s is mounted but not embedded: %v", rel, err)
		}
	}
}

// The note's job is to name the tools. If the prefix ever changes, the file silently
// becomes advice about tools that do not exist — which is how the agent behaved before
// it existed at all.
func TestTheRoutingNoteNamesTheRealToolNames(t *testing.T) {
	t.Parallel()
	body, err := managedFS.ReadFile("managed/" + managedRoutingRel)
	if err != nil {
		t.Fatalf("read embedded note: %v", err)
	}
	text := string(body)
	for _, tool := range []string{
		"mcp_memory_create_entities",
		"mcp_memory_add_observations",
		"mcp_memory_create_relations",
		"mcp_memory_search_nodes",
	} {
		if !strings.Contains(text, tool) {
			t.Errorf("the routing note does not name %s", tool)
		}
	}
	// picoclaw builds the agent-facing name as mcp_<server>_<tool>, and the server is
	// MCPServerName. A rename there must not leave this file pointing at nothing.
	if !strings.Contains(text, "mcp_"+MCPServerName+"_") {
		t.Errorf("the note does not use the mcp_%s_ prefix the agent actually sees", MCPServerName)
	}
	// The honesty rule is why the file exists: the model claimed a graph write it had
	// never made.
	if !strings.Contains(text, "Never claim a save you did not make") {
		t.Error("the routing note lost its honesty rule")
	}
}
