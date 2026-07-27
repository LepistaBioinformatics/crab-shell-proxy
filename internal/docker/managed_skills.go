package docker

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

// Operator-managed workspace content, bind-mounted read-only into every
// container so the agent can neither alter it nor keep an edit past a restart.
// Relative to the container-side ManagedSkillsDir:
//
//	skills/<managedSkillName>/  -> workspace/skills/<managedSkillName> (guidance)
//	memory/<managedMemoryFile>  -> workspace/memory/<managedMemoryFile> (recovery)
const (
	managedSkillRel  = "skills/shared-content"
	managedMemoryRel = "memory/CONTEXT_RECOVERY.md"
)

//go:embed managed
var managedFS embed.FS

// materializeManagedContent writes the embedded managed tree into dst (the
// container-side ManagedSkillsDir), overwriting any prior copy so the canonical
// operator version is what gets bind-mounted, and chowns it to the agent user so
// the read-only binds are readable by the non-root process.
func materializeManagedContent(dst, user string) error {
	const root = "managed"
	err := fs.WalkDir(managedFS, root, func(p string, d fs.DirEntry, walkErr error) error {
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
		b, err := managedFS.ReadFile(p)
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
