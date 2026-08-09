package config

import (
	"os"
	"path/filepath"
	"strings"
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
	if cfg.PicoclawPort != 18790 || cfg.ContainerPrefix != "crabshell" {
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
	if got, want := SessionsDir(root, "t1", "s1", "alpha", "u1", MainWorkspace),
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
	want := []string{"USER.md", "memory/", "skills/"}
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
	// The identity files are DELIVERED BY MOUNT, not copied — copying them would
	// leave a writable duplicate the read-only bind merely hides, which resurfaces
	// the moment the mount goes away.
	for _, e := range WorkspaceSeed {
		if e == "AGENT.md" || e == "SOUL.md" || e == "HEARTBEAT.md" {
			t.Errorf("WorkspaceSeed must not copy the read-only identity file %q", e)
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

const modelsSample = `
listen: ":9000"
hostDataRoot: "/host/data"
network: "net"
agents:
  alpha:
    serviceName: "picoclaw-alpha"
    token: { env: "TOK_ALPHA" }
    template: "alpha"
    mode: "continuous"
    model:
      provider: "deepseek"
      name: "deepseek-chat"
      apiKeyEnv: "DEEPSEEK_KEY"
    models:
      - provider: "openai"
        name: "gpt-4o"
        apiKeyEnv: "OPENAI_KEY"
      - provider: "anthropic"
        name: "claude-sonnet"
        apiKeyEnv: "ANTHROPIC_KEY"
`

func TestModelsAPIKeyResolvedAtLoad(t *testing.T) {
	t.Setenv("TOK_ALPHA", "x")
	t.Setenv("DEEPSEEK_KEY", "sk-deepseek")
	t.Setenv("OPENAI_KEY", "sk-openai")
	t.Setenv("ANTHROPIC_KEY", "sk-anthropic")
	cfg, err := Load(writeConfig(t, modelsSample))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := cfg.Agents["alpha"]
	if a.Model.APIKey != "sk-deepseek" {
		t.Errorf("default model apiKey = %q", a.Model.APIKey)
	}
	if len(a.Models) != 2 {
		t.Fatalf("models = %v", a.Models)
	}
	if a.Models[0].APIKey != "sk-openai" || a.Models[1].APIKey != "sk-anthropic" {
		t.Errorf("models apiKeys = %q, %q", a.Models[0].APIKey, a.Models[1].APIKey)
	}
}

func TestModelsRequireProviderAndName(t *testing.T) {
	t.Setenv("TOK_ALPHA", "x")
	bad := `
listen: ":9000"
hostDataRoot: "/host/data"
network: "net"
agents:
  alpha:
    serviceName: "picoclaw-alpha"
    token: { env: "TOK_ALPHA" }
    template: "alpha"
    mode: "continuous"
    models:
      - provider: "openai"
        apiKeyEnv: "OPENAI_KEY"
`
	if _, err := Load(writeConfig(t, bad)); err == nil {
		t.Fatal("expected error for models entry missing name")
	}
}

func TestModelsRejectsDuplicateProviderName(t *testing.T) {
	t.Setenv("TOK_ALPHA", "x")
	dup := `
listen: ":9000"
hostDataRoot: "/host/data"
network: "net"
agents:
  alpha:
    serviceName: "picoclaw-alpha"
    token: { env: "TOK_ALPHA" }
    template: "alpha"
    mode: "continuous"
    models:
      - provider: "openai"
        name: "gpt-4o"
        apiKeyEnv: "OPENAI_KEY"
      - provider: "openai"
        name: "gpt-4o"
        apiKeyEnv: "OPENAI_KEY2"
`
	if _, err := Load(writeConfig(t, dup)); err == nil {
		t.Fatal("expected error for duplicate {provider,name} in models")
	}
}

func TestSelectableModelsDedupAndOrder(t *testing.T) {
	t.Setenv("TOK_ALPHA", "x")
	t.Setenv("DEEPSEEK_KEY", "sk-deepseek")
	t.Setenv("OPENAI_KEY", "sk-openai")
	t.Setenv("ANTHROPIC_KEY", "sk-anthropic")
	cfg, err := Load(writeConfig(t, modelsSample))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := cfg.Agents["alpha"]
	sel := a.SelectableModels()
	if len(sel) != 3 {
		t.Fatalf("SelectableModels = %d entries, want 3: %+v", len(sel), sel)
	}
	if sel[0].Provider != "deepseek" || sel[1].Provider != "openai" || sel[2].Provider != "anthropic" {
		t.Errorf("SelectableModels order = %v", sel)
	}

	// Dedup: an agent whose Models happens to repeat the default Model
	// {provider,name} keeps only one entry.
	dup := Agent{
		Model: &ModelConfig{Provider: "deepseek", Name: "deepseek-chat"},
		Models: []*ModelConfig{
			{Provider: "deepseek", Name: "deepseek-chat"},
			{Provider: "openai", Name: "gpt-4o"},
		},
	}
	dsel := dup.SelectableModels()
	if len(dsel) != 2 {
		t.Fatalf("dedup SelectableModels = %d entries, want 2: %+v", len(dsel), dsel)
	}
}

func TestFindModelHitAndMiss(t *testing.T) {
	t.Setenv("TOK_ALPHA", "x")
	t.Setenv("DEEPSEEK_KEY", "sk-deepseek")
	t.Setenv("OPENAI_KEY", "sk-openai")
	t.Setenv("ANTHROPIC_KEY", "sk-anthropic")
	cfg, err := Load(writeConfig(t, modelsSample))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := cfg.Agents["alpha"]
	if mc := a.FindModel("openai", "gpt-4o"); mc == nil || mc.APIKey != "sk-openai" {
		t.Errorf("FindModel hit = %v", mc)
	}
	if mc := a.FindModel("openai", "gpt-3.5"); mc != nil {
		t.Errorf("FindModel miss should be nil, got %v", mc)
	}
}

func TestModelOverrideFilePaths(t *testing.T) {
	if got, want := TenantModelOverrideFile("/data", "t1"),
		"/data/tenants/t1/shared/model.json"; got != want {
		t.Errorf("TenantModelOverrideFile = %q, want %q", got, want)
	}
	if got, want := SubscriptionModelOverrideFile("/data", "t1", "s1"),
		"/data/tenants/t1/subscriptions/s1/shared/model.json"; got != want {
		t.Errorf("SubscriptionModelOverrideFile = %q, want %q", got, want)
	}
	if got, want := UserModelOverrideFile("/data", "t1", "s1", "alpha", "u1"),
		"/data/tenants/t1/subscriptions/s1/agents/alpha/users/u1/.crab-model.json"; got != want {
		t.Errorf("UserModelOverrideFile = %q, want %q", got, want)
	}
}

// harnessSample declares one agent with the harness field omitted and one that
// names it explicitly, so both accepted spellings are exercised by one Load.
const harnessSample = `
listen: ":9000"
hostDataRoot: "/host/data"
network: "net"
agents:
  alpha:
    serviceName: "picoclaw-alpha"
    token: { env: "TOK_ALPHA" }
    template: "alpha"
    mode: "continuous"
  beta:
    harness: picoclaw
    serviceName: "picoclaw-beta"
    token: { env: "TOK_BETA" }
    template: "beta"
    mode: "continuous"
`

// TestHarnessAcceptsPicoclawAndDefaultsWhenOmitted pins the two legal spellings.
// An omitted harness must keep defaulting to picoclaw: every shipped agent block
// omits it, so requiring it would break all of them.
func TestHarnessAcceptsPicoclawAndDefaultsWhenOmitted(t *testing.T) {
	t.Setenv("TOK_ALPHA", "x")
	t.Setenv("TOK_BETA", "y")
	cfg, err := Load(writeConfig(t, harnessSample))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, key := range []string{"alpha", "beta"} {
		a, ok := cfg.Agents[key]
		if !ok {
			t.Fatalf("agent %q should be registered", key)
		}
		if a.Harness != HarnessPicoclaw {
			t.Errorf("agent %q harness = %q, want %q", key, a.Harness, HarnessPicoclaw)
		}
	}
}

// TestUnknownHarnessIsRejected pins the removal of the hermes harness: a stale
// operator config naming a runtime this proxy no longer orchestrates must fail
// loudly at Load, not boot into a code path that is gone. Silently defaulting it
// to picoclaw would hand a user a picoclaw container under a role provisioned for
// something else.
func TestUnknownHarnessIsRejected(t *testing.T) {
	t.Setenv("TOK_ALPHA", "x")
	t.Setenv("TOK_BETA", "y")
	stale := strings.Replace(harnessSample, "harness: picoclaw", "harness: hermes", 1)
	_, err := Load(writeConfig(t, stale))
	if err == nil {
		t.Fatal("Load should reject an unknown harness value")
	}
	if !strings.Contains(err.Error(), HarnessPicoclaw) {
		t.Errorf("error should name the accepted value %q, got: %v", HarnessPicoclaw, err)
	}
	if !strings.Contains(err.Error(), "hermes") {
		t.Errorf("error should echo the rejected value, got: %v", err)
	}
}

// TestPathComponentsCannotEscapeTheRoot pins the invariant every workspace path in
// this service rests on, and that CodeQL cannot see.
//
// `go/path-injection` flags UserWorkspace's callers as "uncontrolled data used in a
// path expression" because it recognizes neither of the two things that make them
// safe: `uuid.Parse(...).String()` at the HTTP boundary, and identity.SanitizeID here.
// The alert is a false positive, but "it is sanitized" is a claim, and this is the
// evidence — so the claim keeps being true after someone edits SanitizeID's regex.
//
// The property: whatever a component contains, the result is still under root and the
// component is still ONE path segment. A traversal needs a separator, and SanitizeID
// replaces every byte outside [a-zA-Z0-9._-] with "-", so no separator can survive.
// Pure-dot values are trimmed to empty and fall back to a hash.
func TestPathComponentsCannotEscapeTheRoot(t *testing.T) {
	const root = "/data"
	// A well-formed path has exactly these separators:
	// /data/tenants/<t>/subscriptions/<s>/agents/<role>/users/<u>
	const wantSeparators = 9

	hostile := []string{
		"../../etc/passwd",
		"..",
		"../..",
		"....//....//etc",
		"/etc/passwd",
		"a/../../b",
		"%2e%2e%2f",
		`..\..\windows`,
		".",
		"./.",
		"\x00/etc/passwd",
		"foo/bar",
		strings.Repeat("../", 40) + "etc",
	}

	for _, bad := range hostile {
		paths := map[string]string{
			"tenant":       UserWorkspace(root, bad, "s", "alpha", "u"),
			"subscription": UserWorkspace(root, "t", bad, "alpha", "u"),
			"role":         UserWorkspace(root, "t", "s", bad, "u"),
			"user":         UserWorkspace(root, "t", "s", "alpha", bad),
		}
		for component, got := range paths {
			clean := filepath.Clean(got)
			if !strings.HasPrefix(clean, root+"/") {
				t.Errorf("%s = %q escaped the root: %q", component, bad, clean)
			}
			if n := strings.Count(clean, "/"); n != wantSeparators {
				t.Errorf("%s = %q became more than one segment: %q (%d separators, want %d)",
					component, bad, clean, n, wantSeparators)
			}
		}
	}
}

// --- memory-graph-mcp: mcpTokenSecret / mcpBaseURL ---

const mcpSample = `
hostDataRoot: "/host/data"
network: "net"
mcpTokenSecret: { env: "TEST_MCP_SECRET" }
agents:
  alpha:
    serviceName: "picoclaw-alpha"
    token: "inline"
    template: "alpha"
    mode: "continuous"
`

func TestLoadResolvesMCPTokenSecretFromTheEnvironment(t *testing.T) {
	t.Setenv("TEST_MCP_SECRET", "signing-value")
	cfg, err := Load(writeConfig(t, mcpSample))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ResolvedMCPTokenSecret != "signing-value" {
		t.Errorf("ResolvedMCPTokenSecret = %q, want %q", cfg.ResolvedMCPTokenSecret, "signing-value")
	}
	// The secret must never be readable from the parsed file structure itself —
	// only the env name is declared there.
	if cfg.MCPTokenSecret.Value != "" {
		t.Errorf("MCPTokenSecret.Value = %q; the secret must live only in the environment", cfg.MCPTokenSecret.Value)
	}
	if cfg.MCPTokenSecret.Env != "TEST_MCP_SECRET" {
		t.Errorf("MCPTokenSecret.Env = %q, want TEST_MCP_SECRET", cfg.MCPTokenSecret.Env)
	}
}

// FR-4.5: unlike webhookSecret, an unset MCP secret must NOT fail the load. A
// deployment that has not configured memory still has to boot and chat.
func TestLoadToleratesAnUnsetMCPTokenSecret(t *testing.T) {
	t.Setenv("TEST_MCP_SECRET", "")
	cfg, err := Load(writeConfig(t, mcpSample))
	if err != nil {
		t.Fatalf("Load with an unset MCP secret failed: %v — it must disable the feature, not the proxy", err)
	}
	if cfg.ResolvedMCPTokenSecret != "" {
		t.Errorf("ResolvedMCPTokenSecret = %q, want empty", cfg.ResolvedMCPTokenSecret)
	}
}

func TestLoadToleratesNoMCPTokenSecretFieldAtAll(t *testing.T) {
	// `sample` predates this feature and declares no mcpTokenSecret at all, which
	// is exactly the case an existing deployment's config.yaml is in.
	t.Setenv("TOK_ALPHA", "resolved-alpha")
	cfg, err := Load(writeConfig(t, sample))
	if err != nil {
		t.Fatalf("Load of a config with no mcpTokenSecret field failed: %v", err)
	}
	if cfg.ResolvedMCPTokenSecret != "" {
		t.Errorf("ResolvedMCPTokenSecret = %q, want empty when the field is absent", cfg.ResolvedMCPTokenSecret)
	}
}

func TestMCPBaseURLDefaultAndOverrides(t *testing.T) {
	t.Run("defaults to the compose service name", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, mcpSample))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.MCPBaseURL != "http://crab-shell-proxy:8080" {
			t.Errorf("MCPBaseURL = %q, want the default", cfg.MCPBaseURL)
		}
	})

	t.Run("file value is used", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, mcpSample+"\nmcpBaseURL: \"http://from-file:1234\"\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.MCPBaseURL != "http://from-file:1234" {
			t.Errorf("MCPBaseURL = %q, want the file value", cfg.MCPBaseURL)
		}
	})

	t.Run("env wins over the file", func(t *testing.T) {
		t.Setenv("CRAB_MCP_BASE_URL", "http://from-env:9999")
		cfg, err := Load(writeConfig(t, mcpSample+"\nmcpBaseURL: \"http://from-file:1234\"\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.MCPBaseURL != "http://from-env:9999" {
			t.Errorf("MCPBaseURL = %q, want the env value to win", cfg.MCPBaseURL)
		}
	})
}
