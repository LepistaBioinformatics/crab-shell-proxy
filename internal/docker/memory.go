package docker

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sgelias/crab-shell-proxy/internal/config"
)

// MemoryFileName is the file the user edits directly to leave standing notes for
// the agent. It lives inside the agent's workspace memory dir
// (~/.picoclaw/workspace/memory/), next to the agent's own memory, so picoclaw
// picks it up with the rest of its memory. The name is a fixed constant -- no
// client filename to sanitize (unlike media).
const MemoryFileName = "MEMORY_CUSTOM.md"

// MemoryDirName is the workspace subdir holding the agent's memory files.
const MemoryDirName = "memory"

func (m *Manager) memoryDir(key WorkspaceKey) string {
	return filepath.Join(
		config.UserWorkspace(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID),
		"workspace", MemoryDirName,
	)
}

func (m *Manager) memoryPath(key WorkspaceKey) string {
	return filepath.Join(m.memoryDir(key), MemoryFileName)
}

// ReadMemory returns the current MEMORY_CUSTOM.md contents for the caller's
// workspace. An absent file is an empty document (not an error) -- the editor
// simply opens blank.
func (m *Manager) ReadMemory(key WorkspaceKey) (string, error) {
	data, err := os.ReadFile(m.memoryPath(key))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
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
func (m *Manager) WriteMemory(key WorkspaceKey, content string) error {
	dir := m.memoryDir(key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir memory: %w", err)
	}
	if err := os.WriteFile(m.memoryPath(key), []byte(content), 0o600); err != nil {
		return fmt.Errorf("write memory: %w", err)
	}
	if err := chownTree(dir, m.cfg.PicoclawUser); err != nil {
		return fmt.Errorf("chown memory: %w", err)
	}
	return nil
}
