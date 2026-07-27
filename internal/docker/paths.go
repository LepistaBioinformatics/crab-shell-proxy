package docker

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Containment checks for every filesystem path built from request-derived data.
//
// The inputs were already safe before this file existed: identity.SanitizeID
// replaces everything outside [A-Za-z0-9._-] with "-", so no component can grow
// a separator, and validateSecretName is an anchored charset match that also
// rejects "", ".", ".." and any ".." substring. Neither is a barrier CodeQL's
// go/path-injection model recognises, which is how twelve high alerts stood open
// against secrets.go — but silencing the tool is not why this exists.
//
// The real gap it closes is that the guarantee lived at the ENTRY POINTS. Every
// handler had to remember to validate, and nothing checked the path actually
// about to be opened. A new caller that forgets, or a future SanitizeID that
// grows a permissive case, reopens the hole with no test failing. These two
// functions move the invariant to the place the filesystem is touched, which is
// the one thing every path has in common.

// underRoot refuses a path that is not root itself or something beneath it.
//
// Cleaned and compared with the separator appended, so a sibling directory whose
// name merely starts with the root ("/data-evil" against root "/data") does not
// pass as contained.
func underRoot(root, path string) error {
	cleanRoot := filepath.Clean(root)
	cleanPath := filepath.Clean(path)
	if cleanPath == cleanRoot {
		return nil
	}
	if !strings.HasPrefix(cleanPath, cleanRoot+string(filepath.Separator)) {
		return fmt.Errorf("%w: %q is outside %q", ErrInvalidSecretName, path, cleanRoot)
	}
	return nil
}

// containedJoin joins name onto dir and refuses a result that leaves dir.
//
// Used where a request-supplied name becomes a path segment rather than a
// literal filename — the file-format secret sink, and nothing else in this
// package. filepath.Join already cleans, so "../x" resolves before the check
// rather than hiding inside the string.
func containedJoin(dir, name string) (string, error) {
	// A name is one path element. Containment alone would accept "/etc/passwd"
	// and "a/b" — filepath.Join makes an absolute name relative, so both land
	// INSIDE the store — but they land as nested directories nobody asked for,
	// and a "name" that silently becomes a subtree is a different object from
	// the one the caller thinks it wrote.
	if name != filepath.Base(name) || strings.ContainsRune(name, filepath.Separator) {
		return "", fmt.Errorf("%w: %q must name a single file", ErrInvalidSecretName, name)
	}
	joined := filepath.Join(dir, name)
	if err := underRoot(dir, joined); err != nil {
		return "", fmt.Errorf("%w: %q escapes its store", ErrInvalidSecretName, name)
	}
	if joined == filepath.Clean(dir) {
		// "." and "" both join back to dir itself: not an escape, but not a file
		// either, and os.WriteFile on a directory fails with a confusing error.
		return "", fmt.Errorf("%w: %q names no file", ErrInvalidSecretName, name)
	}
	return joined, nil
}
