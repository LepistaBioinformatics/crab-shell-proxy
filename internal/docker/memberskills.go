package docker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// The member's own skills: the third skill layer, and until now the only one
// with no way to reach it.
//
// Three layers reach an agent, and a member sees all three in one list:
//
//	managed  operator content embedded in this binary, bind-mounted :ro
//	shared   the administrator's cascade, merged and bind-mounted :ro
//	member   <UserWorkspace>/workspace/skills -- written by the workspace seed
//	         and by the harness's own evolution, and now by the member
//
// Only the third is writable, and the distinction cannot be made from the
// member's directory alone: the managed binds land INSIDE it (see
// managedContentBinds). Origin is therefore decided here, from the roots, and
// handed to the caller -- never inferred downstream.
//
// A skill is a DIRECTORY, not a file: SKILL.md plus whatever else it needs.
// Everything under it is reachable, because a skill that references a template
// or a script the member cannot see is a skill they cannot understand.
//
// MAIN WORKSPACE ONLY. A project workspace has a skills directory too, but the
// harness's loader is fixed on the main one at boot
// (crab-ganglion-harness/cmd/crab-ganglion/main.go wires Loader.Workspace once),
// so a skill written into workspace-<id>/skills is read by nothing. Offering a
// project-scoped surface would let a member write inert files; see the spec's
// DEC-5 and the finding it records.
const (
	SkillOriginMember  = "member"
	SkillOriginShared  = "shared"
	SkillOriginManaged = "managed"
)

// SkillsDirName is the skills directory inside a workspace. The same name on
// both harnesses -- crab-ganglion-harness/internal/skillfile.DirName.
const SkillsDirName = "skills"

const skillDocName = "SKILL.md"

// skillFileMaxBytes caps one file inside a skill. Generous: only the frontmatter
// reaches the prompt, and everything else is read with the shell when the agent
// decides the skill applies.
const skillFileMaxBytes = 256 << 10

// skillFileMaxCount bounds a listing. A skill is guidance plus its supporting
// files; a directory with thousands of entries is not one, and walking it on
// every panel open would be the member's own workspace used against them.
const skillFileMaxCount = 500

var (
	// ErrSkillReadOnly is a write aimed at a layer the member does not own. Worded
	// like ErrMediaReserved, and a 403 for the same reason: the name is legitimate,
	// the caller simply may not have it.
	ErrSkillReadOnly = errors.New("that skill is managed by the system")
	// ErrSkillNameMismatch is a frontmatter name that disagrees with the directory.
	// The harness dedups by the DECLARED name, so a file in foo/ claiming to be
	// skill-creator would shadow or duplicate an operator skill.
	ErrSkillNameMismatch = errors.New("the frontmatter name must match the skill's name")
	// ErrSkillConflict is a write against a version the caller had not read. The
	// harness's evolution writes this same directory.
	ErrSkillConflict = errors.New("the skill changed since it was read")
	// ErrSkillExists is a create whose name is taken.
	ErrSkillExists = errors.New("a skill with that name already exists")
	// ErrSkillFilePath is a path inside a skill that is not a plain relative one.
	ErrSkillFilePath = errors.New("invalid file path inside the skill")
	// ErrSkillFileBinary is an edit aimed at something that is not text.
	ErrSkillFileBinary = errors.New("that file is not text and cannot be edited here")
)

// MemberSkill is one row of the member's skill list: the metadata every layer
// has, plus which layer it came from and whether it is actually in effect.
type MemberSkill struct {
	SkillMeta
	Origin string `json:"origin"`
	// Shadowed marks a member skill the agent does not load, because an
	// administrator published one declaring the same frontmatter name. The
	// harness resolves that collision in the administrator's favour and logs the
	// workspace copy as shadowed; without this flag the member would be editing a
	// file with no effect and nothing would say so.
	Shadowed bool `json:"shadowed,omitempty"`
}

// SkillFile is one file inside a skill directory.
type SkillFile struct {
	// Path is relative to the skill's own directory, slash-separated.
	// "SKILL.md" is the skill itself; anything else is a supporting file.
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modifiedAt"`
	// Binary is set on a READ, never on a listing: deciding it costs the file's
	// bytes, and a listing that read every file to label it would be the walk
	// skillFileMaxCount exists to bound.
	Binary bool `json:"binary,omitempty"`
}

