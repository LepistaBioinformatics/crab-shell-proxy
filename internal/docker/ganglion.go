package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/projects"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// crab-ganglion-harness containers.
//
// This is a SEPARATE creation path rather than conditionals threaded through
// create(). Two reasons, and the second is the load-bearing one:
//
//   - The two harnesses share almost nothing at container level. picoclaw needs
//     a workspace laid out under <HOME>/.picoclaw with a config.json, a
//     .security.yml, a persona cascade of read-only file binds, a skills root
//     and per-project secret binds. The ganglion reads its whole configuration
//     from the environment and owns one directory.
//   - picoclaw is what production runs, and internal/docker's suite does not
//     execute in every environment (it needs lchown privileges). Editing the
//     path that serves every live member, without a test that runs, to
//     accommodate a harness nobody is on yet, is the wrong trade. This file
//     cannot regress picoclaw because picoclaw never enters it.
//
// The Hermes work took the same shape (createHermes) for the same reason.

// ganglionMountDest is the harness's data root inside the container, matching
// its own GANGLION_DATA_DIR default. NOT a mount: only its workspace child is
// bound (ganglionWorkspaceBind), and the rest of the per-user dir stays on the
// host where the agent cannot reach it.
const ganglionMountDest = "/data/.ganglion"

// ganglionTokenFile holds the per-user bearer, in the user directory but
// DELIBERATELY OUTSIDE what the container mounts (see ganglionWorkspaceBind).
// Persisted so a returning user reuses the same token instead of having their
// session invalidated by a recreate.
const ganglionTokenFile = ".crab-ganglion.json"

// ganglionWorkspaceBind mounts only the workspace, not the whole user dir.
//
// The wide bind (hostDir -> ganglionMountDest) is what the first cut used, and
// it put .crab-ganglion.json one level above the shell tool's working
// directory: `cat ../.crab-ganglion.json` read the bearer. That slot -- proxy
// owned state beside the workspace -- is exactly the one picoclaw's
// restrict_to_workspace protects, and the ganglion has no equivalent: its only
// tool is `/bin/sh -c <string>`, so there is no path argument to refuse. The
// container boundary IS the confinement here, which means proxy-owned state
// has to be on the other side of it rather than merely adjacent.
//
// Anything the harness needs to keep across a recreate therefore belongs under
// workspace/; anything the proxy owns belongs above it, and is now unreachable
// rather than one `..` away.
func ganglionWorkspaceBind(hostDir string) string {
	return filepath.Join(hostDir, config.MainWorkspace) + ":" + ganglionMountDest + "/" + config.MainWorkspace
}

// ganglionBinds is the container's whole bind set.
//
// Split out of createGanglion for the reason ganglionEnv was: the drift checks
// read this list back and must agree with it, and neither of them can be
// exercised where a Docker daemon is needed. A disagreement here does not fail
// loudly -- it recreates the container on every single turn, which reads as
// "the harness keeps restarting" with nothing naming the cause.
func ganglionBinds(cfg *config.Config, key WorkspaceKey, hostDir string) []string {
	binds := []string{ganglionWorkspaceBind(hostDir)}
	if cfg.GanglionKeyFile != "" {
		binds = append(binds, cfg.GanglionKeyFile+":"+ganglionKeyFileDest+":ro")
	}
	// The model registry, READ-ONLY and above the workspace, for the same
	// reason the key file is: the workspace is the only thing a command can
	// reach, and a writable model list would let a tool steered by untrusted
	// natural language choose the endpoint its own keys are sent to.
	binds = append(binds, filepath.Join(hostDir, ganglionConfigFile)+":"+ganglionConfigDest+":ro")
	// The admin's shared skills (D-1). Same effective-skills directory picoclaw
	// already mounts, so one admin action reaches both harnesses -- and the
	// same read-only discipline.
	binds = append(binds, config.EffectiveSkillsDir(cfg.HostDataRoot, key.TenantID, key.SubsAccID, key.Role)+
		":"+ganglionSkillsDest+":ro")
	return append(binds, personaBindStrings(cfg, key, ganglionMountDest)...)
}

