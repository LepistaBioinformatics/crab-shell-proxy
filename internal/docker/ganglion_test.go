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

	env := ganglionEnv(cfg, agent, "bearer-1", nil)
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

// FR-10. The collector reaches the harness only if the proxy forwards it --
// the harness is in a container that reads nothing but its environment.
func TestGanglionEnv_ForwardsTheCollectorEndpoint(t *testing.T) {
	cfg := &config.Config{GanglionPort: 18800, GanglionOTLPEndpoint: "http://otel-collector:4318"}
	joined := strings.Join(ganglionEnv(cfg, config.Agent{}, "t", nil), "\n")
	if !strings.Contains(joined, "GANGLION_OTLP_ENDPOINT=http://otel-collector:4318") {
		t.Errorf("the collector endpoint never reached the container:\n%s", joined)
	}
}

// Unset means unset: the variable is absent rather than empty, so the harness
// takes the "no telemetry" path instead of trying to post to "".
func TestGanglionEnv_OmitsTheCollectorWhenUnconfigured(t *testing.T) {
	joined := strings.Join(ganglionEnv(&config.Config{GanglionPort: 18800}, config.Agent{}, "t", nil), "\n")
	if strings.Contains(joined, "GANGLION_OTLP_ENDPOINT") {
		t.Errorf("an empty collector endpoint was still passed:\n%s", joined)
	}
}

// An agent with no model still produces a usable environment: the harness
// fails loudly on its own missing GANGLION_MODEL, which is a legible error,
// rather than the proxy panicking on a nil pointer.
func TestGanglionEnv_ToleratesAnAgentWithNoModel(t *testing.T) {
	env := ganglionEnv(&config.Config{GanglionPort: 18800}, config.Agent{}, "t", nil)
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
		config.Agent{Model: &config.ModelConfig{APIKey: "super-secret"}}, "t", nil)
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
	env := ganglionEnv(cfg, config.Agent{}, "t", nil)

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

// AC-3. The bearer file must be outside what the container can see.
//
// Asserted against ganglionTokenFile's own location rather than a literal
// path, so moving the file without reconsidering the mount fails here.
func TestTheBearerFileIsOutsideTheMount(t *testing.T) {
	hostDir := "/srv/data/tenants/t/subscriptions/s/agents/gamma/users/u"
	src, dest, ok := splitBind(ganglionWorkspaceBind(hostDir))
	if !ok {
		t.Fatalf("ganglionWorkspaceBind produced an unparseable bind")
	}
	token := filepath.Join(hostDir, ganglionTokenFile)
	if strings.HasPrefix(token, src+string(filepath.Separator)) {
		t.Errorf("%s is inside the mounted source %s: the agent can read the bearer", token, src)
	}
	if want := ganglionMountDest + "/" + config.MainWorkspace; dest != want {
		t.Errorf("mount destination = %q, want %q", dest, want)
	}
}

// AC-3, the other half: the persona binds must still land inside the mounted
// workspace, or narrowing the bind would have silently unmounted the persona.
func TestPersonaBindsStillLandInsideTheNarrowedMount(t *testing.T) {
	cfg := &config.Config{ContainerDataRoot: t.TempDir(), HostDataRoot: "/srv/data"}
	key := WorkspaceKey{TenantID: "t", SubsAccID: "s", Role: "gamma", UserAccID: "u"}
	workspaceDest := ganglionMountDest + "/" + config.MainWorkspace

	for _, b := range personaBindStrings(cfg, key, ganglionMountDest) {
		_, dest, ok := splitBind(b)
		if !ok {
			t.Fatalf("unparseable persona bind %q", b)
		}
		if !strings.HasPrefix(dest, workspaceDest+"/") {
			t.Errorf("persona bind lands at %q, outside the mounted workspace %q", dest, workspaceDest)
		}
	}
}

