package docker

import (
	"errors"
	"path/filepath"
	"testing"
)

// The traversal cases these cover cannot be reached through the handlers today —
// validateSecretName and identity.SanitizeID stop them earlier. That is exactly
// why the tests are here: they pin the LAST line of defence, the one a new entry
// point cannot skip, and they fail if someone loosens it.

func TestUnderRootAcceptsTheRootAndWhatIsBeneathIt(t *testing.T) {
	root := filepath.Join("/data", "crab")
	for _, ok := range []string{
		root,
		filepath.Join(root, "user-secrets"),
		filepath.Join(root, "user-secrets", "acc", "alpha", ".env"),
		filepath.Join(root, "a", "..", "b"), // resolves inside; Clean must be applied
	} {
		if err := underRoot(root, ok); err != nil {
			t.Errorf("underRoot(%q) = %v, want nil", ok, err)
		}
	}
}

func TestUnderRootRejectsEscapesAndLookalikeSiblings(t *testing.T) {
	root := filepath.Join("/data", "crab")
	for _, bad := range []string{
		filepath.Join(root, ".."),
		filepath.Join(root, "..", "etc", "passwd"),
		filepath.Join(root, "user-secrets", "..", "..", "etc"),
		"/etc/passwd",
		// The reason the check appends a separator rather than comparing prefixes:
		// this sibling shares the root's spelling and is NOT inside it.
		"/data/crab-evil/secrets",
	} {
		if err := underRoot(root, bad); err == nil {
			t.Errorf("underRoot(%q) = nil, want an error", bad)
		} else if !errors.Is(err, ErrInvalidSecretName) {
			t.Errorf("underRoot(%q) error = %v, want ErrInvalidSecretName", bad, err)
		}
	}
}

func TestContainedJoinKeepsAName(t *testing.T) {
	dir := filepath.Join("/data", "crab", "user-secrets", "acc", "alpha", "secrets")
	got, err := containedJoin(dir, "OPENAI_KEY")
	if err != nil {
		t.Fatalf("containedJoin: %v", err)
	}
	if want := filepath.Join(dir, "OPENAI_KEY"); got != want {
		t.Fatalf("containedJoin = %q, want %q", got, want)
	}
}

func TestContainedJoinRefusesAnythingThatLeavesTheStore(t *testing.T) {
	dir := filepath.Join("/data", "crab", "user-secrets", "acc", "alpha", "secrets")
	for _, bad := range []string{
		"..",
		"../escaped",
		"../../../etc/passwd",
		"a/../../b",
		"/etc/passwd", // absolute: Join makes it relative, but a lone ".." would not
	} {
		if _, err := containedJoin(dir, bad); err == nil {
			t.Errorf("containedJoin(%q) = nil error, want a refusal", bad)
		}
	}
}

// "" and "." join back to the directory itself. Not an escape, but writing there
// fails with an error about a directory rather than about the name, so the sink
// refuses it as a name instead.
func TestContainedJoinRefusesNamesThatResolveToTheStoreItself(t *testing.T) {
	dir := filepath.Join("/data", "crab", "user-secrets", "acc", "alpha", "secrets")
	for _, bad := range []string{"", "."} {
		if _, err := containedJoin(dir, bad); err == nil {
			t.Errorf("containedJoin(%q) = nil error, want a refusal", bad)
		}
	}
}
