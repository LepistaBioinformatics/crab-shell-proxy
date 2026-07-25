package docker

import (
	"archive/zip"
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// Zip-hardening caps (NFR-3): reject anything larger than these before writing.
const (
	skillMaxTotalBytes int64 = 10 << 20 // 10 MiB uncompressed
	skillMaxEntries          = 200
	skillMaxPerFile    int64 = 10 << 20
	skillMaxDepth            = 8
)

var (
	ErrInvalidSkillName  = errors.New("skill name must match ^[a-z0-9][a-z0-9._-]{0,63}$")
	ErrReservedSkillName = errors.New(`"shared-content" is reserved`)
	ErrSkillMetadata     = errors.New("SKILL.md must have non-empty name and description frontmatter")
	ErrSkillArchive      = errors.New("invalid or unsafe skill archive")

	skillNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// SkillMeta is the metadata of a shared skill (never its file bytes).
type SkillMeta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Size        int64  `json:"size"`
	ModifiedAt  string `json:"modifiedAt"`
	HasFiles    bool   `json:"hasFiles"` // more than just SKILL.md
}

// sanitizeSkillName accepts only a lowercase slug that does not change under
// sanitization, rejecting the reserved managed-skill name.
func sanitizeSkillName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "shared-content" {
		return "", ErrReservedSkillName
	}
	if !skillNameRe.MatchString(name) {
		return "", ErrInvalidSkillName
	}
	return name, nil
}

// parseSkillFrontmatter reads the leading ---…--- block of a SKILL.md and
// requires non-empty name and description (values may be quoted). No YAML dep.
func parseSkillFrontmatter(skillMD string) (name, description string, err error) {
	sc := bufio.NewScanner(strings.NewReader(skillMD))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	// Skip leading blank lines; first content line must open the fence.
	opened := false
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if !opened {
			if strings.TrimSpace(line) == "" {
				continue
			}
			if strings.TrimSpace(line) != "---" {
				return "", "", ErrSkillMetadata
			}
			opened = true
			continue
		}
		if strings.TrimSpace(line) == "---" {
			break // end of frontmatter
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		switch key {
		case "name":
			name = val
		case "description":
			description = val
		}
	}
	if !opened || name == "" || description == "" {
		return "", "", ErrSkillMetadata
	}
	return name, description, nil
}

func (m *Manager) sharedSkillsDir(scope Scope) string {
	root := m.cfg.ContainerDataRoot
	switch {
	case scope.Kind == ScopeTenant && scope.AgentKey == "":
		return config.TenantSharedSkillsDir(root, scope.TenantID)
	case scope.Kind == ScopeTenant:
		return config.TenantAgentSharedSkillsDir(root, scope.TenantID, scope.AgentKey)
	case scope.AgentKey == "":
		return config.SubscriptionSharedSkillsDir(root, scope.TenantID, scope.SubsAccID)
	default:
		return config.SubscriptionAgentSharedSkillsDir(root, scope.TenantID, scope.SubsAccID, scope.AgentKey)
	}
}

// ListSharedSkills returns the metadata of every skill at a scope (absent dir →
// empty list). A skill dir without a valid SKILL.md is skipped.
func (m *Manager) ListSharedSkills(scope Scope) ([]SkillMeta, error) {
	dir := m.sharedSkillsDir(scope)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []SkillMeta{}, nil
		}
		return nil, err
	}
	out := []SkillMeta{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta, err := m.skillMeta(dir, e.Name())
		if err != nil {
			continue // not a valid skill dir
		}
		out = append(out, meta)
	}
	return out, nil
}

func (m *Manager) skillMeta(dir, name string) (SkillMeta, error) {
	skillDir := filepath.Join(dir, name)
	raw, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		return SkillMeta{}, err
	}
	_, desc, err := parseSkillFrontmatter(string(raw))
	if err != nil {
		return SkillMeta{}, err
	}
	var total int64
	files := 0
	modAt := ""
	_ = filepath.Walk(skillDir, func(_ string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		files++
		if t := modTime(info); t > modAt {
			modAt = t
		}
		return nil
	})
	return SkillMeta{
		Name: name, Description: desc, Size: total, ModifiedAt: modAt, HasFiles: files > 1,
	}, nil
}

