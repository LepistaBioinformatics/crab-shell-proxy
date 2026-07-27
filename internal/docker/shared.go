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
// scope. AgentKey narrows the scope to a single agent; empty means "all agents"
// — the pre-per-agent store, which every agent under the scope still reads
// (per-agent-injection-scope FR-1).
type Scope struct {
	Kind      ScopeKind
	TenantID  string
	SubsAccID string
	AgentKey  string
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

// sharedFileBind pairs a shared-files store's proxy-side path (which must exist
// and be agent-readable) with the Docker bind string built from its host-side
// twin.
type sharedFileBind struct {
	container string
	bind      string
}

// sharedFileBinds is the read-only shared-files bind set for one workspace: the
// agent-less tenant/subscription pair (all agents) plus the pair scoped to this
// workspace's own agent (per-agent-injection-scope FR-5). Four sibling mounts
// rather than a copy-merge like skills — merging would duplicate arbitrarily
// large uploads on every provision. Pure so the bind set is testable without
// Docker or root.
func sharedFileBinds(cfg *config.Config, key WorkspaceKey, mountDest string) []sharedFileBind {
	type layer struct {
		dest      string
		container string
		host      string
	}
	layers := []layer{
		{
			dest:      "tenant",
			container: config.TenantSharedFilesDir(cfg.ContainerDataRoot, key.TenantID),
			host:      config.TenantSharedFilesDir(cfg.HostDataRoot, key.TenantID),
		},
		{
			dest:      "subscription",
			container: config.SubscriptionSharedFilesDir(cfg.ContainerDataRoot, key.TenantID, key.SubsAccID),
			host:      config.SubscriptionSharedFilesDir(cfg.HostDataRoot, key.TenantID, key.SubsAccID),
		},
		{
			dest:      "tenant-agent",
			container: config.TenantAgentSharedFilesDir(cfg.ContainerDataRoot, key.TenantID, key.Role),
			host:      config.TenantAgentSharedFilesDir(cfg.HostDataRoot, key.TenantID, key.Role),
		},
		{
			dest:      "subscription-agent",
			container: config.SubscriptionAgentSharedFilesDir(cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role),
			host:      config.SubscriptionAgentSharedFilesDir(cfg.HostDataRoot, key.TenantID, key.SubsAccID, key.Role),
		},
	}
	out := make([]sharedFileBind, 0, len(layers))
	for _, l := range layers {
		out = append(out, sharedFileBind{
			container: l.container,
			bind:      l.host + ":" + mountDest + "/workspace/.shared/" + l.dest + ":ro",
		})
	}
	return out
}

func (m *Manager) sharedFilesDir(scope Scope) string {
	root := m.cfg.ContainerDataRoot
	switch {
	case scope.Kind == ScopeTenant && scope.AgentKey == "":
		return config.TenantSharedFilesDir(root, scope.TenantID)
	case scope.Kind == ScopeTenant:
		return config.TenantAgentSharedFilesDir(root, scope.TenantID, scope.AgentKey)
	case scope.AgentKey == "":
		return config.SubscriptionSharedFilesDir(root, scope.TenantID, scope.SubsAccID)
	default:
		return config.SubscriptionAgentSharedFilesDir(root, scope.TenantID, scope.SubsAccID, scope.AgentKey)
	}
}

func (m *Manager) sharedSecretsDir(scope Scope) string {
	root := m.cfg.ContainerDataRoot
	switch {
	case scope.Kind == ScopeTenant && scope.AgentKey == "":
		return config.TenantSharedSecretsDir(root, scope.TenantID)
	case scope.Kind == ScopeTenant:
		return config.TenantAgentSharedSecretsDir(root, scope.TenantID, scope.AgentKey)
	case scope.AgentKey == "":
		return config.SubscriptionSharedSecretsDir(root, scope.TenantID, scope.SubsAccID)
	default:
		return config.SubscriptionAgentSharedSecretsDir(root, scope.TenantID, scope.SubsAccID, scope.AgentKey)
	}
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
// per-user sink writers. Allowed formats are the env-shaped sinks (dotenv, json)
// — what the cascade injects as env — plus native, which since
// native-secrets-admin-only is an admin-only surface and reaches workspaces
// through the same cascade. `file` is not env-shaped and stays rejected.
func (m *Manager) WriteSharedSecret(scope Scope, format, name, value string) error {
	if err := m.checkSharedSecretFormat(scope, format, name); err != nil {
		return err
	}
	dir := m.sharedSecretsDir(scope)
	if err := writeSecret(m.reg, dir, m.scopeSecurityTemplate(scope), format, name, value); err != nil {
		return err
	}
	return chownTree(dir, m.cfg.PicoclawUser)
}

// checkSharedSecretFormat gates which formats a scope accepts and validates the
// name. A native slot is additionally constrained by target: web.<provider>
// works at any target, while a model_list slot must name a single agent
// (FR-4) — the model itself is validated against the inventory by
// writeSecret/validateNativeSlot.
func (m *Manager) checkSharedSecretFormat(scope Scope, format, name string) error {
	// The HTTP layer validates the agent target, but this is a public manager
	// method: without this check, a caller bypassing HTTP validation could pass
	// an unknown agent key and get a scope with no real target instead of a
	// clear error.
	if scope.AgentKey != "" {
		if _, ok := m.cfg.Agents[scope.AgentKey]; !ok {
			return fmt.Errorf("%w: unknown agent %q", ErrInvalidSecretName, scope.AgentKey)
		}
	}
	switch format {
	case FormatDotenv, FormatJSON:
		return validateSecretName(name)
	case FormatNative:
		if err := validateSecretName(name); err != nil {
			return err
		}
		if isNativeModelSlot(name) && scope.AgentKey == "" {
			return fmt.Errorf("%w: a model_list slot must target a single agent, not all agents", ErrUnknownNativeSlot)
		}
		return nil // the slot itself is validated by writeSecret/validateNativeSlot
	default:
		return fmt.Errorf("%w: shared secrets support only dotenv, json and native", ErrInvalidSecretName)
	}
}

// scopeSecurityTemplate is the secPath writeSecret is given for a scope-level
// native write: the target agent's template. Empty for an all-agents scope
// (and for an unknown key, which checkSharedSecretFormat rejects first) —
// harmless, since validateNativeSlot no longer reads it; it is dead weight kept
// only because writeSecret's signature still accepts a secPath argument.
func (m *Manager) scopeSecurityTemplate(scope Scope) string {
	agent, ok := m.cfg.Agents[scope.AgentKey]
	if !ok {
		return ""
	}
	return filepath.Join(config.TemplatesDir(m.cfg.ContainerDataRoot, agent.Template), ".security.yml")
}

// ListSharedSecrets returns the names of a scope's shared secrets per format,
// never a value (write-only API).
func (m *Manager) ListSharedSecrets(scope Scope) (SecretNames, error) {
	return listSecretNames(m.sharedSecretsDir(scope))
}

// DeleteSharedSecret removes one shared secret from a scope. The empty secPath
// makes the native branch store-only: there is no scope-level .security.yml to
// unset, and each affected workspace is rebuilt from the cascade by
// syncEffectiveSecrets.
func (m *Manager) DeleteSharedSecret(scope Scope, format, name string) error {
	if err := m.checkSharedSecretFormat(scope, format, name); err != nil {
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
		// An agent-targeted write only affects that agent's workspaces (FR-7); an
		// all-agents write keeps hitting every one, as before.
		if scope.AgentKey != "" && s.Labels[LabelAgent] != scope.AgentKey {
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
		effDir, err := m.syncEffectiveSecrets(key)
		if err != nil {
			m.logf("restart scope %s/%s: sync secrets %s failed: %v",
				scope.TenantID, scope.SubsAccID, m.ContainerName(key), err)
			continue
		}
		// Native slots are not sink files — they live inside .security.yml — so the
		// mount alone does not deliver them. Merge them into this workspace before
		// the restart (native-secrets-admin-only FR-6).
		if err := m.applyNativeToWorkspace(key, effDir); err != nil {
			m.logf("restart scope %s/%s: apply native %s failed: %v",
				scope.TenantID, scope.SubsAccID, m.ContainerName(key), err)
		}
		if err := m.RestartWorkspace(key); err != nil {
			m.logf("restart scope %s/%s: restart %s failed: %v",
				scope.TenantID, scope.SubsAccID, m.ContainerName(key), err)
		}
	}
	return nil
}

// applyNativeToWorkspace merges the effective native overlay into one
// established workspace's .security.yml. A workspace that has not been
// provisioned yet is a no-op: it picks the overlay up at its first provision.
func (m *Manager) applyNativeToWorkspace(key WorkspaceKey, effDir string) error {
	secPath := filepath.Join(config.UserWorkspace(m.cfg.ContainerDataRoot,
		key.TenantID, key.SubsAccID, key.Role, key.UserAccID), ".security.yml")
	if _, err := os.Stat(secPath); err != nil {
		return nil
	}
	return applyNativeSecrets(secPath, effDir, m.cfg.PicoclawUser, m.logf)
}

// UnsetNativeSlotForScope removes one native slot from the .security.yml of every
// established workspace the scope covers, then immediately re-applies the
// remaining cascade to that same workspace. applyNativeSecrets only ever SETS
// slots, so a deleted shared native secret would otherwise linger; and unsetting
// alone would briefly strip a slot a BROADER layer still provides (tenant/all
// keeps a value the deleted subscription entry was merely overriding). Doing both
// per workspace keeps it atomic and covers stopped workspaces, which RestartScope
// never visits. Per-workspace failures are logged, not returned — the store
// delete already succeeded.
func (m *Manager) UnsetNativeSlotForScope(scope Scope, slot string) {
	for _, key := range m.workspacesInScope(scope) {
		secPath := filepath.Join(config.UserWorkspace(m.cfg.ContainerDataRoot,
			key.TenantID, key.SubsAccID, key.Role, key.UserAccID), ".security.yml")
		if _, err := os.Stat(secPath); err != nil {
			continue
		}
		sec, err := readSecurityConfig(secPath)
		if err != nil {
			m.logf("unset native slot %q in %s: read failed: %v", slot, m.ContainerName(key), err)
			continue
		}
		unsetNativeSlot(sec, slot)
		if err := writeSecurityConfig(secPath, sec, m.cfg.PicoclawUser); err != nil {
			m.logf("unset native slot %q in %s: write failed: %v", slot, m.ContainerName(key), err)
			continue
		}
		effDir, err := m.syncEffectiveSecrets(key)
		if err != nil {
			m.logf("unset native slot %q in %s: sync failed: %v", slot, m.ContainerName(key), err)
			continue
		}
		if err := m.applyNativeToWorkspace(key, effDir); err != nil {
			m.logf("unset native slot %q in %s: reapply failed: %v", slot, m.ContainerName(key), err)
		}
	}
}

// workspacesInScope enumerates the on-disk user workspaces a scope covers,
// narrowed to the scope's target agent when it has one.
func (m *Manager) workspacesInScope(scope Scope) []WorkspaceKey {
	subsIDs := []string{scope.SubsAccID}
	if scope.Kind == ScopeTenant {
		all, err := m.ListTenantSubscriptions(scope.TenantID)
		if err != nil {
			m.logf("workspaces in scope %s: list subscriptions failed: %v", scope.TenantID, err)
			return nil
		}
		subsIDs = all
	}
	out := []WorkspaceKey{}
	for _, sid := range subsIDs {
		users, err := m.ListSubscriptionUsers(scope.TenantID, sid)
		if err != nil {
			m.logf("workspaces in scope %s/%s: list users failed: %v", scope.TenantID, sid, err)
			continue
		}
		for _, u := range users {
			if scope.AgentKey != "" && u.Role != scope.AgentKey {
				continue
			}
			out = append(out, WorkspaceKey{
				TenantID: scope.TenantID, SubsAccID: sid, Role: u.Role, UserAccID: u.AccID,
			})
		}
	}
	return out
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
	sharedDirs := m.sharedSecretsCascade(key)
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

	dotenv, err := cascadeSink(sharedDirs, userStore, userWins, readDotenvMap)
	if err != nil {
		return "", err
	}
	jsonSink, err := cascadeSink(sharedDirs, userStore, userWins,
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
	// Native slots are an ADMIN surface (native-secrets-admin-only FR-5): the
	// user's own overlay is the lowest layer — legacy entries keep working — and
	// each shared layer overrides it, most specific last. This inverts the
	// dotenv/json rule above, where the user wins.
	native, err := cascadeNative(sharedDirs, userStore)
	if err != nil {
		return "", err
	}
	nativePath := filepath.Join(eff, "native.yml")
	if len(native) == 0 {
		// No overlay anywhere: drop a stale effective copy rather than leaving the
		// last merge behind.
		if err := os.Remove(nativePath); err != nil && !os.IsNotExist(err) {
			return "", err
		}
	} else if err := writeOverlay(nativePath, native); err != nil {
		return "", err
	}
	if err := chownTree(eff, m.cfg.PicoclawUser); err != nil {
		return "", fmt.Errorf("chown effective secrets: %w", err)
	}
	return eff, nil
}

// sharedSecretsCascade is the ordered list of shared secret stores that feed a
// workspace's effective view, lowest precedence first: tenant all-agents →
// tenant this-agent → subscription all-agents → subscription this-agent
// (per-agent-injection-scope FR-3). The workspace's own agent is key.Role.
func (m *Manager) sharedSecretsCascade(key WorkspaceKey) []string {
	root := m.cfg.ContainerDataRoot
	return []string{
		config.TenantSharedSecretsDir(root, key.TenantID),
		config.TenantAgentSharedSecretsDir(root, key.TenantID, key.Role),
		config.SubscriptionSharedSecretsDir(root, key.TenantID, key.SubsAccID),
		config.SubscriptionAgentSharedSecretsDir(root, key.TenantID, key.SubsAccID, key.Role),
	}
}

// cascadeNative merges the native.yml overlays: the user's own first (lowest
// precedence, so a pre-gate entry keeps applying) then each shared dir in
// ascending-precedence order on top.
func cascadeNative(sharedDirs []string, userDir string) (map[string]string, error) {
	out := map[string]string{}
	for _, dir := range append([]string{userDir}, sharedDirs...) {
		mm, err := readOverlay(filepath.Join(dir, "native.yml"))
		if err != nil {
			return nil, err
		}
		for k, v := range mm {
			out[k] = v
		}
	}
	return out, nil
}

// cascadeSink merges one sink (read by `read`) across the shared dirs in
// ascending-precedence order and then the user's own, with the user winning: a
// shared entry whose name the user owns (in any sink) is dropped, then the
// user's own entries are layered on top.
func cascadeSink(sharedDirs []string, userDir string, userWins map[string]bool,
	read func(string) (map[string]string, error)) (map[string]string, error) {
	out := map[string]string{}
	for _, dir := range sharedDirs {
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
