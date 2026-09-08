package docker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// seedWorkspaceDir creates the on-disk half of a workspace: the nested layout
// whose PATH carries the tuple, which is what lets the inventory attribute a
// workspace without asking Docker anything.
func seedWorkspaceDir(t *testing.T, root, tenant, subs, role, user string) {
	t.Helper()
	p := filepath.Join(root, "tenants", tenant, "subscriptions", subs, "agents", role, "users", user)
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
}

func managedSummary(m *Manager, key WorkspaceKey, state, mode string) ContainerSummary {
	return ContainerSummary{
		ID:    "id-" + key.UserAccID,
		Names: []string{"/" + m.ContainerName(key)},
		State: state,
		Labels: map[string]string{
			LabelManaged:      "true",
			LabelAgent:        key.Role,
			LabelTenant:       key.TenantID,
			LabelSubscription: key.SubsAccID,
			LabelUser:         key.UserAccID,
			LabelMode:         mode,
		},
	}
}

// The union is the feature. Each of the four states comes from a different
// combination of the two surfaces, and three of them cannot be produced by
// looking at either surface alone.
func TestInstancesUnionOfContainersAndDisk(t *testing.T) {
	f := newFakeDocker()
	m, _ := testManager(t, config.ModeContinuous, f)
	root := m.cfg.ContainerDataRoot

	running := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	stopped := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u2"}
	provisioned := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u3"}
	orphan := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u4"}

	// Three have directories; the orphan deliberately does not.
	seedWorkspaceDir(t, root, "t1", "s1", "alpha", "u1")
	seedWorkspaceDir(t, root, "t1", "s1", "alpha", "u2")
	seedWorkspaceDir(t, root, "t1", "s1", "alpha", "u3")

	// Three have containers; the provisioned one deliberately does not.
	f.listResult = []ContainerSummary{
		managedSummary(m, running, "running", "continuous"),
		managedSummary(m, stopped, "exited", "continuous"),
		managedSummary(m, orphan, "running", "continuous"),
	}

	got, err := m.Instances(context.Background())
	if err != nil {
		t.Fatalf("Instances: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d instances, want 4: %+v", len(got), got)
	}

	// Sorted by tuple, so u1..u4 are in order and the assertion is stable.
	want := []InstanceState{InstanceRunning, InstanceStopped, InstanceProvisioned, InstanceOrphaned}
	for i, w := range want {
		if got[i].State != w {
			t.Errorf("instance %d (user %s) state = %q, want %q",
				i, got[i].UserAccID, got[i].State, w)
		}
	}

	// A provisioned workspace has no container, so its name is derived rather
	// than read back — a consumer must still be told which container it WOULD be.
	if got[2].ContainerName != m.ContainerName(provisioned) {
		t.Errorf("provisioned container name = %q, want the derived %q",
			got[2].ContainerName, m.ContainerName(provisioned))
	}
	if got[0].Mode != "continuous" {
		t.Errorf("mode = %q, want continuous", got[0].Mode)
	}
}

// The tuple must come from the LABELS. It cannot come from the name: that name
// is <prefix>-<role>-<sha256(tenant::subs::user)[:16]>, and the hash is one-way.
// This is the fact the whole endpoint exists to work around.
func TestInstancesRecoversTupleFromLabelsNotName(t *testing.T) {
	f := newFakeDocker()
	m, _ := testManager(t, config.ModeContinuous, f)
	key := WorkspaceKey{TenantID: "tenant-uuid", SubsAccID: "subs-uuid", Role: "alpha", UserAccID: "user-uuid"}
	seedWorkspaceDir(t, m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	f.listResult = []ContainerSummary{managedSummary(m, key, "running", "continuous")}

	got, err := m.Instances(context.Background())
	if err != nil {
		t.Fatalf("Instances: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d instances, want 1", len(got))
	}
	in := got[0]
	if in.TenantID != key.TenantID || in.SubsAccID != key.SubsAccID ||
		in.Agent != key.Role || in.UserAccID != key.UserAccID {
		t.Fatalf("tuple = %+v, want %+v", in, key)
	}
	// Guard against the name accidentally becoming the source: it must not
	// contain the tuple at all.
	if in.ContainerName == "" {
		t.Fatal("container name is empty")
	}
	for _, part := range []string{key.TenantID, key.SubsAccID, key.UserAccID} {
		if contains(in.ContainerName, part) {
			t.Errorf("container name %q leaks %q — the name is supposed to hash the tuple",
				in.ContainerName, part)
		}
	}
}

// FR-P2, at the layer where it can actually be violated: reading the inventory
// must not create, start, stop or remove anything.
func TestInstancesTouchesNoContainerLifecycle(t *testing.T) {
	f := newFakeDocker()
	m, _ := testManager(t, config.ModeContinuous, f)
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
	seedWorkspaceDir(t, m.cfg.ContainerDataRoot, "t1", "s1", "alpha", "u1")
	f.listResult = []ContainerSummary{managedSummary(m, key, "running", "continuous")}

	if _, err := m.Instances(context.Background()); err != nil {
		t.Fatalf("Instances: %v", err)
	}
	if f.createN != 0 || f.startN != 0 || f.stopN != 0 || f.removeN != 0 {
		t.Errorf("lifecycle calls create=%d start=%d stop=%d remove=%d; all must be 0",
			f.createN, f.startN, f.stopN, f.removeN)
	}
}

func TestInstancesEmptyWhenNothingExists(t *testing.T) {
	f := newFakeDocker()
	m, _ := testManager(t, config.ModeContinuous, f)
	got, err := m.Instances(context.Background())
	if err != nil {
		t.Fatalf("Instances: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d instances, want 0: %+v", len(got), got)
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && stringIndex(s, sub) >= 0
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
