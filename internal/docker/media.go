package docker

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sgelias/crab-shell-proxy/internal/config"
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
// bare, traversal-free base name.
func safeStoredName(name string) (string, error) {
	base := filepath.Base(name)
	if base != name || base == "." || base == ".." ||
		strings.Contains(base, "..") || !secretNameRe.MatchString(base) {
		return "", ErrMediaName
	}
	return base, nil
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
	base, err := safeStoredName(storedName)
	if err != nil {
		return err
	}
	full := filepath.Join(
		config.UploadsDir(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID),
		base,
	)
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// OpenMedia opens one uploaded file for download and returns the reader plus its
// display name. The caller must Close the reader.
func (m *Manager) OpenMedia(key WorkspaceKey, storedName string) (io.ReadCloser, string, error) {
	base, err := safeStoredName(storedName)
	if err != nil {
		return nil, "", err
	}
	full := filepath.Join(
		config.UploadsDir(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID),
		base,
	)
	f, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", ErrMediaNotFound
		}
		return nil, "", err
	}
	return f, uidPrefixRe.ReplaceAllString(base, ""), nil
}

// Legacy uploads (before overwrite-by-name) carried an 8-hex uid prefix; strip
// it for display so those files show a clean name too.
var uidPrefixRe = regexp.MustCompile(`^[0-9a-f]{8}-`)

// ListMedia returns the files currently in the caller's workspace uploads dir
// (never their contents). Path is the workspace-relative path the turn
// references; Name is the display name. An absent dir is empty.
func (m *Manager) ListMedia(key WorkspaceKey) ([]StoredMedia, error) {
	dir := config.UploadsDir(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []StoredMedia{}, nil
		}
		return nil, err
	}
	out := make([]StoredMedia, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		stored := e.Name()
		out = append(out, StoredMedia{
			Path: filepath.ToSlash(filepath.Join("uploads", stored)),
			Name: uidPrefixRe.ReplaceAllString(stored, ""),
			Size: info.Size(),
		})
	}
	return out, nil
}
