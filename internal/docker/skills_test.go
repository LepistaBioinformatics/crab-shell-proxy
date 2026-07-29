package docker

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

const goodSkillMD = "---\nname: my-skill\ndescription: does a thing\n---\n\n# My Skill\n"

func skillsManager(t *testing.T) (*Manager, Scope) {
	t.Helper()
	m := &Manager{cfg: &config.Config{ContainerDataRoot: t.TempDir(), PicoclawUser: ""}}
	return m, Scope{Kind: ScopeSubscription, TenantID: "t1", SubsAccID: "s1"}
}

func makeZip(t *testing.T, files map[string]string) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	return bytes.NewReader(buf.Bytes())
}

func TestSanitizeSkillName(t *testing.T) {
	ok := []string{"my-skill", "a", "skill.v2", "a_b-c.d"}
	for _, n := range ok {
		if _, err := sanitizeSkillName(n); err != nil {
			t.Errorf("%q should be valid: %v", n, err)
		}
	}
	if _, err := sanitizeSkillName("shared-content"); !errors.Is(err, ErrReservedSkillName) {
		t.Errorf("shared-content must be reserved, got %v", err)
	}
	for _, n := range []string{"", "Bad", "has space", "../x", "-leading"} {
		if _, err := sanitizeSkillName(n); err == nil {
			t.Errorf("%q should be rejected", n)
		}
	}
}

func TestParseSkillFrontmatter(t *testing.T) {
	if _, d, err := parseSkillFrontmatter(goodSkillMD); err != nil || d != "does a thing" {
		t.Fatalf("good: d=%q err=%v", d, err)
	}
	bad := []string{
		"no frontmatter here",
		"---\nname: x\n---\n",                // missing description
		"---\ndescription: y\n---\n",         // missing name
		"---\nname: \ndescription: y\n---\n", // empty name
	}
	for _, md := range bad {
		if _, _, err := parseSkillFrontmatter(md); err == nil {
			t.Errorf("should reject: %q", md)
		}
	}
}

