package docker

import (
	"errors"
	"fmt"
	"os"
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
func (m *Manager) uploadsDir(key WorkspaceKey, project string) string {
	return config.UploadsDir(m.cfg.ContainerDataRoot,
		key.TenantID, key.SubsAccID, key.Role, key.UserAccID, workspaceSegment(project))
}


// CreateFolder makes a folder (and any missing parents) inside the member's uploads
// tree, chowned so the non-root agent can use it too.
//
// Idempotent: an existing folder is success, not a conflict. A member clicking "new
// folder" twice, or two tabs racing, should not produce an error about something that
// is already true.
func (m *Manager) CreateFolder(key WorkspaceKey, project string, rel string) error {
	// The system's own folder cannot be created by a member: it already exists (or the
	// proxy makes it on the next delivery), and a member-made one by that name would
	// collide with it.
	if isReservedFolder(rel) || isInsideReserved(rel) {
		return ErrMediaReserved
	}
	clean, err := safeStoredPath(rel)
	if err != nil {
		return err
	}
	tree, err := openTree(m.uploadsDir(key, project))
	if err != nil {
		return err
	}
	defer tree.Close()

	if fi, statErr := tree.root.Stat(clean); statErr == nil {
		if !fi.IsDir() {
			return ErrMediaExists
		}
		return nil
	}
	// MkdirAll on the Root: every intermediate component is resolved with the
	// tree as a boundary, so a missing parent cannot be satisfied by a symlink
	// pointing out of it.
	if err := tree.root.MkdirAll(clean, 0o700); err != nil {
		if escaped(err) {
			return ErrMediaName
		}
		return fmt.Errorf("create folder: %w", err)
	}
	// Same reason StoreMedia chowns: the agent runs non-root and has to be able to
	// read and write what the member organised.
	return chownTree(tree.abs(clean), m.cfg.PicoclawUser)
}

// MoveMedia moves a file or a folder to a new path inside the uploads tree. A move
// within the same parent is a rename — deliberately not a separate operation, because
// it is the same filesystem call with the same three failure modes.
func (m *Manager) MoveMedia(key WorkspaceKey, project string, fromRel, toRel string) error {
	// Renaming or moving the system folder detaches every future agent delivery from
	// the place the proxy writes them. Moving INTO it is refused for the mirror
	// reason: the agent treats everything there as its own output.
	if isReservedFolder(fromRel) || isReservedFolder(toRel) ||
		isInsideReserved(fromRel) || isInsideReserved(toRel) {
		return ErrMediaReserved
	}
	from, err := safeStoredPath(fromRel)
	if err != nil {
		return err
	}
	to, err := safeStoredPath(toRel)
	if err != nil {
		return err
	}
	tree, err := openTreeIfExists(m.uploadsDir(key, project))
	if err != nil {
		return err
	}
	defer tree.Close()

	if from == to {
		return nil
	}
	if _, statErr := tree.root.Stat(to); statErr == nil {
		// Never overwrite. Rename would happily replace a file, and a drag that
		// lands on a same-named file would destroy it with no undo.
		return ErrMediaExists
	}

	// Dropping a folder into itself or into its own descendant detaches the subtree.
	// Compared on the CLEANED relative paths — both are already normalised, root-
	// relative and separator-free at the ends — with a separator appended, so a
	// sibling whose name merely starts with the source's ("notes" vs "notes-old")
	// is not mistaken for a descendant.
	if fi, statErr := tree.root.Stat(from); statErr == nil && fi.IsDir() {
		if to == from || strings.HasPrefix(to, from+"/") {
			return ErrMediaIntoSelf
		}
	}

	if err := tree.root.Rename(from, to); err != nil {
		if os.IsNotExist(err) {
			return ErrMediaNotFound
		}
		if escaped(err) {
			return ErrMediaName
		}
		return fmt.Errorf("move: %w", err)
	}
	return chownTree(tree.abs(to), m.cfg.PicoclawUser)
}

// DeleteFolder removes a folder and everything under it, returning how many FILES
// were removed.
//
// Recursive by design, and destructive: this can take work the agent produced. The
// count is returned so the interface can name it in a confirmation — the member is
// told "12 files" before the click, not after. The uploads root itself is refused
// outright; it is not the member's to delete.
func (m *Manager) DeleteFolder(key WorkspaceKey, project string, rel string) (int, error) {
	if isReservedFolder(rel) {
		return 0, ErrMediaReserved
	}
	// The uploads root itself, in the three spellings that name it. Checked BEFORE
	// safeStoredPath, which would reject all of them as malformed and answer "that
	// is not a valid name" — true, but not the answer to what was asked. The member
	// aimed at the root, and the message says the root is not theirs to delete.
	if trimmed := strings.Trim(strings.TrimSpace(rel), "/"); trimmed == "" || trimmed == "." {
		return 0, ErrMediaRoot
	}
	clean, err := safeStoredPath(rel)
	if err != nil {
		return 0, err
	}
	tree, err := openTreeIfExists(m.uploadsDir(key, project))
	if err != nil {
		return 0, err
	}
	defer tree.Close()

	fi, err := tree.stat(clean)
	if err != nil {
		return 0, ErrMediaNotFound
	}
	if !fi.IsDir() {
		return 0, ErrMediaNotFolder
	}

	// Counted BEFORE the removal: afterwards there is nothing left to count, and the
	// number is what the caller reports back to the member.
	files := tree.countFiles(clean)

	if err := tree.root.RemoveAll(clean); err != nil {
		if escaped(err) {
			return 0, ErrMediaName
		}
		return 0, fmt.Errorf("delete folder: %w", err)
	}
	return files, nil
}

// workspaceSegment maps a project id onto the workspace directory that holds its
// files, memory and scheduled jobs. Empty means the agent's own workspace.
//
// Every surface that reads or writes user content goes through this. The proxy
// used to hardcode the main workspace, which inside a project was not "shared"
// but WRONG: the project's agent writes into workspace-<id>/, so an upload made
// from a project landed somewhere its own agent could not see.
func workspaceSegment(project string) string {
	if project == "" {
		return config.MainWorkspace
	}
	return config.ProjectWorkspace(project)
}
