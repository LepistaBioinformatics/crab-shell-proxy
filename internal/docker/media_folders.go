package docker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// Member-driven organisation of the workspace uploads tree: create a folder, move a
// file or folder, delete a folder and everything under it.
//
// Everything here is a path operation on a directory the AGENT also writes to, so the
// containment rules are the point, not the plumbing. Two boundaries, both reused from
// media.go rather than reinvented:
//
//   - safeStoredPath rejects the SHAPE of a traversal (absolute, "..", NUL, empty
//     segments) on the way in;
//   - resolveWithin / resolveParentWithin prove the path is still inside the uploads
//     root AFTER following symlinks, because a symlink the agent created inside
//     uploads/ could otherwise point anywhere on the host.
//
// Path validation alone has never been enough here, and this file adds the case
// media.go did not have: a destination that does not exist yet.

var (
	// ErrMediaExists means the destination is already taken. Creating a folder over an
	// existing one is fine (idempotent); moving ONTO an existing path is not, because
	// it would silently destroy whatever was there.
	ErrMediaExists = errors.New("destination already exists")
	// ErrMediaIntoSelf means a folder was dropped into itself or into its own
	// descendant. Allowing it detaches the subtree from the tree entirely.
	ErrMediaIntoSelf = errors.New("cannot move a folder into itself")
	// ErrMediaNotFolder means the path exists but is a file where a folder was needed.
	ErrMediaNotFolder = errors.New("not a folder")
	// ErrMediaRoot means an operation targeted the uploads root, which the member does
	// not own — deleting it would take the agent's whole working tree with it.
	ErrMediaRoot = errors.New("cannot operate on the uploads root")
	// ErrMediaReserved means an operation targeted a system-managed folder. See
	// isReservedFolder.
	ErrMediaReserved = errors.New("that folder is managed by the system")
)

// isReservedFolder reports whether a workspace-relative path is a folder the SYSTEM
// owns rather than the member.
//
// Today that is exactly one: the top-level `attachments`, where StoreAgentAttachment
// puts files the AGENT produced. It is created, named and populated by the proxy, so a
// member renaming it would silently detach every future delivery, and creating their
// own folder by that name would collide with it.
//
// Only the TOP LEVEL is reserved. `reports/attachments` is an ordinary folder a member
// may legitimately want; it is not the system one, and forbidding the word everywhere
// would be a rule about vocabulary rather than about ownership.
//
// Enforced HERE rather than only in the interface: hiding a button is not a
// permission. The path arrives from the network.
func isReservedFolder(rel string) bool {
	clean, err := safeStoredPath(rel)
	if err != nil {
		return false // malformed paths are rejected on their own terms
	}
	return clean == AttachmentsSubdir
}

// isInsideReserved reports whether a path lives inside a system-managed folder. Writing
// there would put member files where the agent expects only its own deliveries.
func isInsideReserved(rel string) bool {
	clean, err := safeStoredPath(rel)
	if err != nil {
		return false
	}
	return strings.HasPrefix(clean, AttachmentsSubdir+"/")
}

// uploadsDir is the member's uploads root. The five-argument call is repeated all
// over media.go; naming it once here keeps the three operations below readable.
func (m *Manager) uploadsDir(key WorkspaceKey) string {
	return config.UploadsDir(m.cfg.ContainerDataRoot,
		key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
}

// resolveNewWithin validates rel and returns the absolute path it WOULD have, proving
// containment for a target that may not exist yet — and whose PARENTS may not exist
// either.
//
// resolveWithin cannot be used: it calls EvalSymlinks on the whole path, which fails
// on anything missing. Nor is checking only the immediate parent enough, because
// CreateFolder is documented to create missing parents, so the parent can be absent
// too (that was the first version of this, and the test for a nested create caught
// it).
//
// So: resolve the DEEPEST EXISTING ancestor with symlinks followed, prove THAT is
// inside the root, then append the remaining segments. Each remaining segment was
// already validated by safeStoredPath as a plain name, so nothing appended can climb
// back out — and the final containment check re-proves it anyway.
func resolveNewWithin(root, rel string) (string, error) {
	clean, err := safeStoredPath(rel)
	if err != nil {
		return "", err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}

	segs := strings.Split(clean, "/")
	base := resolvedRoot
	i := 0
	for ; i < len(segs); i++ {
		candidate := filepath.Join(base, segs[i])
		resolved, statErr := filepath.EvalSymlinks(candidate)
		if statErr != nil {
			break // this segment and everything after it does not exist yet
		}
		// An EXISTING segment may be a symlink pointing anywhere. Check every one as
		// we descend rather than only at the end.
		inside, relErr := filepath.Rel(resolvedRoot, resolved)
		if relErr != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
			return "", ErrMediaName
		}
		base = resolved
	}
	full := base
	for ; i < len(segs); i++ {
		full = filepath.Join(full, segs[i])
	}

	inside, err := filepath.Rel(resolvedRoot, full)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", ErrMediaName
	}
	return full, nil
}

