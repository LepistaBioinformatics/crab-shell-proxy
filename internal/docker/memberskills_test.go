package docker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

func skillDoc(name, desc string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\n\nbody\n"
}

func memberSkillFixture(t *testing.T) (*Manager, WorkspaceKey, string) {
	t.Helper()
	root := t.TempDir()
	m := &Manager{cfg: &config.Config{ContainerDataRoot: root}, logf: func(string, ...any) {}}
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	seg := filepath.Join(
		config.UserWorkspace(root, key.TenantID, key.SubsAccID, key.Role, key.UserAccID),
		config.MainWorkspace,
	)
	if err := os.MkdirAll(filepath.Join(seg, SkillsDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	return m, key, seg
}

func writeSkillOnDisk(t *testing.T, dir, name, desc string) {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, skillDocName), []byte(skillDoc(name, desc)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The reserved set must cover every name managedContentBinds can ever mount.
//
// The two lists are built from the same rels, and this is what keeps them that
// way: a fifth managed skill added to the bind builder and forgotten here would
// be a name a member could still create, and their skill would vanish under the
// new read-only bind on the next container start.
func TestManagedSkillNamesCoverEveryBind(t *testing.T) {
	for _, harness := range []string{config.HarnessPicoclaw, config.HarnessGanglion} {
		for _, graph := range []bool{false, true} {
			for _, bind := range managedContentBinds("/base", "/mnt", harness, graph) {
				src, dest, _ := strings.Cut(bind, ":")
				if !strings.Contains(dest, "/"+config.MainWorkspace+"/"+SkillsDirName+"/") {
					continue
				}
				if !isManagedSkillName(filepath.Base(src)) {
					t.Errorf("managed skill %q (harness=%s graph=%v) is not in managedSkillNames",
						filepath.Base(src), harness, graph)
				}
			}
		}
	}
}

// The gating that decides what is MOUNTED must not decide what is RESERVED.
//
// scheduled-tasks is bound only when the MCP token exists. A deployment that
// turns the token on later would otherwise bind over a member skill that was
// legitimately created while it was off.
func TestEveryManagedNameIsReservedRegardlessOfGating(t *testing.T) {
	m, key, _ := memberSkillFixture(t)
	for _, name := range managedSkillNames() {
		_, err := m.WriteMemberSkillFile(key, config.HarnessGanglion, name, "", skillDoc(name, "mine"), "")
		if !errors.Is(err, ErrSkillReadOnly) && !errors.Is(err, ErrReservedSkillName) {
			t.Errorf("write %q = %v, want a read-only refusal", name, err)
		}
		if err := m.DeleteMemberSkill(key, config.HarnessGanglion, name); !errors.Is(err, ErrSkillReadOnly) &&
			!errors.Is(err, ErrReservedSkillName) {
			t.Errorf("delete %q = %v, want a read-only refusal", name, err)
		}
	}
}

// A managed name is never reported as the member's own, and the case that
// matters is not the empty mountpoint runc leaves behind.
//
// The hazard is a member skill that PREDATES the bind: someone created `notes`,
// the operator later shipped a managed skill of the same name, and now a
// read-only bind sits on top of it. On disk the member's file is intact and
// complete; in the container it is invisible, and the agent loads the
// operator's. Listing it as the member's own would offer an edit control over a
// file that cannot affect anything, and a save that reports success and changes
// nothing.
//
// Seeded with a VALID SKILL.md for exactly that reason: an empty stub is
// discarded by the frontmatter parse anyway, so a fixture built from empty dirs
// would pass whether the skip existed or not.
func TestAMemberSkillHiddenBeneathAManagedBindIsNotListedAsTheirs(t *testing.T) {
	m, key, seg := memberSkillFixture(t)
	skills := filepath.Join(seg, SkillsDirName)
	for _, name := range managedSkillNames() {
		writeSkillOnDisk(t, skills, name, "written before the bind existed")
	}
	writeSkillOnDisk(t, skills, "mine", "a real one")

	got, err := m.ListMemberSkills(key, config.HarnessGanglion)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, s := range got {
		if s.Origin == SkillOriginMember && s.Name != "mine" {
			t.Errorf("listed %q as the member's own; it is hidden beneath a managed bind", s.Name)
		}
	}
}

// The three layers, the member's first, each carrying its origin.
func TestTheThreeLayersAreListedMemberFirst(t *testing.T) {
	m, key, seg := memberSkillFixture(t)
	writeSkillOnDisk(t, filepath.Join(seg, SkillsDirName), "mine", "the member's")

	shared := config.EffectiveSkillsDir(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role)
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSkillOnDisk(t, shared, "house-style", "the administrator's")

	got, err := m.ListMemberSkills(key, config.HarnessGanglion)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("got %d skills, want the member's and the administrator's", len(got))
	}
	if got[0].Name != "mine" || got[0].Origin != SkillOriginMember {
		t.Errorf("first row = %q/%s, want mine/member", got[0].Name, got[0].Origin)
	}
	var sharedRow *MemberSkill
	for i := range got {
		if got[i].Name == "house-style" {
			sharedRow = &got[i]
		}
	}
	if sharedRow == nil || sharedRow.Origin != SkillOriginShared {
		t.Fatalf("administrator's skill missing or mis-labelled: %+v", sharedRow)
	}
}

// A member skill whose DECLARED name collides with an administrator's loses.
// The harness logs it as shadowed and loads the administrator's; the list has to
// say so, or the member edits a file with no effect and nothing reports it.
func TestAShadowedMemberSkillSaysSo(t *testing.T) {
	m, key, seg := memberSkillFixture(t)
	writeSkillOnDisk(t, filepath.Join(seg, SkillsDirName), "house-style", "mine")

	shared := config.EffectiveSkillsDir(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role)
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSkillOnDisk(t, shared, "house-style", "the administrator's")

	got, err := m.ListMemberSkills(key, config.HarnessGanglion)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, s := range got {
		if s.Origin == SkillOriginMember && s.Name == "house-style" {
			found = true
			if !s.Shadowed {
				t.Error("the member's copy is not marked shadowed")
			}
		}
	}
	if !found {
		t.Fatal("the member's copy is missing from the list")
	}
}

// picoclaw's collision precedence has not been verified in this chain, so the
// flag is not claimed there. A badge that is right on one harness and a guess on
// the other is worse than no badge.
func TestShadowingIsNotClaimedForPicoclaw(t *testing.T) {
	m, key, seg := memberSkillFixture(t)
	writeSkillOnDisk(t, filepath.Join(seg, SkillsDirName), "house-style", "mine")
	shared := config.EffectiveSkillsDir(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role)
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSkillOnDisk(t, shared, "house-style", "the administrator's")

	got, err := m.ListMemberSkills(key, config.HarnessPicoclaw)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, s := range got {
		if s.Origin == SkillOriginMember && s.Shadowed {
			t.Error("shadowing claimed on picoclaw, whose precedence is unverified")
		}
	}
}

// Everything the harness would silently ignore is refused at the door instead.
func TestAWriteTheHarnessCouldNotLoadIsRefused(t *testing.T) {
	m, key, _ := memberSkillFixture(t)
	cases := []struct {
		name    string
		skill   string
		content string
		want    error
	}{
		{"a declared name that disagrees with the directory", "notes",
			skillDoc("something-else", "d"), ErrSkillNameMismatch},
		{"an empty description", "notes",
			"---\nname: notes\ndescription:\n---\nbody\n", ErrSkillMetadata},
		{"no frontmatter at all", "notes", "just a body\n", ErrSkillMetadata},
		{"an unusable directory name", "../escape", skillDoc("escape", "d"), ErrInvalidSkillName},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := m.WriteMemberSkillFile(key, config.HarnessGanglion, c.skill, "", c.content, ""); !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
}

// The harness's evolution writes this same directory in apply mode. An
// unconditional write would drop a skill the agent learned with no trace.
func TestAStaleModifiedAtConflicts(t *testing.T) {
	m, key, _ := memberSkillFixture(t)
	first, err := m.WriteMemberSkillFile(key, config.HarnessGanglion, "notes", "", skillDoc("notes", "v1"), "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := m.WriteMemberSkillFile(key, config.HarnessGanglion, "notes", "", skillDoc("notes", "v2"),
		"1999-01-01T00:00:00Z"); !errors.Is(err, ErrSkillConflict) {
		t.Errorf("stale write = %v, want ErrSkillConflict", err)
	}
	if _, err := m.WriteMemberSkillFile(key, config.HarnessGanglion, "notes", "", skillDoc("notes", "v2"),
		first.ModifiedAt); err != nil {
		t.Errorf("write with the read version = %v, want success", err)
	}
}

// A create is a write with no version, so it must not silently replace.
func TestACreateOverAnExistingNameIsRefused(t *testing.T) {
	m, key, _ := memberSkillFixture(t)
	if _, err := m.WriteMemberSkillFile(key, config.HarnessGanglion, "notes", "", skillDoc("notes", "v1"), ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := m.WriteMemberSkillFile(key, config.HarnessGanglion, "notes", "", skillDoc("notes", "v2"), ""); !errors.Is(err, ErrSkillExists) {
		t.Errorf("second create = %v, want ErrSkillExists", err)
	}
}

func TestDeleteRemovesTheSkillAndItsFiles(t *testing.T) {
	m, key, seg := memberSkillFixture(t)
	skills := filepath.Join(seg, SkillsDirName)
	writeSkillOnDisk(t, skills, "notes", "d")
	if err := os.WriteFile(filepath.Join(skills, "notes", "extra.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteMemberSkill(key, config.HarnessGanglion, "notes"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(skills, "notes")); !os.IsNotExist(err) {
		t.Errorf("skill dir still present: %v", err)
	}
	if err := m.DeleteMemberSkill(key, config.HarnessGanglion, "notes"); !errors.Is(err, ErrMediaNotFound) {
		t.Errorf("second delete = %v, want ErrMediaNotFound", err)
	}
}

// The agent owns this directory and can put a symlink in it. A root-written,
// caller-supplied SKILL.md landing at a path the AGENT chose is the escalation
// the os.Root confinement closes -- the same one media_root_test.go pins for
// memory.
func TestAMemberSkillWriteCannotBeRedirectedBySymlink(t *testing.T) {
	m, key, seg := memberSkillFixture(t)
	elsewhere := t.TempDir()
	if err := os.RemoveAll(filepath.Join(seg, SkillsDirName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(seg, SkillsDirName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := m.WriteMemberSkillFile(key, config.HarnessGanglion, "notes", "", skillDoc("notes", "d"), ""); err == nil {
		t.Fatal("write through a symlinked skills dir succeeded")
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "notes", skillDocName)); !os.IsNotExist(err) {
		t.Errorf("the write landed outside the workspace: %v", err)
	}
}

// The read side of the same swap: serving whatever the link points at would be
// arbitrary-file disclosure through the skills panel.
func TestAMemberSkillReadCannotBeRedirectedBySymlink(t *testing.T) {
	m, key, seg := memberSkillFixture(t)
	elsewhere := t.TempDir()
	writeSkillOnDisk(t, elsewhere, "notes", "someone else's")
	if err := os.RemoveAll(filepath.Join(seg, SkillsDirName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(seg, SkillsDirName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	content, _, _, err := m.ReadMemberSkillFile(key, config.HarnessGanglion, "notes", "")
	if err == nil && strings.Contains(content, "someone else's") {
		t.Fatal("read followed the symlink out of the workspace")
	}
}

// THE ROW A LISTING PUBLISHES MUST BE THE ROW A READ AND A DELETE RESOLVE.
//
// A skill's DIRECTORY and its frontmatter `name` need not agree. Writes through
// this API refuse a disagreement, but nothing else does: `skill-creator` teaches
// the agent to write a skill with the shell, and picoclaw's template seeds nine
// of them. The first version of this code published the DECLARED name and
// resolved by DIRECTORY, so such a row 404'd the moment it was opened -- falling
// through to the administrator's layer, where it also did not exist -- and could
// not be deleted either.
func TestARowTheListPublishesCanBeOpenedAndDeleted(t *testing.T) {
	m, key, seg := memberSkillFixture(t)
	dir := filepath.Join(seg, SkillsDirName, "weekly-digest")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, skillDocName),
		[]byte(skillDoc("resumo-semanal", "friday digest")), 0o600); err != nil {
		t.Fatal(err)
	}

	list, err := m.ListMemberSkills(key, config.HarnessGanglion)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var row *MemberSkill
	for i := range list {
		if list[i].Origin == SkillOriginMember {
			row = &list[i]
		}
	}
	if row == nil {
		t.Fatal("the member's skill is missing from the list")
	}
	if row.Name != "weekly-digest" {
		t.Errorf("list published %q; the directory is the identity a read resolves", row.Name)
	}
	if _, _, _, err := m.ReadMemberSkillFile(key, config.HarnessGanglion, row.Name, ""); err != nil {
		t.Errorf("opening the published row = %v", err)
	}
	if err := m.DeleteMemberSkill(key, config.HarnessGanglion, row.Name); err != nil {
		t.Errorf("deleting the published row = %v", err)
	}
}

// Shadowing is decided on the DECLARED name on BOTH sides, because that is the
// key the harness dedups by. Comparing directories would miss this collision
// entirely: the two directories differ and the two declared names do not.
func TestShadowingComparesDeclaredNamesOnBothSides(t *testing.T) {
	m, key, seg := memberSkillFixture(t)
	mine := filepath.Join(seg, SkillsDirName, "my-house-style")
	if err := os.MkdirAll(mine, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mine, skillDocName),
		[]byte(skillDoc("house-style", "mine")), 0o600); err != nil {
		t.Fatal(err)
	}
	shared := config.EffectiveSkillsDir(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role)
	admins := filepath.Join(shared, "corporate-style")
	if err := os.MkdirAll(admins, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(admins, skillDocName),
		[]byte(skillDoc("house-style", "the administrator's")), 0o600); err != nil {
		t.Fatal(err)
	}

	list, err := m.ListMemberSkills(key, config.HarnessGanglion)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, s := range list {
		if s.Origin == SkillOriginMember && s.Name == "my-house-style" && !s.Shadowed {
			t.Error("the collision was missed: both declare house-style, so the agent loads the administrator's")
		}
	}
}

// picoclaw SHIPS skills carrying `metadata:` and `homepage:` -- seven of the nine
// in its own workspace template -- so its loader accepts them. An earlier draft
// refused a third key and would have stopped a picoclaw member from saving any
// edit to the skills they were seeded with.
func TestExtraFrontmatterKeysAreAccepted(t *testing.T) {
	m, key, _ := memberSkillFixture(t)
	doc := "---\nname: notes\ndescription: with more than two keys\nmetadata:\n  version: 1\nhomepage: https://example.invalid\n---\n\nbody\n"
	if _, err := m.WriteMemberSkillFile(key, config.HarnessGanglion, "notes", "", doc, ""); err != nil {
		t.Fatalf("write = %v, want success: picoclaw's own template ships these keys", err)
	}
}

// The operator layer had no coverage at all, and its absence would have looked
// like the intended design rather than a wrong root. This is also the only check
// that ensureManagedContent writes under the root ListMemberSkills reads from
// (DEC-10, in the other direction).
func TestTheManagedLayerIsListedFromTheRootItIsMaterializedInto(t *testing.T) {
	m, key, _ := memberSkillFixture(t)
	if err := materializeManagedContent(config.ManagedSkillsDir(m.cfg.ContainerDataRoot), ""); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	list, err := m.ListMemberSkills(key, config.HarnessGanglion)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := map[string]bool{}
	for _, s := range list {
		if s.Origin == SkillOriginManaged {
			seen[s.Name] = true
			if s.Description == "" {
				t.Errorf("managed skill %q listed with no description", s.Name)
			}
		}
	}
	if !seen["skill-creator"] || !seen["shared-content"] {
		t.Errorf("managed layer = %v, want at least skill-creator and shared-content", seen)
	}
}

// --- files inside a skill -------------------------------------------------

func TestASkillsSupportingFilesAreListedWithTheSkillFirst(t *testing.T) {
	m, key, seg := memberSkillFixture(t)
	dir := filepath.Join(seg, SkillsDirName, "report")
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, skillDocName), []byte(skillDoc("report", "d")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "templates", "letter.md"), []byte("Dear"), 0o600); err != nil {
		t.Fatal(err)
	}

	files, origin, err := m.ListMemberSkillFiles(key, config.HarnessGanglion, "report")
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	if origin != SkillOriginMember {
		t.Errorf("origin = %q, want member", origin)
	}
	got := []string{}
	for _, f := range files {
		got = append(got, f.Path)
	}
	if len(got) != 2 || got[0] != skillDocName || got[1] != "templates/letter.md" {
		t.Errorf("files = %v, want [SKILL.md templates/letter.md] — the skill itself comes first", got)
	}
}

func TestASupportingFileIsReadAndWrittenBySubpath(t *testing.T) {
	m, key, _ := memberSkillFixture(t)
	if _, err := m.WriteMemberSkillFile(key, config.HarnessGanglion, "report", "", skillDoc("report", "d"), ""); err != nil {
		t.Fatalf("create skill: %v", err)
	}
	written, err := m.WriteMemberSkillFile(key, config.HarnessGanglion, "report", "templates/letter.md", "Dear", "")
	if err != nil {
		t.Fatalf("create supporting file: %v", err)
	}
	content, file, origin, err := m.ReadMemberSkillFile(key, config.HarnessGanglion, "report", "templates/letter.md")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if content != "Dear" || file.Path != "templates/letter.md" || origin != SkillOriginMember {
		t.Errorf("read = %q %+v %q", content, file, origin)
	}
	// A supporting file is NOT a SKILL.md and is not held to its grammar: a
	// template or a script has no frontmatter and refusing it would defeat the
	// point of a skill being a directory.
	if _, err := m.WriteMemberSkillFile(key, config.HarnessGanglion, "report", "templates/letter.md",
		"Dear reader", written.ModifiedAt); err != nil {
		t.Errorf("editing a supporting file = %v, want success", err)
	}
}

func TestASupportingFilePathCannotLeaveTheSkill(t *testing.T) {
	m, key, _ := memberSkillFixture(t)
	if _, err := m.WriteMemberSkillFile(key, config.HarnessGanglion, "report", "", skillDoc("report", "d"), ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, bad := range []string{"../escape.md", "/etc/passwd", "a/../../b", "./x", "sub/"} {
		if _, err := m.WriteMemberSkillFile(key, config.HarnessGanglion, "report", bad, "x", ""); err == nil {
			t.Errorf("write to %q succeeded", bad)
		}
		if _, _, _, err := m.ReadMemberSkillFile(key, config.HarnessGanglion, "report", bad); err == nil {
			t.Errorf("read of %q succeeded", bad)
		}
	}
}

// Something that is not text comes back LABELLED rather than mangled, so the
// panel can say "not editable here" instead of offering to overwrite an image
// with its own lossy decoding.
func TestANonTextFileIsLabelledRatherThanMangled(t *testing.T) {
	m, key, seg := memberSkillFixture(t)
	dir := filepath.Join(seg, SkillsDirName, "report")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, skillDocName), []byte(skillDoc("report", "d")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logo.png"), []byte{0x89, 'P', 'N', 'G', 0x00, 0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	content, file, _, err := m.ReadMemberSkillFile(key, config.HarnessGanglion, "report", "logo.png")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !file.Binary || content != "" {
		t.Errorf("binary=%v content=%q, want it labelled and withheld", file.Binary, content)
	}
}

// A read-only skill's files are browsable -- a member who cannot see the
// template an administrator's skill fills in cannot tell what it will do -- but
// not writable.
func TestAReadOnlySkillsFilesAreBrowsableAndNotWritable(t *testing.T) {
	m, key, _ := memberSkillFixture(t)
	if err := materializeManagedContent(config.ManagedSkillsDir(m.cfg.ContainerDataRoot), ""); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	files, origin, err := m.ListMemberSkillFiles(key, config.HarnessGanglion, "skill-creator")
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	if origin != SkillOriginManaged || len(files) == 0 {
		t.Fatalf("origin=%q files=%d, want the managed layer listed", origin, len(files))
	}
	if _, _, _, err := m.ReadMemberSkillFile(key, config.HarnessGanglion, "skill-creator", ""); err != nil {
		t.Errorf("reading a managed skill = %v, want success", err)
	}
	if _, err := m.WriteMemberSkillFile(key, config.HarnessGanglion, "skill-creator", "",
		skillDoc("skill-creator", "mine"), ""); !errors.Is(err, ErrSkillReadOnly) {
		t.Errorf("write = %v, want ErrSkillReadOnly", err)
	}
}

// A write whose ifModifiedAt names a version that is GONE must not resurrect it.
// The switch that decides this once fell through to "create" when the file did
// not exist, so a member editing a skill the agent had just deleted recreated it.
func TestEditingSomethingThatWasDeletedConflicts(t *testing.T) {
	m, key, _ := memberSkillFixture(t)
	if _, err := m.WriteMemberSkillFile(key, config.HarnessGanglion, "notes", "", skillDoc("notes", "v1"), ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := m.DeleteMemberSkill(key, config.HarnessGanglion, "notes"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := m.WriteMemberSkillFile(key, config.HarnessGanglion, "notes", "",
		skillDoc("notes", "v2"), "2026-09-01T10:00:00Z"); !errors.Is(err, ErrSkillConflict) {
		t.Errorf("err = %v, want ErrSkillConflict — an edit must not resurrect a deleted skill", err)
	}
}

// The two proxy-owned layers are confined too, and this is what says so.
//
// Nothing can plant a link in them TODAY -- an admin upload is unpacked by
// hardened zip code, the operator's tree is embedded in the binary -- so this
// pins the guarantee rather than a live hole. `name` and `path` come from the
// request, and the validators that refuse traversal are the message; os.Root is
// the guarantee, which is the division media_root.go draws for the member's own
// tree and had no reason to stop at.
func TestAReadOnlyLayerCannotBeWalkedOutOfBySymlink(t *testing.T) {
	m, key, _ := memberSkillFixture(t)
	shared := config.EffectiveSkillsDir(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role)
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "SKILL.md"), []byte("someone else's"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(shared, "house-style")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	content, _, _, err := m.ReadMemberSkillFile(key, config.HarnessGanglion, "house-style", "")
	if err == nil && strings.Contains(content, "someone else's") {
		t.Error("the read followed a link out of the administrator's layer")
	}
	if files, _, err := m.ListMemberSkillFiles(key, config.HarnessGanglion, "house-style"); err == nil && len(files) > 0 {
		t.Errorf("the listing walked out of the layer: %v", files)
	}
}
