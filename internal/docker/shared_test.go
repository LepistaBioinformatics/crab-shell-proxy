package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// sharedManager builds a Manager over a temp root with no chown (PicoclawUser
// empty) so the filesystem-only shared-content methods run unprivileged.
func sharedManager(t *testing.T) *Manager {
	t.Helper()
	cfg := &config.Config{
		HostDataRoot:      "/host/data",
		ContainerDataRoot: t.TempDir(),
		ContainerPrefix:   "picoclaw",
	}
	return NewManager(cfg, nil, func(context.Context, string, int) error { return nil }, nil)
}

func tenantScope() Scope {
	return Scope{Kind: ScopeTenant, TenantID: "t1"}
}

func TestSharedFileRoundTrip(t *testing.T) {
	m := sharedManager(t)
	scope := tenantScope()
	if _, err := m.WriteSharedFile(scope, "policy.txt", strings.NewReader("hello")); err != nil {
		t.Fatalf("WriteSharedFile: %v", err)
	}
	files, err := m.ListSharedFiles(scope)
	if err != nil {
		t.Fatalf("ListSharedFiles: %v", err)
	}
	if len(files) != 1 || files[0].Name != "policy.txt" || files[0].Size != 5 || files[0].ModifiedAt == "" {
		t.Fatalf("list = %+v", files)
	}
	rc, meta, err := m.ReadSharedFile(scope, "policy.txt")
	if err != nil {
		t.Fatalf("ReadSharedFile: %v", err)
	}
	defer rc.Close()
	if meta.Size != 5 {
		t.Errorf("meta size = %d, want 5", meta.Size)
	}
	if err := m.DeleteSharedFile(scope, "policy.txt"); err != nil {
		t.Fatalf("DeleteSharedFile: %v", err)
	}
	files, _ = m.ListSharedFiles(scope)
	if len(files) != 0 {
		t.Errorf("after delete list = %+v, want empty", files)
	}
}

// TestSharedFileTraversalRejected proves NFR-5: a traversal name can never
// escape the scope dir (write/read/delete all reject it).
func TestSharedFileTraversalRejected(t *testing.T) {
	m := sharedManager(t)
	scope := tenantScope()
	for _, name := range []string{"../escape", "../../etc/passwd", "a/../../b"} {
		if _, err := m.WriteSharedFile(scope, name, strings.NewReader("x")); err == nil {
			// sanitizeFilename may reduce to a safe base; ensure nothing landed
			// outside the scope dir.
			outside := filepath.Join(m.cfg.ContainerDataRoot, "tenants", "escape")
			if _, statErr := os.Stat(outside); statErr == nil {
				t.Errorf("traversal name %q escaped the scope dir", name)
			}
		}
	}
	// A pure traversal token is rejected outright.
	if _, _, err := m.ReadSharedFile(scope, ".."); err == nil {
		t.Error("ReadSharedFile(\"..\") should be rejected")
	}
}

// TestSharedSecretFormatRestricted proves shared secrets accept only the
// env-shaped sinks (dotenv/json); file/native are rejected.
func TestSharedSecretFormatRestricted(t *testing.T) {
	m := sharedManager(t)
	scope := tenantScope()
	if err := m.WriteSharedSecret(scope, FormatDotenv, "A", "1"); err != nil {
		t.Errorf("dotenv should be accepted: %v", err)
	}
	if err := m.WriteSharedSecret(scope, FormatJSON, "B", "2"); err != nil {
		t.Errorf("json should be accepted: %v", err)
	}
	// file is not env-shaped, and "yaml" is not a format at all. native IS now
	// accepted at scope level (native-secrets-admin-only) — its slot rules are
	// covered by TestSharedNativeSecretTargetRules.
	for _, f := range []string{FormatFile, "yaml"} {
		if err := m.WriteSharedSecret(scope, f, "C", "3"); err == nil {
			t.Errorf("format %q should be rejected for shared secrets", f)
		}
	}
	names, err := m.ListSharedSecrets(scope)
	if err != nil {
		t.Fatalf("ListSharedSecrets: %v", err)
	}
	if len(names.Dotenv) != 1 || names.Dotenv[0] != "A" || len(names.JSON) != 1 || names.JSON[0] != "B" {
		t.Errorf("shared secret names = %+v", names)
	}
}

