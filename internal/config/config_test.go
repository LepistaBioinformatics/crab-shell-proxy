package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const sample = `
listen: ":9000"
hostDataRoot: "/host/data"
network: "net"
startupDeadline: 30s
turnTimeout: 90s
agents:
  alpha:
    serviceName: "picoclaw-alpha"
    token: { env: "TOK_ALPHA" }
    template: "alpha"
    mode: "scale-to-zero"
    idleTimeout: 15m
  gamma:
    serviceName: "picoclaw-gamma"
    token: "inline-token"
    template: "gamma"
    mode: "continuous"
`

func TestLoadResolvesAndParses(t *testing.T) {
	t.Setenv("TOK_ALPHA", "resolved-alpha")
	cfg, err := Load(writeConfig(t, sample))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":9000" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	if cfg.StartupDeadline.Std() != 30*time.Second {
		t.Errorf("startupDeadline = %v", cfg.StartupDeadline.Std())
	}
	a := cfg.Agents["alpha"]
	if a.Key != "alpha" {
		t.Errorf("agent key not stamped: %q", a.Key)
	}
	if a.ResolvedToken != "resolved-alpha" {
		t.Errorf("token env not resolved: %q", a.ResolvedToken)
	}
	if a.IdleTimeout.Std() != 15*time.Minute {
		t.Errorf("idleTimeout = %v", a.IdleTimeout.Std())
	}
	if cfg.Agents["gamma"].ResolvedToken != "inline-token" {
		t.Errorf("inline token = %q", cfg.Agents["gamma"].ResolvedToken)
	}
	// Defaults applied.
	if cfg.PicoclawPort != 18790 || cfg.ContainerPrefix != "picoclaw" {
		t.Errorf("defaults not applied: port=%d prefix=%q", cfg.PicoclawPort, cfg.ContainerPrefix)
	}
}

