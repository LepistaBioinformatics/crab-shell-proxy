package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// A returning user must get the same bearer. A recreate that rotated it would
// invalidate a live session for no reason -- the Hermes work persisted its
// bearer for exactly this and the note is worth honouring.
func TestProvisionGanglion_ReusesAnExistingToken(t *testing.T) {
	dir := t.TempDir()

	first, err := provisionGanglion(dir, "")
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if first == "" {
		t.Fatal("no token generated")
	}
	second, err := provisionGanglion(dir, "")
	if err != nil {
		t.Fatalf("re-provision: %v", err)
	}
	if first != second {
		t.Errorf("token rotated on re-provision: %q -> %q", first, second)
	}
}

// A corrupt state file costs one bearer, not a member who cannot start a
// container until someone deletes a file by hand.
func TestProvisionGanglion_ReplacesACorruptStateFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ganglionTokenFile), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := provisionGanglion(dir, "")
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if tok == "" {
		t.Error("a corrupt state file produced no token")
	}
}

func TestProvisionGanglion_CreatesTheWorkspace(t *testing.T) {
	dir := t.TempDir()
	if _, err := provisionGanglion(dir, ""); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "workspace")); err != nil || !fi.IsDir() {
		t.Errorf("workspace not created: %v", err)
	}
}

// The environment IS the harness's configuration contract. A typo in a name
// here fails as "the container would not boot", with nothing naming the cause,
// so the names are pinned.
func TestGanglionEnv_CarriesTheConfigurationContract(t *testing.T) {
	cfg := &config.Config{GanglionPort: 18800}
	agent := config.Agent{Model: &config.ModelConfig{
		Name:    "deepseek-chat",
		BaseURL: "https://api.deepseek.com/v1",
		APIKey:  "secret",
	}}

	env := ganglionEnv(cfg, agent, "bearer-1")
	joined := strings.Join(env, "\n")

	for _, want := range []string{
		"GANGLION_ADDR=0.0.0.0:18800",
		"GANGLION_DATA_DIR=/data/.ganglion",
		"GANGLION_TOKEN=bearer-1",
		"GANGLION_MODEL=deepseek-chat",
		"GANGLION_BASE_URL=https://api.deepseek.com/v1",
		"GANGLION_API_KEY=secret",
		"GANGLION_SYSTEM_FILE=/data/.ganglion/workspace/AGENT.md",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
}

// An agent with no model still produces a usable environment: the harness
// fails loudly on its own missing GANGLION_MODEL, which is a legible error,
// rather than the proxy panicking on a nil pointer.
func TestGanglionEnv_ToleratesAnAgentWithNoModel(t *testing.T) {
	env := ganglionEnv(&config.Config{GanglionPort: 18800}, config.Agent{}, "t")
	if len(env) == 0 {
		t.Fatal("no environment produced")
	}
	for _, e := range env {
		if strings.HasPrefix(e, "GANGLION_MODEL=") {
			t.Errorf("a model was invented for an agent that declares none: %q", e)
		}
	}
}

// The API key must never become a container label: labels are readable by
// anything that can list containers, including harness-sphere.
func TestGanglionEnv_KeyIsEnvironmentNotLabel(t *testing.T) {
	env := ganglionEnv(&config.Config{GanglionPort: 18800},
		config.Agent{Model: &config.ModelConfig{APIKey: "super-secret"}}, "t")
	found := false
	for _, e := range env {
		if e == "GANGLION_API_KEY=super-secret" {
			found = true
		}
	}
	if !found {
		t.Error("the API key did not reach the environment")
	}
}

// The persona cascade must reach a ganglion container.
//
// It did not, and the harness said so on every boot: "persona file
// /data/.ganglion/workspace/AGENT.md unreadable ... no such file or
// directory". GANGLION_SYSTEM_FILE was wired to a path nothing created, so
// every member's agent ran with no identity while the env var claimed
// otherwise.
func TestGanglionEnvAndPersonaAgree(t *testing.T) {
	cfg := &config.Config{GanglionPort: 18800}
	env := ganglionEnv(cfg, config.Agent{}, "t")

	var systemFile string
	for _, e := range env {
		if strings.HasPrefix(e, "GANGLION_SYSTEM_FILE=") {
			systemFile = strings.TrimPrefix(e, "GANGLION_SYSTEM_FILE=")
		}
	}
	if systemFile == "" {
		t.Fatal("GANGLION_SYSTEM_FILE is not set")
	}

	// personaBinds lands each file at <mountDest>/workspace/<name>. The env
	// var must name a path in that set, or it points at nothing again.
	want := ganglionMountDest + "/workspace/AGENT.md"
	if systemFile != want {
		t.Errorf("GANGLION_SYSTEM_FILE = %q, but the persona cascade mounts at %q", systemFile, want)
	}
}

// A persona bind whose file exists must be produced for the ganglion mount
// destination, not only for picoclaw's.
func TestPersonaBindsCoverTheGanglionMountDest(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{ContainerDataRoot: root, HostDataRoot: root}
	key := WorkspaceKey{TenantID: "t", SubsAccID: "s", Role: "gamma", UserAccID: "u"}

	dir := config.EffectivePersonaDir(root, key.TenantID, key.SubsAccID, key.Role)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("sou a eva"), 0o644); err != nil {
		t.Fatal(err)
	}

	binds := personaBindStrings(cfg, key, ganglionMountDest)
	found := false
	for _, b := range binds {
		if strings.Contains(b, ganglionMountDest+"/workspace/AGENT.md:ro") {
			found = true
		}
	}
	if !found {
		t.Errorf("AGENT.md is not bound into the ganglion mount: %v", binds)
	}
}
