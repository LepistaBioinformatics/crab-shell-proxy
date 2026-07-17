package docker

import (
	"crypto/rand"
	"encoding/hex"
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

// StoreMedia writes an uploaded file into the caller's agent-readable workspace
// uploads dir under a sanitized, uid-prefixed (unique) name, chowned to the
// picoclaw user. The size cap and type allowlist are enforced by the caller
// (the handler) before this is reached. Returns the "uploads/<file>" path the
// turn references.
func (m *Manager) StoreMedia(key WorkspaceKey, rawName string, r io.Reader) (StoredMedia, error) {
	name, err := sanitizeFilename(rawName)
	if err != nil {
		return StoredMedia{}, err
	}
	dir := config.UploadsDir(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return StoredMedia{}, fmt.Errorf("mkdir uploads: %w", err)
	}

	uid, err := mediaUID()
	if err != nil {
		return StoredMedia{}, err
	}
	stored := uid + "-" + name
	full := filepath.Join(dir, stored)

	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
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
		Path: filepath.ToSlash(filepath.Join("uploads", stored)),
		Name: name,
		Size: n,
	}, nil
}

func mediaUID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// DeleteMedia removes one uploaded file (by its stored filename, i.e. the
// uid-prefixed name from the list) from the caller's uploads dir. The name is
// validated to a safe base name so it can never escape the dir. Missing file =
// success (idempotent).
func (m *Manager) DeleteMedia(key WorkspaceKey, storedName string) error {
	base := filepath.Base(storedName)
	if base != storedName || base == "." || base == ".." ||
		strings.Contains(base, "..") || !secretNameRe.MatchString(base) {
		return ErrMediaName
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

var uidPrefixRe = regexp.MustCompile(`^[0-9a-f]{8}-`)

// ListMedia returns the files currently in the caller's workspace uploads dir
// (never their contents). Name drops the storage uid prefix for display; Path
// is the workspace-relative path the turn references. An absent dir is empty.
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