func TestSkillDocRoundTrip(t *testing.T) {
	m, scope := skillsManager(t)
	if err := m.WriteSharedSkillDoc(scope, "my-skill", goodSkillMD); err != nil {
		t.Fatalf("write: %v", err)
	}
	doc, meta, err := m.ReadSharedSkillDoc(scope, "my-skill")
	if err != nil || doc != goodSkillMD {
		t.Fatalf("read: doc=%q err=%v", doc, err)
	}
	if meta.Description != "does a thing" || meta.HasFiles {
		t.Errorf("meta wrong: %+v", meta)
	}
	list, _ := m.ListSharedSkills(scope)
	if len(list) != 1 || list[0].Name != "my-skill" {
		t.Fatalf("list: %+v", list)
	}
	// Writing an invalid-frontmatter doc is rejected.
	if err := m.WriteSharedSkillDoc(scope, "bad", "no fm"); !errors.Is(err, ErrSkillMetadata) {
		t.Errorf("bad frontmatter: want ErrSkillMetadata, got %v", err)
	}
	if err := m.DeleteSharedSkill(scope, "my-skill"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if list, _ := m.ListSharedSkills(scope); len(list) != 0 {
		t.Errorf("should be empty after delete: %+v", list)
	}
	// Delete is idempotent.
	if err := m.DeleteSharedSkill(scope, "my-skill"); err != nil {
		t.Errorf("idempotent delete: %v", err)
	}
}

func TestSkillZipGoodAndArchive(t *testing.T) {
	m, scope := skillsManager(t)
	z := makeZip(t, map[string]string{"SKILL.md": goodSkillMD, "references/x.md": "hi"})
	if err := m.WriteSharedSkillZip(scope, "zskill", z); err != nil {
		t.Fatalf("write zip: %v", err)
	}
	meta, err := m.skillMeta(m.sharedSkillsDir(scope), "zskill")
	if err != nil || !meta.HasFiles {
		t.Errorf("zskill should have files: %+v err=%v", meta, err)
	}
	// The supporting file was extracted.
	if _, err := os.Stat(filepath.Join(m.sharedSkillsDir(scope), "zskill", "references", "x.md")); err != nil {
		t.Errorf("reference file missing: %v", err)
	}
	// Archive round-trips into a readable zip containing SKILL.md.
	var out bytes.Buffer
	if err := m.ArchiveSharedSkill(scope, "zskill", &out); err != nil {
		t.Fatalf("archive: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range zr.File {
		if f.Name == "SKILL.md" {
			found = true
		}
	}
	if !found {
		t.Error("archive missing SKILL.md")
	}
}

// `zip -r auto-harness.zip auto-harness/` — and the Finder/Explorer equivalents —
// wrap everything in a directory named after the folder, so the entries are
// `auto-harness/SKILL.md`, not `SKILL.md`. That is the normal output of zipping a
// directory, and it used to be rejected with "archive has no top-level SKILL.md".
func TestSkillZipStripsSingleWrappingDir(t *testing.T) {
	m, scope := skillsManager(t)
	z := makeZip(t, map[string]string{
		"auto-harness/SKILL.md":        goodSkillMD,
		"auto-harness/references/x.md": "hi",
	})
	if err := m.WriteSharedSkillZip(scope, "zskill", z); err != nil {
		t.Fatalf("wrapped zip: %v", err)
	}
	// The wrapper is NOT part of the installed skill: the destination directory is
	// named by the upload's `name`, and SKILL.md has to sit directly inside it or
	// picoclaw will not find the skill at all.
	dir := m.sharedSkillsDir(scope)
	if _, err := os.Stat(filepath.Join(dir, "zskill", "SKILL.md")); err != nil {
		t.Errorf("SKILL.md should be at the skill root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "zskill", "references", "x.md")); err != nil {
		t.Errorf("supporting file should keep its relative path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "zskill", "auto-harness")); err == nil {
		t.Error("the wrapping directory must not be extracted")
	}
}

// Stripping applies ONLY when there is exactly one top-level directory and nothing
// beside it. Anything else keeps its paths, so a zip that genuinely has no root
// SKILL.md is still rejected rather than being silently reinterpreted.
func TestSkillZipStripsOnlyAnUnambiguousWrapper(t *testing.T) {
	m, scope := skillsManager(t)
	// Two top-level directories: there is no single wrapper to strip.
	err := m.WriteSharedSkillZip(scope, "two", makeZip(t, map[string]string{
		"a/SKILL.md": goodSkillMD,
		"b/other.md": "x",
	}))
	if !errors.Is(err, ErrSkillArchive) {
		t.Errorf("two top-level dirs: want ErrSkillArchive, got %v", err)
	}
	// A root-level file beside the directory: the root is already meaningful.
	err = m.WriteSharedSkillZip(scope, "mixed", makeZip(t, map[string]string{
		"pack/SKILL.md": goodSkillMD,
		"readme.txt":    "x",
	}))
	if !errors.Is(err, ErrSkillArchive) {
		t.Errorf("root file beside dir: want ErrSkillArchive, got %v", err)
	}
	// A wrapper that holds no SKILL.md is still an archive without one.
	err = m.WriteSharedSkillZip(scope, "empty", makeZip(t, map[string]string{
		"pack/readme.txt": "x",
	}))
	if !errors.Is(err, ErrSkillArchive) {
		t.Errorf("wrapper without SKILL.md: want ErrSkillArchive, got %v", err)
	}
	if list, _ := m.ListSharedSkills(scope); len(list) != 0 {
		t.Errorf("rejected uploads must write nothing: %+v", list)
	}
}

// `zip auto-harness.zip auto-harness` WITHOUT -r stores the directory entry and
// none of its contents, so the archive holds no files at all. "no top-level
// SKILL.md" is true of it but reads as "your SKILL.md is in the wrong place",
// which sent a real operator looking for a layout problem that did not exist.
func TestSkillZipEmptyArchiveSaysSo(t *testing.T) {
	m, scope := skillsManager(t)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// A trailing slash is what makes Go's zip reader report an entry as a
	// directory — this is byte-for-byte what `zip` without -r produces.
	if _, err := zw.Create("auto-harness/"); err != nil {
		t.Fatal(err)
	}
	zw.Close()

	err := m.WriteSharedSkillZip(scope, "empty", bytes.NewReader(buf.Bytes()))
	if !errors.Is(err, ErrSkillArchive) {
		t.Fatalf("want ErrSkillArchive, got %v", err)
	}
	if !strings.Contains(err.Error(), "no files") {
		t.Errorf("error should say the archive holds no files, got: %v", err)
	}
	if strings.Contains(err.Error(), "top-level") {
		t.Errorf("an archive with no files must not be blamed on SKILL.md placement: %v", err)
	}

	// A truly empty archive is the same story.
	var empty bytes.Buffer
	ez := zip.NewWriter(&empty)
	ez.Close()
	err = m.WriteSharedSkillZip(scope, "none", bytes.NewReader(empty.Bytes()))
	if !errors.Is(err, ErrSkillArchive) || !strings.Contains(err.Error(), "no files") {
		t.Errorf("empty archive: %v", err)
	}
}

// An archive that DOES carry files but none of them is SKILL.md keeps the
// placement message — that one really is about where the file sits.
func TestSkillZipWithFilesButNoSkillMDKeepsPlacementMessage(t *testing.T) {
	m, scope := skillsManager(t)
	err := m.WriteSharedSkillZip(scope, "nomd", makeZip(t, map[string]string{"readme.txt": "x"}))
	if !errors.Is(err, ErrSkillArchive) {
		t.Fatalf("want ErrSkillArchive, got %v", err)
	}
	if !strings.Contains(err.Error(), "top-level SKILL.md") {
		t.Errorf("want the placement message, got: %v", err)
	}
}

func TestSkillZipRejects(t *testing.T) {
	m, scope := skillsManager(t)
	// Path traversal.
	if err := m.WriteSharedSkillZip(scope, "evil", makeZip(t, map[string]string{"SKILL.md": goodSkillMD, "../escape": "x"})); !errors.Is(err, ErrSkillArchive) {
		t.Errorf("traversal: want ErrSkillArchive, got %v", err)
	}
	// No SKILL.md.
	if err := m.WriteSharedSkillZip(scope, "nomd", makeZip(t, map[string]string{"readme.txt": "x"})); !errors.Is(err, ErrSkillArchive) {
		t.Errorf("no SKILL.md: want ErrSkillArchive, got %v", err)
	}
	// SKILL.md with bad frontmatter.
	if err := m.WriteSharedSkillZip(scope, "badfm", makeZip(t, map[string]string{"SKILL.md": "nope"})); !errors.Is(err, ErrSkillMetadata) {
		t.Errorf("bad fm zip: want ErrSkillMetadata, got %v", err)
	}
	// Nothing was written for the rejected uploads.
	if list, _ := m.ListSharedSkills(scope); len(list) != 0 {
		t.Errorf("rejected uploads must write nothing: %+v", list)
	}
}
