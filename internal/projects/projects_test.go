package projects

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(filepath.Join(t.TempDir(), ".projects.json"))
}

var testClock = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

func mustCreate(t *testing.T, s *Store, name string) Project {
	t.Helper()
	p, err := s.Create(name, "", testClock)
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	return p
}

func TestListAbsentFileIsEmpty(t *testing.T) {
	// A user with no projects is the normal state, not an error: List is called
	// on every ensure, long before anyone creates a project.
	list, err := newTestStore(t).List()
	if err != nil {
		t.Fatalf("List on absent file: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("len = %d, want 0", len(list))
	}
}

func TestGenerateIDSlugging(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"Seed Trial", "seed-trial"},
		{"seed-trial", "seed-trial"},
		{"Seed  Trial", "seed-trial"},    // runs collapse to one separator
		{"  Seed Trial  ", "seed-trial"}, // outer space trimmed
		{"Análise de Solo", "an-lise-de-solo"},
		{"R&D / 2026", "r-d-2026"},
		{"2026 planning", "2026-planning"}, // a leading digit is legal
		{"snake_case_name", "snake_case_name"},
		{"--leading-dashes--", "leading-dashes"},
		{"_leading_underscore", "leading_underscore"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := generateID(tt.name, map[string]bool{})
			if err != nil {
				t.Fatalf("generateID: %v", err)
			}
			if got != tt.want {
				t.Errorf("generateID(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// TestGenerateIDIsPicoclawFixedPoint is the load-bearing one. picoclaw runs every
// agent id through NormalizeAgentID; if that rewrites ours, the agent it
// registers and the id baked into our dispatch rule stop agreeing and the
// project silently stops routing. So every generated id must already be a fixed
// point of picoclaw's own regex, reproduced here from
// pkg/routing/agent_id.go:16.
func TestGenerateIDIsPicoclawFixedPoint(t *testing.T) {
	picoclawValidID := regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

	names := []string{
		"Seed Trial", "R&D / 2026", "Análise de Solo", "项目一", "🌱🌱🌱",
		"...", "___", "---", "a", "9",
		strings.Repeat("long name ", 20),
		"Ω≈ç√∫˜µ", "tab\tand\nnewline", "trailing dash -", "- leading dash",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			id, err := generateID(name, map[string]bool{})
			if err != nil {
				t.Fatalf("generateID(%q): %v", name, err)
			}
			if !picoclawValidID.MatchString(id) {
				t.Errorf("generateID(%q) = %q, which picoclaw's NormalizeAgentID would rewrite", name, id)
			}
			// The two characters that must never appear, for reasons beyond the
			// regex: "*" would inject a wildcard into the project's own dispatch
			// pattern, and "." is the separator in the "p.<id>.<key>" session id.
			if strings.ContainsAny(id, ".*") {
				t.Errorf("generateID(%q) = %q, contains '.' or '*'", name, id)
			}
			if len(id) > MaxIDLength {
				t.Errorf("generateID(%q) = %q, length %d > %d", name, id, len(id), MaxIDLength)
			}
		})
	}
}

func TestGenerateIDFallbackForUnmappableNames(t *testing.T) {
	// Names made entirely of characters outside the alphabet are legitimate — the
	// user sees Name, the id is internal — so they get a fallback rather than a
	// rejection.
	for _, name := range []string{"项目一", "🌱", "...", "!!!"} {
		got, err := generateID(name, map[string]bool{})
		if err != nil {
			t.Fatalf("generateID(%q): %v", name, err)
		}
		if got != "project" {
			t.Errorf("generateID(%q) = %q, want %q", name, got, "project")
		}
	}
}

func TestGenerateIDCollisionSuffixing(t *testing.T) {
	taken := map[string]bool{"seed-trial": true, "seed-trial-2": true}
	got, err := generateID("Seed Trial", taken)
	if err != nil {
		t.Fatalf("generateID: %v", err)
	}
	if got != "seed-trial-3" {
		t.Errorf("got %q, want %q", got, "seed-trial-3")
	}
}

func TestGenerateIDSuffixKeepsLengthBound(t *testing.T) {
	long := strings.Repeat("a", MaxIDLength)
	got, err := generateID(long, map[string]bool{long: true})
	if err != nil {
		t.Fatalf("generateID: %v", err)
	}
	if len(got) > MaxIDLength {
		t.Errorf("len(%q) = %d, want <= %d", got, len(got), MaxIDLength)
	}
	if got == long {
		t.Error("collision not resolved")
	}
}

func TestCreateNeverClaimsMain(t *testing.T) {
	// The projection always emits {id: "main", default: true}; a project holding
	// that id would collide with it and take over the default agent.
	s := newTestStore(t)
	p := mustCreate(t, s, "Main")
	if p.ID == DefaultAgentID {
		t.Fatalf("project claimed the reserved id %q", DefaultAgentID)
	}
	if p.Name != "Main" {
		t.Errorf("Name = %q, want %q", p.Name, "Main")
	}
}

func TestCreateRejectsEmptyName(t *testing.T) {
	s := newTestStore(t)
	for _, name := range []string{"", "   ", "\t"} {
		if _, err := s.Create(name, "", testClock); !errors.Is(err, ErrEmptyName) {
			t.Errorf("Create(%q) error = %v, want ErrEmptyName", name, err)
		}
	}
}

func TestCreateRejectsDuplicateName(t *testing.T) {
	s := newTestStore(t)
	mustCreate(t, s, "Seed Trial")
	if _, err := s.Create("seed trial", "", testClock); !errors.Is(err, ErrDuplicate) {
		t.Errorf("error = %v, want ErrDuplicate (name match is case-insensitive)", err)
	}
}

func TestCreateDistinctNamesSharingASlug(t *testing.T) {
	// "Seed Trial" and "Seed-Trial" are different names but slug identically. The
	// name check must not reject the second, and the ids must not collide.
	s := newTestStore(t)
	a := mustCreate(t, s, "Seed Trial")
	b := mustCreate(t, s, "Seed-Trial")
	if a.ID == b.ID {
		t.Fatalf("both projects got id %q", a.ID)
	}
	if b.ID != "seed-trial-2" {
		t.Errorf("second id = %q, want %q", b.ID, "seed-trial-2")
	}
}

func TestCreateListRoundTripPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".projects.json")
	first := NewStore(path)
	mustCreate(t, first, "Seed Trial")
	mustCreate(t, first, "Soil Analysis")

	// A separate Store over the same file: the projection reads this on every
	// ensure through a fresh instance.
	list, err := NewStore(path).List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
	if list[0].Name != "Seed Trial" || list[1].Name != "Soil Analysis" {
		t.Errorf("creation order not preserved: %q, %q", list[0].Name, list[1].Name)
	}
	if !list[0].CreatedAt.Equal(testClock) {
		t.Errorf("CreatedAt = %v, want %v", list[0].CreatedAt, testClock)
	}
}

