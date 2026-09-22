package docker

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// sweepDir builds a user directory with a workspace in it.
func sweepDir(t *testing.T, files map[string]string) (*Manager, string) {
	t.Helper()
	ud := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ud, config.MainWorkspace), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range files {
		full := filepath.Join(ud, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Manager{cfg: &config.Config{}, logf: func(string, ...any) {}}, ud
}

// shippedSkill returns the template's own SKILL.md for a skill, as the binary
// carries it.
func shippedSkill(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(defaultTemplateFS, path.Join(
		"defaulttemplate", config.HarnessPicoclaw, "workspace", "skills", name, "SKILL.md"))
	if err != nil {
		t.Fatalf("the template ships no %s: %v", name, err)
	}
	return string(b)
}

// backupOf returns the single timestamped backup directory, or "".
func backupOf(t *testing.T, ud string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(ud, PicoclawBackupDir))
	if err != nil {
		return ""
	}
	if len(entries) != 1 {
		t.Fatalf("want one backup directory, got %d", len(entries))
	}
	return filepath.Join(ud, PicoclawBackupDir, entries[0].Name())
}

// THE WORKSPACES THIS SERVES. An account that was picoclaw before the
// deployment moved to the ganglion carries the harness's own config, its secret
// sink, its cron store and its native skills -- none of which the ganglion
// reads, and two of which sit above the workspace where the container cannot
// see them at all.
func TestAMigratedWorkspaceGivesUpPicoclawsFiles(t *testing.T) {
	m, ud := sweepDir(t, map[string]string{
		"config.json":                              `{"agents":{}}`,
		".security.yml":                            "channel_list:\n",
		"workspace/.secrets/openai":                "sk-x",
		"workspace/cron/jobs.json":                 "[]",
		"workspace/skills/tmux/SKILL.md":           shippedSkill(t, "tmux"),
		"workspace/skills/picoclaw-agent/SKILL.md": shippedSkill(t, "picoclaw-agent"),
	})

	moved, err := m.sweepPicoclawLeftovers(ud)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if moved != 6 {
		t.Errorf("moved %d, want all six", moved)
	}
	for _, rel := range []string{
		"config.json", ".security.yml", "workspace/.secrets",
		"workspace/cron", "workspace/skills/tmux", "workspace/skills/picoclaw-agent",
	} {
		if _, err := os.Lstat(filepath.Join(ud, filepath.FromSlash(rel))); err == nil {
			t.Errorf("%s is still in the workspace", rel)
		}
	}
	// The content survives the move; this is a backup, not a delete.
	b, err := os.ReadFile(filepath.Join(backupOf(t, ud), "workspace", ".secrets", "openai"))
	if err != nil || string(b) != "sk-x" {
		t.Errorf("the backed-up secret is gone: %q %v", b, err)
	}
}

// THE LOAD-BEARING CHECK. `skills/github` is either the template's copy or a
// skill this member's agent wrote -- evolution's apply mode writes to exactly
// that path -- and a name cannot tell them apart.
func TestASkillTheMemberEditedIsTheirs(t *testing.T) {
	edited := shippedSkill(t, "github") + "\n\nOur team uses a self-hosted instance.\n"
	m, ud := sweepDir(t, map[string]string{
		"workspace/skills/github/SKILL.md": edited,
	})

	moved, err := m.sweepPicoclawLeftovers(ud)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if moved != 0 {
		t.Fatalf("moved %d; an edited skill is the member's", moved)
	}
	b, _ := os.ReadFile(filepath.Join(ud, "workspace", "skills", "github", "SKILL.md"))
	if string(b) != edited {
		t.Error("the member's edit did not survive")
	}
}

// A skill the member's agent wrote from nothing shares no name with the
// template and must be invisible to this.
func TestASkillTheAgentInventedIsUntouched(t *testing.T) {
	m, ud := sweepDir(t, map[string]string{
		"workspace/skills/seed-trial-notes/SKILL.md": "---\nname: seed-trial-notes\n---\n",
	})
	if moved, err := m.sweepPicoclawLeftovers(ud); err != nil || moved != 0 {
		t.Errorf("moved %d (%v); a skill nobody shipped is not a leftover", moved, err)
	}
}

// `skill-creator` is the one template name that is ALSO a live read-only
// mountpoint, on both harnesses. Excluded by name rather than left to the byte
// comparison: the two files differ today, so a comparison would leave it alone
// now and move it out from under a running mount the moment they converged.
func TestSkillCreatorIsNeverSwept(t *testing.T) {
	for _, name := range picoclawTemplateSkills {
		if name == "skill-creator" {
			t.Fatal("skill-creator is in the sweep list; it is a live mountpoint")
		}
	}
	m, ud := sweepDir(t, map[string]string{
		"workspace/skills/skill-creator/SKILL.md": shippedSkill(t, "skill-creator"),
	})
	if moved, _ := m.sweepPicoclawLeftovers(ud); moved != 0 {
		t.Errorf("moved %d; skill-creator is mounted, not a leftover", moved)
	}
}