// ReadSharedSkillDoc returns a skill's SKILL.md text plus its metadata.
func (m *Manager) ReadSharedSkillDoc(scope Scope, rawName string) (string, SkillMeta, error) {
	name, err := sanitizeSkillName(rawName)
	if err != nil {
		return "", SkillMeta{}, err
	}
	dir := m.sharedSkillsDir(scope)
	raw, err := os.ReadFile(filepath.Join(dir, name, "SKILL.md"))
	if err != nil {
		return "", SkillMeta{}, err
	}
	meta, err := m.skillMeta(dir, name)
	if err != nil {
		return "", SkillMeta{}, err
	}
	return string(raw), meta, nil
}

// WriteSharedSkillDoc writes/replaces a skill's SKILL.md (editor mode), leaving
// any existing supporting files untouched. Frontmatter is validated first.
func (m *Manager) WriteSharedSkillDoc(scope Scope, rawName, body string) error {
	name, err := sanitizeSkillName(rawName)
	if err != nil {
		return err
	}
	if _, _, err := parseSkillFrontmatter(body); err != nil {
		return err
	}
	dir := filepath.Join(m.sharedSkillsDir(scope), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir skill: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600); err != nil {
		return fmt.Errorf("write SKILL.md: %w", err)
	}
	return chownTree(dir, m.cfg.PicoclawUser)
}

// WriteSharedSkillZip replaces a skill dir from an uploaded zip: hardened
// against traversal/symlink/oversize/too-many-entries, requires a top-level
// SKILL.md with valid frontmatter, and is applied atomically (extract to a temp
// sibling, then swap).
func (m *Manager) WriteSharedSkillZip(scope Scope, rawName string, r io.Reader) error {
	name, err := sanitizeSkillName(rawName)
	if err != nil {
		return err
	}
	buf, err := io.ReadAll(io.LimitReader(r, skillMaxTotalBytes+1))
	if err != nil {
		return err
	}
	if int64(len(buf)) > skillMaxTotalBytes {
		return fmt.Errorf("%w: archive exceeds %d bytes", ErrSkillArchive, skillMaxTotalBytes)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSkillArchive, err)
	}
	if len(zr.File) > skillMaxEntries {
		return fmt.Errorf("%w: too many entries", ErrSkillArchive)
	}

	base := m.sharedSkillsDir(scope)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(base, ".tmp-"+name+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	var total int64
	sawSkillMD := false
	for _, f := range zr.File {
		clean := path.Clean(f.Name)
		if clean == "." || strings.HasPrefix(clean, "..") || strings.HasPrefix(clean, "/") ||
			strings.Contains(clean, "../") {
			return fmt.Errorf("%w: unsafe path %q", ErrSkillArchive, f.Name)
		}
		if strings.Count(clean, "/") > skillMaxDepth {
			return fmt.Errorf("%w: nesting too deep", ErrSkillArchive)
		}
		mode := f.Mode()
		if mode&os.ModeSymlink != 0 || (!mode.IsRegular() && !f.FileInfo().IsDir()) {
			return fmt.Errorf("%w: irregular entry %q", ErrSkillArchive, f.Name)
		}
		dest := filepath.Join(tmp, filepath.FromSlash(clean))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dest, 0o700); err != nil {
				return err
			}
			continue
		}
		if f.UncompressedSize64 > uint64(skillMaxPerFile) {
			return fmt.Errorf("%w: file %q too large", ErrSkillArchive, f.Name)
		}
		if clean == "SKILL.md" {
			sawSkillMD = true
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			rc.Close()
			return err
		}
		n, copyErr := io.Copy(out, io.LimitReader(rc, skillMaxPerFile+1))
		rc.Close()
		out.Close()
		if copyErr != nil {
			return copyErr
		}
		total += n
		if total > skillMaxTotalBytes {
			return fmt.Errorf("%w: total size exceeded", ErrSkillArchive)
		}
	}
	if !sawSkillMD {
		return fmt.Errorf("%w: archive has no top-level SKILL.md", ErrSkillArchive)
	}
	md, err := os.ReadFile(filepath.Join(tmp, "SKILL.md"))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSkillArchive, err)
	}
	if _, _, err := parseSkillFrontmatter(string(md)); err != nil {
		return err
	}

	final := filepath.Join(base, name)
	if err := os.RemoveAll(final); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	return chownTree(final, m.cfg.PicoclawUser)
}

