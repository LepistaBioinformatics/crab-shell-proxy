package docker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMaterializeDefaultTemplate(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "templates", "alpha")
	if err := materializeDefaultTemplate(dst, "picoclaw", ""); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// The essential seed files (incl. the dotfile .security.yml) and the
	// workspace persona/dirs must be present.
	for _, rel := range []string{
		"config.json",
		".security.yml",
		"workspace/AGENT.md",
		"workspace/SOUL.md",
		"workspace/USER.md",
		"workspace/memory/MEMORY.md",
	} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err != nil {
			t.Errorf("expected %s to be materialized: %v", rel, err)
		}
	}
}

// provision must fall back to the bundled template when the agent's template
// dir is absent, seeding the user's config.json instead of erroring.
func TestProvisionSelfHealsMissingTemplate(t *testing.T) {
	root := t.TempDir()
	userDir := filepath.Join(root, "users", "u1")
	templateDir := filepath.Join(root, "templates", "alpha") // does NOT exist
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := provision(userDir, templateDir, "", "/data", "", WorkspaceKey{}, "x@y"); err != nil {
		t.Fatalf("provision should self-heal, got: %v", err)
	}
	// The fallback template was materialized...
	if _, err := os.Stat(filepath.Join(templateDir, "config.json")); err != nil {
		t.Errorf("template config.json not materialized: %v", err)
	}
	// ...and seeded into the user's dir.
	if _, err := os.Stat(filepath.Join(userDir, "config.json")); err != nil {
		t.Errorf("user config.json not seeded: %v", err)
	}
}
