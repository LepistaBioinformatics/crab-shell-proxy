package docker

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// templateFiles is the config-only allowlist copied into a fresh per-user data
// dir. CRITICAL: never copy workspace/ (holds conversation history) — the
// shared alpha/beta dirs' sessions would otherwise leak into every new user's
// container. logs/ and .picoclaw.pid are likewise excluded; picoclaw recreates
// workspace/ empty on first gateway run.
var templateFiles = []string{"config.json", ".security.yml"}

// provision ensures the per-user data dir (userDir, a path INSIDE this proxy)
// is seeded from templateDir and returns the pico token to connect with. If the
// dir already has config.json it is treated as a returning user and left as-is.
func provision(userDir, templateDir string) (picoToken string, err error) {
	configPath := filepath.Join(userDir, "config.json")
	if _, statErr := os.Stat(configPath); statErr != nil {
		if err := seedFromTemplate(userDir, templateDir); err != nil {
			return "", err
		}
	}
	tok, err := readPicoToken(filepath.Join(userDir, ".security.yml"))
	if err != nil {
		return "", fmt.Errorf("read pico token: %w", err)
	}
	if tok == "" {
		return "", fmt.Errorf("no pico channel token found in %s/.security.yml", userDir)
	}
	return tok, nil
}

func seedFromTemplate(userDir, templateDir string) error {
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		return fmt.Errorf("create user data dir: %w", err)
	}
	for _, name := range templateFiles {
		src := filepath.Join(templateDir, name)
		dst := filepath.Join(userDir, name)
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("seed %s: %w", name, err)
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

var (
	picoBlockRe = regexp.MustCompile(`^(\s*)pico:\s*$`)
	tokenLineRe = regexp.MustCompile(`^\s*token:\s*(.+?)\s*$`)
)

// readPicoToken extracts channel_list.pico(.settings).token from a picoclaw
// .security.yml without a YAML dependency — the same nested-block scan
// picoclaw-openai-proxy/server.js uses (find a `pico:` line, then the first
// `token:` line more-indented than it).
func readPicoToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(raw), "\n")
	for i := 0; i < len(lines); i++ {
		m := picoBlockRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		baseIndent := len(m[1])
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "" {
				continue
			}
			indent := len(lines[j]) - len(strings.TrimLeft(lines[j], " \t"))
			if indent <= baseIndent {
				break // left the pico: block
			}
			if tm := tokenLineRe.FindStringSubmatch(lines[j]); tm != nil {
				return strings.Trim(tm[1], `'"`), nil
			}
		}
	}
	return "", nil
}