// EVERYTHING THE GANGLION ACTUALLY USES. Each of these is shared with picoclaw
// or is the member's own, and moving any of them is the failure this feature
// would be worst at: silent, and only visible as the agent forgetting.
func TestItNeverTouchesWhatTheGanglionReads(t *testing.T) {
	keep := map[string]string{
		"workspace/AGENT.md":                           "you are a crab",
		"workspace/SOUL.md":                            "picoclaw's, but a live mount",
		"workspace/HEARTBEAT.md":                       "picoclaw's, but a live mount",
		"workspace/USER.md":                            "what the agent learned",
		"workspace/memory/MEMORY.md":                   "its own notes",
		"workspace/memory/MEMORY_CUSTOM.md":            "the member's standing notes",
		"workspace/memory/FILE_DELIVERY.md":            "a managed mount",
		"workspace/sessions/s1.jsonl":                  `{"role":"user"}`,
		"workspace/windows/s1.json":                    "{}",
		"workspace/public/attachments/a.txt":           "delivered",
		"workspace/skills/shared-content/SKILL.md":     "a managed mount",
		"workspace/skills/ganglion-workspace/SKILL.md": "a managed mount",
		"workspace/skills/scheduled-tasks/SKILL.md":    "a managed mount, conditionally",
		"workspace/shared-skills/x/SKILL.md":           "the admin's, mounted",
		"workspace/.shared/tenant/note.md":             "published by an admin",
		"logs/old.log":                                 "unread by anything, and protected by another sweep's test",
		".crab-ganglion.json":                          "the proxy's token",
		".schedules.json":                              "the ganglion's cron store",
	}
	m, ud := sweepDir(t, keep)

	if moved, err := m.sweepPicoclawLeftovers(ud); err != nil || moved != 0 {
		t.Fatalf("moved %d (%v); none of these is picoclaw-only cruft", moved, err)
	}
	for rel, body := range keep {
		b, err := os.ReadFile(filepath.Join(ud, filepath.FromSlash(rel)))
		if err != nil || string(b) != body {
			t.Errorf("%s did not survive the sweep: %v", rel, err)
		}
	}
}

// A workspace created as a ganglion never had any of this, and that is the
// ordinary case. It must cost nothing and say nothing.
func TestAGanglionNativeWorkspaceIsUntouched(t *testing.T) {
	m, ud := sweepDir(t, nil)
	moved, err := m.sweepPicoclawLeftovers(ud)
	if err != nil || moved != 0 {
		t.Errorf("moved %d (%v) in a workspace that never saw picoclaw", moved, err)
	}
	if _, err := os.Stat(filepath.Join(ud, PicoclawBackupDir)); err == nil {
		t.Error("an empty sweep created a backup directory")
	}
}

// Idempotent: the second pass finds nothing because the first moved it.
func TestASecondSweepFindsNothing(t *testing.T) {
	m, ud := sweepDir(t, map[string]string{"config.json": "{}"})
	if moved, _ := m.sweepPicoclawLeftovers(ud); moved != 1 {
		t.Fatal("the first sweep did not move the config")
	}
	if moved, err := m.sweepPicoclawLeftovers(ud); err != nil || moved != 0 {
		t.Errorf("the second sweep moved %d (%v)", moved, err)
	}
}

// The backup is ABOVE the workspace, which is the whole of its safety: the
// ganglion mounts workspace/ and nothing over it, so a file moved here is
// unreachable from the container rather than merely moved aside. Inside
// memory/ it would be worse than leaving it -- workspaceSections reads every
// .md there and would put it back in the prompt.
func TestTheBackupIsOutsideEveryBind(t *testing.T) {
	if strings.HasPrefix(PicoclawBackupDir, config.MainWorkspace) {
		t.Fatalf("the backup dir %q is inside the workspace", PicoclawBackupDir)
	}
	m, ud := sweepDir(t, map[string]string{"workspace/cron/jobs.json": "[]"})
	if _, err := m.sweepPicoclawLeftovers(ud); err != nil {
		t.Fatal(err)
	}
	if got := backupOf(t, ud); !strings.HasPrefix(got, filepath.Join(ud, PicoclawBackupDir)) {
		t.Errorf("backup landed at %s", got)
	}
}

// The same boundary WriteMemory documents, and the attack it actually stops:
// `workspace` itself is a component the agent can replace, and this runs as
// root. A plain os.Rename would follow the link and move another tree's files
// into this member's backup directory.
func TestTheSweepCannotBeRedirectedBySymlink(t *testing.T) {
	ud := t.TempDir()
	elsewhere := t.TempDir()
	victim := filepath.Join(elsewhere, "cron", "jobs.json")
	if err := os.MkdirAll(filepath.Dir(victim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("another tree's"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(ud, config.MainWorkspace)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	m := &Manager{cfg: &config.Config{}, logf: func(string, ...any) {}}

	moved, err := m.sweepPicoclawLeftovers(ud)

	if moved != 0 {
		t.Errorf("moved %d through a symlinked workspace", moved)
	}
	if err == nil {
		t.Error("a redirected move was reported as a clean sweep")
	}
	if b, rerr := os.ReadFile(victim); rerr != nil || string(b) != "another tree's" {
		t.Errorf("the sweep followed the link and took another tree's file: %q %v", b, rerr)
	}
}
