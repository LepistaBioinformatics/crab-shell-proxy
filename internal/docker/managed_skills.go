package docker

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

// Operator-managed workspace content, bind-mounted read-only into every
// container so the agent can neither alter it nor keep an edit past a restart.
// Relative to the container-side ManagedSkillsDir:
//
//	skills/<managedSkillName>/  -> workspace/skills/<managedSkillName> (guidance)
//	memory/<managedMemoryFile>  -> workspace/memory/<managedMemoryFile> (recovery)
const (
	managedSkillRel  = "skills/shared-content"
	managedMemoryRel = "memory/CONTEXT_RECOVERY.md"
	// managedRoutingRel tells the agent WHICH memory to write to — the knowledge
	// graph for facts, MEMORY.md for its own notes — and forbids claiming a save it
	// did not make.
	//
	// A managed MEMORY file rather than a skill, deliberately. Skills are loaded by
	// relevance: the agent has to decide to look for one, so a rule that must apply on
	// every turn would only be found when the agent was already thinking about memory
	// — the moment it least needs the reminder. Same reason the memory-graph tools are
	// not registered `deferred`. picoclaw reads the memory dir every turn.
	//
	// Observed, not assumed: the tools were available for two turns and the model used
	// `append_file` on MEMORY.md instead, then told the user it had also written to the
	// graph. It only used the graph after an instruction naming the tools.
	managedRoutingRel = "memory/MEMORY_ROUTING.md"
	// managedDeliveryRel is the hard rule about WHERE a produced file goes:
	// public/attachments/, the only directory the member's interface lists.
	//
	// Unconditional, unlike the routing note: it depends on no tool and no config.
	// A file written outside public/ is invisible to the member no matter how this
	// deployment is set up, so there is no build of it where this advice is wrong.
	//
	// It also carries the reason the agent must NAME the path in its own reply: the
	// 📎 notice the proxy appends is stream-only and is never persisted, so after a
	// reload the only account of a delivered file is whatever the model itself wrote
	// (picoclaw's own line is "Requested output delivered via tool attachment.",
	// which names nothing).
	managedDeliveryRel = "memory/FILE_DELIVERY.md"
)

//go:embed managed
var managedFS embed.FS

// managedContentBinds are the read-only bind specs for the operator-managed content, in
// a stable order.
//
// A pure function of the two paths and one flag, so it is testable without a container
// and without root: the TestCreate* family cannot run here (chown needs privileges),
// which is exactly why the mount list is built somewhere a test can reach it.
//
// The routing note is included ONLY when the memory graph is switched on. With no
// CRAB_MCP_TOKEN_SECRET the agent has no mcp_memory_* tools at all, and a file
// instructing it to prefer them would be actively wrong — worse than silent.
func managedContentBinds(managedBase, mountDest string, memoryGraphEnabled bool) []string {
	rels := []string{managedSkillRel, managedMemoryRel, managedDeliveryRel}
	if memoryGraphEnabled {
		rels = append(rels, managedRoutingRel)
	}
	out := make([]string, 0, len(rels))
	for _, rel := range rels {
		out = append(out, filepath.Join(managedBase, rel)+":"+mountDest+"/workspace/"+rel+":ro")
	}
	return out
}

// materializeManagedContent writes the embedded managed tree into dst (the
// container-side ManagedSkillsDir), overwriting any prior copy so the canonical
// operator version is what gets bind-mounted, and chowns it to the agent user so
// the read-only binds are readable by the non-root process.
func materializeManagedContent(dst, user string) error {
	const root = "managed"
	err := fs.WalkDir(managedFS, root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := managedFS.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		return err
	}
	return chownTree(dst, user)
}