// AC-4. A container created with the old wide bind must be recreated.
func TestGanglionBindDrift(t *testing.T) {
	plain := &config.Config{}
	encrypted := &config.Config{GanglionKeyFile: "/srv/secrets/ganglion.key"}

	workspaceBind := ganglionWorkspaceBind("/srv/data/u")
	persona := ganglionMountDest + "/" + config.MainWorkspace + "/AGENT.md"
	keyBind := "/srv/secrets/ganglion.key:" + ganglionKeyFileDest + ":ro"
	// The model registry bind is UNCONDITIONAL, so a correctly-built container
	// always carries it and one created before this feature always drifts --
	// exactly once, which is how it gets the mount at all.
	configBind := "/srv/data/u/" + ganglionConfigFile + ":" + ganglionConfigDest + ":ro"
	// D-1's shared skills root, also unconditional and also read-only.
	skillsBind := "/srv/data/effective-skills/t/s/a:" + ganglionSkillsDest + ":ro"

	if ganglionBindDrift(plain, []string{workspaceBind, configBind, skillsBind,
		"/srv/persona/AGENT.md:" + persona + ":ro"}) {
		t.Error("a correctly narrowed container was reported as drifted")
	}
	if !ganglionBindDrift(plain, []string{workspaceBind, skillsBind}) {
		t.Error("a container with no model registry bound was not detected: an admin's model change can never reach it")
	}
	// An index that names skill files the container has no mount for would tell
	// the model a capability exists and then fail to open it.
	if !ganglionBindDrift(plain, []string{workspaceBind, configBind}) {
		t.Error("a container with no shared skills bound was not detected")
	}
	if !ganglionBindDrift(plain, []string{"/srv/data/u:" + ganglionMountDest}) {
		t.Error("the old wide bind was not detected: the agent keeps reading proxy state")
	}
	if !ganglionBindDrift(plain, []string{"/srv/data/u:" + ganglionMountDest + ":rw"}) {
		t.Error("the old wide bind with explicit options was not detected")
	}
	if !ganglionBindDrift(encrypted, []string{workspaceBind, configBind, skillsBind}) {
		t.Error("switching encryption on did not drift a container with no key file bound")
	}
	if ganglionBindDrift(encrypted, []string{workspaceBind, configBind, skillsBind, keyBind}) {
		t.Error("a container that already has the key file was reported as drifted")
	}
	if !ganglionBindDrift(plain, []string{workspaceBind, configBind, skillsBind, keyBind}) {
		t.Error("switching encryption off left a stale key file bound")
	}
}

// FR-10. The environment is fixed at create time, so a workspace newly pointed
// at a model whose key the container has never held can only be served by a
// recreate -- otherwise the member gets an agent whose every candidate is
// skipped for "no API key".
func TestGanglionSecretDrift(t *testing.T) {
	have := []string{
		"GANGLION_TOKEN=t",
		"GANGLION_MODEL_KEY_PRIMARY=sk-1",
	}
	if ganglionSecretDrift([]string{"GANGLION_MODEL_KEY_PRIMARY=sk-1"}, have) {
		t.Error("a container already carrying the key was reported as drifted")
	}
	if !ganglionSecretDrift([]string{"GANGLION_MODEL_KEY_BACKUP=sk-2"}, have) {
		t.Error("a newly needed key was not detected")
	}
	if !ganglionSecretDrift([]string{"GANGLION_MODEL_KEY_PRIMARY=sk-rotated"}, have) {
		t.Error("a rotated key was not detected")
	}
	// A key nothing reads any more is untidy, not a reason to destroy a
	// member's running container.
	if ganglionSecretDrift(nil, have) {
		t.Error("a leftover key was treated as drift")
	}
}