// ganglionSkillsDest is where the admin's shared skills land in the container.
//
// INSIDE the workspace, unlike every other proxy-owned mount, and that is
// deliberate rather than an inconsistency.
//
// The harness renders a skills INDEX into the system prompt -- name, description
// and PATH -- and the agent reads the body it needs with the shell tool. That
// tool runs under a Landlock ruleset whose only writable-or-readable hierarchy
// is the workspace, so a skills root beside the data dir would produce an index
// pointing at files the agent cannot open. An index of unreachable paths is
// worse than no index: the model is told a capability exists and then fails to
// use it.
//
// Read-only at the MOUNT, which is a kernel guarantee independent of Landlock --
// Landlock only ever narrows, so it can never grant a write to a read-only
// bind. That is what makes "evolution may only write into the workspace copy"
// (spec R5.2) enforced rather than merely intended.
const ganglionSkillsDest = ganglionMountDest + "/" + config.MainWorkspace + "/shared-skills"

// ganglionKeyFileDest is where the credential key file lands in the container.
//
// Beside the data root, NOT under workspace/ -- and that placement is the
// whole second factor. The workspace is the only hierarchy the harness's
// Landlock ruleset grants a command, so a file here is unreachable from the
// shell tool while remaining readable by the harness process at boot. Move it
// under workspace/ and the two-factor scheme collapses to one factor plus a
// file the agent can print.
const ganglionKeyFileDest = ganglionMountDest + "/credential.key"

// ganglionBindDrift reports whether an existing container's mounts no longer
// match what createGanglion would build.
//
// Without this a running container keeps the bind set it was created with
// forever, because bind sets are fixed at create time. Two cases, both
// invisible otherwise and both security-relevant:
//
//   - the OLD WIDE BIND, which exposed the whole per-user directory and with
//     it the proxy's own state;
//   - the CREDENTIAL KEY FILE appearing or disappearing, so that turning
//     encryption on reaches members already on the harness instead of leaving
//     them booting against a value they cannot resolve.
//
// Same class of invisible staleness as personaBindDrift and imageDrift, and it
// sits beside them for that reason.
func ganglionBindDrift(cfg *config.Config, actual []string) bool {
	haveKeyFile, haveConfig, haveSkills := false, false, false
	for _, b := range actual {
		_, dest, ok := splitBind(b)
		if !ok {
			continue
		}
		if dest == ganglionMountDest {
			return true
		}
		switch dest {
		case ganglionKeyFileDest:
			haveKeyFile = true
		case ganglionConfigDest:
			haveConfig = true
		case ganglionSkillsDest:
			haveSkills = true
		}
	}
	// Both are unconditional, so a container created before either feature has
	// none and must be recreated exactly once to get it.
	if !haveConfig || !haveSkills {
		return true
	}
	return haveKeyFile != (cfg.GanglionKeyFile != "")
}

// ganglionSecretDrift reports whether a running container is missing a key it
// now needs.
//
// The asymmetry with the config file is the point, and it is FR-10: the FILE is
// re-read by the harness on change, so switching a workspace between models it
// already carries keys for costs nothing. The ENVIRONMENT is fixed at create
// time, so introducing a model whose key the container has never held requires
// a recreate -- and the proxy has to perform it rather than leave a candidate
// that can only ever be skipped for "no API key".
//
// Only ADDITIONS and CHANGES count. A container holding a key for a model no
// longer in the chain is carrying a variable nothing reads, which is untidy and
// not worth destroying a member's running container over.
func ganglionSecretDrift(want, actual []string) bool {
	have := make(map[string]string, len(actual))
	for _, kv := range actual {
		if k, v, ok := strings.Cut(kv, "="); ok {
			have[k] = v
		}
	}
	for _, kv := range want {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if got, present := have[k]; !present || got != v {
			return true
		}
	}
	return false
}

