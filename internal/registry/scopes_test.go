package registry

import (
	"errors"
	"testing"
)

func TestScopeSelKeyShapes(t *testing.T) {
	cases := []struct {
		name string
		sel  ScopeSel
		want string
	}{
		{"global", ScopeSel{Level: LevelGlobal}, "global"},
		{"agent", ScopeSel{Level: LevelAgent, Agent: "alpha"}, "agent/alpha"},
		{"tenant", ScopeSel{Level: LevelTenant, TenantID: "t1"}, "tenant/t1"},
		{"subscription", ScopeSel{Level: LevelSubscription, TenantID: "t1", SubsAccID: "s1"}, "subs/t1/s1"},
	}
	for _, c := range cases {
		got, err := c.sel.Key()
		if err != nil {
			t.Errorf("%s: Key: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: Key = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestScopeSelKeyRejectsMissingIdentifiers(t *testing.T) {
	bad := []ScopeSel{
		{Level: LevelAgent},
		{Level: LevelTenant},
		{Level: LevelSubscription, TenantID: "t1"},
		{Level: "nonsense"},
		{Level: LevelUser, TenantID: "t1", SubsAccID: "s1"},
	}
	for _, sel := range bad {
		if _, err := sel.Key(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: want ErrInvalid, got %v", sel, err)
		}
	}
}

func TestSetScopeDefaultRequiresAnActiveModel(t *testing.T) {
	r := testRegistry(t)
	sel := ScopeSel{Level: LevelTenant, TenantID: "t1"}

	if err := r.SetScopeDefault(sel, "ghost"); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown model: want ErrInvalid, got %v", err)
	}

	shelf := mustCreate(t, r, "shelf")
	if _, err := r.SetStatus("shelf", shelf.Version, StatusDisabled, ""); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := r.SetScopeDefault(sel, "shelf"); !errors.Is(err, ErrInvalid) {
		t.Errorf("disabled model: want ErrInvalid, got %v", err)
	}

	mustCreate(t, r, "good")
	if err := r.SetScopeDefault(sel, "good"); err != nil {
		t.Fatalf("SetScopeDefault: %v", err)
	}
	got, err := r.GetScopeDefault(sel)
	if err != nil || got.ModelName != "good" {
		t.Errorf("GetScopeDefault = %+v (err %v), want good", got, err)
	}
}

func TestSetScopeDefaultAcceptsADeprecatedModelSoRetirementIsNotBlocked(t *testing.T) {
	r := testRegistry(t)
	old := mustCreate(t, r, "old")
	mustCreate(t, r, "new")
	sel := ScopeSel{Level: LevelTenant, TenantID: "t1"}
	if err := r.SetScopeDefault(sel, "old"); err != nil {
		t.Fatalf("seed default: %v", err)
	}

	// Deprecating a model that IS a scope default must stay possible: that is the
	// normal retirement path, and the resolver hops to the replacement for new
	// users without the admin having to re-point every scope first.
	if _, err := r.Deprecate("old", old.Version, "new"); err != nil {
		t.Fatalf("Deprecate a scope default: %v", err)
	}
}

func TestClearAndListScopeDefaults(t *testing.T) {
	r := testRegistry(t)
	mustCreate(t, r, "m")
	tenant := ScopeSel{Level: LevelTenant, TenantID: "t1"}
	subs := ScopeSel{Level: LevelSubscription, TenantID: "t1", SubsAccID: "s1"}
	if err := r.SetScopeDefault(tenant, "m"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetScopeDefault(subs, "m"); err != nil {
		t.Fatal(err)
	}

	all, err := r.ListScopeDefaults()
	if err != nil {
		t.Fatalf("ListScopeDefaults: %v", err)
	}
	if len(all) != 2 || all["tenant/t1"].ModelName != "m" || all["subs/t1/s1"].ModelName != "m" {
		t.Errorf("listed = %+v, want both keys", all)
	}

	if err := r.ClearScopeDefault(tenant); err != nil {
		t.Fatalf("ClearScopeDefault: %v", err)
	}
	if _, err := r.GetScopeDefault(tenant); !errors.Is(err, ErrNotFound) {
		t.Errorf("cleared default still present: %v", err)
	}
	// Clearing twice is a success — the admin's intent is already true.
	if err := r.ClearScopeDefault(tenant); err != nil {
		t.Errorf("second clear should be idempotent, got %v", err)
	}
}
