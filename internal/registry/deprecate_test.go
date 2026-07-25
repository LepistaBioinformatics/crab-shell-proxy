package registry

import (
	"errors"
	"strings"
	"testing"
)

func TestDeprecateRequiresAnExistingActiveReplacement(t *testing.T) {
	r := testRegistry(t)
	old := mustCreate(t, r, "old")

	// No replacement at all.
	if _, err := r.Deprecate("old", old.Version, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("no replacement: want ErrInvalid, got %v", err)
	}
	// A replacement that does not exist.
	if _, err := r.Deprecate("old", old.Version, "ghost"); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown replacement: want ErrInvalid, got %v", err)
	}
	// A replacement that exists but is disabled — it could not serve anyone.
	shelf := mustCreate(t, r, "shelf")
	if _, err := r.SetStatus("shelf", shelf.Version, StatusDisabled, ""); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := r.Deprecate("old", old.Version, "shelf"); !errors.Is(err, ErrInvalid) {
		t.Errorf("disabled replacement: want ErrInvalid, got %v", err)
	}
	// Itself.
	if _, err := r.Deprecate("old", old.Version, "old"); !errors.Is(err, ErrInvalid) {
		t.Errorf("self replacement: want ErrInvalid, got %v", err)
	}

	after, _ := r.GetModel("old")
	if after.Status != StatusActive || after.ReplacedBy != "" {
		t.Errorf("rejected deprecations still wrote: %+v", after)
	}
}

func TestDeprecateSucceedsAndRecordsTheReplacement(t *testing.T) {
	r := testRegistry(t)
	old := mustCreate(t, r, "old")
	mustCreate(t, r, "new")

	got, err := r.Deprecate("old", old.Version, "new")
	if err != nil {
		t.Fatalf("Deprecate: %v", err)
	}
	if got.Status != StatusDeprecated || got.ReplacedBy != "new" {
		t.Errorf("deprecated = %+v, want status deprecated replaced_by new", got)
	}
	if got.Version != old.Version+1 {
		t.Errorf("Version = %d, want %d", got.Version, old.Version+1)
	}
}

func TestDeprecateRejectsACycle(t *testing.T) {
	r := testRegistry(t)
	a := mustCreate(t, r, "a")
	b := mustCreate(t, r, "b")

	// a -> b is fine while b is active.
	if _, err := r.Deprecate("a", a.Version, "b"); err != nil {
		t.Fatalf("first deprecate: %v", err)
	}
	// b -> a would close a loop: a is deprecated pointing at b.
	if _, err := r.Deprecate("b", b.Version, "a"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cycle: want ErrInvalid, got %v", err)
	}
	after, _ := r.GetModel("b")
	if after.Status != StatusActive {
		t.Errorf("rejected cycle still wrote: %+v", after)
	}
}

func TestResolveReplacementWalksTheChainToAnActiveModel(t *testing.T) {
	r := testRegistry(t)
	v1 := mustCreate(t, r, "v1")
	v2 := mustCreate(t, r, "v2")
	mustCreate(t, r, "v3")

	if _, err := r.Deprecate("v2", v2.Version, "v3"); err != nil {
		t.Fatalf("deprecate v2: %v", err)
	}
	if _, err := r.Deprecate("v1", v1.Version, "v2"); err != nil {
		t.Fatalf("deprecate v1: %v", err)
	}

	got, err := r.ResolveReplacement("v1")
	if err != nil {
		t.Fatalf("ResolveReplacement: %v", err)
	}
	if got.ModelName != "v3" {
		t.Errorf("resolved = %q, want v3 (v1 -> v2 -> v3)", got.ModelName)
	}
}

func TestResolveReplacementBoundsTheHopCount(t *testing.T) {
	r := testRegistry(t)
	// Build a chain longer than the bound using the raw mutator, which skips the
	// cycle check the public API enforces — this asserts the traversal itself is
	// bounded and does not rely on write-time validation alone.
	names := []string{"m0", "m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8", "m9", "m10"}
	for _, n := range names {
		mustCreate(t, r, n)
	}
	for i := 0; i < len(names)-1; i++ {
		next := names[i+1]
		if _, err := r.UpdateModelRaw(names[i], func(m *Model) error {
			m.Status = StatusDeprecated
			m.ReplacedBy = next
			return nil
		}); err != nil {
			t.Fatalf("seed chain: %v", err)
		}
	}

	_, err := r.ResolveReplacement("m0")
	if err == nil || !strings.Contains(err.Error(), "chain") {
		t.Fatalf("want a bounded-chain error, got %v", err)
	}
}

func TestResolveReplacementOnANonDeprecatedModelReturnsItself(t *testing.T) {
	r := testRegistry(t)
	mustCreate(t, r, "fine")

	got, err := r.ResolveReplacement("fine")
	if err != nil {
		t.Fatalf("ResolveReplacement: %v", err)
	}
	if got.ModelName != "fine" {
		t.Errorf("resolved = %q, want fine", got.ModelName)
	}
}