func TestStoreFileIsNotAgentReadable(t *testing.T) {
	// 0600: the proxy is the only reader. The file decides which agent identities
	// exist, so its mode is part of the isolation story, not housekeeping.
	path := filepath.Join(t.TempDir(), ".projects.json")
	mustCreate(t, NewStore(path), "Seed Trial")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

func TestRenameKeepsID(t *testing.T) {
	// The id is baked into a dispatch rule, a workspace directory and every
	// session id already issued. Re-deriving it on rename would orphan all three.
	s := newTestStore(t)
	created := mustCreate(t, s, "Seed Trial")

	renamed, err := s.Rename(created.ID, "Field Trial 2026")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if renamed.ID != created.ID {
		t.Errorf("ID changed on rename: %q -> %q", created.ID, renamed.ID)
	}
	if renamed.Name != "Field Trial 2026" {
		t.Errorf("Name = %q", renamed.Name)
	}
}

func TestRenameRejectsDuplicateAndEmpty(t *testing.T) {
	s := newTestStore(t)
	a := mustCreate(t, s, "Seed Trial")
	mustCreate(t, s, "Soil Analysis")

	if _, err := s.Rename(a.ID, "soil analysis"); !errors.Is(err, ErrDuplicate) {
		t.Errorf("error = %v, want ErrDuplicate", err)
	}
	if _, err := s.Rename(a.ID, "  "); !errors.Is(err, ErrEmptyName) {
		t.Errorf("error = %v, want ErrEmptyName", err)
	}
	// A no-op rename to its own current name must still be allowed — the
	// duplicate check compares against the OTHER projects only.
	if _, err := s.Rename(a.ID, "Seed Trial"); err != nil {
		t.Errorf("self-rename rejected: %v", err)
	}
}

func TestSetInstructions(t *testing.T) {
	s := newTestStore(t)
	p := mustCreate(t, s, "Seed Trial")

	updated, err := s.SetInstructions(p.ID, "Always cite the trial protocol.")
	if err != nil {
		t.Fatalf("SetInstructions: %v", err)
	}
	if updated.Instructions != "Always cite the trial protocol." {
		t.Errorf("Instructions = %q", updated.Instructions)
	}

	reloaded, err := s.Get(p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if reloaded.Instructions != updated.Instructions {
		t.Error("instructions did not persist")
	}
}

func TestDelete(t *testing.T) {
	s := newTestStore(t)
	a := mustCreate(t, s, "Seed Trial")
	b := mustCreate(t, s, "Soil Analysis")

	if err := s.Delete(a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("after delete, list = %+v", list)
	}

	// A deleted id frees its slug for reuse.
	reused := mustCreate(t, s, "Seed Trial")
	if reused.ID != a.ID {
		t.Errorf("reused id = %q, want %q", reused.ID, a.ID)
	}
}

func TestMissingIDErrors(t *testing.T) {
	s := newTestStore(t)
	mustCreate(t, s, "Seed Trial")

	if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get error = %v, want ErrNotFound", err)
	}
	if _, err := s.Rename("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Rename error = %v, want ErrNotFound", err)
	}
	if _, err := s.SetInstructions("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetInstructions error = %v, want ErrNotFound", err)
	}
	if err := s.Delete("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete error = %v, want ErrNotFound", err)
	}
}

func TestCorruptFileIsAnErrorNotAnEmptyList(t *testing.T) {
	// Silently reading a corrupt store as "no projects" would make the projection
	// delete every dispatch rule the user has — their projects would stop routing
	// and the workspaces would look abandoned.
	path := filepath.Join(t.TempDir(), ".projects.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path).List(); err == nil {
		t.Fatal("List on a corrupt file returned no error")
	}
}