// CreateFolder makes a folder (and any missing parents) inside the member's uploads
// tree, chowned so the non-root agent can use it too.
//
// Idempotent: an existing folder is success, not a conflict. A member clicking "new
// folder" twice, or two tabs racing, should not produce an error about something that
// is already true.
func (m *Manager) CreateFolder(key WorkspaceKey, rel string) error {
	// The system's own folder cannot be created by a member: it already exists (or the
	// proxy makes it on the next delivery), and a member-made one by that name would
	// collide with it.
	if isReservedFolder(rel) || isInsideReserved(rel) {
		return ErrMediaReserved
	}
	root := m.uploadsDir(key)
	target, err := resolveNewWithin(root, rel)
	if err != nil {
		return err
	}
	if fi, statErr := os.Stat(target); statErr == nil {
		if !fi.IsDir() {
			return ErrMediaExists
		}
		return nil
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return fmt.Errorf("create folder: %w", err)
	}
	// Same reason StoreMedia chowns: the agent runs non-root and has to be able to
	// read and write what the member organised.
	return chownTree(target, m.cfg.PicoclawUser)
}

// MoveMedia moves a file or a folder to a new path inside the uploads tree. A move
// within the same parent is a rename — deliberately not a separate operation, because
// it is the same filesystem call with the same three failure modes.
func (m *Manager) MoveMedia(key WorkspaceKey, fromRel, toRel string) error {
	// Renaming or moving the system folder detaches every future agent delivery from
	// the place the proxy writes them. Moving INTO it is refused for the mirror
	// reason: the agent treats everything there as its own output.
	if isReservedFolder(fromRel) || isReservedFolder(toRel) ||
		isInsideReserved(fromRel) || isInsideReserved(toRel) {
		return ErrMediaReserved
	}
	root := m.uploadsDir(key)

	from, err := resolveWithin(root, fromRel)
	if err != nil {
		return err
	}
	to, err := resolveNewWithin(root, toRel)
	if err != nil {
		return err
	}
	if from == to {
		return nil
	}
	if _, statErr := os.Stat(to); statErr == nil {
		// Never overwrite. os.Rename would happily replace a file, and a drag that
		// lands on a same-named file would destroy it with no undo.
		return ErrMediaExists
	}

	// Dropping a folder into itself or into its own descendant detaches the subtree.
	// Compared on the RESOLVED paths with a separator appended, so a sibling whose
	// name merely starts with the source's ("notes" vs "notes-old") is not mistaken
	// for a descendant.
	if fi, statErr := os.Stat(from); statErr == nil && fi.IsDir() {
		if to == from || strings.HasPrefix(to, from+string(filepath.Separator)) {
			return ErrMediaIntoSelf
		}
	}

	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("move: %w", err)
	}
	return chownTree(to, m.cfg.PicoclawUser)
}

// DeleteFolder removes a folder and everything under it, returning how many FILES
// were removed.
//
// Recursive by design, and destructive: this can take work the agent produced. The
// count is returned so the interface can name it in a confirmation — the member is
// told "12 files" before the click, not after. The uploads root itself is refused
// outright; it is not the member's to delete.
func (m *Manager) DeleteFolder(key WorkspaceKey, rel string) (int, error) {
	if isReservedFolder(rel) {
		return 0, ErrMediaReserved
	}
	root := m.uploadsDir(key)
	target, err := resolveWithin(root, rel)
	if err != nil {
		return 0, err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return 0, err
	}
	if target == resolvedRoot {
		return 0, ErrMediaRoot
	}
	fi, err := os.Stat(target)
	if err != nil {
		return 0, ErrMediaNotFound
	}
	if !fi.IsDir() {
		return 0, ErrMediaNotFolder
	}

	// Counted BEFORE the removal: afterwards there is nothing left to count, and the
	// number is what the caller reports back to the member.
	files := 0
	_ = filepath.Walk(target, func(_ string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if !info.IsDir() {
			files++
		}
		return nil
	})

	if err := os.RemoveAll(target); err != nil {
		return 0, fmt.Errorf("delete folder: %w", err)
	}
	return files, nil
}