// ArchiveSharedSkill streams a skill directory to w as a zip.
func (m *Manager) ArchiveSharedSkill(scope Scope, rawName string, w io.Writer) error {
	name, err := sanitizeSkillName(rawName)
	if err != nil {
		return err
	}
	root := filepath.Join(m.sharedSkillsDir(scope), name)
	if _, err := os.Stat(root); err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	defer zw.Close()
	return filepath.Walk(root, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return walkErr
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		fw, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(fw, src)
		return err
	})
}

// DeleteSharedSkill removes a skill directory (idempotent).
func (m *Manager) DeleteSharedSkill(scope Scope, rawName string) error {
	name, err := sanitizeSkillName(rawName)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(m.sharedSkillsDir(scope), name)); err != nil {
		return err
	}
	return nil
}

// syncEffectiveSkills rebuilds the merged effective-skills dir for one
// (tenant, subscription, agent): later source wins by name, so an agent-targeted
// skill overrides an all-agents one and a subscription overrides a tenant; the
// reserved shared-content is skipped. Copies (not symlinks) so the read-only
// bind exposes the content inside the container.
func (m *Manager) syncEffectiveSkills(tenantID, subsAccID, agentKey string) error {
	root := m.cfg.ContainerDataRoot
	eff := config.EffectiveSkillsDir(root, tenantID, subsAccID, agentKey)
	ordered := []string{
		config.TenantSharedSkillsDir(root, tenantID),
		config.TenantAgentSharedSkillsDir(root, tenantID, agentKey),
		config.SubscriptionSharedSkillsDir(root, tenantID, subsAccID),
		config.SubscriptionAgentSharedSkillsDir(root, tenantID, subsAccID, agentKey),
	}

	sources := map[string]string{}
	for _, d := range ordered { // ascending precedence → last wins
		entries, err := os.ReadDir(d)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		for _, e := range entries {
			if !e.IsDir() || e.Name() == "shared-content" {
				continue
			}
			if _, err := os.Stat(filepath.Join(d, e.Name(), "SKILL.md")); err != nil {
				continue
			}
			sources[e.Name()] = filepath.Join(d, e.Name())
		}
	}

	// Clear the effective dir's CONTENTS but keep the dir itself: containers
	// bind-mount this directory, and removing+recreating it would orphan their
	// mount to the old (now-empty) inode. Emptying its children in place keeps
	// the inode stable so the RO bind reflects updates live (same discipline as
	// the effective-secrets dir).
	if err := os.MkdirAll(eff, 0o700); err != nil {
		return err
	}
	existing, err := os.ReadDir(eff)
	if err != nil {
		return err
	}
	for _, e := range existing {
		if err := os.RemoveAll(filepath.Join(eff, e.Name())); err != nil {
			return err
		}
	}
	for name, src := range sources {
		if err := copyTree(src, filepath.Join(eff, name)); err != nil {
			return err
		}
	}
	return chownTree(eff, m.cfg.PicoclawUser)
}

// SyncEffectiveSkillsForScope rebuilds the effective-skills dir(s) affected by a
// change at a scope: the subscriptions in range (just one, or every subscription
// under the tenant) crossed with the agents in range (just the targeted one, or
// every configured agent when the target is all-agents).
func (m *Manager) SyncEffectiveSkillsForScope(scope Scope) error {
	subsIDs := []string{scope.SubsAccID}
	if scope.Kind == ScopeTenant {
		all, err := m.ListTenantSubscriptions(scope.TenantID)
		if err != nil {
			return err
		}
		subsIDs = all
	}
	for _, s := range subsIDs {
		for _, agentKey := range m.agentsInScope(scope) {
			if err := m.syncEffectiveSkills(scope.TenantID, s, agentKey); err != nil {
				return err
			}
		}
	}
	return nil
}

// agentsInScope is the set of agent keys a scope-level write reaches: the single
// targeted agent, or every configured agent for an all-agents write.
func (m *Manager) agentsInScope(scope Scope) []string {
	if scope.AgentKey != "" {
		return []string{scope.AgentKey}
	}
	out := make([]string, 0, len(m.cfg.Agents))
	for key := range m.cfg.Agents {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
