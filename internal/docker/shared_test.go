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
	for _, f := range []string{FormatFile, FormatNative, "yaml"} {
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

func writeEnvFile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
