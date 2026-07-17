package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
)

// ScopeKind names the tier a shared-content operation targets.
type ScopeKind string

const (
	// ScopeTenant addresses tenant-scope shared content (visible to all of T).
	ScopeTenant ScopeKind = "tenant"
	// ScopeSubscription addresses subscription-scope shared content (all of S).
	ScopeSubscription ScopeKind = "subscription"
)

// Scope identifies where shared content lives. SubsAccID is empty for a tenant
// scope.
type Scope struct {
	Kind      ScopeKind
	TenantID  string
	SubsAccID string
}

// FileMeta is the metadata-only view of a stored file (never its bytes). It is
// what the admin list endpoints return, including the user-file list (FR-7:
// metadata carries no content).
type FileMeta struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modifiedAt"`
}

// UserRef is one end user under a subscription, discovered on disk. Role is the
// agent whose workspace the user was found under (retained so a caller can scope
// a user-file op to that agent).
type UserRef struct {
	AccID string `json:"accId"`
	Role  string `json:"role"`
	Email string `json:"email"`
}

func (m *Manager) sharedFilesDir(scope Scope) string {
	if scope.Kind == ScopeTenant {
		return config.TenantSharedFilesDir(m.cfg.ContainerDataRoot, scope.TenantID)
	}
	return config.SubscriptionSharedFilesDir(m.cfg.ContainerDataRoot, scope.TenantID, scope.SubsAccID)
}

func (m *Manager) sharedSecretsDir(scope Scope) string {
	if scope.Kind == ScopeTenant {
		return config.TenantSharedSecretsDir(m.cfg.ContainerDataRoot, scope.TenantID)
	}
	return config.SubscriptionSharedSecretsDir(m.cfg.ContainerDataRoot, scope.TenantID, scope.SubsAccID)
}

// ListSharedFiles returns the metadata of the files stored at a scope (never
// their bytes). An absent dir is an empty list.
func (m *Manager) ListSharedFiles(scope Scope) ([]FileMeta, error) {
	return listFileMeta(m.sharedFilesDir(scope))
}

// WriteSharedFile stores an uploaded file at a scope under a sanitized name
// (latest-write-wins — no versioning, spec out-of-scope), chowned to the
// picoclaw user so mounted containers can read it. The size cap is enforced by
// the handler.
func (m *Manager) WriteSharedFile(scope Scope, rawName string, r io.Reader) (StoredMedia, error) {
	name, err := sanitizeFilename(rawName)
	if err != nil {
		return StoredMedia{}, err
	}
	dir := m.sharedFilesDir(scope)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return StoredMedia{}, fmt.Errorf("mkdir shared files: %w", err)
	}
	full := filepath.Join(dir, name)
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return StoredMedia{}, fmt.Errorf("create shared file: %w", err)
	}
	n, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(full)
		return StoredMedia{}, fmt.Errorf("write shared file: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(full)
		return StoredMedia{}, closeErr
	}
	if err := chownTree(dir, m.cfg.PicoclawUser); err != nil {
		return StoredMedia{}, fmt.Errorf("chown shared files: %w", err)
	}
	return StoredMedia{Path: name, Name: name, Size: n}, nil
}

// ReadSharedFile opens a scope's shared file for download and returns it with
// its metadata. The name is sanitized so it can never escape the scope dir.
func (m *Manager) ReadSharedFile(scope Scope, name string) (io.ReadCloser, FileMeta, error) {
	safe, err := sanitizeFilename(name)
	if err != nil {
		return nil, FileMeta{}, err
	}
	full := filepath.Join(m.sharedFilesDir(scope), safe)
	info, err := os.Stat(full)
	if err != nil {
		return nil, FileMeta{}, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, FileMeta{}, err
	}
	return f, FileMeta{Name: safe, Size: info.Size(), ModifiedAt: modTime(info)}, nil
}