// splitBind pulls the destination out of a "src:dest[:opts]" bind string.
func splitBind(b string) (src, dest string, ok bool) {
	parts := strings.Split(b, ":")
	if len(parts) < 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

type ganglionState struct {
	Token string `json:"token"`
}

// provisionGanglion prepares the per-user directory and returns the bearer the
// proxy will present to that container.
//
// Unlike picoclaw's provision it writes no config: everything the harness needs
// arrives as environment, which is why the generic dotenv secret sink covers it
// and the picoclaw-only native .security.yml sink is simply unused.
func provisionGanglion(userDir, user string) (string, error) {
	if err := os.MkdirAll(filepath.Join(userDir, "workspace"), 0o755); err != nil {
		return "", fmt.Errorf("ganglion workspace: %w", err)
	}
	statePath := filepath.Join(userDir, ganglionTokenFile)

	if b, err := os.ReadFile(statePath); err == nil {
		var st ganglionState
		if json.Unmarshal(b, &st) == nil && st.Token != "" {
			return st.Token, nil
		}
		// A corrupt or empty state file is replaced rather than fatal: the cost
		// is one invalidated bearer, and the alternative is a member who cannot
		// start a container until someone deletes a file by hand.
	}

	tok, err := randomToken()
	if err != nil {
		return "", fmt.Errorf("ganglion token: %w", err)
	}
	b, err := json.Marshal(ganglionState{Token: tok})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(statePath, b, 0o600); err != nil {
		return "", fmt.Errorf("write ganglion state: %w", err)
	}
	if err := chownTree(userDir, user); err != nil {
		return "", fmt.Errorf("chown ganglion dir: %w", err)
	}
	return tok, nil
}

// ganglionEnv builds the container environment.
//
// Split out from createGanglion so it is assertable without a Docker daemon --
// which matters more here than usual, because this is the only place the
// harness's configuration contract is expressed and a typo in a variable name
// fails as "the harness would not boot", with nothing naming the cause.
func ganglionEnv(cfg *config.Config, agent config.Agent, token string, secrets []string) []string {
	env := []string{
		fmt.Sprintf("GANGLION_ADDR=0.0.0.0:%d", cfg.GanglionPort),
		"GANGLION_DATA_DIR=" + ganglionMountDest,
		"GANGLION_TOKEN=" + token,
		// Where the bound registry lands. Set explicitly rather than left to
		// the harness's default so the two sides of this contract are both
		// visible in `docker inspect`.
		"GANGLION_CONFIG_FILE=" + ganglionConfigDest,
	}
	if agent.Model != nil {
		env = append(env,
			"GANGLION_MODEL="+agent.Model.Name,
			"GANGLION_BASE_URL="+agent.Model.BaseURL,
			"GANGLION_API_KEY="+agent.Model.APIKey,
		)
	}
	if cfg.GanglionOTLPEndpoint != "" {
		env = append(env, "GANGLION_OTLP_ENDPOINT="+cfg.GanglionOTLPEndpoint)
	}
	// The other half of an enc:// credential. The ciphertext travels in
	// GANGLION_API_KEY above, already opaque to this proxy; this is the factor
	// that is useless on its own without the bound key file.
	if cfg.GanglionKeyPassphrase != "" {
		env = append(env, "GANGLION_KEY_PASSPHRASE="+cfg.GanglionKeyPassphrase)
		env = append(env, "GANGLION_KEY_FILE="+ganglionKeyFileDest)
	}
	// The persona file the proxy's cascade already materializes. Read at every
	// turn rather than at boot, so an admin's edit reaches the member without a
	// container restart.
	env = append(env, "GANGLION_SYSTEM_FILE="+ganglionMountDest+"/workspace/AGENT.md")
	// The admin's shared skills root. The agent's own <workspace>/skills is
	// found without being told, so this names only the half the proxy owns.
	env = append(env, "GANGLION_SKILLS_ROOT="+ganglionSkillsDest)
	// The lifecycle mode. The harness cannot observe whether its own container
	// stops when idle, and it needs that for one decision: refusing a SCHEDULED
	// evolution pass on a scale-to-zero agent, which would store an intention
	// and fire nothing. Same argument the cron routes already make.
	env = append(env, "GANGLION_LIFECYCLE_MODE="+string(agent.Mode))
	// One variable per model and per search provider, already ordered. The
	// GANGLION_MODEL/BASE_URL/API_KEY trio above stays: it is what a workspace
	// whose cascade resolves nothing still runs on, and dropping it would make
	// this change a cutover instead of an addition.
	return append(env, secrets...)
}

// materializeGanglion resolves this workspace's model from the inventory, writes
// the harness's configuration file and seeds the member's project subtrees,
// returning the credential variables the container needs.
//
// Called on EVERY ensure, not only at create. That is what makes an admin's
// model change reach a container that is already running: the file is rewritten,
// the harness notices the mtime at the next turn, and nothing is destroyed.
//
// A workspace the cascade resolves NOTHING for is not an error here, unlike
// picoclaw's provision which refuses outright. The difference is real: a
// ganglion agent has always had a working model from config.yaml, and failing
// to start it because an inventory that never governed it has nothing to say
// would turn adding a feature into an outage.
func (m *Manager) materializeGanglion(agent config.Agent, key WorkspaceKey, userDir string) ([]string, error) {
	ref := m.workspaceRef(key)
	// governed says the resolution came from the INVENTORY rather than from the
	// agent's static configuration. It gates RecordMaterialization, and getting
	// that wrong is not cosmetic -- see below.
	governed := true
	res, err := m.reg.Resolve(ref)
	if err != nil {
		if !errors.Is(err, registry.ErrNoModelResolvable) {
			return nil, err
		}
		if agent.Model == nil {
			return nil, err
		}
		// The floor: the agent's own static model, exactly what the three
		// environment variables already carry. Written into the file too, so
		// the harness reads one shape whether or not an inventory governs it.
		//
		// CascadeName is deliberately left empty, because no cascade resolved
		// this. That is also why it must not be recorded.
		governed = false
		res = registry.Resolution{Primary: registry.Model{
			ModelName: agent.Model.Name,
			Provider:  agent.Model.Provider,
			Model:     agent.Model.Name,
			APIBase:   agent.Model.BaseURL,
			APIKey:    agent.Model.APIKey,
		}}
	}
	for _, name := range res.Skipped {
		m.logf("ganglion %s: fallback %q is not active, skipped", ref.Key(), name)
	}

	web, err := ganglionWebSecrets(config.EffectiveSecretsDir(m.cfg.ContainerDataRoot, key.UserAccID, key.Role))
	if err != nil {
		return nil, fmt.Errorf("read search provider secrets: %w", err)
	}

	doc, err := ganglionConfigDoc(res, web)
	if err != nil {
		return nil, err
	}
	changed, err := writeGanglionConfig(userDir, doc, m.cfg.PicoclawUser)
	if err != nil {
		return nil, err
	}
	if changed {
		m.logf("ganglion %s: model configuration rewritten (primary %q)", ref.Key(), res.Primary.ModelName)
	}
	// Recorded for the same reason picoclaw's materialization records it: a
	// model in use must not be deletable, and the referrer list is how the
	// inventory knows. A ganglion workspace that never recorded would let an
	// admin delete a model an agent is actively running.
	//
	// ONLY WHEN THE INVENTORY RESOLVED IT. RecordMaterialization writes an
	// Assignment unconditionally, keyed on res.CascadeName -- which the
	// synthesized fallback above leaves EMPTY. Recording it would store an
	// assignment naming no model, and worse, would overwrite a legitimate
	// earlier assignment's model name with "" the first time a cascade stopped
	// resolving. There is nothing to protect from deletion here either: the
	// fallback model comes from config.yaml and is not in the inventory.
	//
	// Also outside the `changed` branch, deliberately. The two are unrelated:
	// an unchanged FILE says nothing about whether the referrer has been
	// recorded, and tying them meant a workspace whose config happened to
	// render identically was never registered as using its model.
	if governed {
		if rerr := m.reg.RecordMaterialization(ref, res); rerr != nil {
			m.logf("ganglion %s: could not record the assignment: %v", ref.Key(), rerr)
		}
	}

	if err := m.seedGanglionProjects(key, userDir); err != nil {
		return nil, err
	}
	return ganglionSecretEnv(res, web), nil
}

// ganglionProjectDirs are the subtrees the harness resolves under a project
// root: its transcripts, its context window and the images generate_image
// writes, plus the member's own uploads.
//
// Created by the PROXY even though the harness would create the first three
// itself, for the reason projectWorkspaceDirs gives on the picoclaw side: the
// proxy runs as root and the harness does not, so a directory conjured later by
// whichever of the two got there first is a coin flip on ownership. Making them
// here, and chowning them here, means the agent always finds a tree it can
// write.
//
// config.PublicDirName rather than the "uploads" a member sees in a path
// reference: `uploads` is LegacyPublicDirName, and publicRoot MIGRATES a
// directory by that name into `public` on first access. Seeding the legacy
// spelling would make every ensure recreate what every media call then renames.
var ganglionProjectDirs = []string{"sessions", "windows", "media", config.PublicDirName}

// seedGanglionProjects brings every project's subtree under the ganglion
// workspace to its intended state, on every ensure.
//
// NOT A BIND, and that is decision D-1 rather than an implementation detail:
// these are directories INSIDE the one bind the container already has, so a
// project created for a scale-to-zero agent changes nothing ganglionBindDrift
// can see and costs no recreate. A container that may not even be running must
// not be a prerequisite for making a project.
//
// A workspace with no projects gets no `projects` directory at all -- the loop
// simply does not run -- so an agent that has never had one behaves exactly as
// it does today.
//
// Every path is resolved through an os.Root anchored at the workspace, because
// `projects`, the project id and each leaf are all components the shell tool can
// replace with a symlink: the harness's Landlock domain grants the whole
// workspace tree (D-1), so a root-owned MkdirAll here would follow whatever the
// agent pointed it at. The same boundary WriteMemory documents, for the same
// reason.
func (m *Manager) seedGanglionProjects(key WorkspaceKey, userDir string) error {
	list, err := m.projectStore(key).List()
	if err != nil {
		return fmt.Errorf("read projects: %w", err)
	}
	root := filepath.Join(userDir, config.MainWorkspace)
	if len(list) == 0 {
		// NFR-1: a workspace that has never had a project gets no `projects`
		// directory, so an agent that never had one behaves exactly as today.
		// But one that HAD projects and no longer does still has orphans to
		// sweep, so an empty list is not on its own a reason to stop.
		//
		// A plain Stat rather than a rooted one: it only decides whether there
		// is work, and every path that does work goes through the os.Root below.
		if _, serr := os.Stat(filepath.Join(root, ganglionProjectsDirName)); serr != nil {
			return nil
		}
	}

	tree, err := openTree(root)
	if err != nil {
		return err
	}
	defer tree.Close()

	for _, p := range list {
		// Derived from the same helper the segment resolution uses, minus the
		// workspace prefix the tree is already anchored at. Deriving it twice is
		// how the seeder and the reader would come to disagree about where a
		// transcript lives.
		rel := strings.TrimPrefix(config.GanglionProjectWorkspace(p.ID), config.MainWorkspace+"/")
		for _, sub := range ganglionProjectDirs {
			if err := tree.root.MkdirAll(rel+"/"+sub, 0o700); err != nil {
				if escaped(err) {
					return ErrMediaName
				}
				return fmt.Errorf("create ganglion project dir %s/%s: %w", rel, sub, err)
			}
		}
		if err := tree.root.WriteFile(rel+"/"+ganglionProjectFileName,
			[]byte(ganglionProjectDoc(p)), 0o600); err != nil {
			if escaped(err) {
				return ErrMediaName
			}
			return fmt.Errorf("write ganglion %s: %w", ganglionProjectFileName, err)
		}
		if err := chownTree(tree.abs(rel), m.cfg.PicoclawUser); err != nil {
			return fmt.Errorf("chown ganglion project %s: %w", p.ID, err)
		}
	}
	return m.sweepGanglionProjects(key, tree, list)
}

// ganglionProjectsDirName is the directory holding the per-project subtrees,
// matching the harness's own domain.ProjectsDirName.
const ganglionProjectsDirName = "projects"

// sweepGanglionProjects removes the subtree of a project that no longer exists.
//
// The same argument syncProjectWorkspaces makes on the picoclaw side, and it is
// not tidiness: DeleteProject removes the RECORD first and the directory second,
// so a crash between the two leaves a member's transcripts on disk, invisible to
// them, after the interface told them the project was gone. Nothing else would
// ever remove it — the ganglion ensure path returns before
// syncProjectWorkspaces runs.
func (m *Manager) sweepGanglionProjects(key WorkspaceKey, tree *treeRoot, list []projects.Project) error {
	live := make(map[string]bool, len(list))
	for _, p := range list {
		live[p.ID] = true
	}
	// Listed through the Root's own fs.FS, so a symlink planted where `projects`
	// should be cannot make this enumerate -- and then delete -- somewhere else.
	entries, err := fs.ReadDir(tree.root.FS(), ganglionProjectsDirName)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("scan ganglion projects: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || live[e.Name()] {
			continue
		}
		if err := tree.root.RemoveAll(ganglionProjectsDirName + "/" + e.Name()); err != nil {
			return fmt.Errorf("remove orphaned ganglion project %s: %w", e.Name(), err)
		}
		m.logf("workspace %s/%s: removed orphaned ganglion project dir %s",
			key.Role, key.UserAccID, e.Name())
	}
	return nil
}

