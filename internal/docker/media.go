package docker

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// ErrMediaName is returned when an uploaded filename can't be sanitized into a
// safe, non-empty name.
var ErrMediaName = errors.New("invalid file name")

// ErrMediaNotFound is returned when a requested upload doesn't exist.
var ErrMediaNotFound = errors.New("file not found")

// StoredMedia is the result of a successful upload: the workspace-relative path
// the agent can open, plus the stored name and byte size.
type StoredMedia struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	// IsDir marks a folder rather than a file. Additive and `omitempty`, so every
	// existing consumer that only ever reads files is unaffected — but the listing can
	// now carry an EMPTY folder, which has no file paths to be inferred from and was
	// therefore invisible.
	IsDir bool `json:"isDir,omitempty"`
}

var unsafeFilenameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitizeFilename reduces a client-supplied name to a safe base name: strip any
// directory, collapse unsafe chars to "_", drop leading dots, and reject
// anything that would still be empty or contain traversal.
func sanitizeFilename(raw string) (string, error) {
	base := filepath.Base(strings.TrimSpace(raw))
	base = unsafeFilenameChars.ReplaceAllString(base, "_")
	base = strings.TrimLeft(base, ".")
	if base == "" || strings.Contains(base, "..") {
		return "", ErrMediaName
	}
	if len(base) > 128 {
		base = base[len(base)-128:]
	}
	return base, nil
}

// safeStoredName validates a stored filename (from list/delete/download) as a
// bare, traversal-free base name. Used for names this proxy itself created via
// StoreMedia, which sanitizeFilename has already reduced to a safe base name.
func safeStoredName(name string) (string, error) {
	base := filepath.Base(name)
	if base != name || base == "." || base == ".." ||
		strings.Contains(base, "..") || !secretNameRe.MatchString(base) {
		return "", ErrMediaName
	}
	return base, nil
}

