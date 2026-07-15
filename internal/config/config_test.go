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

func TestSessionsDir(t *testing.T) {
	c := &Config{ContainerDataRoot: "/data/agents"}
	want := "/data/agents/alpha/hash/workspace/sessions"
	if got := c.SessionsDir("alpha", "hash"); got != want {
		t.Errorf("SessionsDir = %q, want %q", got, want)
	}
}
