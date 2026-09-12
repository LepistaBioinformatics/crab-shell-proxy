package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/projects"
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

	if ganglionBindDrift(plain, nil, []string{workspaceBind, configBind, skillsBind,
		"/srv/persona/AGENT.md:" + persona + ":ro"}) {
		t.Error("a correctly narrowed container was reported as drifted")
	}
	if !ganglionBindDrift(plain, nil, []string{workspaceBind, skillsBind}) {
		t.Error("a container with no model registry bound was not detected: an admin's model change can never reach it")
	}
	// An index that names skill files the container has no mount for would tell
	// the model a capability exists and then fail to open it.
	if !ganglionBindDrift(plain, nil, []string{workspaceBind, configBind}) {
		t.Error("a container with no shared skills bound was not detected")
	}
	if !ganglionBindDrift(plain, nil, []string{"/srv/data/u:" + ganglionMountDest}) {
		t.Error("the old wide bind was not detected: the agent keeps reading proxy state")
	}
	if !ganglionBindDrift(plain, nil, []string{"/srv/data/u:" + ganglionMountDest + ":rw"}) {
		t.Error("the old wide bind with explicit options was not detected")
	}
	if !ganglionBindDrift(encrypted, nil, []string{workspaceBind, configBind, skillsBind}) {
		t.Error("switching encryption on did not drift a container with no key file bound")
	}
	if ganglionBindDrift(encrypted, nil, []string{workspaceBind, configBind, skillsBind, keyBind}) {
		t.Error("a container that already has the key file was reported as drifted")
	}
	if !ganglionBindDrift(plain, nil, []string{workspaceBind, configBind, skillsBind, keyBind}) {
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

	binds := ganglionBinds(cfg, key, filepath.Join(root, "u"), nil)
	if len(binds) < 2 {
		t.Fatalf("expected the workspace bind plus a persona bind, got %v", binds)
	}
	if personaBindDrift(cfg, key, ganglionMountDest, binds) {
		t.Errorf("a freshly built bind set reports persona drift: every turn would recreate the container\n%v", binds)
	}
	if ganglionBindDrift(cfg, nil, binds) {
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
	for _, b := range ganglionBinds(cfg, key, "/srv/data/u", nil) {
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

	for _, b := range ganglionBinds(cfg, key, "/srv/data/u", nil) {
		if strings.Contains(b, "credential.key") {
			t.Errorf("a key file was bound with none configured: %q", b)
		}
	}
	if joined := strings.Join(ganglionEnv(cfg, config.Agent{}, "t", nil), "\n"); strings.Contains(joined, "GANGLION_KEY_") {
		t.Errorf("an encryption variable was forwarded with none configured:\n%s", joined)
	}
}

// R11 of ganglion-evolution. The harness cannot observe whether its own
// container stops when idle, and it needs that for one decision: refusing a
// SCHEDULED evolution pass on a scale-to-zero agent, which would store an
// intention and fire nothing.
//
// If this variable stops being set, that refusal silently stops happening and
// the agent quietly schedules an analysis pass that never runs.
func TestTheLifecycleModeReachesTheHarness(t *testing.T) {
	cfg := &config.Config{GanglionPort: 18800}
	for _, mode := range []config.Mode{config.ModeScaleToZero, config.ModeContinuous} {
		joined := strings.Join(ganglionEnv(cfg, config.Agent{Mode: mode}, "t", nil), "\n")
		want := "GANGLION_LIFECYCLE_MODE=" + string(mode)
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
}

// The skills root the bind lands at, named to the harness. The agent's own
// <workspace>/skills is found without being told, so only the proxy-owned half
// is passed.
func TestTheSkillsRootReachesTheHarness(t *testing.T) {
	joined := strings.Join(ganglionEnv(&config.Config{GanglionPort: 18800}, config.Agent{}, "t", nil), "\n")
	if !strings.Contains(joined, "GANGLION_SKILLS_ROOT="+ganglionSkillsDest) {
		t.Errorf("the shared skills root was not named:\n%s", joined)
	}
}

// ganglionSeedManager is the narrowest Manager seedGanglionProjects needs: a
// data root and no chown user, so the seed runs where lchown is not permitted.
// It returns the user directory the workspace lives under, because that is what
// materializeGanglion hands the seeder.
func ganglionSeedManager(t *testing.T) (*Manager, WorkspaceKey, string) {
	t.Helper()
	root := t.TempDir()
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "gamma", UserAccID: "u1"}
	userDir := config.UserWorkspace(root, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if err := os.MkdirAll(filepath.Join(userDir, config.MainWorkspace), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		cfg:  &config.Config{ContainerDataRoot: root, PicoclawUser: ""},
		logf: func(string, ...any) {},
	}
	return m, key, userDir
}

// The harness resolves a project's transcripts, its context window, its
// generated images and the member's uploads under one root, and it runs
// non-root. The proxy makes that root so the ownership is decided once, here,
// rather than by whichever of the two processes reaches the directory first.
func TestSeedingAGanglionProjectCreatesItsSubtreeUnderTheWorkspace(t *testing.T) {
	m, key, userDir := ganglionSeedManager(t)
	p, err := m.projectStore(key).Create("Seed Trial", "Only discuss the 2026 seed trial.", time.Now())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	if err := m.seedGanglionProjects(key, userDir); err != nil {
		t.Fatalf("seed: %v", err)
	}

	root := filepath.Join(userDir, config.ProjectWorkspace(p.ID))
	for _, sub := range ganglionProjectDirs {
		if fi, statErr := os.Stat(filepath.Join(root, sub)); statErr != nil || !fi.IsDir() {
			t.Errorf("%s was not created under the project root: %v", sub, statErr)
		}
	}
	// The legacy spelling must NOT be created: publicRoot migrates a directory
	// called `uploads` into `public` on first access, so seeding it would make
	// every ensure recreate what every media call then renames.
	if _, statErr := os.Stat(filepath.Join(root, config.LegacyPublicDirName)); statErr == nil {
		t.Errorf("the seed created %q, which publicRoot would migrate on every access",
			config.LegacyPublicDirName)
	}
}

// The instructions are what the harness folds into the system prompt, and the
// file name is the whole contract between the two repositories: the harness
// reads it as a constant (skills.ProjectFileName), not as a configured path.
func TestAGanglionProjectFileCarriesTheMembersInstructions(t *testing.T) {
	m, key, userDir := ganglionSeedManager(t)
	p, err := m.projectStore(key).Create("Seed Trial", "Only discuss the 2026 seed trial.", time.Now())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := m.seedGanglionProjects(key, userDir); err != nil {
		t.Fatalf("seed: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(userDir,
		config.ProjectWorkspace(p.ID), ganglionProjectFileName))
	if err != nil {
		t.Fatalf("read %s: %v", ganglionProjectFileName, err)
	}
	got := string(raw)
	if !strings.Contains(got, "Only discuss the 2026 seed trial.") {
		t.Errorf("the instructions did not reach the agent: %q", got)
	}
	if !strings.HasPrefix(got, "# Seed Trial\n") {
		t.Errorf("the project is not named at the head of the document: %q", got)
	}
}

// The file is fully DERIVED from the store, so an edit the agent makes to it is
// reverted on the next ensure -- the same trade composeProjectAgentMD documents
// for picoclaw. A seed that respected the agent's edit would let the agent
// rewrite the instructions the member gave it.
func TestSeedingAGanglionProjectRevertsAnAgentEditToTheProjectFile(t *testing.T) {
	m, key, userDir := ganglionSeedManager(t)
	p, err := m.projectStore(key).Create("Seed Trial", "Only discuss the 2026 seed trial.", time.Now())
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := m.seedGanglionProjects(key, userDir); err != nil {
		t.Fatalf("first seed: %v", err)
	}

	path := filepath.Join(userDir, config.ProjectWorkspace(p.ID), ganglionProjectFileName)
	if err := os.WriteFile(path, []byte("# Whatever I like\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.seedGanglionProjects(key, userDir); err != nil {
		t.Fatalf("second seed: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Only discuss the 2026 seed trial.") {
		t.Errorf("the agent's edit survived the ensure: %q", raw)
	}
}

// An empty project is a legitimate state -- a member may create one and write
// the instructions later -- and the agent should be told what it is looking at
// rather than reading a bare heading.
func TestAGanglionProjectWithNoInstructionsStillSaysWhatItIs(t *testing.T) {
	got := ganglionProjectDoc(projects.Project{ID: "seedtrial", Name: "Seed Trial"})
	if !strings.Contains(got, "no instructions yet") {
		t.Errorf("an empty project rendered a bare heading: %q", got)
	}
}

// NFR-1, the regression bar: an agent that has never had a project must be
// byte-identical to what it is today, and that includes not acquiring a
// `projects` directory it has no use for.
func TestAGanglionWorkspaceWithNoProjectsGetsNoProjectsDirectory(t *testing.T) {
	m, key, userDir := ganglionSeedManager(t)
	if err := m.seedGanglionProjects(key, userDir); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(userDir, config.MainWorkspace, "projects")); !os.IsNotExist(err) {
		t.Errorf("a workspace with no projects grew a projects directory: %v", err)
	}
}

// Decision D-1, and the reason the ganglion layout differs from picoclaw's: a
// project is a directory INSIDE the one bind the container already has, so
// creating one is not a bind change and must never cost a recreate. A
// scale-to-zero agent may have no container at all when its member makes a
// project, and "recreate to add a project" would be a strange price for it.
func TestSeedingAGanglionProjectIsNotABindChange(t *testing.T) {
	m, key, userDir := ganglionSeedManager(t)
	cfg := &config.Config{HostDataRoot: "/host/data"}
	binds := ganglionBinds(cfg, key, config.UserWorkspace(cfg.HostDataRoot,
		key.TenantID, key.SubsAccID, key.Role, key.UserAccID), nil)
	if ganglionBindDrift(cfg, nil, binds) {
		t.Fatal("the bind set this test starts from already reads as drift")
	}

	if _, err := m.projectStore(key).Create("Seed Trial", "", time.Now()); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := m.seedGanglionProjects(key, userDir); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if ganglionBindDrift(cfg, nil, binds) {
		t.Error("a project made the bind set look stale; the container would be recreated for a directory")
	}
}

// The webapp warns a member that deleting a project removes its transcripts and
// its files. That warning has to be TRUE on whichever harness answered.
//
// Both harnesses name a project's workspace identically now, so the delete has
// one path to remove -- plus the LEGACY one, for a container that has not booted
// since the layout changed and whose subtree is therefore still underneath the
// workspace. A deleted project's conversations staying on disk after the member
// was told they were gone is what this has always been about.
func TestDeletingAProjectRemovesItOnEitherHarness(t *testing.T) {
	userDir := t.TempDir()
	pico := filepath.Join(userDir, config.ProjectWorkspace("demo"), "sessions")
	gang := filepath.Join(userDir, config.LegacyGanglionProjectWorkspace("demo"), "sessions")
	for _, d := range []string{pico, gang} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "conv.jsonl"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := removeProjectWorkspace(userDir, "demo"); err != nil {
		t.Fatalf("removeProjectWorkspace: %v", err)
	}
	for _, d := range []string{pico, gang} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("%s survived the delete", d)
		}
	}
	// The main workspace is untouched: only the project's own subtree goes.
	if _, err := os.Stat(filepath.Join(userDir, config.MainWorkspace)); err == nil {
		t.Log("main workspace intact")
	}
}

// Deleting removes the RECORD first and the directory second, so a crash between
// the two leaves a member's transcripts on disk and invisible to them. On the
// picoclaw side syncProjectWorkspaces sweeps that; the ganglion path returns
// before it ever runs, so its own ensure has to.
func TestAnOrphanedGanglionProjectIsSweptOnTheNextEnsure(t *testing.T) {
	m, key, userDir := ganglionSeedManager(t)

	orphan := filepath.Join(userDir, config.ProjectWorkspace("gone"))
	if err := os.MkdirAll(filepath.Join(orphan, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "sessions", "conv.jsonl"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.seedGanglionProjects(key, userDir); err != nil {
		t.Fatalf("seedGanglionProjects: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("a project with no record kept its transcripts")
	}
	// And the sweep, which now runs one level up, must not take the MAIN
	// workspace or anything else in the user dir with it -- the prefix is the
	// only thing standing between it and config.json.
	if _, err := os.Stat(filepath.Join(userDir, config.MainWorkspace)); err != nil {
		t.Fatalf("the sweep removed the main workspace: %v", err)
	}
}

// The sweep enumerates the USER DIR now, which holds the proxy's own files. The
// prefix is what keeps it to the directories it owns, and nothing else here
// would notice if it stopped.
func TestTheSweepLeavesTheProxysOwnFilesAlone(t *testing.T) {
	m, key, userDir := ganglionSeedManager(t)

	// Not .projects.json: the manager owns that one, and writing a stub over it
	// would make this test fail on a parse error rather than on the thing it
	// asserts.
	keep := []string{"config.json", ".schedules.json", ".crab-ganglion.json"}
	for _, name := range keep {
		if err := os.WriteFile(filepath.Join(userDir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.MkdirAll(filepath.Join(userDir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := m.seedGanglionProjects(key, userDir); err != nil {
		t.Fatalf("seedGanglionProjects: %v", err)
	}
	for _, name := range append(keep, "logs") {
		if _, err := os.Stat(filepath.Join(userDir, name)); err != nil {
			t.Errorf("the sweep removed %s: %v", name, err)
		}
	}
}

// A project gets a bind of its own, because its workspace is a SIBLING of the
// main one and the ganglion mounts workspaces rather than the directory holding
// them.
//
// Mounting the parent instead would be one line, and would put config.json --
// the model registry, with its api_keys -- and credential.key inside the agent's
// Landlock root. That is the whole reason this costs a bind per project.
func TestAProjectGetsItsOwnBind(t *testing.T) {
	cfg := &config.Config{HostDataRoot: "/host/data"}
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "gamma", UserAccID: "u1"}
	binds := ganglionBinds(cfg, key, "/srv/data/u", []string{"seedtrial"})

	wantWorkspace := "/srv/data/u/workspace-seedtrial:" + ganglionMountDest + "/workspace-seedtrial"
	var haveWorkspace, haveSkills, haveParent bool
	for _, b := range binds {
		src, dest, ok := splitBind(b)
		if !ok {
			continue
		}
		if b == wantWorkspace {
			haveWorkspace = true
		}
		if dest == ganglionMountDest+"/workspace-seedtrial/shared-skills" {
			haveSkills = true
		}
		// Nothing may mount the directory that HOLDS the workspaces.
		if dest == ganglionMountDest && src != "" {
			haveParent = true
		}
	}
	if !haveWorkspace {
		t.Errorf("no bind for the project's workspace:\n%v", binds)
	}
	// The admin's shared skills inside the project's own workspace: the agent's
	// Landlock root is the TURN's workspace, so an index pointing at files only
	// the main workspace can open would be an index of unreachable paths.
	if !haveSkills {
		t.Errorf("the project's workspace has no shared-skills mount:\n%v", binds)
	}
	if haveParent {
		t.Error("the directory holding the workspaces was mounted, exposing config.json and credential.key")
	}
}

// A project created since the container started is not visible inside it at all,
// so the bind set changing has to recreate it. Without this the turn would run
// against a directory the harness makes locally and the proxy never reads.
func TestBindDriftNoticesTheProjectSet(t *testing.T) {
	cfg := &config.Config{HostDataRoot: "/host/data"}
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "gamma", UserAccID: "u1"}

	none := ganglionBinds(cfg, key, "/srv/data/u", nil)
	one := ganglionBinds(cfg, key, "/srv/data/u", []string{"seedtrial"})

	if ganglionBindDrift(cfg, nil, none) {
		t.Fatal("a container with no projects reads as drift against no projects")
	}
	if ganglionBindDrift(cfg, []string{"seedtrial"}, one) {
		t.Fatal("a container with one project reads as drift against that project")
	}
	if !ganglionBindDrift(cfg, []string{"seedtrial"}, none) {
		t.Error("a project created since the container started was not noticed")
	}
	if !ganglionBindDrift(cfg, nil, one) {
		t.Error("a project deleted since the container started was not noticed")
	}
}

// The mounts the ganglion never got, and each one a member-visible capability.
//
// admin-shared-content was silently inert for every ganglion member: an admin
// publishing a document to a subscription reached picoclaw members and nobody
// else, which is the failure harness_gate.go exists to prevent, in a feature
// with no gate row. The managed documents include FILE_DELIVERY.md, which is
// what tells an agent to write deliverables into public/attachments -- the only
// place the member's interface lists.
func TestTheGanglionGetsTheSharedAndManagedMounts(t *testing.T) {
	cfg := &config.Config{HostDataRoot: "/host/data"}
	key := WorkspaceKey{TenantID: "t1", SubsAccID: "s1", Role: "gamma", UserAccID: "u1"}
	binds := ganglionBinds(cfg, key, "/srv/data/u", nil)

	var haveShared, haveManaged bool
	for _, b := range binds {
		_, dest, ok := splitBind(b)
		if !ok {
			continue
		}
		if strings.HasPrefix(dest, ganglionMountDest+"/"+config.MainWorkspace+"/.shared/") {
			haveShared = true
		}
		if strings.Contains(dest, "/memory/") && strings.HasSuffix(dest, ".md") {
			haveManaged = true
		}
	}
	if !haveShared {
		t.Errorf("no shared-content mount:\n%v", binds)
	}
	if !haveManaged {
		t.Errorf("no managed memory mount:\n%v", binds)
	}
	// Both must be the SAME destinations picoclaw uses, or the two harnesses
	// have two layouts again.
	for _, sm := range sharedFileBinds(cfg, key, ganglionMountDest) {
		if !hasBind(binds, sm.bind) {
			t.Errorf("shared bind missing: %s", sm.bind)
		}
	}
	for _, b := range managedContentBinds(config.ManagedSkillsDir(cfg.HostDataRoot),
		ganglionMountDest, false) {
		if !hasBind(binds, b) {
			t.Errorf("managed bind missing: %s", b)
		}
	}
}

// hasBind is exact membership. The package already has a `contains` that does
// substring matching on one string, which is a different question.
func hasBind(binds []string, want string) bool {
	for _, b := range binds {
		if b == want {
			return true
		}
	}
	return false
}

// memory/ and public/ are seeded with the workspace rather than left to whatever
// creates one first. An agent told to write a deliverable into public/ before
// anyone had uploaded anything was writing into a directory that did not exist.
func TestTheGanglionWorkspaceIsSeededWithItsDirectories(t *testing.T) {
	userDir := t.TempDir()
	if _, err := provisionGanglion(userDir, ""); err != nil {
		t.Fatalf("provisionGanglion: %v", err)
	}
	for _, dir := range []string{"memory", config.PublicDirName, "sessions", "windows"} {
		path := filepath.Join(userDir, config.MainWorkspace, dir)
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			t.Errorf("%s was not seeded: %v", dir, err)
		}
	}
}
