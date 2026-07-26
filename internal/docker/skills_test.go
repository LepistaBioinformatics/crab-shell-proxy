package docker

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
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
