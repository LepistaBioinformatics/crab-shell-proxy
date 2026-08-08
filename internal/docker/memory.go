package docker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// MemoryFileName is the file the user edits directly to leave standing notes for
// the agent. It lives inside the agent's workspace memory dir
// (~/.picoclaw/workspace/memory/), next to the agent's own memory, so picoclaw
// picks it up with the rest of its memory. The name is a fixed constant -- no
// client filename to sanitize (unlike media).
const MemoryFileName = "MEMORY_CUSTOM.md"

// MemoryDirName is the workspace subdir holding the agent's memory files.
const MemoryDirName = "memory"

// workspaceDir is the segment root the memory dir lives in — the agent's own
// workspace, or a project's. Confinement is anchored HERE rather than at the
// memory dir, because `memory` is itself a component the agent can replace.
func (m *Manager) workspaceDir(key WorkspaceKey, project string) string {
	return filepath.Join(
		config.UserWorkspace(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID),
		workspaceSegment(project),
	)
}

// memoryRel is MEMORY_CUSTOM.md's path relative to that root. Two fixed
// components, no member input anywhere in it — which is exactly why the old
// version looked safe.
const memoryRel = MemoryDirName + "/" + MemoryFileName

// ReadMemory returns the current MEMORY_CUSTOM.md contents for the caller's
// workspace. An absent file is an empty document (not an error) -- the editor
// simply opens blank.
func (m *Manager) ReadMemory(key WorkspaceKey, project string) (string, error) {
	tree, err := openTreeIfExists(m.workspaceDir(key, project))
	if err != nil {
		if errors.Is(err, ErrMediaNotFound) {
			return "", nil // no workspace yet is an empty document, not a failure
		}
		return "", err
	}
	defer tree.Close()

	data, err := tree.root.ReadFile(memoryRel)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		// A `memory` component that resolves out of the workspace is not a document
		// to read; refusing beats serving whatever it pointed at as the member's
		// own notes.
		if escaped(err) {
			return "", ErrMediaName
		}
		return "", err
	}
	return string(data), nil
}

// WriteMemory replaces MEMORY_CUSTOM.md with content, then chowns the memory dir
// to the picoclaw user so the non-root agent can traverse it and read what the
// (root) proxy wrote -- mirroring StoreMedia's chown of the uploads dir. The
// memory dir is a dedicated workspace subdir (not the workspace root, which
// holds the read-only .secrets/.shared bind mounts), so chowning it is safe.
//
// THIS IS THE WRITE THAT MATTERS. The proxy runs as root; the workspace it
// writes into is bind-mounted read-write into the agent's container and chowned
// to the uid the agent runs as (manager.go create: hostDir + ":" + mountDest).
// So the agent OWNS `workspace/memory`, and the plain MkdirAll + WriteFile this
// used to be followed a symlink there without complaint: root-written,
// caller-supplied content, landing at a path the agent chose — another tenant's
// workspace, or the proxy's own filesystem.
//
// Confined to the workspace root, both components of memory/MEMORY_CUSTOM.md are
// resolved by the kernel against that boundary, so a swapped component fails the
// syscall instead of redirecting the write.
func (m *Manager) WriteMemory(key WorkspaceKey, project, content string) error {
	tree, err := openTree(m.workspaceDir(key, project))
	if err != nil {
		return err
	}
	defer tree.Close()

	if err := tree.root.MkdirAll(MemoryDirName, 0o700); err != nil {
		if escaped(err) {
			return ErrMediaName
		}
		return fmt.Errorf("mkdir memory: %w", err)
	}
	if err := tree.root.WriteFile(memoryRel, []byte(content), 0o600); err != nil {
		if escaped(err) {
			return ErrMediaName
		}
		return fmt.Errorf("write memory: %w", err)
	}
	if err := chownTree(tree.abs(MemoryDirName), m.cfg.PicoclawUser); err != nil {
		return fmt.Errorf("chown memory: %w", err)
	}
	return nil
}