// TestEffectiveSecretsCascade proves the effective .secrets view precedence:
// tenant is the base, subscription overrides it, and the user's own value wins.
// The merged secrets land in the mounted store as sink files (no env, no
// recreate).
func TestEffectiveSecretsCascade(t *testing.T) {
	m := sharedManager(t)
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}

	// tenant: SHARED=tenant, TENANT_ONLY=t
	tenantSecrets := config.TenantSharedSecretsDir(m.cfg.ContainerDataRoot, "t1")
	writeEnvFile(t, tenantSecrets, "SHARED=tenant\nTENANT_ONLY=t\n")
	// subscription overrides SHARED and adds USER_WINS (which the user also sets)
	subsSecrets := config.SubscriptionSharedSecretsDir(m.cfg.ContainerDataRoot, "t1", "s1")
	writeEnvFile(t, subsSecrets, "SHARED=subscription\nUSER_WINS=shared\n")
	// user's own store sets USER_WINS — must win.
	userStore := config.StoreDir(m.cfg.ContainerDataRoot, "u1", "alpha")
	writeEnvFile(t, userStore, "USER_WINS=mine\n")

	effDir, err := m.syncEffectiveSecrets(key)
	if err != nil {
		t.Fatalf("syncEffectiveSecrets: %v", err)
	}
	eff, err := readDotenvMap(effDir)
	if err != nil {
		t.Fatalf("readDotenvMap: %v", err)
	}
	if eff["SHARED"] != "subscription" {
		t.Errorf("subscription must override tenant for SHARED; got %q", eff["SHARED"])
	}
	if eff["TENANT_ONLY"] != "t" {
		t.Errorf("tenant-only secret missing; got %q", eff["TENANT_ONLY"])
	}
	if eff["USER_WINS"] != "mine" {
		t.Errorf("user value must win; got %q", eff["USER_WINS"])
	}
}

// TestEffectiveSecretsPerAgentCascade proves per-agent-injection-scope FR-3/AC-3:
// the four shared layers stack tenant-all → tenant-agent → subscription-all →
// subscription-agent, an agent-targeted value beats the all-agents one at the
// same tier, and the other agent's store never leaks in.
func TestEffectiveSecretsPerAgentCascade(t *testing.T) {
	m := sharedManager(t)
	root := m.cfg.ContainerDataRoot
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}

	writeEnvFile(t, config.TenantSharedSecretsDir(root, "t1"),
		"LADDER=tenant-all\nALL_ONLY=yes\n")
	writeEnvFile(t, config.TenantAgentSharedSecretsDir(root, "t1", "alpha"),
		"LADDER=tenant-alpha\nAGENT_ONLY=alpha\n")
	writeEnvFile(t, config.SubscriptionSharedSecretsDir(root, "t1", "s1"),
		"LADDER=subs-all\n")
	writeEnvFile(t, config.SubscriptionAgentSharedSecretsDir(root, "t1", "s1", "alpha"),
		"LADDER=subs-alpha\n")
	// A different agent's stores must not reach an alpha workspace.
	writeEnvFile(t, config.TenantAgentSharedSecretsDir(root, "t1", "beta"),
		"BETA_LEAK=nope\n")
	writeEnvFile(t, config.SubscriptionAgentSharedSecretsDir(root, "t1", "s1", "beta"),
		"BETA_LEAK_2=nope\n")

	effDir, err := m.syncEffectiveSecrets(key)
	if err != nil {
		t.Fatalf("syncEffectiveSecrets: %v", err)
	}
	eff, err := readDotenvMap(effDir)
	if err != nil {
		t.Fatalf("readDotenvMap: %v", err)
	}
	if eff["LADDER"] != "subs-alpha" {
		t.Errorf("most specific layer must win; LADDER = %q, want subs-alpha", eff["LADDER"])
	}
	if eff["ALL_ONLY"] != "yes" {
		t.Errorf("all-agents entry must still cascade; ALL_ONLY = %q", eff["ALL_ONLY"])
	}
	if eff["AGENT_ONLY"] != "alpha" {
		t.Errorf("agent-scoped entry missing; AGENT_ONLY = %q", eff["AGENT_ONLY"])
	}
	if _, ok := eff["BETA_LEAK"]; ok {
		t.Error("another agent's tenant-scope secret leaked into this workspace")
	}
	if _, ok := eff["BETA_LEAK_2"]; ok {
		t.Error("another agent's subscription-scope secret leaked into this workspace")
	}
}

