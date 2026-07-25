package registry

import (
	"errors"
	"testing"
)

func ref() WorkspaceRef {
	return WorkspaceRef{TenantID: "t1", SubsAccID: "s1", Agent: "alpha", UserAccID: "u1"}
}

func TestResolveWalksEveryCascadeLevel(t *testing.T) {
	r := testRegistry(t)
	for _, n := range []string{"g", "ag", "te", "su", "us"} {
		mustCreate(t, r, n)
	}

	if _, err := r.Resolve(ref()); !errors.Is(err, ErrNoModelResolvable) {
		t.Fatalf("empty cascade: want ErrNoModelResolvable, got %v", err)
	}

	steps := []struct {
		set   func()
		want  string
		level ScopeLevel
	}{
		{func() { _ = r.SetScopeDefault(ScopeSel{Level: LevelGlobal}, "g") }, "g", LevelGlobal},
		{func() { _ = r.SetScopeDefault(ScopeSel{Level: LevelAgent, Agent: "alpha"}, "ag") }, "ag", LevelAgent},
		{func() { _ = r.SetScopeDefault(ScopeSel{Level: LevelTenant, TenantID: "t1"}, "te") }, "te", LevelTenant},
		{func() {
			_ = r.SetScopeDefault(ScopeSel{Level: LevelSubscription, TenantID: "t1", SubsAccID: "s1"}, "su")
		}, "su", LevelSubscription},
		{func() {
			_ = r.PutAssignment(ref(), Assignment{ModelName: "us", Source: SourceExplicit})
		}, "us", LevelUser},
	}
	for _, st := range steps {
		st.set()
		got, err := r.Resolve(ref())
		if err != nil {
			t.Fatalf("Resolve after setting %s: %v", st.level, err)
		}
		if got.Primary.ModelName != st.want || got.Level != st.level {
			t.Errorf("Resolve = %q at %q, want %q at %q",
				got.Primary.ModelName, got.Level, st.want, st.level)
		}
	}
}

func TestResolveIgnoresAnInheritedAssignmentAsAnOverride(t *testing.T) {
	r := testRegistry(t)
	mustCreate(t, r, "scope")
	mustCreate(t, r, "stale")
	if err := r.SetScopeDefault(ScopeSel{Level: LevelSubscription, TenantID: "t1", SubsAccID: "s1"}, "scope"); err != nil {
		t.Fatal(err)
	}
	// An inherited assignment records what WAS materialized, not a pin. Treating
	// it as an override would freeze every workspace at its first model and make
	// scope defaults inert after the first provision.
	if err := r.PutAssignment(ref(), Assignment{ModelName: "stale", Source: SourceInherited}); err != nil {
		t.Fatal(err)
	}

	got, err := r.Resolve(ref())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Primary.ModelName != "scope" || got.Level != LevelSubscription {
		t.Errorf("Resolve = %q at %q, want scope at subscription", got.Primary.ModelName, got.Level)
	}
}

