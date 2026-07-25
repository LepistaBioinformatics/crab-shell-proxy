package registry

import (
	"errors"
	"testing"
)

func mustCreate(t *testing.T, r *Registry, name string, fallbacks ...string) Model {
	t.Helper()
	m, err := r.CreateModel(Model{
		ModelName: name, Provider: "openai", Model: name,
		APIBase: "https://api.openai.com/v1", APIKey: "sk-" + name,
		Status: StatusActive, Fallbacks: fallbacks,
	})
	if err != nil {
		t.Fatalf("CreateModel(%q): %v", name, err)
	}
	return m
}

func TestCreateModelStampsVersionAndTimestamps(t *testing.T) {
	r := testRegistry(t)
	m := mustCreate(t, r, "gpt-5.4")

	if m.Version != 1 {
		t.Errorf("Version = %d, want 1", m.Version)
	}
	if m.CreatedAt.IsZero() || !m.UpdatedAt.Equal(m.CreatedAt) {
		t.Errorf("timestamps = %v / %v, want both set and equal", m.CreatedAt, m.UpdatedAt)
	}

	got, err := r.GetModel("gpt-5.4")
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if got.APIKey != "sk-gpt-5.4" {
		t.Errorf("stored APIKey = %q, want the key round-tripped", got.APIKey)
	}
}

func TestCreateModelRejectsDuplicateName(t *testing.T) {
	r := testRegistry(t)
	mustCreate(t, r, "gpt-5.4")

	// picoclaw itself permits same-named model_list entries as a round-robin
	// group; the inventory forbids them because model_name also keys
	// .security.yml, so homonyms would share one credential slot.
	_, err := r.CreateModel(Model{ModelName: "gpt-5.4", Provider: "azure", Model: "x", APIBase: "https://y", Status: StatusActive})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate create: want ErrDuplicate, got %v", err)
	}
}

func TestCreateModelRejectsSelfFallbackAndUnknownFallback(t *testing.T) {
	r := testRegistry(t)

	_, err := r.CreateModel(Model{
		ModelName: "a", Provider: "p", Model: "a", APIBase: "https://x",
		Status: StatusActive, Fallbacks: []string{"a"},
	})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("self-fallback: want ErrInvalid, got %v", err)
	}

	_, err = r.CreateModel(Model{
		ModelName: "b", Provider: "p", Model: "b", APIBase: "https://x",
		Status: StatusActive, Fallbacks: []string{"nope"},
	})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown fallback: want ErrInvalid, got %v", err)
	}
}

func TestUpdateModelBumpsVersionAndRejectsStale(t *testing.T) {
	r := testRegistry(t)
	m := mustCreate(t, r, "gpt-5.4")

	updated, err := r.UpdateModel("gpt-5.4", m.Version, func(cur *Model) error {
		cur.APIBase = "https://proxy.internal/v1"
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateModel: %v", err)
	}
	if updated.Version != 2 || updated.APIBase != "https://proxy.internal/v1" {
		t.Errorf("updated = version %d base %q, want 2 and the new base", updated.Version, updated.APIBase)
	}

	// The stale version must be rejected AND nothing written.
	_, err = r.UpdateModel("gpt-5.4", m.Version, func(cur *Model) error {
		cur.APIBase = "https://clobber"
		return nil
	})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale update: want ErrVersionConflict, got %v", err)
	}
	after, _ := r.GetModel("gpt-5.4")
	if after.APIBase != "https://proxy.internal/v1" {
		t.Errorf("rejected update still wrote: base = %q", after.APIBase)
	}
}

func TestUpdateModelKeepsKeyWhenMutatorLeavesItAlone(t *testing.T) {
	r := testRegistry(t)
	m := mustCreate(t, r, "gpt-5.4")

	if _, err := r.UpdateModel("gpt-5.4", m.Version, func(cur *Model) error {
		cur.Provider = "azure"
		return nil
	}); err != nil {
		t.Fatalf("UpdateModel: %v", err)
	}
	got, _ := r.GetModel("gpt-5.4")
	if got.APIKey != "sk-gpt-5.4" {
		t.Errorf("APIKey lost on unrelated update: %q", got.APIKey)
	}
}

func TestListModelsSortsByPositionThenName(t *testing.T) {
	r := testRegistry(t)
	mustCreate(t, r, "c")
	mustCreate(t, r, "a")
	mustCreate(t, r, "b")

	if err := r.SetPositions([]string{"b", "a", "c"}); err != nil {
		t.Fatalf("SetPositions: %v", err)
	}
	got, err := r.ListModels()
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	names := []string{got[0].ModelName, got[1].ModelName, got[2].ModelName}
	if names[0] != "b" || names[1] != "a" || names[2] != "c" {
		t.Errorf("order = %v, want [b a c]", names)
	}
}

func TestSetPositionsDoesNotBumpVersion(t *testing.T) {
	r := testRegistry(t)
	m := mustCreate(t, r, "a")
	mustCreate(t, r, "b")

	if err := r.SetPositions([]string{"b", "a"}); err != nil {
		t.Fatalf("SetPositions: %v", err)
	}
	got, _ := r.GetModel("a")
	// Position is presentation only. Bumping Version would make a harmless drag
	// invalidate every open edit form with a spurious 409.
	if got.Version != m.Version {
		t.Errorf("Version = %d after reorder, want %d unchanged", got.Version, m.Version)
	}
}

func TestGetModelUnknownIsNotFound(t *testing.T) {
	r := testRegistry(t)
	if _, err := r.GetModel("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
