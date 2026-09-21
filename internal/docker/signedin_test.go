package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	mycelium "github.com/LepistaBioinformatics/mycelium-sdk-go"
	"github.com/google/uuid"
)

func ptr(s string) *string { return &s }

// Distinctive ids, not "t"/"s"/"u": one of these tests asserts that NONE of them
// reaches the document, and a one-letter id is a substring of ordinary prose.
const (
	seedTenant = "tenant-7f3a"
	seedSubs   = "subs-91c2"
	seedRole   = "alpha"
	seedUser   = "useracc-4d8e"
)

func seedKey() WorkspaceKey {
	return WorkspaceKey{
		TenantID: seedTenant, SubsAccID: seedSubs, Role: seedRole, UserAccID: seedUser,
	}
}

// seedManager builds a Manager whose writes land in a temp tree. PicoclawUser is
// empty on purpose: chownTree is a no-op then, so the whole of SeedSignedInUser
// runs without root.
func seedManager(t *testing.T) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	ws := filepath.Join(
		config.UserWorkspace(root, seedTenant, seedSubs, seedRole, seedUser),
		config.MainWorkspace)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	return &Manager{
		cfg:  &config.Config{ContainerDataRoot: root},
		logf: func(string, ...any) {},
	}, ws
}

// THE POINT OF THE FEATURE. The proxy has resolved the member's name on every
// turn since identity decoding existed and wrote it nowhere the prompt could see,
// so an agent opened every conversation knowing nothing about a signed-in person.
func TestTheSignedInAccountLandsWhereTheHarnessReadsIt(t *testing.T) {
	m, ws := seedManager(t)

	err := m.SeedSignedInUser(seedKey(), config.HarnessGanglion, Owner{
		Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace",
	})
	if err != nil {
		t.Fatalf("SeedSignedInUser: %v", err)
	}

	// The memory directory, because that is the one both harnesses read in full
	// every turn -- skills.Prompt enumerates every `.md` there rather than naming
	// files, which is why this needs no harness change.
	b, err := os.ReadFile(filepath.Join(ws, MemoryDirName, SignedInFileName))
	if err != nil {
		t.Fatalf("read %s: %v", SignedInFileName, err)
	}
	for _, want := range []string{"Ada Lovelace", "ada@example.com"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %q from:\n%s", want, b)
		}
	}
}

