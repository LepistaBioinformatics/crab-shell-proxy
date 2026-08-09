package registry

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func mustCreateOwn(t *testing.T, r *Registry, owner, slug string) UserModel {
	t.Helper()
	m, err := r.CreateUserModel(UserModel{
		OwnerAccID: owner, Slug: slug, Label: slug,
		Provider: "openai", Model: "gpt-5.4",
		APIBase: "https://api.openai.com/v1", APIKey: "sk-" + slug,
	})
	if err != nil {
		t.Fatalf("CreateUserModel(%q): %v", slug, err)
	}
	return m
}

func TestUserModelRequiresEndpointAndKey(t *testing.T) {
	r := testRegistry(t)
	base := UserModel{
		OwnerAccID: "u1", Slug: "mine", Provider: "openai", Model: "gpt-5.4",
		APIBase: "https://api.openai.com/v1", APIKey: "sk-1",
	}
	cases := map[string]func(*UserModel){
		"no owner":    func(m *UserModel) { m.OwnerAccID = "" },
		"bad slug":    func(m *UserModel) { m.Slug = "Not A Slug" },
		"no provider": func(m *UserModel) { m.Provider = "" },
		"no model":    func(m *UserModel) { m.Model = "" },
		// api_base is optional in the INVENTORY when auth_method is set. Here it
		// never is: the oauth branch that allows it is exactly what a member
		// cannot complete, and what the probe cannot represent.
		"no api_base": func(m *UserModel) { m.APIBase = "" },
		"no api_key":  func(m *UserModel) { m.APIKey = "" },
	}
	for name, break_ := range cases {
		m := base
		break_(&m)
		if _, err := r.CreateUserModel(m); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func TestUserModelSlugsAreScopedToTheirOwner(t *testing.T) {
	r := testRegistry(t)
	mustCreateOwn(t, r, "u1", "mine")
	// The same slug for a different member is NOT a duplicate: personal names
	// live in a per-account namespace, which is half the reason they are not rows
	// in the shared inventory.
	mustCreateOwn(t, r, "u2", "mine")

	if _, err := r.CreateUserModel(UserModel{
		OwnerAccID: "u1", Slug: "mine", Provider: "openai", Model: "x",
		APIBase: "https://api.openai.com/v1", APIKey: "sk-2",
	}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("same owner + slug: want ErrDuplicate, got %v", err)
	}

	list, err := r.ListUserModels("u1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListUserModels(u1) = %d entries, %v", len(list), err)
	}
}

func TestUserModelListIsNeverAnotherMembersList(t *testing.T) {
	r := testRegistry(t)
	mustCreateOwn(t, r, "u1", "a")
	mustCreateOwn(t, r, "u1", "b")
	mustCreateOwn(t, r, "u2", "c")

	list, err := r.ListUserModels("u1")
	if err != nil {
		t.Fatalf("ListUserModels: %v", err)
	}
	for _, m := range list {
		if m.OwnerAccID != "u1" {
			t.Fatalf("leaked %s/%s into u1's list", m.OwnerAccID, m.Slug)
		}
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
}

func TestPublicUserModelHasNoKey(t *testing.T) {
	r := testRegistry(t)
	m := mustCreateOwn(t, r, "u1", "mine")

	raw, err := json.Marshal(PublicUser(m))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "sk-mine") || strings.Contains(string(raw), "api_key") {
		t.Fatalf("public shape carries the credential: %s", raw)
	}
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	if decoded["has_key"] != true {
		t.Error("has_key must report that a credential is stored")
	}
}

func TestEditingWhatIsSentDropsTheTestResult(t *testing.T) {
	r := testRegistry(t)
	mustCreateOwn(t, r, "u1", "mine")
	if err := r.RecordUserModelTest("u1", "mine", TestResult{OK: true}); err != nil {
		t.Fatalf("RecordUserModelTest: %v", err)
	}

	// A label is not part of the request the probe makes, so the verdict stands.
	kept, err := r.UpdateUserModel("u1", "mine", 0, func(m *UserModel) error {
		m.Label = "renamed"
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateUserModel: %v", err)
	}
	if kept.LastTest == nil {
		t.Error("renaming must not invalidate a verdict about the endpoint")
	}

	// The endpoint is. Keeping the old verdict would assert something about a
	// request nobody ever made.
	moved, err := r.UpdateUserModel("u1", "mine", 0, func(m *UserModel) error {
		m.APIBase = "https://api.example.com/v1"
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateUserModel: %v", err)
	}
	if moved.LastTest != nil {
		t.Error("changing api_base must drop the stale verdict")
	}
}

func TestDeletingAModelDropsTheSelectionsThatNameIt(t *testing.T) {
	r := testRegistry(t)
	mustCreateOwn(t, r, "u1", "mine")
	// Two workspaces, same member, different agents — both selected it.
	a := WorkspaceRef{TenantID: "t1", SubsAccID: "s1", Agent: "alpha", UserAccID: "u1"}
	b := WorkspaceRef{TenantID: "t1", SubsAccID: "s1", Agent: "beta", UserAccID: "u1"}
	for _, w := range []WorkspaceRef{a, b} {
		if err := r.SetUserSelection(w, "mine"); err != nil {
			t.Fatalf("SetUserSelection: %v", err)
		}
	}
	if refs, err := r.SelectionsOf("u1", "mine"); err != nil || len(refs) != 2 {
		t.Fatalf("SelectionsOf = %d, %v; want 2", len(refs), err)
	}

	if err := r.DeleteUserModel("u1", "mine"); err != nil {
		t.Fatalf("DeleteUserModel: %v", err)
	}
	// A surviving selection would make every one of those workspaces resolve a
	// model that no longer exists.
	if _, err := r.GetUserSelection(a); !errors.Is(err, ErrNotFound) {
		t.Errorf("selection survived the delete: %v", err)
	}
	if _, err := r.GetUserSelection(b); !errors.Is(err, ErrNotFound) {
		t.Errorf("second workspace's selection survived the delete: %v", err)
	}
}

func TestSelectingADisabledModelIsRefused(t *testing.T) {
	r := testRegistry(t)
	mustCreateOwn(t, r, "u1", "mine")
	if _, err := r.SetUserModelEnabled("u1", "mine", false); err != nil {
		t.Fatalf("SetUserModelEnabled: %v", err)
	}
	// Storing it would look like it worked and change nothing — the failure this
	// feature exists to remove, arrived at from the other side.
	if err := r.SetUserSelection(ref(), "mine"); !errors.Is(err, ErrUserModelDisabled) {
		t.Errorf("want ErrUserModelDisabled, got %v", err)
	}
}

func TestScopePolicyMostSpecificWins(t *testing.T) {
	r := testRegistry(t)
	w := ref()

	if allowed, by, err := r.UserModelsAllowed(w); err != nil || !allowed || by != "" {
		t.Fatalf("unset everywhere = (%v, %q, %v); want allowed by nothing", allowed, by, err)
	}

	if err := r.SetScopePolicy(ScopeSel{Level: LevelGlobal}, AllowUserModelsPolicy(false)); err != nil {
		t.Fatalf("SetScopePolicy: %v", err)
	}
	if allowed, by, _ := r.UserModelsAllowed(w); allowed || by != LevelGlobal {
		t.Fatalf("global deny = (%v, %q); want blocked by global", allowed, by)
	}

	// A narrower ALLOW overrides a wider deny: the lock is a cascade, not a
	// one-way ratchet, so a tenant can opt one subscription back in.
	if err := r.SetScopePolicy(ScopeSel{Level: LevelSubscription, TenantID: "t1", SubsAccID: "s1"}, AllowUserModelsPolicy(true)); err != nil {
		t.Fatalf("SetScopePolicy: %v", err)
	}
	if allowed, by, _ := r.UserModelsAllowed(w); !allowed || by != LevelSubscription {
		t.Fatalf("subscription allow = (%v, %q); want allowed by subscription", allowed, by)
	}

	if err := r.ClearScopePolicy(ScopeSel{Level: LevelSubscription, TenantID: "t1", SubsAccID: "s1"}); err != nil {
		t.Fatalf("ClearScopePolicy: %v", err)
	}
	if allowed, by, _ := r.UserModelsAllowed(w); allowed || by != LevelGlobal {
		t.Fatalf("after clear = (%v, %q); want the global deny to apply again", allowed, by)
	}
}

func TestInventoryRefusesTheReservedPrefix(t *testing.T) {
	r := testRegistry(t)
	_, err := r.CreateModel(Model{
		ModelName: OwnPrefix + "mine", Provider: "openai", Model: "gpt-5.4",
		APIBase: "https://api.openai.com/v1", Status: StatusActive,
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid for a reserved name, got %v", err)
	}
}

func TestMaxUserModelsPerAccount(t *testing.T) {
	r := testRegistry(t)
	for i := 0; i < MaxUserModelsPerAccount; i++ {
		mustCreateOwn(t, r, "u1", "m"+string(rune('a'+i)))
	}
	if _, err := r.CreateUserModel(UserModel{
		OwnerAccID: "u1", Slug: "one-too-many", Provider: "openai", Model: "x",
		APIBase: "https://api.openai.com/v1", APIKey: "sk",
	}); !errors.Is(err, ErrUserModelLimit) {
		t.Errorf("want ErrUserModelLimit past the cap, got %v", err)
	}
	// The cap is per account, so another member is unaffected by the first one
	// filling theirs.
	mustCreateOwn(t, r, "u2", "mine")
}

// The two switches live in one record and are set from different controls, so a
// write of one must not reset the other.
func TestSettingOneSwitchLeavesTheOtherAlone(t *testing.T) {
	r := testRegistry(t)
	sel := ScopeSel{Level: LevelTenant, TenantID: "t1"}

	if err := r.SetScopePolicy(sel, AllowCustomEndpointPolicy(true)); err != nil {
		t.Fatalf("SetScopePolicy: %v", err)
	}
	if err := r.SetScopePolicy(sel, AllowUserModelsPolicy(false)); err != nil {
		t.Fatalf("SetScopePolicy: %v", err)
	}

	p, err := r.GetScopePolicy(sel)
	if err != nil {
		t.Fatalf("GetScopePolicy: %v", err)
	}
	if p.AllowCustomEndpoint == nil || !*p.AllowCustomEndpoint {
		t.Error("the endpoint permission was cleared by a write to the other switch")
	}
	if p.AllowUserModels == nil || *p.AllowUserModels {
		t.Error("the personal-model lock did not take")
	}
}

// The two defaults point in opposite directions on purpose: the feature is on
// unless an administrator objects, and naming an endpoint is off until one
// agrees.
func TestCustomEndpointsAreRefusedUntilAnAdminAgrees(t *testing.T) {
	r := testRegistry(t)

	if allowed, by, err := r.CustomEndpointAllowed(ref()); err != nil || allowed || by != "" {
		t.Fatalf("unset = (%v, %q, %v); want refused by nothing", allowed, by, err)
	}
	// While personal models, unset, are allowed.
	if allowed, _, _ := r.UserModelsAllowed(ref()); !allowed {
		t.Error("personal models must be allowed when nothing is set")
	}

	if err := r.SetScopePolicy(ScopeSel{Level: LevelSubscription, TenantID: "t1", SubsAccID: "s1"},
		AllowCustomEndpointPolicy(true)); err != nil {
		t.Fatalf("SetScopePolicy: %v", err)
	}
	if allowed, by, _ := r.CustomEndpointAllowed(ref()); !allowed || by != LevelSubscription {
		t.Errorf("after the grant = (%v, %q); want allowed by subscription", allowed, by)
	}
}
