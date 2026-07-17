package docker

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sgelias/crab-shell-proxy/internal/config"
)

// MemoryFileName is the workspace-root file the user edits directly to leave
// standing notes for the agent. It sits alongside uploads/ and .shared/ in the
// agent's workspace root (~/.picoclaw/workspace), so the agent can read it by
// name at turn time. The name is a fixed constant -- no client filename to
// sanitize (unlike media).
const MemoryFileName = "MEMORY_CUSTOM.md"

func (m *Manager) memoryPath(key WorkspaceKey) string {
	return filepath.Join(
		config.UserWorkspace(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID),
		"workspace", MemoryFileName,
	)
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

// WriteMemory replaces MEMORY_CUSTOM.md with content (O_TRUNC), chowning the
// file to the picoclaw user so the non-root agent can read what the (root)
// proxy wrote -- mirroring StoreMedia. Only the file itself is chowned, never
// the workspace root, which holds the read-only .secrets/.shared bind mounts.
func (m *Manager) WriteMemory(key WorkspaceKey, content string) error {
	full := m.memoryPath(key)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return fmt.Errorf("mkdir workspace: %w", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write memory: %w", err)
	}
	if err := chownTree(full, m.cfg.PicoclawUser); err != nil {
		return fmt.Errorf("chown memory: %w", err)
	}
	return nil
}
