package memgraph

import (
	"path/filepath"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
)

// GraphDirName is the per-user directory holding the knowledge graph.
//
// It sits at the ROOT of the user's picoclaw data dir — i.e. ~/.picoclaw/ inside
// the container — and deliberately NOT under workspace/. The agent's own
// workspace is ~/.picoclaw/workspace with restrict_to_workspace: true and
// allow_read_outside_workspace: false, so a path above it is unreachable by the
// agent's file tools.
//
// Contrast internal/docker/memory.go, which puts MEMORY_CUSTOM.md inside the
// workspace and chowns it to picoclawUser precisely BECAUSE the agent must read
// it. Nothing in this package chowns anything: the proxy is the only writer and
// the only reader, so the directory stays root-owned at 0700 and the non-root
// container process cannot traverse it — not via file tools, and not via a shell
// either. See context.md D-2 for the one configuration where that still leaks
// (picoclawUser: "", containers as root).
const GraphDirName = "memory-graph"

// GraphFileName is the graph itself. The contents are JSONL (see graph.go);
// upstream calls the equivalent file memory.json despite the same format, and the
// honest extension is worth more here than mirroring that name.
const GraphFileName = "memory.jsonl"

// Dir is the directory holding one workspace's graph.
//
// A project gets its OWN directory, a sibling of the main one. Still above
// workspace/ and still root-owned, so the reasoning in GraphDirName's comment
// holds unchanged for every project: the agent cannot reach any memory.jsonl
// through its file tools or a shell, its own included.
func (s *Store) Dir(sc Scope) string {
	dir := GraphDirName
	if sc.Project != "" {
		dir = GraphDirName + "-" + identity.SanitizeID(sc.Project)
	}
	return filepath.Join(
		config.UserWorkspace(s.root, sc.TenantID, sc.SubsAccID, sc.Role, sc.UserAccID),
		dir,
	)
}

// Path is the graph file for one workspace.
func (s *Store) Path(sc Scope) string {
	return filepath.Join(s.Dir(sc), GraphFileName)
}
