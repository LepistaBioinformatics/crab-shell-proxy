package registry

import (
	"errors"
	"testing"
)

// selectOwn registers a personal model for ref()'s member and points the
// workspace at it.
func selectOwn(t *testing.T, r *Registry, slug string) UserModel {
	t.Helper()
	m := mustCreateOwn(t, r, "u1", slug)
	if err := r.SetUserSelection(ref(), slug); err != nil {
		t.Fatalf("SetUserSelection: %v", err)
	}
	return m
}

func TestPersonalModelOutranksAnAdminPin(t *testing.T) {
	r := testRegistry(t)
	mustCreate(t, r, "org")
	if err := r.PutAssignment(ref(), Assignment{ModelName: "org", Source: SourceExplicit}); err != nil {
		t.Fatalf("PutAssignment: %v", err)
	}
	selectOwn(t, r, "mine")

	res, err := r.Resolve(ref())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Primary.ModelName != OwnPrefix+"mine" {
		t.Errorf("Primary = %q, want the member's own model", res.Primary.ModelName)
	}
	if res.Level != LevelUserModel {
		t.Errorf("Level = %q, want %q — a personal model is not an admin pin", res.Level, LevelUserModel)
	}
	if res.UserModel == "" {
		t.Error("UserModel must name the personal record for the assignment write")
	}
}

// The whole point of the automatic fallback: a personal model that fails
// mid-turn degrades to the organisation's model instead of leaving the member
// with an agent that cannot answer.
func TestPersonalModelCarriesTheOrganisationsAsFallback(t *testing.T) {
	r := testRegistry(t)
	mustCreate(t, r, "org")
	if err := r.SetScopeDefault(ScopeSel{Level: LevelSubscription, TenantID: "t1", SubsAccID: "s1"}, "org"); err != nil {
		t.Fatalf("SetScopeDefault: %v", err)
	}
	selectOwn(t, r, "mine")

	res, err := r.Resolve(ref())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Chain) != 1 || res.Chain[0].ModelName != "org" {
		t.Fatalf("Chain = %v, want the cascade model as the runtime fallback", res.ChainNames())
	}
	if res.CascadeName != "org" {
		t.Errorf("CascadeName = %q, want org", res.CascadeName)
	}
}

// A member with their own key can work on an instance whose defaults are unset.
// The ordinary path refuses to provision there — and rightly — but that refusal
// has nothing to help with when the member supplied a working model themselves.
func TestPersonalModelResolvesWithNoCascadeAtAll(t *testing.T) {
	r := testRegistry(t)
	selectOwn(t, r, "mine")

	res, err := r.Resolve(ref())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Primary.ModelName != OwnPrefix+"mine" || len(res.Chain) != 0 || res.CascadeName != "" {
		t.Errorf("resolution = %+v, want the personal model with no fallback", res)
	}
}

func TestADisabledPersonalModelFallsBackToTheCascade(t *testing.T) {
	r := testRegistry(t)
	mustCreate(t, r, "org")
	if err := r.SetScopeDefault(ScopeSel{Level: LevelGlobal}, "org"); err != nil {
		t.Fatalf("SetScopeDefault: %v", err)
	}
	selectOwn(t, r, "mine")
	if _, err := r.SetUserModelEnabled("u1", "mine", false); err != nil {
		t.Fatalf("SetUserModelEnabled: %v", err)
	}

	res, err := r.Resolve(ref())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Primary.ModelName != "org" || res.Level != LevelGlobal {
		t.Errorf("resolution = %q/%q, want the cascade to take over", res.Primary.ModelName, res.Level)
	}
}

func TestAScopeLockIgnoresTheSelection(t *testing.T) {
	r := testRegistry(t)
	mustCreate(t, r, "org")
	if err := r.SetScopeDefault(ScopeSel{Level: LevelGlobal}, "org"); err != nil {
		t.Fatalf("SetScopeDefault: %v", err)
	}
	selectOwn(t, r, "mine")
	if err := r.SetScopePolicy(ScopeSel{Level: LevelTenant, TenantID: "t1"}, AllowUserModelsPolicy(false)); err != nil {
		t.Fatalf("SetScopePolicy: %v", err)
	}

	res, err := r.Resolve(ref())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Primary.ModelName != "org" {
		t.Errorf("Primary = %q, want the lock to win over the selection", res.Primary.ModelName)
	}
	// The selection is NOT deleted: a lock is reversible, and destroying the
	// member's choice would make lifting it a silent no-op.
	if _, err := r.GetUserSelection(ref()); err != nil {
		t.Errorf("the selection must survive a lock: %v", err)
	}
}

// The failure this design exists to avoid: recording `own-…` as the assignment's
// ModelName would preserve it under Source=explicit, and the next deselect would
// resolve a pin naming a model the inventory does not have — which candidateTx
// treats as a hard error, leaving the workspace unbootable.
func TestSelectingThenDeselectingRestoresTheAdminPin(t *testing.T) {
	r := testRegistry(t)
	mustCreate(t, r, "org")
	mustCreate(t, r, "pinned")
	if err := r.SetScopeDefault(ScopeSel{Level: LevelGlobal}, "org"); err != nil {
		t.Fatalf("SetScopeDefault: %v", err)
	}
	if err := r.PutAssignment(ref(), Assignment{ModelName: "pinned", Source: SourceExplicit}); err != nil {
		t.Fatalf("PutAssignment: %v", err)
	}

	// Select, and record the materialization the way the docker layer does.
	selectOwn(t, r, "mine")
	res, err := r.Resolve(ref())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := r.RecordMaterialization(ref(), res); err != nil {
		t.Fatalf("RecordMaterialization: %v", err)
	}
	a, err := r.GetAssignment(ref())
	if err != nil {
		t.Fatalf("GetAssignment: %v", err)
	}
	if a.ModelName != "pinned" {
		t.Fatalf("ModelName = %q, want the pin to survive under the personal model", a.ModelName)
	}
	if a.UserModel == "" {
		t.Error("UserModel must record what is actually primary")
	}

	// Deselect: the pin comes back, and resolution succeeds.
	if err := r.ClearUserSelection(ref()); err != nil {
		t.Fatalf("ClearUserSelection: %v", err)
	}
	back, err := r.Resolve(ref())
	if err != nil {
		t.Fatalf("Resolve after deselect: %v", err)
	}
	if back.Primary.ModelName != "pinned" || back.Level != LevelUser {
		t.Errorf("resolution = %q/%q, want the admin pin restored", back.Primary.ModelName, back.Level)
	}
}

// A member with no pin and no selection is unaffected by any of this.
func TestNoSelectionLeavesTheCascadeUntouched(t *testing.T) {
	r := testRegistry(t)
	if _, err := r.Resolve(ref()); !errors.Is(err, ErrNoModelResolvable) {
		t.Fatalf("want ErrNoModelResolvable, got %v", err)
	}
}