// DeleteSharedFile removes one shared file from a scope. Missing file is a
// success (idempotent).
func (m *Manager) DeleteSharedFile(scope Scope, name string) error {
	safe, err := sanitizeFilename(name)
	if err != nil {
		return err
	}
	full := filepath.Join(m.sharedFilesDir(scope), safe)
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// WriteSharedSecret upserts one shared secret into a scope's store, reusing the
// per-user sink writers. Restricted to the env-shaped sinks (dotenv, json) —
// those are what the cascade injects as env; native has no per-workspace
// .security.yml to validate against at scope level, and file isn't env-shaped.
func (m *Manager) WriteSharedSecret(scope Scope, format, name, value string) error {
	if format != FormatDotenv && format != FormatJSON {
		return fmt.Errorf("%w: shared secrets support only dotenv and json", ErrInvalidSecretName)
	}
	if err := validateSecretName(name); err != nil {
		return err
	}
	dir := m.sharedSecretsDir(scope)
	if err := writeSecret(dir, "", format, name, value); err != nil {
		return err
	}
	return chownTree(dir, m.cfg.PicoclawUser)
}

// ListSharedSecrets returns the names of a scope's shared secrets per format,
// never a value (write-only API).
func (m *Manager) ListSharedSecrets(scope Scope) (SecretNames, error) {
	return listSecretNames(m.sharedSecretsDir(scope))
}

// DeleteSharedSecret removes one shared secret from a scope.
func (m *Manager) DeleteSharedSecret(scope Scope, format, name string) error {
	if format != FormatDotenv && format != FormatJSON {
		return fmt.Errorf("%w: shared secrets support only dotenv and json", ErrInvalidSecretName)
	}
	if err := validateSecretName(name); err != nil {
		return err
	}
	dir := m.sharedSecretsDir(scope)
	if err := deleteSecret(dir, "", "", format, name); err != nil {
		return err
	}
	return chownTree(dir, m.cfg.PicoclawUser)
}

// ListSubscriptionUsers enumerates the end users under a subscription by
// scanning the on-disk .../agents/<role>/users/<u> leaves, reading each user's
// .crab-owner.json marker for the owner email.
func (m *Manager) ListSubscriptionUsers(tenantID, subsAccID string) ([]UserRef, error) {
	pattern := filepath.Join(config.SubscriptionRoot(m.cfg.ContainerDataRoot, tenantID, subsAccID),
		"*", "users", "*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	out := []UserRef{}
	for _, uw := range matches {
		fi, err := os.Stat(uw)
		if err != nil || !fi.IsDir() {
			continue
		}
		// .../agents/<role>/users/<u> — role is two segments up from the leaf.
		role := filepath.Base(filepath.Dir(filepath.Dir(uw)))
		out = append(out, UserRef{
			AccID: filepath.Base(uw),
			Role:  role,
			Email: ownerEmail(uw),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Role != out[j].Role {
			return out[i].Role < out[j].Role
		}
		return out[i].AccID < out[j].AccID
	})
	return out, nil
}

// ListTenants returns the tenant ids present on disk (dir names under
// <root>/tenants), used by scope discovery for an Instance-tier caller.
func (m *Manager) ListTenants() ([]string, error) {
	return dirNames(filepath.Join(m.cfg.ContainerDataRoot, "tenants"))
}

// ListTenantSubscriptions returns the subscription account ids present on disk
// under a tenant, so scope discovery can enumerate the subscriptions a
// Tenant/Instance-tier caller manages (FR-8).
func (m *Manager) ListTenantSubscriptions(tenantID string) ([]string, error) {
	return dirNames(filepath.Join(m.cfg.ContainerDataRoot, "tenants",
		identity.SanitizeID(tenantID), "subscriptions"))
}

// dirNames returns the names of the subdirectories of dir (sorted). An absent
// dir is an empty list.
func dirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// ListUserFiles returns the metadata (never bytes) of a user's private uploaded
// files for the addressed agent's workspace (FR-6/FR-7). "Private files" are
// the user's uploads dir — the content the user themselves uploaded.
func (m *Manager) ListUserFiles(key WorkspaceKey) ([]FileMeta, error) {
	return listFileMeta(config.UploadsDir(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID))
}

// DeleteUserFile removes one of a user's private uploaded files by its stored
// name. The name is validated to a safe base name so it can never escape the
// uploads dir. Missing file is a success (idempotent). It NEVER reads the bytes
// (FR-7).
func (m *Manager) DeleteUserFile(key WorkspaceKey, name string) error {
	return m.DeleteMedia(key, name)
}

// RestartScope is the best-effort propagation of a shared-content write/delete
// (NFR-4): it recreates the running containers under the affected scope so a
// changed shared file (live RO mount) is re-read and a changed shared secret
// (injected as env at create time) takes effect. Idled/absent containers pick
// up the change on their next cold start regardless. Per-container failures are
// logged, not returned — the write already succeeded.
func (m *Manager) RestartScope(scope Scope) error {
	ctx := context.Background()
	summaries, err := m.docker.List(ctx, LabelManaged+"=true")
	if err != nil {
		return err
	}
	for _, s := range summaries {
		if s.State != "running" || s.Labels[LabelTenant] != scope.TenantID {
			continue
		}
		if scope.Kind == ScopeSubscription && s.Labels[LabelSubscription] != scope.SubsAccID {
			continue
		}
		key := WorkspaceKey{
			TenantID:  s.Labels[LabelTenant],
			SubsAccID: s.Labels[LabelSubscription],
			Role:      s.Labels[LabelAgent],
			UserAccID: s.Labels[LabelUser],
		}
		// Rebuild the effective secret view (so the change is on disk) then
		// stop/start — NOT recreate — so picoclaw reloads it while keeping its
		// live session; a recreate would truncate the transcript.
		if _, err := m.syncEffectiveSecrets(key); err != nil {
			m.logf("restart scope %s/%s: sync secrets %s failed: %v",
				scope.TenantID, scope.SubsAccID, m.ContainerName(key), err)
			continue
		}
		if err := m.RestartWorkspace(key); err != nil {
			m.logf("restart scope %s/%s: restart %s failed: %v",
				scope.TenantID, scope.SubsAccID, m.ContainerName(key), err)
		}
	}
	return nil
}

// syncEffectiveSecrets materializes the user's EFFECTIVE secret view — the
// tenant- and subscription-shared secrets (dotenv/json) cascaded with the user's
// own store on top (user wins) — into EffectiveSecretsDir, and returns its
// container path. That dir is bind-mounted read-only at workspace/.secrets, so
// shared secrets reach picoclaw exactly like the user's own (as sink files),
// delivered live via the mount and needing only a stop/start (never a recreate)
// to take effect (FR-5). Called on every provision and whenever a user or shared
// secret changes.
func (m *Manager) syncEffectiveSecrets(key WorkspaceKey) (string, error) {
	userStore := config.StoreDir(m.cfg.ContainerDataRoot, key.UserAccID, key.Role)
	tenantShared := config.TenantSharedSecretsDir(m.cfg.ContainerDataRoot, key.TenantID)
	subsShared := config.SubscriptionSharedSecretsDir(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID)
	eff := config.EffectiveSecretsDir(m.cfg.ContainerDataRoot, key.UserAccID, key.Role)
	if err := os.MkdirAll(eff, 0o700); err != nil {
		return "", fmt.Errorf("create effective secrets dir: %w", err)
	}

	// Any name the user has set (in either sink) wins over a shared entry.
	userNames, err := listSecretNames(userStore)
	if err != nil {
		return "", err
	}
	userWins := map[string]bool{}
	for _, n := range append(append([]string{}, userNames.Dotenv...), userNames.JSON...) {
		userWins[n] = true
	}

	dotenv, err := cascadeSink(tenantShared, subsShared, userStore, userWins, readDotenvMap)
	if err != nil {
		return "", err
	}
	jsonSink, err := cascadeSink(tenantShared, subsShared, userStore, userWins,
		func(dir string) (map[string]string, error) { return readJSONMap(filepath.Join(dir, "secrets.json")) })
	if err != nil {
		return "", err
	}
	if err := writeDotenvFile(filepath.Join(eff, ".env"), dotenv); err != nil {
		return "", err
	}
	if err := writeJSONFile(filepath.Join(eff, "secrets.json"), jsonSink); err != nil {
		return "", err
	}
	// The user's native overlay is never shared; pass it through unchanged.
	if err := copyFileIfExists(filepath.Join(userStore, "native.yml"), filepath.Join(eff, "native.yml")); err != nil {
		return "", err
	}
	if err := chownTree(eff, m.cfg.PicoclawUser); err != nil {
		return "", fmt.Errorf("chown effective secrets: %w", err)
	}
	return eff, nil
}

// cascadeSink merges one sink (read by `read`) across tenant → subscription →
// user, with the user winning: a shared entry whose name the user owns (in any
// sink) is dropped, then the user's own entries are layered on top.
func cascadeSink(tenantDir, subsDir, userDir string, userWins map[string]bool,
	read func(string) (map[string]string, error)) (map[string]string, error) {
	out := map[string]string{}
	for _, dir := range []string{tenantDir, subsDir} {
		mm, err := read(dir)
		if err != nil {
			return nil, err
		}
		for k, v := range mm {
			if !userWins[k] {
				out[k] = v
			}
		}
	}
	userMap, err := read(userDir)
	if err != nil {
		return nil, err
	}
	for k, v := range userMap {
		out[k] = v
	}
	return out, nil
}

// readDotenvMap reads a store dir's .env sink into a NAME→value map (absent → empty).
func readDotenvMap(dir string) (map[string]string, error) {
	out := map[string]string{}
	lines, err := readLines(filepath.Join(dir, ".env"))
	if err != nil {
		return nil, err
	}
	for _, l := range lines {
		if k, v, ok := strings.Cut(l, "="); ok {
			if k = strings.TrimSpace(k); k != "" {
				out[k] = v
			}
		}
	}
	return out, nil
}

func writeDotenvFile(path string, m map[string]string) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(m[k])
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func writeJSONFile(path string, m map[string]string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// copyFileIfExists copies src→dst, or removes dst when src is absent (so a
// deleted upstream overlay doesn't leave a stale copy in the effective view).
func copyFileIfExists(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			_ = os.Remove(dst)
			return nil
		}
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

// listFileMeta returns the metadata of the regular files in dir (never their
// bytes). An absent dir is an empty list.
func listFileMeta(dir string) ([]FileMeta, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []FileMeta{}, nil
		}
		return nil, err
	}
	out := make([]FileMeta, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, FileMeta{Name: e.Name(), Size: info.Size(), ModifiedAt: modTime(info)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func modTime(info os.FileInfo) string {
	return info.ModTime().UTC().Format(time.RFC3339)
}

// ownerEmail reads the owner email from a user workspace's .crab-owner.json
// marker, or "" when absent/unreadable.
func ownerEmail(userDir string) string {
	raw, err := os.ReadFile(filepath.Join(userDir, ".crab-owner.json"))
	if err != nil {
		return ""
	}
	var info struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return ""
	}
	return info.Email
}