func TestResolveHopsDeprecationOnlyForAnUnmaterializedWorkspace(t *testing.T) {
	r := testRegistry(t)
	old := mustCreate(t, r, "old")
	mustCreate(t, r, "new")
	if err := r.SetScopeDefault(ScopeSel{Level: LevelTenant, TenantID: "t1"}, "old"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Deprecate("old", old.Version, "new"); err != nil {
		t.Fatalf("Deprecate: %v", err)
	}

	// A brand-new workspace lands on the replacement.
	fresh, err := r.Resolve(ref())
	if err != nil {
		t.Fatalf("Resolve fresh: %v", err)
	}
	if fresh.Primary.ModelName != "new" {
		t.Errorf("fresh workspace = %q, want new", fresh.Primary.ModelName)
	}

	// One already running the deprecated model keeps it.
	if err := r.PutAssignment(ref(), Assignment{ModelName: "old", Source: SourceInherited}); err != nil {
		t.Fatal(err)
	}
	kept, err := r.Resolve(ref())
	if err != nil {
		t.Fatalf("Resolve materialized: %v", err)
	}
	if kept.Primary.ModelName != "old" {
		t.Errorf("materialized workspace = %q, want old kept", kept.Primary.ModelName)
	}
}

func TestResolveChainIsThePrimarysDeclaredFallbacksOneLevelDeep(t *testing.T) {
	r := testRegistry(t)
	mustCreate(t, r, "c")
	mustCreate(t, r, "b", "c") // b declares c, which must NOT be walked
	mustCreate(t, r, "a", "b")
	if err := r.SetScopeDefault(ScopeSel{Level: LevelGlobal}, "a"); err != nil {
		t.Fatal(err)
	}

	got, err := r.Resolve(ref())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got.Chain) != 1 || got.Chain[0].ModelName != "b" {
		names := make([]string, len(got.Chain))
		for i, m := range got.Chain {
			names[i] = m.ModelName
		}
		t.Errorf("chain = %v, want exactly [b] (one level, no transitive walk)", names)
	}
}

func TestResolveSkipsANonActiveFallbackAndReportsIt(t *testing.T) {
	r := testRegistry(t)
	mustCreate(t, r, "good")
	shelf := mustCreate(t, r, "shelf")
	mustCreate(t, r, "main", "shelf", "good")
	if err := r.SetScopeDefault(ScopeSel{Level: LevelGlobal}, "main"); err != nil {
		t.Fatal(err)
	}
	// Detach so disable is permitted, then re-attach via the raw mutator: this
	// reproduces the real state where a fallback was retired after being declared.
	if _, err := r.UpdateModelRaw("main", func(m *Model) error {
		m.Fallbacks = []string{"good"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetStatus("shelf", shelf.Version, StatusDisabled, ""); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := r.UpdateModelRaw("main", func(m *Model) error {
		m.Fallbacks = []string{"shelf", "good"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got, err := r.Resolve(ref())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got.Chain) != 1 || got.Chain[0].ModelName != "good" {
		t.Errorf("chain = %+v, want only good", got.Chain)
	}
	if len(got.Skipped) != 1 || got.Skipped[0] != "shelf" {
		t.Errorf("Skipped = %v, want [shelf] so the caller can log it", got.Skipped)
	}
}

func TestWorkspacesUsingFindsPrimaryAndChainHolders(t *testing.T) {
	r := testRegistry(t)
	mustCreate(t, r, "fb")
	mustCreate(t, r, "main", "fb")

	a := WorkspaceRef{TenantID: "t1", SubsAccID: "s1", Agent: "alpha", UserAccID: "u1"}
	b := WorkspaceRef{TenantID: "t1", SubsAccID: "s1", Agent: "alpha", UserAccID: "u2"}
	c := WorkspaceRef{TenantID: "t1", SubsAccID: "s1", Agent: "alpha", UserAccID: "u3"}
	if err := r.PutAssignment(a, Assignment{ModelName: "main", Chain: []string{"fb"}}); err != nil {
		t.Fatal(err)
	}
	if err := r.PutAssignment(b, Assignment{ModelName: "fb"}); err != nil {
		t.Fatal(err)
	}
	if err := r.PutAssignment(c, Assignment{ModelName: "other"}); err != nil {
		t.Fatal(err)
	}

	got, err := r.WorkspacesUsing("fb")
	if err != nil {
		t.Fatalf("WorkspacesUsing: %v", err)
	}
	// Both the chain holder and the primary holder must be returned: a key edit
	// that reached only primaries would leave the chain holder on a stale
	// credential.
	if len(got) != 2 {
		t.Fatalf("WorkspacesUsing = %+v, want 2 refs", got)
	}
	seen := map[string]bool{got[0].Key(): true, got[1].Key(): true}
	if !seen[a.Key()] || !seen[b.Key()] {
		t.Errorf("refs = %+v, want %s and %s", got, a.Key(), b.Key())
	}
}
