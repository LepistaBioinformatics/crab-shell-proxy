package docker

import (
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// The operator-managed content the platform mounts read-only into every picoclaw
// container. Asserted through the pure bind builder rather than through create(),
// because create() chowns and cannot run without privileges here — that is the whole
// reason the list is built in a function a test can reach.

func TestManagedContentBindsAreReadOnly(t *testing.T) {
	t.Parallel()
	for _, b := range managedContentBinds("/host/managed", "/data/.picoclaw", config.HarnessPicoclaw, true) {
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
	binds := managedContentBinds("/host/managed", "/data/.picoclaw", config.HarnessPicoclaw, true)
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
	off := managedContentBinds("/host/managed", "/data/.picoclaw", config.HarnessPicoclaw, false)
	for _, b := range off {
		if strings.Contains(b, "MEMORY_ROUTING.md") {
			t.Errorf("the routing note was mounted with the memory graph switched off: %q", b)
		}
	}
	if len(off) != 4 {
		t.Errorf("binds with the graph off = %d, want 4 (two skills + context recovery + file delivery)", len(off))
	}

	if on := managedContentBinds("/host/managed", "/data/.picoclaw", config.HarnessPicoclaw, true); len(on) != 5 {
		t.Errorf("binds with the graph on = %d, want 5 (the delivery rule is unconditional)", len(on))
	}
}

// The second gate, and it is the same principle one level up: a file describing an
// alpine image with busybox and a shell confined to the turn's workspace is a
// description of the WRONG MACHINE for a picoclaw agent.
func TestTheHarnessNoteIsMountedOnlyForTheHarnessItDescribes(t *testing.T) {
	t.Parallel()
	for _, b := range managedContentBinds("/m", "/d", config.HarnessPicoclaw, true) {
		if strings.Contains(b, "ganglion-workspace") {
			t.Errorf("the ganglion note was mounted into a picoclaw container: %q", b)
		}
	}

	found := false
	for _, b := range managedContentBinds("/m", "/d", config.HarnessGanglion, true) {
		if b == "/m/skills/ganglion-workspace:/d/workspace/skills/ganglion-workspace:ro" {
			found = true
		}
	}
	if !found {
		t.Error("the ganglion note is not mounted into a ganglion container")
	}
}

// Both harnesses read the same SKILL.md format -- crab-ganglion-harness's own
// `internal/skillfile` package says so -- so the skill about writing one is not gated.
func TestTheSkillCreatorReachesBothHarnesses(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{config.HarnessPicoclaw, config.HarnessGanglion} {
		found := false
		for _, b := range managedContentBinds("/m", "/d", harness, false) {
			if strings.Contains(b, "/workspace/skills/skill-creator:ro") {
				found = true
			}
		}
		if !found {
			t.Errorf("skill-creator is not mounted for %s", harness)
		}
	}
}

// Stable order, so a container's bind list does not churn between starts — anything
// comparing them would otherwise read that as drift.
func TestManagedContentBindOrderIsStable(t *testing.T) {
	t.Parallel()
	first := managedContentBinds("/m", "/d", config.HarnessPicoclaw, true)
	for i := 0; i < 5; i++ {
		again := managedContentBinds("/m", "/d", config.HarnessPicoclaw, true)
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
	for _, rel := range []string{
		managedSkillRel, managedSkillCreatorRel, managedGanglionRel,
		managedMemoryRel, managedRoutingRel, managedDeliveryRel,
	} {
		if _, err := managedFS.ReadDir("managed/" + rel); err == nil {
			continue // a directory, fine
		}
		if _, err := managedFS.ReadFile("managed/" + rel); err != nil {
			t.Errorf("managed/%s is mounted but not embedded: %v", rel, err)
		}
	}
}

// THE CONTRADICTION THIS SUITE DID NOT CATCH. `shared-content` told the agent to write
// deliverables to `uploads/attachments`, while FILE_DELIVERY.md -- bound into the same
// workspace, and read every turn -- says `public/attachments` and explicitly says not to
// create an `uploads/` folder. `uploads` is LegacyPublicDirName: the one-time migration
// is the only thing that should still name it, and a file written there is invisible to
// the member on both harnesses.
//
// Asserted against config.PublicDirName rather than the literal, so a second rename
// cannot leave these documents behind again.
func TestTheShippedSkillsNameTheDirectoryTheMemberCanSee(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{managedSkillRel, managedDeliveryRel, managedGanglionRel} {
		body := readManaged(t, rel)
		if !strings.Contains(body, config.PublicDirName+"/attachments") {
			t.Errorf("managed/%s never names %s/attachments", rel, config.PublicDirName)
		}
		for _, line := range strings.Split(body, "\n") {
			// The delivery rule has a section explaining that `uploads/` is the OLD
			// name, so the word itself is allowed -- telling the agent to WRITE there
			// is not.
			if strings.Contains(line, config.LegacyPublicDirName+"/attachments") &&
				!strings.Contains(line, "old") && !strings.Contains(line, "not create") {
				t.Errorf("managed/%s still points a write at the legacy directory: %q", rel, line)
			}
		}
	}
}

// A skill is found by its description and nothing else: only name and description reach
// a turn's prompt, and a body nobody opens is a body nobody reads. An empty description
// is a skill that has been written and cannot be found.
func TestEveryShippedSkillDeclaresNameAndDescription(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{managedSkillRel, managedSkillCreatorRel, managedGanglionRel} {
		body := readManaged(t, rel+"/SKILL.md")
		name := strings.TrimPrefix(rel, "skills/")
		if !strings.Contains(body, "name: "+name) {
			t.Errorf("managed/%s does not declare `name: %s`; the format requires it to "+
				"match the directory", rel, name)
		}
		if !strings.Contains(body, "description:") {
			t.Errorf("managed/%s has no description, so nothing will ever open it", rel)
		}
	}
}

// Reads either a managed file or the SKILL.md inside a managed directory.
func readManaged(t *testing.T, rel string) string {
	t.Helper()
	body, err := managedFS.ReadFile("managed/" + rel)
	if err != nil {
		body, err = managedFS.ReadFile("managed/" + rel + "/SKILL.md")
	}
	if err != nil {
		t.Fatalf("read embedded managed/%s: %v", rel, err)
	}
	return string(body)
}

// A GANGLION WORKSPACE HAS NO `.secrets/`: credentials reach that harness as
// environment variables, and ganglionWorkspaceDirs says so. The first draft of this
// skill listed the directory anyway -- carried over from `shared-content`, which was
// written for the other harness -- which is exactly the failure the file's own closing
// paragraph warns about: acting on a capability a document named rather than one the
// machine has.
func TestTheGanglionNoteDoesNotInventASecretsDirectory(t *testing.T) {
	t.Parallel()
	for _, dir := range ganglionWorkspaceDirs {
		if dir == ".secrets" {
			t.Fatal("a ganglion workspace now HAS .secrets/; the skill has to say so")
		}
	}
	body := readManaged(t, managedGanglionRel)
	if !strings.Contains(body, "no `.secrets/` here") {
		t.Error("the ganglion note no longer says the secrets directory is absent")
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