// TestSharedStoresAreAgentSeparated proves FR-1/AC-2: the same scope addressed
// with and without an agent target reads and writes distinct stores, and the
// agent-less one keeps its pre-feature path (no migration, AC-5).
func TestSharedStoresAreAgentSeparated(t *testing.T) {
	m := sharedManager(t)
	all := Scope{Kind: ScopeTenant, TenantID: "t1"}
	alpha := Scope{Kind: ScopeTenant, TenantID: "t1", AgentKey: "alpha"}

	if _, err := m.WriteSharedFile(all, "everyone.txt", strings.NewReader("a")); err != nil {
		t.Fatalf("write all-agents file: %v", err)
	}
	if _, err := m.WriteSharedFile(alpha, "alpha-only.txt", strings.NewReader("b")); err != nil {
		t.Fatalf("write agent file: %v", err)
	}

	allFiles, _ := m.ListSharedFiles(all)
	if len(allFiles) != 1 || allFiles[0].Name != "everyone.txt" {
		t.Errorf("all-agents listing = %+v, want only everyone.txt", allFiles)
	}
	alphaFiles, _ := m.ListSharedFiles(alpha)
	if len(alphaFiles) != 1 || alphaFiles[0].Name != "alpha-only.txt" {
		t.Errorf("agent listing = %+v, want only alpha-only.txt", alphaFiles)
	}
	// The agent-less store must still sit at the pre-feature path.
	legacy := filepath.Join(config.TenantSharedFilesDir(m.cfg.ContainerDataRoot, "t1"), "everyone.txt")
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("all-agents file must stay at the legacy path %s: %v", legacy, err)
	}
}

// TestNativeCascadeAdminWinsOverUser proves native-secrets-admin-only AC-5/AC-6:
// in the effective native overlay an admin scope value overrides the user's
// legacy entry, while a legacy entry no admin touched keeps applying.
func TestNativeCascadeAdminWinsOverUser(t *testing.T) {
	m := sharedManager(t)
	root := m.cfg.ContainerDataRoot
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}

	writeNativeOverlay(t, config.StoreDir(root, "u1", "alpha"), map[string]string{
		"web.brave":  "user-legacy",
		"web.tavily": "user-only",
	})
	writeNativeOverlay(t, config.TenantSharedSecretsDir(root, "t1"), map[string]string{
		"web.brave": "tenant-all",
	})
	writeNativeOverlay(t, config.SubscriptionAgentSharedSecretsDir(root, "t1", "s1", "alpha"), map[string]string{
		"web.brave": "subs-alpha",
	})

	effDir, err := m.syncEffectiveSecrets(key)
	if err != nil {
		t.Fatalf("syncEffectiveSecrets: %v", err)
	}
	eff, err := readOverlay(filepath.Join(effDir, "native.yml"))
	if err != nil {
		t.Fatalf("readOverlay: %v", err)
	}
	if eff["web.brave"] != "subs-alpha" {
		t.Errorf("admin scope must beat the user's legacy native value; got %q", eff["web.brave"])
	}
	if eff["web.tavily"] != "user-only" {
		t.Errorf("a legacy slot no admin set must keep applying; got %q", eff["web.tavily"])
	}
}

