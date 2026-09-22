package docker

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// PicoclawBackupDir is where a migrated workspace's picoclaw leftovers are put.
//
// ABOVE the workspace, which is the whole of its safety. The ganglion mounts
// <userDir>/workspace and nothing over it, so a file moved here is unreachable
// from inside the container rather than merely moved aside -- and specifically
// not inside memory/, where workspaceSections reads every .md and a backed-up
// SOUL.md would land straight back in the prompt it was taken out of.
const PicoclawBackupDir = ".picoclaw-backup"

// picoclawUserFiles are picoclaw's own files in the user directory. Neither is
// read for a ganglion: harnessConfigFile resolves .ganglion-config.json instead
// of config.json, and provisionGanglion's doc says the .security.yml sink "is
// simply unused".
var picoclawUserFiles = []string{"config.json", ".security.yml"}

// picoclawWorkspaceDirs are picoclaw-only directories inside the workspace.
//
// ganglionWorkspaceDirs says a ganglion workspace has "no cron/ ... and no
// .secrets/", and config.CronFile says picoclaw owns the cron file outright --
// a ganglion's schedules live in <userDir>/.schedules.json, above the bind.
var picoclawWorkspaceDirs = []string{".secrets", "cron"}

// picoclawTemplateSkills are the template's own skill directories.
//
// A NAME HERE IS NOT ENOUGH TO MOVE ONE. `skills/github` in a member's
// workspace is either this template's copy or a skill their agent wrote --
// evolution's apply mode writes to exactly that path -- and a name cannot tell
// them apart. Each candidate's SKILL.md is compared against the embedded
// template byte for byte, and anything edited is the member's.
//
// `skill-creator` is deliberately absent. The template ships one AND
// managedSkillCreatorRel mounts one read-only at the same path, to both
// harnesses: it is the single template name that is also a live mountpoint.
// Leaving it out by name rather than trusting the byte comparison is the
// difference between "different today" and "safe" -- the two files converging
// would turn a passing comparison into a move out from under a running mount.
var picoclawTemplateSkills = []string{
	"agent-browser", "deliver-file", "github", "hardware",
	"picoclaw-agent", "summarize", "tmux", "weather",
}

// sweepPicoclawLeftovers moves picoclaw's files out of a migrated ganglion
// workspace and returns how many it moved.
//
// ONLY A MIGRATED WORKSPACE HAS ANY. EnsureRunning returns before provision()
// for a ganglion, so a workspace created as one never received the template;
// this serves the accounts that were picoclaw before the deployment moved, and
// that population is finite and shrinking. For everyone else it is a handful of
// stat calls that find nothing.
//
// Confined through an os.Root anchored at the user directory: `workspace`,
// `skills` and every leaf are components the agent can replace with a symlink,
// and this runs as root. os.Root.Rename keeps the move inside that boundary, so
// a swapped component fails the syscall instead of redirecting a root-owned
// move somewhere the agent chose.
//
// A path that cannot move does not stop the others and does not fail the
// ensure. The member is trying to have a conversation, and a config file
// staying where it has sat for months is not worth refusing that.
func (m *Manager) sweepPicoclawLeftovers(userDir string) (moved int, err error) {
	candidates := m.picoclawLeftovers(userDir)
	if len(candidates) == 0 {
		return 0, nil
	}

	tree, err := openTree(userDir)
	if err != nil {
		return 0, err
	}
	defer tree.Close()

	// One directory per sweep rather than one for all time: a member who
	// restores something by hand would otherwise make the next pass decide which
	// copy is current, which is the guess this whole file refuses to make.
	stamp := path.Join(PicoclawBackupDir, time.Now().UTC().Format("20060102T150405Z"))

	var stuck []string
	for _, rel := range candidates {
		dst := path.Join(stamp, rel)
		// A destination that already exists is left alone rather than merged or
		// replaced. MigrateProjects states the reason and it holds here: both
		// are guesses about which copy is current.
		if _, serr := tree.root.Lstat(dst); serr == nil {
			stuck = append(stuck, rel)
			continue
		}
		if merr := tree.root.MkdirAll(path.Dir(dst), 0o700); merr != nil {
			stuck = append(stuck, rel)
			continue
		}
		if rerr := tree.root.Rename(rel, dst); rerr != nil {
			stuck = append(stuck, rel)
			continue
		}
		moved++
	}
	if len(stuck) > 0 {
		return moved, fmt.Errorf("left in place: %v", stuck)
	}
	return moved, nil
}

// picoclawLeftovers lists the paths, relative to the user directory, that this
// workspace holds and picoclaw alone put there.
//
// POSITIVE IDENTIFICATION, never a blocklist. sweepGanglionProjects already
// sets that rule for the directory it owns, and the cost of a false positive
// here is a member's work rather than a stale directory.
func (m *Manager) picoclawLeftovers(userDir string) []string {
	var out []string
	add := func(rel string) {
		if _, err := os.Lstat(filepath.Join(userDir, filepath.FromSlash(rel))); err == nil {
			out = append(out, rel)
		}
	}
	for _, name := range picoclawUserFiles {
		add(name)
	}
	for _, name := range picoclawWorkspaceDirs {
		add(path.Join(config.MainWorkspace, name))
	}
	for _, name := range picoclawTemplateSkills {
		rel := path.Join(config.MainWorkspace, "skills", name)
		if untouchedTemplateSkill(userDir, rel, name) {
			out = append(out, rel)
		}
	}
	return out
}

// untouchedTemplateSkill reports whether the skill at rel is this repository's
// own copy, unedited.
//
// Read from the EMBEDDED template rather than from the template directory on
// disk: an operator may have replaced that, and the question being asked is
// "did we ship this file", which only the binary can answer.
func untouchedTemplateSkill(userDir, rel, name string) bool {
	on, err := os.ReadFile(filepath.Join(userDir, filepath.FromSlash(rel), "SKILL.md"))
	if err != nil {
		return false
	}
	shipped, err := fs.ReadFile(defaultTemplateFS, path.Join(
		"defaulttemplate", config.HarnessPicoclaw, "workspace", "skills", name, "SKILL.md"))
	if err != nil {
		return false
	}
	return bytes.Equal(on, shipped)
}