// ganglionProjectFileName is the file the harness reads as the project's
// instructions and folds into the system prompt. It is a CONSTANT on that side
// (skills.ProjectFileName), not a configured path, so the name is the whole
// contract between the two repositories.
const ganglionProjectFileName = "PROJECT.md"

// ganglionProjectDoc renders one project record as that file.
//
// FULLY DERIVED from the project store and rewritten on every ensure, which is
// the same trade composeProjectAgentMD documents for picoclaw: nothing authored
// by a human lives only here, and the cost is that an edit the AGENT makes to
// this file is reverted on the member's next turn.
//
// No frontmatter, unlike the picoclaw twin. There is nothing to inherit: the
// ganglion has one agent whose persona comes from GANGLION_SYSTEM_FILE, and this
// file is appended to that prompt rather than replacing it.
func ganglionProjectDoc(p projects.Project) string {
	instructions := strings.TrimSpace(p.Instructions)
	if instructions == "" {
		// An empty project is legitimate -- a member may create one and write the
		// instructions later -- but the agent should be told what it is looking at
		// rather than reading a bare heading.
		instructions = "This project has no instructions yet."
	}
	return "# " + p.Name + "\n\n" + instructions + "\n"
}

// createGanglion creates (but does not start) a ganglion container.
func (m *Manager) createGanglion(ctx context.Context, agent config.Agent, key WorkspaceKey, name string) error {
	hostDir := config.UserWorkspace(m.cfg.HostDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	containerDir := config.UserWorkspace(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)

	token, err := provisionGanglion(containerDir, m.cfg.PicoclawUser)
	if err != nil {
		return err
	}
	secrets, err := m.materializeGanglion(agent, key, containerDir)
	if err != nil {
		return err
	}
	// The merged (tenant, subscription, agent) skills directory the bind below
	// points at. Built before create, because a bind whose source does not
	// exist is created by the daemon as an empty root-owned directory and then
	// never populated.
	if err := m.syncEffectiveSkills(key.TenantID, key.SubsAccID, key.Role); err != nil {
		return fmt.Errorf("sync effective skills: %w", err)
	}

	image := m.harnessImage(agent)
	if image == "" {
		// Unreachable through config.Load, which rejects such an agent. Kept so
		// a programmatic Config cannot create a container from an empty image
		// name and fail as a confusing daemon error.
		return fmt.Errorf("agent %q: no ganglion image configured", agent.Key)
	}

	spec := CreateSpec{
		Name:  name,
		Image: image,
		// Same uid the proxy already chowns per-user volumes to. Unlike Hermes,
		// there is no s6 setuidgid step to break by setting this.
		User: m.cfg.PicoclawUser,
		Env:  ganglionEnv(m.cfg, agent, token, secrets),
		Labels: map[string]string{
			LabelManaged:      "true",
			LabelAgent:        key.Role,
			LabelTenant:       key.TenantID,
			LabelSubscription: key.SubsAccID,
			LabelUser:         key.UserAccID,
			LabelMode:         string(agent.Mode),
		},
		// The per-user volume, plus the persona cascade READ-ONLY on top of it.
		//
		// personaBinds is harness-agnostic -- it takes the mount destination --
		// and it lands each file at <mountDest>/workspace/<name>, which is
		// exactly where GANGLION_SYSTEM_FILE points. Without this the harness
		// logged "persona file ... no such file or directory" every boot and
		// ran every member's agent with no identity at all: the env var was
		// wired to a path nothing created.
		Binds:   ganglionBinds(m.cfg, key, hostDir),
		Network: m.cfg.Network,
		// One process, PID 1, signals handled in main. No supervisor to reap
		// children for.
		Init: false,
	}
	if err := m.docker.EnsureImage(ctx, image); err != nil {
		return fmt.Errorf("ensure image %s: %w", image, err)
	}
	if _, err := m.docker.Create(ctx, spec); err != nil {
		return err
	}
	m.logf("created ganglion container %s (mode=%s)", name, agent.Mode)
	return nil
}

// ensureGanglionRunning brings a ganglion container to health-ready and
// returns how to reach it.
//
// It is the tail of EnsureRunning for this harness, and it is short for the
// same reason ganglion.go exists at all: none of what makes the picoclaw tail
// long — the persona bind cascade, per-project secret binds, the skills root,
// the managed-content mounts — has a ganglion equivalent yet. When one does, it
// belongs here rather than as another branch in the picoclaw path.
func (m *Manager) ensureGanglionRunning(
	ctx context.Context, agent config.Agent, key WorkspaceKey, name, authToken string,
) (Target, error) {
	st, err := m.docker.Inspect(ctx, name)
	if err != nil {
		return Target{}, err // daemon unreachable etc. — surfaced as 502 upstream
	}

	// Re-materialized on every ensure, BEFORE the drift decision, because it is
	// what the drift decision reads: the file is rewritten here, and whether the
	// container is missing a key for what the file now names is what
	// ganglionSecretDrift then answers.
	//
	// A failure to rewrite is logged rather than fatal when the container
	// already exists. The member has a working agent on the previous
	// configuration; refusing to serve their turn because an inventory write
	// failed would trade a stale model for no model.
	containerDir := config.UserWorkspace(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	var wantSecrets []string
	if st.Exists {
		if sec, merr := m.materializeGanglion(agent, key, containerDir); merr != nil {
			m.logf("container %s: could not refresh the model configuration, keeping the previous one: %v", name, merr)
		} else {
			wantSecrets = sec
		}
	}

	switch {
	case !st.Exists:
		if cerr := m.createGanglion(ctx, agent, key, name); cerr != nil {
			return Target{}, cerr
		}
		if serr := m.docker.Start(ctx, name); serr != nil {
			return Target{}, serr
		}

	case personaBindDrift(m.cfg, key, ganglionMountDest, st.Binds) ||
		ganglionBindDrift(m.cfg, st.Binds) ||
		ganglionSecretDrift(wantSecrets, st.Env) ||
		m.imageDrift(ctx, agent, st):
		// Four drifts, all invisible without this check.
		//
		// Bind sets are fixed at create time, so a container created before a
		// persona file existed has no mount for it and an admin's save can
		// never arrive however many times it is bounced.
		//
		// An image is worse: a container reuses whatever it was created from
		// for as long as it exists, so publishing a new harness image and
		// redeploying the stack changes nothing on its own.
		//
		// The third is the bind set: a container created before the mount was
		// narrowed still exposes the proxy's own state to the agent, and one
		// created before encryption was switched on has no key file to
		// resolve its own credential with.
		//
		// The fourth is the credential set. Unlike the model configuration --
		// a file the harness re-reads -- environment is fixed at create time,
		// so a workspace newly pointed at a model this container has never held
		// a key for can only be served by a recreate. Leaving it would give the
		// member an agent whose every candidate is skipped for "no API key".
		m.logf("container %s: persona mounts, bind set, credentials or harness image stale, recreating", name)
		if st.Running {
			if serr := m.docker.Stop(ctx, name, 10*time.Second); serr != nil {
				return Target{}, serr
			}
		}
		if rerr := m.docker.Remove(ctx, name); rerr != nil {
			return Target{}, rerr
		}
		if cerr := m.createGanglion(ctx, agent, key, name); cerr != nil {
			return Target{}, cerr
		}
		if serr := m.docker.Start(ctx, name); serr != nil {
			return Target{}, serr
		}

	case !st.Running:
		if serr := m.docker.Start(ctx, name); serr != nil {
			return Target{}, serr
		}
	}

	port := m.harnessPort(agent)
	if err := m.waitHealthy(ctx, name, port, m.startupDeadline(agent)); err != nil {
		return Target{}, fmt.Errorf("%s %s did not become ready: %w", agent.Harness, name, err)
	}
	// The idle timer is NOT armed here. EnsureRunning already holds this
	// container's keyState lock and ArmIdle takes it again -- arming from
	// inside would deadlock. The picoclaw path does not arm here either: the
	// caller re-arms after the turn, which is also the only moment "idle" is
	// true.
	return Target{
		Name:      name,
		Endpoint:  m.endpoint(agent, name),
		AuthToken: authToken,
		Harness:   agent.Harness,
	}, nil
}