// safeStoredPath validates a workspace-relative media path that MAY contain
// directories -- the agent organizes its own files into folders, and those were
// invisible while listing was flat.
//
// It rejects the shape of a traversal (absolute, "..", NUL); it deliberately
// does NOT apply secretNameRe per segment, because agent-created files are not
// run through sanitizeFilename and legitimately contain characters that regex
// forbids. The real boundary is resolveWithin below, which re-checks the
// resolved path against the uploads root after following symlinks.
func safeStoredPath(name string) (string, error) {
	n := strings.TrimSpace(name)
	if n == "" || filepath.IsAbs(n) || strings.HasPrefix(n, "/") || strings.HasPrefix(n, `\`) {
		return "", ErrMediaName
	}
	if strings.ContainsRune(n, 0) {
		return "", ErrMediaName
	}
	clean := path.Clean(filepath.ToSlash(n))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", ErrMediaName
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", ErrMediaName
		}
	}
	return clean, nil
}

// resolveWithin joins a validated relative path onto root and proves the result
// is still inside it AFTER following symlinks. Path validation alone is not
// enough: a symlink inside uploads/ could otherwise point anywhere on the host.
//
// Go 1.23 has no os.OpenRoot, so this is done by hand.
func resolveWithin(root, rel string) (string, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrMediaNotFound
		}
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrMediaNotFound
		}
		return "", err
	}
	inside, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", ErrMediaName
	}
	return resolved, nil
}

// StoreMedia writes an uploaded file into the caller's agent-readable workspace
// uploads dir, keyed by the sanitized filename so re-uploading the same name
// OVERWRITES (one file per name — no accumulating duplicates), chowned to the
// picoclaw user. The size cap + type allowlist are enforced by the caller
// before this is reached. Returns the "uploads/<name>" path the turn references.
func (m *Manager) StoreMedia(key WorkspaceKey, rawName string, r io.Reader) (StoredMedia, error) {
	name, err := sanitizeFilename(rawName)
	if err != nil {
		return StoredMedia{}, err
	}
	dir := config.UploadsDir(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return StoredMedia{}, fmt.Errorf("mkdir uploads: %w", err)
	}

	full := filepath.Join(dir, name)
	// O_TRUNC: a re-upload of the same name replaces the previous file.
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return StoredMedia{}, fmt.Errorf("create upload: %w", err)
	}
	n, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(full)
		return StoredMedia{}, fmt.Errorf("write upload: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(full)
		return StoredMedia{}, closeErr
	}

	if err := chownTree(dir, m.cfg.PicoclawUser); err != nil {
		return StoredMedia{}, fmt.Errorf("chown uploads: %w", err)
	}

	return StoredMedia{
		Path: filepath.ToSlash(filepath.Join("uploads", name)),
		Name: name,
		Size: n,
	}, nil
}

// DeleteMedia removes one uploaded file (by its stored filename from the list)
// from the caller's uploads dir. Missing file = success (idempotent).
func (m *Manager) DeleteMedia(key WorkspaceKey, storedName string) error {
	rel, err := safeStoredPath(storedName)
	if err != nil {
		return err
	}
	root := config.UploadsDir(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	full, err := resolveWithin(root, rel)
	if err != nil {
		if errors.Is(err, ErrMediaNotFound) {
			return nil // idempotent
		}
		return err
	}
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// OpenMedia opens one uploaded file for download and returns the reader plus its
// display name. The caller must Close the reader.
func (m *Manager) OpenMedia(key WorkspaceKey, storedName string) (io.ReadCloser, string, error) {
	rel, err := safeStoredPath(storedName)
	if err != nil {
		return nil, "", err
	}
	root := config.UploadsDir(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	full, err := resolveWithin(root, rel)
	if err != nil {
		return nil, "", err
	}
	// Only regular files are downloadable: resolveWithin proves containment, but
	// a directory or a device node would still be openable.
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", ErrMediaNotFound
		}
		return nil, "", err
	}
	if !info.Mode().IsRegular() {
		return nil, "", ErrMediaNotFound
	}
	f, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", ErrMediaNotFound
		}
		return nil, "", err
	}
	// Download name is the base only -- a browser can't save into a folder.
	return f, uidPrefixRe.ReplaceAllString(path.Base(rel), ""), nil
}

// Legacy uploads (before overwrite-by-name) carried an 8-hex uid prefix; strip
// it for display so those files show a clean name too.
var uidPrefixRe = regexp.MustCompile(`^[0-9a-f]{8}-`)

// Upper bound on a single listing, so a pathological workspace tree can't turn
// the sidebar request into an unbounded walk.
const maxListedMedia = 2000

// ListMedia returns the files AND folders currently in the caller's workspace uploads
// dir (never their contents). Path is the workspace-relative path the turn references;
// Name is the display name. An absent dir is empty.
//
// Folders are listed explicitly, with IsDir set. They used to be skipped, and the
// interface derived the tree purely from the folder PREFIXES of file paths — which
// works until a folder is empty. A member who created one saw nothing: no row, and
// therefore no drop target to put a file into, which made creating a folder pointless.
func (m *Manager) ListMedia(key WorkspaceKey) ([]StoredMedia, error) {
	dir := config.UploadsDir(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	out := make([]StoredMedia, 0, 32)
	err := filepath.WalkDir(dir, func(full string, e fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree must not blank the whole listing.
			if full == dir {
				return err
			}
			return nil
		}
		if len(out) >= maxListedMedia {
			return fs.SkipAll
		}
		// Never follow a symlink out of the workspace, and never descend into
		// one: WalkDir doesn't follow them, but a symlinked FILE would still be
		// listed and then fail to open.
		if e.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		rel, relErr := filepath.Rel(dir, full)
		if relErr != nil {
			return nil
		}
		if e.IsDir() {
			// The root itself is the container, not an entry in it.
			if rel == "." {
				return nil
			}
			slashRel := filepath.ToSlash(rel)
			out = append(out, StoredMedia{
				Path: path.Join("uploads", slashRel),
				// No uid-prefix stripping: that prefix is something StoreMedia adds to
				// FILE base names, and a folder legitimately named like one would be
				// renamed on screen.
				Name:  slashRel,
				IsDir: true,
			})
			return nil
		}
		info, infoErr := e.Info()
		if infoErr != nil {
			return nil
		}
		slashRel := filepath.ToSlash(rel)
		out = append(out, StoredMedia{
			Path: path.Join("uploads", slashRel),
			// The uid prefix is only ever added to the BASE name by StoreMedia,
			// so strip it there and keep any folder prefix -- it is what tells
			// two same-named files in different folders apart.
			Name: path.Join(path.Dir(slashRel), uidPrefixRe.ReplaceAllString(path.Base(slashRel), "")),
			Size: info.Size(),
		})
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return []StoredMedia{}, nil
		}
		return nil, err
	}
	return out, nil
}

// AttachmentsSubdir is where a file the AGENT produced lands inside the user's
// uploads dir. A fixed segment, so no caller-supplied path ever reaches it.
const AttachmentsSubdir = "attachments"

// StoreAgentAttachment saves a file the harness delivered out-of-band into
// uploads/attachments/<name>, which is how it reaches the user at all: the media
// list already walks nested folders and the download route already reads nested
// paths, so the file shows up in the uploads sidebar with click-to-download and no
// frontend work — the same way a file the user uploaded does.
//
// The name is sanitized exactly like a browser upload's, but the extension
// ALLOWLIST is deliberately not applied. That allowlist constrains what an outside
// caller may push INTO a container; this file was written by the agent inside its
// own workspace, so refusing it here would drop legitimate deliverables while
// adding no boundary that the workspace itself does not already have.
func (m *Manager) StoreAgentAttachment(key WorkspaceKey, rawName string, r io.Reader) (StoredMedia, error) {
	name, err := sanitizeFilename(rawName)
	if err != nil {
		return StoredMedia{}, err
	}
	root := config.UploadsDir(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	dir := filepath.Join(root, AttachmentsSubdir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return StoredMedia{}, fmt.Errorf("mkdir attachments: %w", err)
	}

	full := filepath.Join(dir, name)
	// O_TRUNC, like StoreMedia: one file per name rather than an accumulating pile
	// of report(1).pdf.
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return StoredMedia{}, fmt.Errorf("create attachment: %w", err)
	}
	n, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(full)
		return StoredMedia{}, fmt.Errorf("write attachment: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(full)
		return StoredMedia{}, closeErr
	}
	if err := chownTree(root, m.cfg.PicoclawUser); err != nil {
		return StoredMedia{}, fmt.Errorf("chown uploads: %w", err)
	}

	return StoredMedia{
		Path: path.Join("uploads", AttachmentsSubdir, name),
		Name: path.Join(AttachmentsSubdir, name),
		Size: n,
	}, nil
}