func TestAgentByServiceName(t *testing.T) {
	t.Setenv("TOK_ALPHA", "x")
	cfg, err := Load(writeConfig(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := cfg.AgentByServiceName("picoclaw-gamma"); !ok || a.Key != "gamma" {
		t.Errorf("lookup by service name failed: %v %v", a, ok)
	}
	if _, ok := cfg.AgentByServiceName("nope"); ok {
		t.Error("unknown service name should not match")
	}
}

func TestValidateRejectsBadMode(t *testing.T) {
	t.Setenv("TOK_ALPHA", "x")
	body := `
hostDataRoot: "/d"
network: "n"
agents:
  bad:
    serviceName: "s"
    token: "t"
    template: "tpl"
    mode: "sometimes"
`
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Error("expected error for invalid mode")
	}
}

func TestValidateRejectsMissingIdleTimeout(t *testing.T) {
	body := `
hostDataRoot: "/d"
network: "n"
agents:
  a:
    serviceName: "s"
    token: "t"
    template: "tpl"
    mode: "scale-to-zero"
`
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Error("scale-to-zero without idleTimeout must error")
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("TOK_ALPHA", "x")
	t.Setenv("CRAB_HOST_DATA_ROOT", "/override/data")
	t.Setenv("CRAB_NETWORK", "override-net")
	cfg, err := Load(writeConfig(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HostDataRoot != "/override/data" {
		t.Errorf("host data root override = %q", cfg.HostDataRoot)
	}
	if cfg.Network != "override-net" {
		t.Errorf("network override = %q", cfg.Network)
	}
}

func TestModelAPIKeyFromEnv(t *testing.T) {
	t.Setenv("TOK_ALPHA", "x")
	t.Setenv("DEEPSEEK_API_KEY", "sk-deepseek-123")
	body := `
hostDataRoot: "/d"
network: "n"
agents:
  alpha:
    serviceName: "picoclaw-alpha"
    token: { env: "TOK_ALPHA" }
    template: "alpha"
    mode: "continuous"
    model:
      provider: "deepseek"
      name: "deepseek-chat"
      apiKeyEnv: "DEEPSEEK_API_KEY"
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m := cfg.Agents["alpha"].Model
	if m == nil {
		t.Fatal("model not parsed")
	}
	if m.Provider != "deepseek" || m.Name != "deepseek-chat" {
		t.Errorf("provider/name = %q/%q", m.Provider, m.Name)
	}
	if m.APIKey != "sk-deepseek-123" {
		t.Errorf("APIKey not resolved from env: %q", m.APIKey)
	}
}

func TestModelRequiresProviderAndName(t *testing.T) {
	t.Setenv("TOK_ALPHA", "x")
	body := `
hostDataRoot: "/d"
network: "n"
agents:
  alpha:
    serviceName: "picoclaw-alpha"
    token: { env: "TOK_ALPHA" }
    template: "alpha"
    mode: "continuous"
    model:
      apiKeyEnv: "DEEPSEEK_API_KEY"
`
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Error("model without provider/name must error")
	}
}

func TestLayoutBuilders(t *testing.T) {
	root := "/data"
	if got, want := TemplatesDir(root, "alpha"), "/data/templates/alpha"; got != want {
		t.Errorf("TemplatesDir = %q, want %q", got, want)
	}
	if got, want := SubscriptionRoot(root, "t1", "s1"),
		"/data/tenants/t1/subscriptions/s1/agents"; got != want {
		t.Errorf("SubscriptionRoot = %q, want %q", got, want)
	}
	if got, want := UserWorkspace(root, "t1", "s1", "alpha", "u1"),
		"/data/tenants/t1/subscriptions/s1/agents/alpha/users/u1"; got != want {
		t.Errorf("UserWorkspace = %q, want %q", got, want)
	}
	if got, want := SessionsDir(root, "t1", "s1", "alpha", "u1"),
		"/data/tenants/t1/subscriptions/s1/agents/alpha/users/u1/workspace/sessions"; got != want {
		t.Errorf("SessionsDir = %q, want %q", got, want)
	}
	// Host and container roots build the same relative tree; only the prefix differs.
	if got, want := UserWorkspace("/host/data", "t1", "s1", "alpha", "u1"),
		"/host/data/tenants/t1/subscriptions/s1/agents/alpha/users/u1"; got != want {
		t.Errorf("UserWorkspace(host) = %q, want %q", got, want)
	}
}

func TestLayoutBuildersSanitizeSegments(t *testing.T) {
	// A dynamic segment with a path separator must not escape the tree.
	got := UserWorkspace("/data", "t/../evil", "s1", "alpha", "u1")
	want := "/data/tenants/t-..-evil/subscriptions/s1/agents/alpha/users/u1"
	if got != want {
		t.Errorf("UserWorkspace with unsafe tenant = %q, want %q", got, want)
	}
}

func TestWebhookSecretFromEnv(t *testing.T) {
	t.Setenv("TOK_ALPHA", "x")
	t.Setenv("CRAB_WEBHOOK_SECRET", "wh-secret-123")
	body := sample + "webhookSecret: { env: \"CRAB_WEBHOOK_SECRET\" }\n"
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ResolvedWebhookSecret != "wh-secret-123" {
		t.Errorf("webhookSecret not resolved from env: %q", cfg.ResolvedWebhookSecret)
	}
}

func TestStoreDir(t *testing.T) {
	if got, want := StoreDir("/data", "u1", "alpha"), "/data/user-secrets/u1/alpha"; got != want {
		t.Errorf("StoreDir = %q, want %q", got, want)
	}
	// Host and container roots build the same relative tree under user-secrets.
	if got, want := StoreDir("/host/data", "u1", "alpha"),
		"/host/data/user-secrets/u1/alpha"; got != want {
		t.Errorf("StoreDir(host) = %q, want %q", got, want)
	}
	// Both dynamic segments are sanitized so neither can escape the store tree.
	if got, want := StoreDir("/data", "u/../evil", "al/pha"),
		"/data/user-secrets/u-..-evil/al-pha"; got != want {
		t.Errorf("StoreDir sanitize = %q, want %q", got, want)
	}
}

func TestWorkspaceSeedAllowlist(t *testing.T) {
	want := []string{"AGENT.md", "SOUL.md", "USER.md", "memory/", "skills/"}
	if len(WorkspaceSeed) != len(want) {
		t.Fatalf("WorkspaceSeed = %v, want %v", WorkspaceSeed, want)
	}
	for i := range want {
		if WorkspaceSeed[i] != want[i] {
			t.Errorf("WorkspaceSeed[%d] = %q, want %q", i, WorkspaceSeed[i], want[i])
		}
	}
	// The isolation invariant: runtime state / history is NEVER in the allowlist.
	for _, e := range WorkspaceSeed {
		if e == "sessions/" || e == "logs/" || e == ".picoclaw.pid" {
			t.Errorf("WorkspaceSeed must never contain %q", e)
		}
	}
}

func TestWebhookSecretUnsetIsEmpty(t *testing.T) {
	t.Setenv("TOK_ALPHA", "x")
	cfg, err := Load(writeConfig(t, sample))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ResolvedWebhookSecret != "" {
		t.Errorf("webhookSecret should be empty when unset, got %q", cfg.ResolvedWebhookSecret)
	}
}