// FR-3. reconcile restarts containers with no caller and therefore no identity.
// Rendering a document from that would replace a correct file with an empty one
// at every restart -- and the symptom is the agent forgetting who it is talking
// to, which is the bug this feature exists to fix.
func TestARestartWithNoIdentityLeavesTheFileAlone(t *testing.T) {
	m, ws := seedManager(t)
	path := filepath.Join(ws, MemoryDirName, SignedInFileName)

	if err := m.SeedSignedInUser(seedKey(), config.HarnessGanglion, Owner{
		Email: "ada@example.com", FirstName: "Ada",
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.SeedSignedInUser(seedKey(), config.HarnessGanglion, Owner{}); err != nil {
		t.Fatalf("an empty owner must be a no-op, not an error: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the file was removed by an owner-less ensure: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("the document was rewritten from an empty profile:\n%s", after)
	}
}

// FR-2. The agent's own USER.md is where it accumulates what it learns; this file
// is derived from the login and replaced whenever it changes. Writing to the same
// name would destroy the first on every turn -- projects_test.go pins a regression
// with exactly that shape for the workspace-root USER.md.
func TestItNeverTouchesTheFileTheAgentOwns(t *testing.T) {
	m, ws := seedManager(t)
	learned := filepath.Join(ws, MemoryDirName, "USER.md")
	if err := os.MkdirAll(filepath.Dir(learned), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(learned, []byte("Prefers short answers.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := m.SeedSignedInUser(seedKey(), config.HarnessGanglion, Owner{
		FirstName: "Ada", Email: "ada@example.com",
	}); err != nil {
		t.Fatal(err)
	}

	if SignedInFileName == "USER.md" {
		t.Fatal("the derived document must not be named for the one the agent owns")
	}
	b, _ := os.ReadFile(learned)
	if string(b) != "Prefers short answers.\n" {
		t.Errorf("what the agent had learned was overwritten: %q", b)
	}
}

// The same boundary WriteMemory documents: the proxy is root, `workspace/memory`
// is owned by the agent's uid, and a plain WriteFile there follows whatever the
// agent planted as `memory`.
func TestTheWriteCannotBeRedirectedBySymlink(t *testing.T) {
	m, ws := seedManager(t)
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(ws, MemoryDirName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := m.SeedSignedInUser(seedKey(), config.HarnessGanglion, Owner{FirstName: "Ada"})
	if err == nil {
		t.Fatal("a redirected write succeeded")
	}
	if _, statErr := os.Stat(filepath.Join(elsewhere, SignedInFileName)); statErr == nil {
		t.Error("the document landed outside the workspace")
	}
}

// FR-5. An agent handed `Name: (unknown)` says "(unknown)" back to the member.
func TestAMissingFieldIsOmittedRatherThanPlaceheld(t *testing.T) {
	doc := signedInDoc(Owner{Email: "ada@example.com"})
	if strings.Contains(doc, "Name:") {
		t.Errorf("a name the profile did not carry was rendered:\n%s", doc)
	}
	if !strings.Contains(doc, "ada@example.com") {
		t.Errorf("the one field there was is missing:\n%s", doc)
	}
}

// Nothing to say means nothing written, rather than a heading over three absent
// bullets.
func TestAnEmptyOwnerWritesNothing(t *testing.T) {
	if !(Owner{}).Empty() {
		t.Error("an owner with no fields is not reported empty")
	}
	if (Owner{Username: "ada"}).Empty() {
		t.Error("a username alone is something to say")
	}
}

// It shares a directory with the file the agent owns, and the obvious place to
// record something learned about the member is the file that already describes
// them. Without the pointer the next turn would overwrite it.
func TestTheDocumentSendsWhatIsLearnedToTheAgentsOwnFile(t *testing.T) {
	doc := signedInDoc(Owner{FirstName: "Ada"})
	if !strings.Contains(doc, "memory/USER.md") {
		t.Errorf("the document does not say where a learned fact belongs:\n%s", doc)
	}
}

// FR-5. Isolation keys are not things to say to a person, and the only thing an
// agent can do with a UUID it was handed every turn is repeat it.
func TestTheDocumentCarriesNoAccountIdentifiers(t *testing.T) {
	m, ws := seedManager(t)
	key := seedKey()
	if err := m.SeedSignedInUser(key, config.HarnessGanglion, Owner{
		FirstName: "Ada", Email: "ada@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(ws, MemoryDirName, SignedInFileName))
	for _, id := range []string{key.TenantID, key.SubsAccID, key.UserAccID} {
		if strings.Contains(string(b), id) {
			t.Errorf("the document names the isolation key %q:\n%s", id, b)
		}
	}
}

// One mapping, shared by all three turn paths. A second copy is how one of them
// comes to send a different name than the others.
func TestOwnerFromReadsThePrincipalOwner(t *testing.T) {
	got := OwnerFrom(identity.Identity{
		Email: "principal@example.com",
		Profile: &mycelium.Profile{
			AccID: uuid.New(),
			Owners: []mycelium.Owner{
				{Email: "other@example.com", FirstName: ptr("Other")},
				{Email: "principal@example.com", FirstName: ptr("Ada"), LastName: ptr("Lovelace"),
					Username: ptr("ada"), IsPrincipal: true},
			},
		},
	})
	if got.Name() != "Ada Lovelace" || got.Username != "ada" {
		t.Errorf("OwnerFrom = %+v, want the principal owner's name", got)
	}
}

// A scheduled turn carries an e-mail and no profile at all.
func TestOwnerFromSurvivesAProfilelessIdentity(t *testing.T) {
	got := OwnerFrom(identity.Identity{Email: "ada@example.com"})
	if got.Email != "ada@example.com" || got.Name() != "" {
		t.Errorf("OwnerFrom = %+v, want the e-mail alone", got)
	}
}
