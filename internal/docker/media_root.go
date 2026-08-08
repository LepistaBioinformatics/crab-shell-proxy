package docker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// treeRoot confines every uploads-tree operation to one directory, enforced by
// the kernel rather than by arithmetic on strings.
//
// WHAT THIS REPLACES. The previous version resolved a member-supplied path by
// hand: EvalSymlinks on the target, EvalSymlinks on the root, then filepath.Rel
// to prove the first was under the second. That was careful and, as far as the
// audit could tell, correct — but it proves containment at one instant and then
// performs the operation at another. Between those two moments the path can
// change: a directory inside uploads/ replaced by a symlink to /etc, by the
// agent (which runs in the container with write access to this very tree) or by
// a second request. The check passes, the operation escapes. That window cannot
// be closed by checking harder.
//
// os.Root closes it structurally. Each method resolves the name inside the
// kernel with the root as a boundary (openat2 RESOLVE_BENEATH on Linux), so a
// component that points outside fails the syscall itself. There is no interval
// during which the answer can become stale, because there is no separate answer.
//
// safeStoredPath still runs first, and not as a leftover: it rejects malformed
// input with the precise error the API returns to the member, before any
// filesystem work happens. The kernel is the guarantee; the validator is the
// message.
//
// Limits worth knowing, from os.Root's own documentation: it does not stop
// traversal of bind mounts or /proc, and Chmod/Chown remain racy against a
// regular-file→symlink swap. Neither applies here — the tree holds member
// uploads, and chownTree runs on paths this code just created.
type treeRoot struct {
	root *os.Root
	// The absolute root, kept only for chownTree, which walks a real path and
	// has no Root-relative equivalent. Every path built from it has already been
	// proven contained by a Root operation.
	path string
}

// openTree opens dir as a confined root, creating it if absent. For the WRITING
// operations only — the caller is expected to chown afterwards.
//
// Creating on a read would be a side effect on a GET, and worse: this process is
// root, chownTree runs only on the write paths, and picoclaw runs non-root — so
// a directory conjured by a download would be one the agent cannot traverse
// until some later upload happens to heal it. openTreeIfExists is the read door.
func openTree(dir string) (*treeRoot, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir uploads: %w", err)
	}
	return openRootAt(dir)
}

// openTreeIfExists opens an existing uploads tree, reporting ErrMediaNotFound
// when there is none. A member who has uploaded nothing has no tree, and asking
// that member's tree for a file is a miss, not a failure.
func openTreeIfExists(dir string) (*treeRoot, error) {
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrMediaNotFound
		}
		return nil, err
	}
	return openRootAt(dir)
}

func openRootAt(dir string) (*treeRoot, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open uploads root: %w", err)
	}
	return &treeRoot{root: r, path: dir}, nil
}

func (t *treeRoot) Close() error { return t.root.Close() }

// abs returns the absolute path of a rel already accepted by a Root operation.
// Only for chownTree — never to hand back to an os.* call that would re-resolve
// it outside the root.
func (t *treeRoot) abs(rel string) string { return filepath.Join(t.path, filepath.FromSlash(rel)) }

// pathEscapesMsg is os's own wording when a name would leave the root. os keeps
// the sentinel unexported, so this matches the text — and
// TestEscapedRecognisesOnlyTheRefusal fails loudly if a Go upgrade rewords it,
// which is the only way this could rot.
const pathEscapesMsg = "path escapes from parent"

// escaped reports whether err is specifically the refusal to leave the root,
// which this package surfaces as a name error: the member supplied a path that
// does not name anything they own.
//
// POSITIVE match, deliberately. The first version of this asked "is it an error
// that is not NotExist or Exist", which is also true of ENOSPC, EACCES, EIO and
// EMFILE — so a full disk during an upload, or a permission fault during a
// delete, was reported to the member as an invalid filename. Infrastructure
// failures must stay infrastructure failures; they are the operator's problem
// and a 500, not a 400 telling someone their folder name is wrong.
func escaped(err error) bool {
	var pe *fs.PathError
	return errors.As(err, &pe) && pe.Err != nil &&
		strings.Contains(pe.Err.Error(), pathEscapesMsg)
}

// statMedia resolves rel inside the tree, in the media API's vocabulary: a path
// that would leave the root is ErrMediaName, an absent one ErrMediaNotFound,
// anything else stays the real failure it was.
//
// The mapping lives here rather than in the generic layer because it is the
// MEDIA API's contract. memory and projects use the same confinement with
// different error vocabularies — see their own call sites.
func (t *treeRoot) statMedia(rel string) (os.FileInfo, error) {
	info, err := t.root.Stat(rel)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrMediaNotFound
		}
		if escaped(err) {
			return nil, ErrMediaName
		}
		return nil, err // a real failure stays one
	}
	return info, nil
}

// countFiles walks rel and returns how many regular files are under it, so a
// recursive delete can name the number BEFORE it happens.
//
// Walks the Root's own fs.FS, so the traversal cannot leave the tree either — a
// symlink to / inside a folder being counted would otherwise walk the host.
func (t *treeRoot) countFiles(rel string) int {
	files := 0
	_ = fs.WalkDir(t.root.FS(), rel, func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if !d.IsDir() {
			files++
		}
		return nil
	})
	return files
}

// copyIntoTree copies an outside source file to a path INSIDE the tree. The
// source is proxy-owned (the effective persona set, materialized by the proxy);
// only the destination is in territory the agent can reshape, so only the
// destination goes through the Root.
func copyIntoTree(t *treeRoot, src, destRel string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := t.root.WriteFile(destRel, data, 0o600); err != nil {
		if escaped(err) {
			return ErrMediaName
		}
		return err
	}
	return nil
}
