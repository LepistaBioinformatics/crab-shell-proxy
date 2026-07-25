package registry

import (
	"errors"
	"testing"
)

func TestReferrersFindsAllFourKinds(t *testing.T) {
	r := testRegistry(t)
	mustCreate(t, r, "target")
	mustCreate(t, r, "primary", "target") // fallback referrer
	mustCreate(t, r, "successor")

	ref := WorkspaceRef{TenantID: "t", SubsAccID: "s", Agent: "alpha", UserAccID: "u"}
	if err := r.PutAssignment(ref, Assignment{ModelName: "target", Source: SourceInherited}); err != nil {
		t.Fatalf("PutAssignment: %v", err)
	}
	if err := r.PutScopeDefault("tenant/t", ScopeDefault{ModelName: "target"}); err != nil {
		t.Fatalf("PutScopeDefault: %v", err)
	}
	// successor -> deprecated placeholder pointing at target via replaced_by.
	if _, err := r.UpdateModelRaw("successor", func(m *Model) error {
		m.Status = StatusDeprecated
		m.ReplacedBy = "target"
		return nil
	}); err != nil {
		t.Fatalf("seed replaced_by: %v", err)
	}

	got, err := r.Referrers("target")
	if err != nil {
		t.Fatalf("Referrers: %v", err)
	}
	kinds := map[string]bool{}
	for _, rr := range got {
		kinds[rr.Kind] = true
	}
	for _, want := range []string{"workspace", "scope_default", "replaced_by", "fallback"} {
		if !kinds[want] {
			t.Errorf("missing referrer kind %q in %+v", want, got)
		}
	}
}

func TestReferrersCountsChainMembership(t *testing.T) {
	r := testRegistry(t)
	mustCreate(t, r, "fb")
	mustCreate(t, r, "main", "fb")

	ref := WorkspaceRef{TenantID: "t", SubsAccID: "s", Agent: "alpha", UserAccID: "u"}
	if err := r.PutAssignment(ref, Assignment{ModelName: "main", Chain: []string{"fb"}, Source: SourceInherited}); err != nil {
		t.Fatalf("PutAssignment: %v", err)
	}

	// A workspace holding "fb" only as a fallback still counts as a workspace
	// referrer: it has fb's key on disk and names it in model_fallbacks.
	got, err := r.Referrers("fb")
	if err != nil {
		t.Fatalf("Referrers: %v", err)
	}
	var ws int
	for _, rr := range got {
		if rr.Kind == "workspace" {
			ws++
		}
	}
	if ws != 1 {
		t.Errorf("workspace referrers via chain = %d, want 1 (%+v)", ws, got)
	}
}

func TestDeleteModelBlockedWhileReferencedThenAllowedAfterDetach(t *testing.T) {
	r := testRegistry(t)
	mustCreate(t, r, "fb")
	main := mustCreate(t, r, "main", "fb")

	var inUse *InUseError
	err := r.DeleteModel("fb")
	if !errors.As(err, &inUse) {
		t.Fatalf("delete referenced: want *InUseError, got %v", err)
	}
	if len(inUse.Referrers) == 0 || inUse.Referrers[0].Kind != "fallback" {
		t.Errorf("referrers should name the fallback holder: %+v", inUse.Referrers)
	}
	if _, err := r.GetModel("fb"); err != nil {
		t.Errorf("rejected delete still removed the record: %v", err)
	}

	// Detaching is the concrete action the rejection points at.
	if _, err := r.UpdateModel("main", main.Version, func(m *Model) error {
		m.Fallbacks = nil
		return nil
	}); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if err := r.DeleteModel("fb"); err != nil {
		t.Fatalf("delete after detach: %v", err)
	}
	if _, err := r.GetModel("fb"); !errors.Is(err, ErrNotFound) {
		t.Errorf("model still present after delete: %v", err)
	}
}

func TestSetStatusDisabledSharesTheDeletePrecondition(t *testing.T) {
	r := testRegistry(t)
	mustCreate(t, r, "fb")
	mustCreate(t, r, "main", "fb")
	fb, _ := r.GetModel("fb")

	var inUse *InUseError
	_, err := r.SetStatus("fb", fb.Version, StatusDisabled, "")
	if !errors.As(err, &inUse) {
		t.Fatalf("disable referenced: want *InUseError, got %v", err)
	}
	after, _ := r.GetModel("fb")
	if after.Status != StatusActive {
		t.Errorf("rejected disable still wrote: status = %q", after.Status)
	}
}

func TestSetStatusDisableAndReactivateUnreferencedModel(t *testing.T) {
	r := testRegistry(t)
	m := mustCreate(t, r, "shelf")

	off, err := r.SetStatus("shelf", m.Version, StatusDisabled, "")
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if off.Status != StatusDisabled {
		t.Errorf("status = %q, want disabled", off.Status)
	}
	back, err := r.SetStatus("shelf", off.Version, StatusActive, "")
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if back.Status != StatusActive || back.Position != m.Position {
		t.Errorf("reactivated = status %q position %d, want active and position %d preserved",
			back.Status, back.Position, m.Position)
	}
}

func TestDeleteUnknownModelIsNotFound(t *testing.T) {
	r := testRegistry(t)
	if err := r.DeleteModel("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