// managedSkillNames is every operator-managed skill name, GATING IGNORED.
//
// Deliberately ungated, unlike managedContentBinds. Two of these are bound only
// on the ganglion, and one only when the MCP token exists; both conditions can
// change under a deployment that already has member skills on disk, and the
// failure then is a member's skill silently disappearing beneath a new
// read-only bind. Reserving the name always costs one refused create.
//
// TestManagedSkillNamesCoverEveryBind pins this against managedContentBinds so
// the two cannot drift.
func managedSkillNames() []string {
	rels := []string{managedSkillRel, managedSkillCreatorRel, managedGanglionRel, managedScheduleRel}
	out := make([]string, 0, len(rels))
	for _, rel := range rels {
		out = append(out, path.Base(rel))
	}
	return out
}

func isManagedSkillName(name string) bool {
	for _, n := range managedSkillNames() {
		if n == name {
			return true
		}
	}
	return false
}

// memberSkillsRoot is the segment the member's skills live under. Confinement is
// anchored at the SEGMENT, not at the skills dir, for the reason memory.go
// records about its own: `skills` is itself a component the agent can replace.
func (m *Manager) memberSkillsRoot(key WorkspaceKey, harness string) string {
	return m.workspaceDir(key, harness, "")
}

// ListMemberSkills returns all three layers, the member's own first.
//
// The order is not cosmetic: the member's layer is the one they can act on, and
// the other two are there to explain why the agent behaves as it does.
func (m *Manager) ListMemberSkills(key WorkspaceKey, harness string) ([]MemberSkill, error) {
	sharedDir := config.EffectiveSkillsDir(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role)
	shared := skillMetasIn(m, sharedDir)

	// Collision is decided on the DECLARED name, on both sides, because that is
	// the key the harness dedups by. Comparing directories instead would miss a
	// shadowing whose two directories happen to differ, and invent one whose
	// declared names differ.
	sharedDeclared := map[string]bool{}
	for _, s := range shared {
		sharedDeclared[declaredNameIn(path.Join(sharedDir, s.Name))] = true
	}

	out := []MemberSkill{}
	for _, e := range m.ownSkills(key, harness) {
		shadowed := sharedDeclared[e.declared] && effectiveHarness(harness) == config.HarnessGanglion
		out = append(out, MemberSkill{SkillMeta: e.meta, Origin: SkillOriginMember, Shadowed: shadowed})
	}
	for _, meta := range shared {
		out = append(out, MemberSkill{SkillMeta: meta, Origin: SkillOriginShared})
	}
	for _, meta := range m.managedSkills(harness) {
		out = append(out, MemberSkill{SkillMeta: meta, Origin: SkillOriginManaged})
	}
	return out, nil
}

// memberSkillEntry is one directory in the member's skills tree.
//
// The DIRECTORY name and the DECLARED frontmatter name are normally equal --
// writes through this API refuse a disagreement -- but a skill the agent wrote
// with the shell (which `skill-creator` teaches it to do) or one a template
// seeded can disagree, and the two must not be conflated:
//
//   - the DIRECTORY is what a read, a write and a delete resolve, so it is the
//     identity a listing has to publish;
//   - the DECLARED name is what the harness indexes and dedups by, so it is the
//     one shadowing is decided on.
//
// Publishing the declared name produced a list whose rows 404 when opened, and
// a delete that could not find what it had just listed.
type memberSkillEntry struct {
	meta     SkillMeta
	declared string
}

// ownSkills reads the member's own layer, through the confined tree.
//
// Managed names are skipped rather than reported. The empty mountpoint runc
// leaves behind would be discarded by the frontmatter parse anyway; what this
// catches is a member skill that PREDATES the bind, complete on disk and
// invisible in the container.
func (m *Manager) ownSkills(key WorkspaceKey, harness string) []memberSkillEntry {
	tree, err := openTreeIfExists(m.memberSkillsRoot(key, harness))
	if err != nil {
		return nil
	}
	defer tree.Close()

	entries, err := fs.ReadDir(tree.root.FS(), SkillsDirName)
	if err != nil {
		return nil
	}
	out := []memberSkillEntry{}
	for _, e := range entries {
		if !e.IsDir() || isManagedSkillName(e.Name()) {
			continue
		}
		meta, declared, err := memberSkillMeta(tree, e.Name())
		if err != nil {
			continue // the harness would skip it too
		}
		out = append(out, memberSkillEntry{meta: meta, declared: declared})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].meta.Name < out[j].meta.Name })
	return out
}

