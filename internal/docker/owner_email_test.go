package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The owner marker is where a member's email lives on disk, and it is what
// ListSubscriptionUsers labels them from -- so the mangrove's directory can only
// find somebody whose marker carries their address.
//
// It was written ONCE, on first provision, and one provisioning path (reconcile,
// at boot) passes no email at all. A workspace that came up that way recorded ""
// and kept it forever: every directory search answered with nothing while the
// address sat in mycelium the whole time.

func markerEmail(t *testing.T, dir string) string {
	t.Helper()
	return ownerEmail(dir)
}

func key() WorkspaceKey {
	return WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}
}

// THE CASE THAT WAS BROKEN: a workspace already on disk with an empty marker.
func TestAnEmptyOwnerEmailIsHealedOnTheNextTurn(t *testing.T) {
	dir := t.TempDir()
	if err := writeOwnerFile(dir, key(), ""); err != nil {
		t.Fatal(err)
	}
	if got := markerEmail(t, dir); got != "" {
		t.Fatalf("setup: marker = %q, want empty", got)
	}

	if err := refreshOwnerEmail(dir, key(), "member@x.test"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := markerEmail(t, dir); got != "member@x.test" {
		t.Errorf("marker = %q; the workspace is still unfindable by email", got)
	}
}

// Reconcile passes "" at boot. A sweep must not undo what a turn wrote.
func TestAnEmptyIncomingEmailNeverOverwritesAGoodOne(t *testing.T) {
	dir := t.TempDir()
	if err := writeOwnerFile(dir, key(), "member@x.test"); err != nil {
		t.Fatal(err)
	}
	if err := refreshOwnerEmail(dir, key(), ""); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := markerEmail(t, dir); got != "member@x.test" {
		t.Errorf("marker = %q, want the address to survive a boot sweep", got)
	}
}

// A changed address follows the account.
func TestAChangedEmailIsPickedUp(t *testing.T) {
	dir := t.TempDir()
	if err := writeOwnerFile(dir, key(), "old@x.test"); err != nil {
		t.Fatal(err)
	}
	if err := refreshOwnerEmail(dir, key(), "new@x.test"); err != nil {
		t.Fatal(err)
	}
	if got := markerEmail(t, dir); got != "new@x.test" {
		t.Errorf("marker = %q, want the new address", got)
	}
}

// Before the workspace exists there is nothing to refresh, and provision writes
// the marker itself. Touching a path that is not there would create a stray file
// outside any workspace.
func TestNothingIsWrittenBeforeTheWorkspaceExists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-provisioned-yet")
	if err := refreshOwnerEmail(dir, key(), "member@x.test"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("a workspace directory was created by a refresh")
	}
}

// The rest of the tuple survives: the marker is also how an operator finds which
// human a container belongs to.
func TestTheRefreshKeepsTheWholeTuple(t *testing.T) {
	dir := t.TempDir()
	if err := writeOwnerFile(dir, key(), ""); err != nil {
		t.Fatal(err)
	}
	if err := refreshOwnerEmail(dir, key(), "member@x.test"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".crab-owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"tenantId": "t1"`, `"subsAccId": "s1"`, `"role": "alpha"`, `"userAccId": "u1"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("marker lost %s:\n%s", want, raw)
		}
	}
}