// TestSharedNativeSecretTargetRules proves native-secrets-admin-only FR-4/AC-3:
// a web slot is publishable at any target, a model_list slot needs a single-agent
// target, and channel_list stays rejected everywhere.
func TestSharedNativeSecretTargetRules(t *testing.T) {
	m := sharedManager(t)
	m.cfg.Agents = map[string]config.Agent{"alpha": {Key: "alpha", Template: "alpha"}}
	// The agent template is what a model slot validates against.
	tmpl := config.TemplatesDir(m.cfg.ContainerDataRoot, "alpha")
	if err := os.MkdirAll(tmpl, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpl, ".security.yml"),
		[]byte("model_list:\n  known-model:\n    api_keys: [placeholder]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	all := Scope{Kind: ScopeTenant, TenantID: "t1"}
	alpha := Scope{Kind: ScopeTenant, TenantID: "t1", AgentKey: "alpha"}

	if err := m.WriteSharedSecret(all, FormatNative, "web.brave", "k"); err != nil {
		t.Errorf("a web slot must be publishable to all agents: %v", err)
	}
	if err := m.WriteSharedSecret(all, FormatNative, "model_list.known-model.api_keys", "k"); err == nil {
		t.Error("a model_list slot must be rejected for an all-agents target")
	}
	if err := m.WriteSharedSecret(alpha, FormatNative, "model_list.known-model.api_keys", "k"); err != nil {
		t.Errorf("a model_list slot in the agent template must be accepted: %v", err)
	}
	if err := m.WriteSharedSecret(alpha, FormatNative, "model_list.absent-model.api_keys", "k"); err == nil {
		t.Error("a model absent from the agent template must be rejected")
	}
	if err := m.WriteSharedSecret(alpha, FormatNative, "channel_list.pico.settings.token", "k"); err == nil {
		t.Error("the pico channel token must never be settable")
	}
	if err := m.WriteSharedSecret(alpha, FormatFile, "SOME_FILE", "k"); err == nil {
		t.Error("the file format is not env-shaped and must stay rejected at scope level")
	}
}

// TestSharedFileBinds proves per-agent-injection-scope FR-5: a workspace mounts
// four read-only shared-files stores — the all-agents pair plus its own agent's
// pair — from the HOST root, at distinct sibling paths. This is the one F2 edit
// the container tests cannot reach (they need root to chown), so it is asserted
// here as a pure function.
func TestSharedFileBinds(t *testing.T) {
	cfg := &config.Config{HostDataRoot: "/host/data", ContainerDataRoot: "/data"}
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "alpha", UserAccID: "u1"}

	binds := sharedFileBinds(cfg, key, "/ws")
	if len(binds) != 4 {
		t.Fatalf("got %d binds, want 4: %+v", len(binds), binds)
	}
	want := []string{
		"/host/data/tenants/t1/shared/files:/ws/workspace/.shared/tenant:ro",
		"/host/data/tenants/t1/subscriptions/s1/shared/files:/ws/workspace/.shared/subscription:ro",
		"/host/data/tenants/t1/shared/agents/alpha/files:/ws/workspace/.shared/tenant-agent:ro",
		"/host/data/tenants/t1/subscriptions/s1/shared/agents/alpha/files:/ws/workspace/.shared/subscription-agent:ro",
	}
	for i, w := range want {
		if binds[i].bind != w {
			t.Errorf("bind[%d] = %q, want %q", i, binds[i].bind, w)
		}
		if !strings.HasPrefix(binds[i].container, "/data/") {
			t.Errorf("bind[%d] container path %q must sit under the CONTAINER root", i, binds[i].container)
		}
	}
}

func writeNativeOverlay(t *testing.T, dir string, m map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeOverlay(filepath.Join(dir, "native.yml"), m); err != nil {
		t.Fatal(err)
	}
}

func writeEnvFile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