// sharedSkillsForMember reads the MERGED set this agent actually receives.
//
// EffectiveSkillsDir, not the four layer directories: those hold skills that lost
// the cascade or belong to a different agent, and listing them would be a
// catalogue of things that are not in the agent's prompt.
//
// ContainerDataRoot, not HostDataRoot. The bind-building code uses the host root
// because a bind string is read by the daemon; a read performed by this process
// must use the root this process sees, or it is an ENOENT that looks like "the
// administrator published nothing".
func (m *Manager) sharedSkillsDirFor(key WorkspaceKey) string {
	return config.EffectiveSkillsDir(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role)
}

// managedSkills reads the operator layer from ManagedSkillsDir -- the canonical
// copy -- rather than from the mountpoint stubs left in the member's workspace.
//
// Gated here, unlike the reserved-name set: this is what the member's agent is
// actually given, and a skill that is not bound is not in its prompt.
func (m *Manager) managedSkills(harness string) []SkillMeta {
	out := []SkillMeta{}
	for _, src := range m.managedSkillDirs(harness) {
		meta, err := m.skillMeta(path.Dir(src), path.Base(src))
		if err != nil {
			continue
		}
		out = append(out, meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// managedSkillDirs is the source half of every managed SKILL bind, gated exactly
// as the binds are, so what a member is shown is what their agent was given.
func (m *Manager) managedSkillDirs(harness string) []string {
	base := config.ManagedSkillsDir(m.cfg.ContainerDataRoot)
	const anyMount = "/mnt" // only the source half of each bind is used
	out := []string{}
	for _, bind := range managedContentBinds(base, anyMount, effectiveHarness(harness), m.cfg.ResolvedMCPTokenSecret != "") {
		src, dest, ok := strings.Cut(bind, ":")
		if !ok || !strings.Contains(dest, "/"+config.MainWorkspace+"/"+SkillsDirName+"/") {
			continue // the managed MEMORY notes are not skills
		}
		out = append(out, src)
	}
	return out
}

func skillMetasIn(m *Manager, dir string) []SkillMeta {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := []SkillMeta{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta, err := m.skillMeta(dir, e.Name())
		if err != nil {
			continue
		}
		out = append(out, meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// declaredNameIn reads just the frontmatter `name` of a proxy-owned skill dir,
// falling back to the directory. Used for collision detection, which is decided
// on the declared name because that is what the harness dedups by.
func declaredNameIn(skillDir string) string {
	raw, err := os.ReadFile(path.Join(skillDir, skillDocName))
	if err != nil {
		return path.Base(skillDir)
	}
	declared, _, err := parseSkillFrontmatter(string(raw))
	if err != nil || declared == "" {
		return path.Base(skillDir)
	}
	return declared
}

// memberSkillMeta is skillMeta's confined twin: every path is resolved by the
// kernel against the workspace root, because this tree is agent-writable and a
// component of it may be a symlink the agent placed.
//
// Name is the DIRECTORY. The declared frontmatter name comes back beside it
// rather than in it -- see memberSkillEntry for why conflating them was a bug.
func memberSkillMeta(tree *treeRoot, name string) (SkillMeta, string, error) {
	rel := path.Join(SkillsDirName, name)
	raw, err := tree.root.ReadFile(path.Join(rel, skillDocName))
	if err != nil {
		if escaped(err) {
			return SkillMeta{}, "", ErrMediaName
		}
		return SkillMeta{}, "", err
	}
	declared, desc, err := parseSkillFrontmatter(string(raw))
	if err != nil {
		return SkillMeta{}, "", err
	}
	if declared == "" {
		declared = name
	}
	total, files, modAt := treeUsage(tree, rel)
	return SkillMeta{
		Name: name, Description: desc, Size: total, ModifiedAt: modAt, HasFiles: files > 1,
	}, declared, nil
}

// treeUsage walks a skill directory inside the confined root. Walking the Root's
// own fs.FS means the traversal cannot leave the tree either -- a symlink to /
// planted inside a skill would otherwise walk the host.
func treeUsage(tree *treeRoot, rel string) (total int64, files int, modAt string) {
	_ = fs.WalkDir(tree.root.FS(), rel, func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		files++
		if t := modTime(info); t > modAt {
			modAt = t
		}
		return nil
	})
	return total, files, modAt
}

// --- files inside a skill -------------------------------------------------

// ListMemberSkillFiles returns every file in a skill, whichever layer it is in.
//
// Read-only layers are browsable on purpose: a member who cannot see the
// template an administrator's skill tells the agent to fill in cannot tell what
// that skill will do.
func (m *Manager) ListMemberSkillFiles(key WorkspaceKey, harness, rawName string) ([]SkillFile, string, error) {
	name, err := sanitizeSkillName(rawName)
	if err != nil {
		return nil, "", err
	}
	if base, origin, ok := m.readOnlySkillRoot(name); ok {
		files, err := listSkillFilesUnder(base, name)
		return files, origin, err
	}

	tree, err := openTreeIfExists(m.memberSkillsRoot(key, harness))
	if err != nil {
		return nil, "", ErrMediaNotFound
	}
	defer tree.Close()

	rel := path.Join(SkillsDirName, name)
	if _, err := tree.root.Stat(rel); err != nil {
		if escaped(err) {
			return nil, "", ErrMediaName
		}
		return nil, "", ErrMediaNotFound
	}
	out := []SkillFile{}
	_ = fs.WalkDir(tree.root.FS(), rel, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || len(out) >= skillFileMaxCount {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, SkillFile{
			Path:       strings.TrimPrefix(p, rel+"/"),
			Size:       info.Size(),
			ModifiedAt: modTime(info),
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return skillFileLess(out[i].Path, out[j].Path) })
	return out, SkillOriginMember, nil
}

// skillFileLess puts SKILL.md first and then sorts by path. The skill itself is
// what a member opened the row to read; its supporting files are context.
func skillFileLess(a, b string) bool {
	if (a == skillDocName) != (b == skillDocName) {
		return a == skillDocName
	}
	return a < b
}

// skillFileRel validates a path INSIDE a skill.
//
// The kernel is the guarantee -- every read and write below goes through the
// confined root -- and this is the message: a member gets told their path is
// unusable before any filesystem work happens, which is the division
// media_root.go's own comment draws.
func skillFileRel(raw string) (string, error) {
	rel := strings.TrimSpace(raw)
	if rel == "" {
		return skillDocName, nil // the skill itself is the default document
	}
	if path.IsAbs(rel) || strings.Contains(rel, `\`) {
		return "", ErrSkillFilePath
	}
	clean := path.Clean(rel)
	if clean != rel || clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", ErrSkillFilePath
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", ErrSkillFilePath
		}
	}
	return clean, nil
}

// readOnlySkillRoot names the PARENT directory of a skill in the one layer that
// takes precedence over the member's own -- the operator's, which is mounted
// over the workspace copy.
//
// The PARENT, not the skill: everything below resolves the caller-supplied name
// BENEATH a confined root, so the boundary has to sit above the part that came
// from the request.
func (m *Manager) readOnlySkillRoot(name string) (string, string, bool) {
	if isManagedSkillName(name) {
		return path.Join(config.ManagedSkillsDir(m.cfg.ContainerDataRoot), SkillsDirName),
			SkillOriginManaged, true
	}
	return "", "", false
}

// The two proxy-owned layers are read through os.Root as well, and not because
// anything can plant a symlink in them today -- an admin upload is unpacked by
// hardened zip code and the operator's tree is embedded in this binary.
//
// It is that `name` and `path` come from the request. `sanitizeSkillName` and
// `skillFileRel` already refuse traversal, but that is the VALIDATOR, and
// media_root.go states this repository's division: the kernel is the guarantee,
// the validator is the message. A defence resting on "and nobody can place a
// link there" is one deployment decision away from being wrong, silently.
//
// CodeQL caught this as go/path-injection on the plain os.ReadFile these
// replaced, which is the same observation arrived at mechanically.
func listSkillFilesUnder(base, name string) ([]SkillFile, error) {
	root, err := os.OpenRoot(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrMediaNotFound
		}
		return nil, err
	}
	defer root.Close()
	if _, err := root.Stat(name); err != nil {
		if escaped(err) {
			return nil, ErrMediaName
		}
		return nil, ErrMediaNotFound
	}
	out := []SkillFile{}
	_ = fs.WalkDir(root.FS(), name, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || len(out) >= skillFileMaxCount {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, SkillFile{
			Path:       strings.TrimPrefix(p, name+"/"),
			Size:       info.Size(),
			ModifiedAt: modTime(info),
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return skillFileLess(out[i].Path, out[j].Path) })
	return out, nil
}

func readSkillFileUnder(base, name, rel, origin string) (string, SkillFile, string, error) {
	root, err := os.OpenRoot(base)
	if err != nil {
		if os.IsNotExist(err) {
			return "", SkillFile{}, "", ErrMediaNotFound
		}
		return "", SkillFile{}, "", err
	}
	defer root.Close()
	sub := path.Join(name, rel)
	raw, err := root.ReadFile(sub)
	if err != nil {
		if escaped(err) {
			return "", SkillFile{}, "", ErrMediaName
		}
		return "", SkillFile{}, "", err
	}
	info, _ := root.Stat(sub)
	return decodeSkillFile(raw, rel, info, origin)
}

// ReadMemberSkillFile returns one file's text from whichever layer holds the
// skill, plus that layer's origin.
//
// Tolerant of a SKILL.md whose frontmatter does not parse: the harness ignores
// such a skill, but a member who cannot open it cannot repair it either.
func (m *Manager) ReadMemberSkillFile(key WorkspaceKey, harness, rawName, rawPath string) (string, SkillFile, string, error) {
	name, err := sanitizeSkillName(rawName)
	if err != nil {
		return "", SkillFile{}, "", err
	}
	rel, err := skillFileRel(rawPath)
	if err != nil {
		return "", SkillFile{}, "", err
	}

	if base, origin, ok := m.readOnlySkillRoot(name); ok {
		return readSkillFileUnder(base, name, rel, origin)
	}

	tree, openErr := openTreeIfExists(m.memberSkillsRoot(key, harness))
	if openErr == nil {
		defer tree.Close()
		full := path.Join(SkillsDirName, name, rel)
		raw, readErr := tree.root.ReadFile(full)
		switch {
		case readErr == nil:
			info, _ := tree.root.Stat(full)
			return decodeSkillFile(raw, rel, info, SkillOriginMember)
		case escaped(readErr):
			return "", SkillFile{}, "", ErrMediaName
		case !os.IsNotExist(readErr):
			return "", SkillFile{}, "", readErr
		}
	} else if !errors.Is(openErr, ErrMediaNotFound) {
		return "", SkillFile{}, "", openErr
	}

	// Not the member's; the administrator's cascade is the remaining layer.
	return readSkillFileUnder(m.sharedSkillsDirFor(key), name, rel, SkillOriginShared)
}

// decodeSkillFile labels a file the panel cannot edit rather than handing back
// mangled text. Valid UTF-8 with no NUL is the test: it accepts every document a
// skill actually holds and refuses images and archives.
func decodeSkillFile(raw []byte, rel string, info os.FileInfo, origin string) (string, SkillFile, string, error) {
	f := SkillFile{Path: rel}
	if info != nil {
		f.Size = info.Size()
		f.ModifiedAt = modTime(info)
	}
	if !utf8.Valid(raw) || strings.ContainsRune(string(raw), 0) {
		f.Binary = true
		return "", f, origin, nil
	}
	return string(raw), f, origin, nil
}

// --- writing --------------------------------------------------------------

// WriteMemberSkillFile creates or replaces one file inside one of the member's
// own skills. rawPath empty means SKILL.md, the skill itself.
//
// ifModifiedAt is the ModifiedAt the caller last read; empty means "this is a
// create". The harness's evolution writes this same directory in apply mode, so
// an unconditional write can drop a skill the agent learned with no trace.
func (m *Manager) WriteMemberSkillFile(
	key WorkspaceKey, harness, rawName, rawPath, content, ifModifiedAt string,
) (SkillFile, error) {
	name, err := sanitizeSkillName(rawName)
	if err != nil {
		return SkillFile{}, err
	}
	if isManagedSkillName(name) {
		return SkillFile{}, ErrSkillReadOnly
	}
	rel, err := skillFileRel(rawPath)
	if err != nil {
		return SkillFile{}, err
	}
	if len(content) > skillFileMaxBytes {
		return SkillFile{}, fmt.Errorf("file exceeds the %d-byte limit", skillFileMaxBytes)
	}
	// Only SKILL.md is the skill. A supporting file is whatever the member needs
	// it to be, and validating it against the frontmatter grammar would refuse
	// every template and script a skill exists to carry.
	if rel == skillDocName {
		if err := validateMemberSkillDoc(name, content); err != nil {
			return SkillFile{}, err
		}
	}

	tree, err := openTree(m.memberSkillsRoot(key, harness))
	if err != nil {
		return SkillFile{}, err
	}
	defer tree.Close()

	full := path.Join(SkillsDirName, name, rel)
	info, statErr := tree.root.Stat(full)
	switch {
	case escaped(statErr):
		return SkillFile{}, ErrMediaName
	case statErr == nil && ifModifiedAt == "":
		return SkillFile{}, ErrSkillExists
	case statErr == nil && modTime(info) != ifModifiedAt:
		return SkillFile{}, ErrSkillConflict
	case statErr != nil && !os.IsNotExist(statErr):
		return SkillFile{}, statErr
	case statErr != nil && ifModifiedAt != "":
		// The caller believed they were editing something. It is gone -- deleted
		// by the member elsewhere, or replaced by the agent's own evolution.
		// Recreating it silently would resurrect a skill somebody removed.
		return SkillFile{}, ErrSkillConflict
	}

	if err := tree.root.MkdirAll(path.Dir(full), 0o700); err != nil {
		if escaped(err) {
			return SkillFile{}, ErrMediaName
		}
		return SkillFile{}, fmt.Errorf("mkdir skill: %w", err)
	}
	if err := tree.root.WriteFile(full, []byte(content), 0o600); err != nil {
		if escaped(err) {
			return SkillFile{}, ErrMediaName
		}
		return SkillFile{}, fmt.Errorf("write skill file: %w", err)
	}
	// The agent runs non-root and must be able to traverse and read what this
	// (root) process wrote -- the same reason WriteMemory chowns its own dir.
	if err := chownTree(tree.abs(path.Join(SkillsDirName, name)), m.cfg.PicoclawUser); err != nil {
		return SkillFile{}, fmt.Errorf("chown skill: %w", err)
	}

	written, err := tree.root.Stat(full)
	if err != nil {
		return SkillFile{}, err
	}
	return SkillFile{Path: rel, Size: written.Size(), ModifiedAt: modTime(written)}, nil
}

// DeleteMemberSkill removes one of the member's own skill directories.
func (m *Manager) DeleteMemberSkill(key WorkspaceKey, harness, rawName string) error {
	name, err := sanitizeSkillName(rawName)
	if err != nil {
		return err
	}
	if isManagedSkillName(name) {
		return ErrSkillReadOnly
	}
	tree, err := openTreeIfExists(m.memberSkillsRoot(key, harness))
	if err != nil {
		if errors.Is(err, ErrMediaNotFound) {
			return ErrMediaNotFound
		}
		return err
	}
	defer tree.Close()

	rel := path.Join(SkillsDirName, name)
	if _, err := tree.root.Stat(rel); err != nil {
		if os.IsNotExist(err) {
			return ErrMediaNotFound
		}
		if escaped(err) {
			return ErrMediaName
		}
		return err
	}
	if err := removeTreeAll(tree, rel); err != nil {
		if escaped(err) {
			return ErrMediaName
		}
		return err
	}
	return nil
}

// removeTreeAll deletes rel and everything under it, depth first, every step
// resolved inside the root. os.Root has no RemoveAll, and os.RemoveAll on
// tree.abs(rel) would re-resolve the path outside the boundary -- which is the
// whole thing this file is confined against.
func removeTreeAll(tree *treeRoot, rel string) error {
	entries, err := fs.ReadDir(tree.root.FS(), rel)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		child := path.Join(rel, e.Name())
		if e.IsDir() {
			if err := removeTreeAll(tree, child); err != nil {
				return err
			}
			continue
		}
		if err := tree.root.Remove(child); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return tree.root.Remove(rel)
}

// validateMemberSkillDoc refuses a SKILL.md the harness would not load.
//
// A stored file the loader ignores is worse than a rejection: the member is told
// their change was saved, the agent never changes, and nothing reports a fault.
//
// It does NOT refuse a third frontmatter key, and an earlier draft did. picoclaw
// SHIPS skills carrying `metadata:` and `homepage:` -- seven of the nine in its
// own workspace template -- so its loader plainly accepts them, and the rule
// would have stopped a picoclaw member from saving any edit to the skills they
// were seeded with. The two-key restriction belongs to picoclaw's EVOLUTION
// validator, which governs what an agent may write about itself, not what a
// loader will read.
func validateMemberSkillDoc(name, content string) error {
	declared, desc, err := parseSkillFrontmatter(content)
	if err != nil {
		return err
	}
	if declared == "" || desc == "" {
		return ErrSkillMetadata
	}
	if declared != name {
		return ErrSkillNameMismatch
	}
	return nil
}

func effectiveHarness(harness string) string {
	if harness == "" {
		return config.DefaultHarness
	}
	return harness
}
