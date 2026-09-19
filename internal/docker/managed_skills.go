package docker

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// Operator-managed workspace content, bind-mounted read-only into every
// container so the agent can neither alter it nor keep an edit past a restart.
// Relative to the container-side ManagedSkillsDir:
//
//	skills/<managedSkillName>/  -> workspace/skills/<managedSkillName> (guidance)
//	memory/<managedMemoryFile>  -> workspace/memory/<managedMemoryFile> (recovery)
const (
	managedSkillRel = "skills/shared-content"
	// The SKILL.md format and the two roots it is read from. Both harnesses run
	// the same format — crab-ganglion-harness's `internal/skillfile` says so in
	// its own package comment — so this ships to both, unconditionally: a
	// workspace always has a skills directory and an agent can always write one.
	managedSkillCreatorRel = "skills/skill-creator"
	// GANGLION ONLY, and the one entry in this list that is. It describes an
	// alpine image with busybox and a shell confined to the turn's workspace,
	// which is a description of the WRONG MACHINE for a picoclaw agent — and the
	// file's own closing paragraph is that an agent must act on the tools it has
	// rather than on the ones a document named.
	//
	// It stands in for picoclaw's `picoclaw-agent`, which is bundled in that
	// harness's workspace template. A ganglion agent is provisioned with no
	// template at all (manager.go skips it deliberately), so the managed tree is
	// the only place a harness-specific default can come from.
	managedGanglionRel = "skills/ganglion-workspace"

	// managedScheduleRel tells the agent how to schedule work for itself: what a
	// scheduled run is and is not, that the member approves each one, and the
	// limits it will be refused by.
	//
	// GANGLION ONLY, and only when the schedule tools actually exist. The tools
	// are registered on the MCP server, which is not registered at all without a
	// token secret -- and a skill describing tools the model cannot see is worse
	// than no skill: it will keep reaching for them and reading the refusal as
	// something it did wrong.
	managedScheduleRel = "skills/scheduled-tasks"
	managedMemoryRel   = "memory/CONTEXT_RECOVERY.md"
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
// A pure function of its arguments, so it is testable without a container
// and without root: the TestCreate* family cannot run here (chown needs privileges),
// which is exactly why the mount list is built somewhere a test can reach it.
//
// The routing note is included ONLY when the memory graph is switched on. With no
// CRAB_MCP_TOKEN_SECRET the agent has no mcp_memory_* tools at all, and a file
// instructing it to prefer them would be actively wrong — worse than silent.
//
// `harness` gates on the same principle one level up: a file describing this
// container's shell, its image and its layout is a description of the wrong machine
// for the other harness, and the failure it would produce is the same one — an
// agent acting on a capability it was told about rather than one it has.
func managedContentBinds(managedBase, mountDest, harness string, memoryGraphEnabled bool) []string {
	rels := []string{managedSkillRel, managedSkillCreatorRel, managedMemoryRel, managedDeliveryRel}
	if harness == config.HarnessGanglion {
		rels = append(rels, managedGanglionRel)
		// The schedule tools live on the MCP server, which does not exist
		// without a secret to verify its tokens with.
		if memoryGraphEnabled {
			rels = append(rels, managedScheduleRel)
		}
	}
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
