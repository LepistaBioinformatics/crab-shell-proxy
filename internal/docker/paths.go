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

// underRoot refuses a path that is not strictly beneath root.
//
// Compared with the separator appended, so a sibling whose name merely starts
// with the root ("/data-evil" against root "/data") is not mistaken for
// containment. Returns the cleaned path so the caller forwards the value this
// function checked rather than the one it was handed.
//
// DO NOT widen the condition. It was written as
//
//	if cleaned != cleanRoot && !strings.HasPrefix(cleaned, cleanRoot+sep)
//
// to also allow root itself, and that compound form is not a shape CodeQL's
// go/path-injection recognises as a barrier guard: measured against a local
// database, ten alerts stayed open with the `&&` and zero remained without it.
// A single !strings.HasPrefix is both the recognised shape and all this needs —
// every caller builds root/<segments>, never root itself.
func underRoot(root, path string) (string, error) {
	cleanRoot := filepath.Clean(root)
	cleaned := filepath.Clean(path)
	if !strings.HasPrefix(cleaned, cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q is outside %q", ErrInvalidSecretName, path, cleanRoot)
	}
	return cleaned, nil
}

// containedJoin joins name onto dir and refuses a result that leaves dir.
//
// Used where a request-supplied name becomes a path segment rather than a
// literal filename — the file-format secret sink, and nothing else in this
// package.
//
// The clean-and-compare is written out here rather than delegated to underRoot
// on purpose. Interprocedurally, taint analysis cannot see that the returned
// path is the one the check covered, so a delegated guard reads as no guard at
// all — which is exactly what CodeQL reported. Keeping the Clean, the comparison
// and the return on one value in one function states the invariant in a form
// both a reader and the analyser can follow.
func containedJoin(dir, name string) (string, error) {
	// A name is one path element. Containment alone would accept "/etc/passwd"
	// and "a/b" — filepath.Join makes an absolute name relative, so both land
	// INSIDE the store — but they land as nested directories nobody asked for,
	// and a "name" that silently becomes a subtree is a different object from
	// the one the caller thinks it wrote.
	if name != filepath.Base(name) || strings.ContainsRune(name, filepath.Separator) {
		return "", fmt.Errorf("%w: %q must name a single file", ErrInvalidSecretName, name)
	}
	cleanDir := filepath.Clean(dir)
	cleaned := filepath.Clean(filepath.Join(dir, name))
	if !strings.HasPrefix(cleaned, cleanDir+string(filepath.Separator)) {
		// Covers both an escape and the names that resolve back to the directory
		// itself ("" and "."), which are not escapes but name no file — and would
		// otherwise fail deep inside os.WriteFile with an error about a directory.
		return "", fmt.Errorf("%w: %q must name a file inside its store", ErrInvalidSecretName, name)
	}
	return cleaned, nil
}