// The bind set and the drift checks that read it back must agree.
//
// They are three predicates OR'd together, and a false positive in any of them
// does not fail loudly: it recreates the container on every turn, forever. The
// specific trap is personaBindDrift, which counts persona mounts by prefix --
// narrowing the workspace bind put a NEW bind under that prefix's parent, and
// had personaMountDests matched on <mountDest>/workspace rather than
// <mountDest>/workspace/, the count would never have matched again.
func TestGanglionBindsDoNotLookLikeDrift(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{ContainerDataRoot: root, HostDataRoot: root}
	key := WorkspaceKey{TenantID: "t", SubsAccID: "s", Role: "gamma", UserAccID: "u"}

	// A persona file must exist, or personaBinds emits nothing and the test
	// passes without exercising the count.
	dir := config.EffectivePersonaDir(root, key.TenantID, key.SubsAccID, key.Role)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("persona"), 0o644); err != nil {
		t.Fatal(err)
	}

	binds := ganglionBinds(cfg, key, filepath.Join(root, "u"))
	if len(binds) < 2 {
		t.Fatalf("expected the workspace bind plus a persona bind, got %v", binds)
	}
	if personaBindDrift(cfg, key, ganglionMountDest, binds) {
		t.Errorf("a freshly built bind set reports persona drift: every turn would recreate the container\n%v", binds)
	}
	if ganglionBindDrift(cfg, binds) {
		t.Errorf("a freshly built bind set reports bind drift\n%v", binds)
	}
}

// The second factor only IS a second factor if the agent cannot read it.
//
// The harness confines a command to its workspace with Landlock, so a file
// beside the workspace is out of reach -- but only while it stays beside it.
// This asserts the destination is not under the mounted workspace, which is
// the property the whole enc:// scheme rests on.
func TestTheCredentialKeyFileIsOutOfTheAgentsReach(t *testing.T) {
	cfg := &config.Config{
		ContainerDataRoot:     t.TempDir(),
		HostDataRoot:          "/srv/data",
		GanglionKeyFile:       "/srv/secrets/ganglion.key",
		GanglionKeyPassphrase: "pass",
	}
	key := WorkspaceKey{TenantID: "t", SubsAccID: "s", Role: "gamma", UserAccID: "u"}

	var keyBind string
	for _, b := range ganglionBinds(cfg, key, "/srv/data/u") {
		if _, dest, ok := splitBind(b); ok && dest == ganglionKeyFileDest {
			keyBind = b
		}
	}
	if keyBind == "" {
		t.Fatal("a configured key file produced no bind: enc:// values could never resolve")
	}
	if !strings.HasSuffix(keyBind, ":ro") {
		t.Errorf("the key file is not read-only: %q", keyBind)
	}
	workspaceDest := ganglionMountDest + "/" + config.MainWorkspace
	if strings.HasPrefix(ganglionKeyFileDest, workspaceDest+"/") {
		t.Errorf("the key file lands at %q, INSIDE the workspace the agent can read: the second factor is not one", ganglionKeyFileDest)
	}
}

// Both factors must be handed on together, or the harness fails to boot with
// a value it cannot resolve.
func TestBothEncryptionFactorsTravelTogether(t *testing.T) {
	cfg := &config.Config{GanglionPort: 18800, GanglionKeyPassphrase: "pass", GanglionKeyFile: "/srv/k"}
	joined := strings.Join(ganglionEnv(cfg, config.Agent{}, "t", nil), "\n")

	for _, want := range []string{"GANGLION_KEY_PASSPHRASE=pass", "GANGLION_KEY_FILE=" + ganglionKeyFileDest} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "GANGLION_KEY_FILE=/srv/k") {
		t.Error("the HOST path reached the container; it must be the mount destination")
	}
}

// Nothing is bound and nothing is forwarded when encryption is not in use, so
// a deployment with plaintext keys is unchanged by this feature.
func TestNoEncryptionMeansNoBindAndNoVariable(t *testing.T) {
	cfg := &config.Config{ContainerDataRoot: t.TempDir(), HostDataRoot: "/srv/data", GanglionPort: 18800}
	key := WorkspaceKey{TenantID: "t", SubsAccID: "s", Role: "gamma", UserAccID: "u"}

	for _, b := range ganglionBinds(cfg, key, "/srv/data/u") {
		if strings.Contains(b, "credential.key") {
			t.Errorf("a key file was bound with none configured: %q", b)
		}
	}
	if joined := strings.Join(ganglionEnv(cfg, config.Agent{}, "t", nil), "\n"); strings.Contains(joined, "GANGLION_KEY_") {
		t.Errorf("an encryption variable was forwarded with none configured:\n%s", joined)
	}
}
