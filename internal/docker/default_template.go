package docker

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// defaultTemplateFS bundles fallback agent templates, one subdir per harness
// (defaulttemplate/<harness>/…). A template is materialized into an agent's
// template dir when the operator hasn't provided one, so a wiped/absent
// data/templates/<agent> self-heals on the next provision instead of failing.
// `all:` is required to include dotfiles like .security.yml (plain //go:embed
// skips names starting with "." or "_").
//
//go:embed all:defaulttemplate
var defaultTemplateFS embed.FS

// materializeDefaultTemplate writes the bundled fallback template for the given
// harness (e.g. "picoclaw") into dst and chowns it to the picoclaw user. It is
// called only when dst has no config.json.
func materializeDefaultTemplate(dst, harness, user string) error {
	root := "defaulttemplate/" + harness
	if _, err := fs.Stat(defaultTemplateFS, root); err != nil {
		return fmt.Errorf("no bundled template for harness %q: %w", harness, err)
	}
	err := fs.WalkDir(defaultTemplateFS, root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, root), "/")
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		b, err := defaultTemplateFS.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600)
	})
	if err != nil {
		return err
	}
	return chownTree(dst, user)
}
