package memgraph

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testScope() Scope {
	return Scope{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(t.TempDir(), func() time.Time {
		return time.UnixMilli(testNow)
	})
}

// The graph must live OUTSIDE the agent's workspace. This is the single assertion
// standing between the agent and its own memory file (E-6/D-2); if a future
// refactor moves it under workspace/, the isolation is gone and nothing else
// would notice.
func TestGraphPathIsOutsideTheAgentWorkspace(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	p := s.Path(testScope())
	if strings.Contains(p, string(filepath.Separator)+"workspace"+string(filepath.Separator)) {
		t.Errorf("graph path %q is inside the agent workspace; the agent must not reach it", p)
	}
	if filepath.Base(filepath.Dir(p)) != GraphDirName {
		t.Errorf("graph dir = %q, want it to be %q", filepath.Dir(p), GraphDirName)
	}
	if filepath.Base(p) != GraphFileName {
		t.Errorf("graph file = %q, want %q", filepath.Base(p), GraphFileName)
	}
}

func TestGraphPathIsPerScope(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	base := testScope()
	seen := map[string]string{}
	for _, v := range []Scope{
		base,
		{TenantID: "t2", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"},
		{TenantID: "t1", SubsAccID: "s2", Role: "alpha", UserAccID: "u1"},
		{TenantID: "t1", SubsAccID: "s1", Role: "beta", UserAccID: "u1"},
		{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u2"},
	} {
		p := s.Path(v)
		if prev, ok := seen[p]; ok {
			t.Errorf("scope %+v shares a path with %s", v, prev)
		}
		seen[p] = p
	}
	if len(seen) != 5 {
		t.Errorf("distinct paths = %d, want 5 — every scope field must be part of the identity", len(seen))
	}
}

func TestLoadOnAbsentFileIsAnEmptyGraph(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	g, err := s.Load(testScope())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if g == nil || len(g.Entities) != 0 || len(g.Relations) != 0 {
		t.Errorf("Load on a missing file = %+v, want an empty graph and no error", g)
	}
}

func TestUpdateWritesAndLoadReadsBack(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	sc := testScope()
	err := s.Update(sc, func(g *Graph) error {
		g.Entities = append(g.Entities, Entity{Name: "a", EntityType: "t", CreatedAt: 5})
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	g, err := s.Load(sc)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(g.Entities) != 1 || g.Entities[0].Name != "a" {
		t.Errorf("loaded %+v, want the entity written", g.Entities)
	}
}

// Nothing this package writes may be chowned to the picoclaw user, because that
// is exactly what would hand the agent its own memory file. Enforced as a source
// gate: the assertion has to survive someone copying memory.go's chown out of
// habit.
func TestPackageNeverChownsTheGraph(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, bad := range []string{"Lchown", "os.Chown", "chownTree"} {
			if bytes.Contains(src, []byte(bad)) {
				t.Errorf("%s references %s; the memory graph dir must stay root-owned 0700", name, bad)
			}
		}
	}
}

func TestUpdateCreatesTheDirWith0700(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	sc := testScope()
	if err := s.Update(sc, func(g *Graph) error { return nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	fi, err := os.Stat(s.Dir(sc))
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %o, want 0700", perm)
	}
	fi, err = os.Stat(s.Path(sc))
	if err != nil {
		t.Fatalf("Stat file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 0600", perm)
	}
}

func TestUpdateLeavesTheFileUntouchedWhenTheCallbackFails(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	sc := testScope()
	if err := s.Update(sc, func(g *Graph) error {
		g.Entities = append(g.Entities, Entity{Name: "keep", EntityType: "t", CreatedAt: 1})
		return nil
	}); err != nil {
		t.Fatalf("seed Update: %v", err)
	}
	before, err := os.ReadFile(s.Path(sc))
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	boom := errors.New("boom")
	err = s.Update(sc, func(g *Graph) error {
		g.Entities = append(g.Entities, Entity{Name: "gone", EntityType: "t"})
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Update err = %v, want boom", err)
	}
	after, err := os.ReadFile(s.Path(sc))
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("a failed callback changed the file:\nbefore %s\nafter  %s", before, after)
	}
}

func TestUpdateRefusesAGraphOverTheSizeLimit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	sc := testScope()
	if err := s.Update(sc, func(g *Graph) error {
		g.Entities = append(g.Entities, Entity{Name: "small", EntityType: "t", CreatedAt: 1})
		return nil
	}); err != nil {
		t.Fatalf("seed Update: %v", err)
	}
	before, err := os.ReadFile(s.Path(sc))
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	big := strings.Repeat("x", 1<<20)
	err = s.Update(sc, func(g *Graph) error {
		for i := 0; i < 8; i++ {
			g.Entities = append(g.Entities, Entity{
				Name:         big + string(rune('a'+i)),
				EntityType:   "t",
				Observations: []Observation{{Content: big, Timestamp: 1}},
			})
		}
		return nil
	})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Update err = %v, want ErrTooLarge", err)
	}
	after, err := os.ReadFile(s.Path(sc))
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("an over-limit write changed the file")
	}
}

func TestUpdateLeavesNoTempFileBehind(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	sc := testScope()
	if err := s.Update(sc, func(g *Graph) error {
		g.Entities = append(g.Entities, Entity{Name: "a", EntityType: "t", CreatedAt: 1})
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// And once more on a failing callback, which aborts after the temp file could
	// already have been created.
	_ = s.Update(sc, func(g *Graph) error { return errors.New("abort") })

	entries, err := os.ReadDir(s.Dir(sc))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != GraphFileName {
			t.Errorf("stray file %q left in the graph dir", e.Name())
		}
	}
}

// Two turns of the same conversation can overlap. Neither may lose the other's
// write, which is the whole reason Update holds a per-scope lock across
// load-mutate-save rather than only across the save.
func TestConcurrentUpdatesOnOneScopeAllLand(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	sc := testScope()
	const n = 24

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := s.Update(sc, func(g *Graph) error {
				g.Entities = append(g.Entities, Entity{
					Name:       string(rune('A' + i)),
					EntityType: "t",
					CreatedAt:  1,
				})
				return nil
			})
			if err != nil {
				t.Errorf("Update %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	g, err := s.Load(sc)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(g.Entities) != n {
		t.Fatalf("entities = %d, want %d — a concurrent write was lost", len(g.Entities), n)
	}
	seen := map[string]bool{}
	for _, e := range g.Entities {
		if seen[e.Name] {
			t.Errorf("entity %q written twice", e.Name)
		}
		seen[e.Name] = true
	}
}

func TestConcurrentUpdatesOnDifferentScopesDoNotInterfere(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	scopes := []Scope{
		{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"},
		{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u2"},
		{TenantID: "t1", SubsAccID: "s1", Role: "beta", UserAccID: "u1"},
	}
	var wg sync.WaitGroup
	for i, sc := range scopes {
		wg.Add(1)
		go func(i int, sc Scope) {
			defer wg.Done()
			err := s.Update(sc, func(g *Graph) error {
				g.Entities = append(g.Entities, Entity{
					Name: string(rune('a' + i)), EntityType: "t", CreatedAt: 1,
				})
				return nil
			})
			if err != nil {
				t.Errorf("Update %+v: %v", sc, err)
			}
		}(i, sc)
	}
	wg.Wait()
	for i, sc := range scopes {
		g, err := s.Load(sc)
		if err != nil {
			t.Fatalf("Load %+v: %v", sc, err)
		}
		if len(g.Entities) != 1 || g.Entities[0].Name != string(rune('a'+i)) {
			t.Errorf("scope %+v holds %+v, want exactly its own entity", sc, g.Entities)
		}
	}
}

func TestNowMillisUsesTheInjectedClock(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if got := s.NowMillis(); got != testNow {
		t.Errorf("NowMillis() = %d, want the injected %d", got, testNow)
	}
	if NewStore("/x", nil).now == nil {
		t.Error("NewStore(nil clock) left now nil; it must default to time.Now")
	}
}

// A legacy file dropped into a workspace must be readable through the Store, not
// only through decodeJSONL — this is AC-5 at the level a member actually hits.
func TestLoadReadsAnImportedUpstreamFile(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	sc := testScope()
	raw, err := os.ReadFile(filepath.Join("testdata", "upstream-legacy.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.MkdirAll(s.Dir(sc), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(s.Path(sc), raw, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	g, err := s.Load(sc)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(g.Entities) != 2 || len(g.Relations) != 1 {
		t.Fatalf("loaded %d entities / %d relations, want 2/1", len(g.Entities), len(g.Relations))
	}
	if g.Entities[0].Observations[0].Confidence != 1.0 {
		t.Errorf("imported bare-string observation was not normalized: %+v", g.Entities[0].Observations[0])
	}
}
