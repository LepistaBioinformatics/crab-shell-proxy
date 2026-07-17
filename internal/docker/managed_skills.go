package docker

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

// managedSkillName is the single operator-managed skill directory bind-mounted
// read-only into every container's workspace skills dir.
const managedSkillName = "shared-content"

//go:embed managed/skills
var managedSkillsFS embed.FS

// materializeManagedSkills writes the embedded managed-skills tree into dst (the
// container-side ManagedSkillsDir), overwriting any prior copy so the canonical
// operator version is what gets bind-mounted, and chowns it to the agent user so
// the read-only bind is readable by the non-root process. Because the source is
// root-owned and mounted read-only, the agent can neither alter it nor keep any
// edit past a restart.
func materializeManagedSkills(dst, user string) error {
	const root = "managed/skills"
	err := fs.WalkDir(managedSkillsFS, root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := managedSkillsFS.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		return err
	}
	return chownTree(dst, user)
}
