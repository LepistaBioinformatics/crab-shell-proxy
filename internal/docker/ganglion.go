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
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
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

// seedGanglionUserFile writes the workspace's USER.md if it has none.
//
// ONLY IF IT HAS NONE. It is the one persona file the agent writes back, so
// overwriting it on every ensure would erase what the agent learned about the
// member every time their container was recreated -- which for a scale-to-zero
// agent is every turn.
func seedGanglionUserFile(cfg *config.Config, key WorkspaceKey, userDir, templateDir string) error {
	dst := filepath.Join(userDir, config.MainWorkspace, ganglionUserFile)
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	body := personaSeedSource(cfg, key, templateDir, ganglionUserFile)
	if body == "" {
		// Nothing in the cascade provides one. An absent USER.md is the ordinary
		// state of a workspace whose operator injected nothing, and writing an
		// empty file would put a heading-less blank in the agent's prompt.
		return nil
	}
	return os.WriteFile(dst, []byte(body), 0o644)
}

// ganglionUserFile is the persona file the agent writes back rather than reads.
const ganglionUserFile = "USER.md"

// ganglionWorkspaceDirs are the directories every ganglion workspace has, seeded
// on provision. "." is the workspace itself.
//
// The set is picoclaw's, minus what only picoclaw has: no cron/ (the proxy holds
// the ganglion's schedules, above the bind) and no .secrets/ (credentials arrive
// as environment). windows/ is the ganglion's own and has no picoclaw
// equivalent.
var ganglionWorkspaceDirs = []string{".", "memory", config.PublicDirName, "sessions", "windows"}

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
func ganglionBinds(cfg *config.Config, key WorkspaceKey, hostDir string, projects []string) []string {
	binds := []string{ganglionWorkspaceBind(hostDir)}
	// ONE BIND PER PROJECT, because a project's workspace is a SIBLING of the
	// main one and the ganglion mounts workspaces, not the directory holding
	// them. Mounting the parent instead would be one line and would expose
	// config.json and credential.key, which sit there precisely because the
	// agent's Landlock root ends below them.
	//
	// The cost is that the bind set changes when the project set does, so the
	// container is recreated -- which ganglionBindDrift already notices. For an
	// agent running scale-to-zero, the mode this harness exists for, the
	// container is created per turn anyway.
	for _, id := range projects {
		seg := config.ProjectWorkspace(id)
		binds = append(binds, filepath.Join(hostDir, seg)+":"+ganglionMountDest+"/"+seg)
		// The admin's shared skills, inside EVERY workspace rather than only the
		// main one. The agent's Landlock root is the TURN's workspace now, so a
		// skills root reachable only from the main one would produce an index
		// pointing at files a project turn cannot open -- the failure
		// ganglionSkillsDest already records, one layout later. picoclaw mounts
		// skills into each project agent's workspace for the same reason.
		binds = append(binds, config.EffectiveSkillsDir(cfg.HostDataRoot, key.TenantID, key.SubsAccID, key.Role)+
			":"+ganglionMountDest+"/"+seg+"/shared-skills:ro")
	}
	if cfg.GanglionKeyFile != "" {
		binds = append(binds, cfg.GanglionKeyFile+":"+ganglionKeyFileDest+":ro")
	}
	// The admin's shared FILES and the operator-managed memory documents, both
	// built by the same pure helpers picoclaw uses and both landing at
	// <mount>/workspace/... -- inside the workspace bind above, which Docker
	// applies first because it sorts by destination depth. The persona binds at
	// the end of this function already rely on that.
	//
	// They were missing, and the cost was not cosmetic. admin-shared-content was
	// silently inert for every ganglion member -- an admin publishing a document
	// to a subscription reached picoclaw members and nobody else, which is the
	// failure harness_gate.go exists to prevent, in a feature with no gate row.
	// And the managed documents include FILE_DELIVERY.md, which is what tells an
	// agent to write deliverables into public/attachments -- the only place the
	// member's interface lists. A ganglion agent had never been told that.
	for _, sm := range sharedFileBinds(cfg, key, ganglionMountDest) {
		binds = append(binds, sm.bind)
	}
	binds = append(binds, managedContentBinds(
		config.ManagedSkillsDir(cfg.HostDataRoot), ganglionMountDest,
		cfg.ResolvedMCPTokenSecret != "")...)
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

// ganglionProjectIDs is the member's projects, in store order, for the bind set
// and the drift check that reads it back. The two MUST agree, which is why they
// ask the same function rather than each listing for itself.
//
// A failure is an empty list rather than an error: the projects store is the
// proxy's own file, and a workspace that cannot read it has a larger problem
// than a missing bind -- one this path would report as "recreate the container",
// forever, on every ensure.
func (m *Manager) ganglionProjectIDs(key WorkspaceKey) []string {
	list, err := m.projectStore(key).List()
	if err != nil {
		m.logf("ganglion %s/%s: read projects for the bind set: %v", key.Role, key.UserAccID, err)
		return nil
	}
	out := make([]string, 0, len(list))
	for _, p := range list {
		out = append(out, p.ID)
	}
	return out
}

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
//   - the PROJECT SET changing. A project's workspace is a sibling of the main
//     one and gets a bind of its own, so a project created since this container
//     started is not visible inside it at all -- the turn would run against a
//     directory the harness creates locally and the proxy never reads. This is
//     the cost the sibling layout accepted, and noticing it here is what makes
//     the cost bounded.
func ganglionBindDrift(cfg *config.Config, projects []string, actual []string) bool {
	haveKeyFile, haveConfig, haveSkills := false, false, false
	mounted := map[string]bool{}
	for _, b := range actual {
		_, dest, ok := splitBind(b)
		if !ok {
			continue
		}
		if dest == ganglionMountDest {
			return true
		}
		if id, isProject := strings.CutPrefix(dest, ganglionMountDest+"/"+ganglionProjectPrefix); isProject {
			// The workspace bind, not the shared-skills one nested under it.
			if !strings.Contains(id, "/") {
				mounted[id] = true
			}
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
	if len(mounted) != len(projects) {
		return true
	}
	for _, id := range projects {
		if !mounted[identity.SanitizeID(id)] {
			return true
		}
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
	// The workspace's own directories, seeded rather than left to whatever
	// happens to create one first.
	//
	// memory/ is where the managed documents are mounted and where the agent's
	// own MEMORY.md lives -- picoclaw's path, and now the harness's. public/ is
	// the only directory the member's interface lists, so an agent told to write
	// a deliverable there before anyone has uploaded anything was writing into a
	// directory that did not exist. Both are one MkdirAll and both remove an
	// ordering dependency nobody would look for.
	for _, dir := range ganglionWorkspaceDirs {
		if err := os.MkdirAll(filepath.Join(userDir, config.MainWorkspace, dir), 0o755); err != nil {
			return "", fmt.Errorf("ganglion workspace %s: %w", dir, err)
		}
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

// resolveGanglionEndpoints fills in every model's api_base, or says why it cannot.
//
// THREE SOURCES THAT COMPLEMENT EACH OTHER rather than compete:
//
//  1. the INVENTORY's own api_base, which is final. A custom model is custom
//     precisely because its endpoint is not its provider's default, so nothing
//     below may overwrite one the admin entered.
//  2. the AGENT's baseUrl from config.yaml.
//  3. the PROVIDER's default, which is what picoclaw resolves internally and
//     never had to write down (ProviderEndpoint).
//
// The third is what makes a migrated agent work at all. picoclaw's configuration
// answers "which provider"; this harness needs "which address", and until now the
// answer existed nowhere on the path. The proxy wrote a config it already knew
// could not work, the harness booted reporting "models: 1 configured", and the
// member met it as `unsupported protocol scheme ""` in their chat.
//
// The FALLBACK CHAIN is filled too: a chain whose primary resolves and whose
// second entry does not is a chain that works until the day it is needed.
//
// A primary with no endpoint is REFUSED here, where an operator can see it. An
// agent that cannot reach a model is not usable either way; the difference is
// whether the failure names its cause.
func resolveGanglionEndpoints(res *registry.Resolution, agent config.Agent, ref string) error {
	fill := func(model *registry.Model) {
		if model.APIBase != "" {
			return
		}
		if agent.Model != nil && agent.Model.BaseURL != "" {
			model.APIBase = agent.Model.BaseURL
			return
		}
		model.APIBase = ProviderEndpoint(model.Provider)
	}
	fill(&res.Primary)
	for i := range res.Chain {
		fill(&res.Chain[i])
	}
	if res.Primary.APIBase == "" {
		return fmt.Errorf(
			"ganglion %s: model %q (provider %q) has no api_base and the provider has no known "+
				"default -- set one on the inventory model, or give the agent a baseUrl in "+
				"config.yaml. picoclaw resolved a provider to an endpoint itself; this harness "+
				"needs it written down",
			ref, res.Primary.ModelName, res.Primary.Provider)
	}
	return nil
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
	if err := resolveGanglionEndpoints(&res, agent, ref.Key()); err != nil {
		return nil, err
	}
	for _, name := range res.Skipped {
		m.logf("ganglion %s: fallback %q is not active, skipped", ref.Key(), name)
	}

	web, err := ganglionWebSecrets(config.EffectiveSecretsDir(m.cfg.ContainerDataRoot, key.UserAccID, key.Role))
	if err != nil {
		return nil, fmt.Errorf("read search provider secrets: %w", err)
	}

	// The memory graph. Minted here rather than in applyMemoryGraphMCP, which is
	// picoclaw's path: that one MERGES a block into a config file picoclaw also
	// writes, while this file is rendered whole on every ensure — so there is
	// nothing to merge into and nothing to leave behind when the secret is
	// unset.
	//
	// A token that cannot be minted is an ERROR, not an empty one, for the reason
	// memoryGraphToken states: a server that always 401s is harder to diagnose
	// than no server.
	mcpToken, err := m.memoryGraphToken(key)
	if err != nil {
		return nil, err
	}
	doc, err := ganglionConfigDoc(res, web, m.cfg.MCPBaseURL, mcpToken)
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
	// Anchored at the USER DIR, not the workspace, because a project's workspace
	// is a sibling of the main one now.
	//
	// That is stricter than the old anchor rather than looser: the user dir is
	// the one directory the agent cannot reach at all (the ganglion binds each
	// workspace separately and nothing above them), while every path beneath it
	// that this function creates IS inside a tree the agent can write. The
	// os.Root is what stops a symlink planted at workspace-<id>/sessions from
	// redirecting a root-owned MkdirAll.
	root := userDir

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
		// The SAME helper the segment resolution uses, with no trimming: both
		// harnesses name a project's workspace identically now, which is the
		// whole point of the change that made them siblings. Deriving it twice
		// is how a seeder and a reader come to disagree about where a transcript
		// lives.
		rel := config.ProjectWorkspace(p.ID)
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

// ganglionProjectPrefix is what a project workspace's name starts with,
// matching the harness's own domain.ProjectWorkspacePrefix and picoclaw's
// resolveAgentWorkspace. It is how a sweep tells a project's directory from the
// main workspace beside it -- "workspace" does not start with "workspace-".
const ganglionProjectPrefix = "workspace-"

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
	// Listed through the Root's own fs.FS, so a symlink planted where a project
	// directory should be cannot make this enumerate -- and then delete --
	// somewhere else.
	entries, err := fs.ReadDir(tree.root.FS(), ".")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("scan ganglion projects: %w", err)
	}
	for _, e := range entries {
		// The prefix is what keeps this from touching anything else in the user
		// dir -- config.json, .schedules.json, and the MAIN workspace, which
		// does not start with "workspace-". A sweep anchored one level higher
		// than it used to be has to be that much more careful about what it
		// claims to own.
		id, ok := strings.CutPrefix(e.Name(), ganglionProjectPrefix)
		if !e.IsDir() || !ok || id == "" || live[id] {
			continue
		}
		if err := tree.root.RemoveAll(e.Name()); err != nil {
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
	// The owner marker, which picoclaw's provision writes and this one did not.
	// ownerEmail reads it and ListSubscriptionUsers labels a member from it, so
	// without it every ganglion workspace listed in the admin surface with an
	// empty email.
	if err := writeOwnerFile(containerDir, key, ownerEmail(containerDir)); err != nil {
		return fmt.Errorf("write owner file: %w", err)
	}
	// USER.md, SEEDED and not mounted -- the agent accumulates what it learns
	// about the member there, so a read-only bind would silently disable that
	// write. picoclaw seeds it from the resolved cascade and the ganglion did
	// not, so an operator's injection reached one harness and not the other.
	if err := seedGanglionUserFile(m.cfg, key, containerDir,
		config.TemplatesDir(m.cfg.ContainerDataRoot, agent.Template)); err != nil {
		return fmt.Errorf("seed USER.md: %w", err)
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
	// The shared-file and managed-content SOURCES, for the same reason: a bind
	// whose source does not exist is created by the daemon as an empty
	// root-owned directory the agent cannot even read.
	for _, sm := range sharedFileBinds(m.cfg, key, ganglionMountDest) {
		if err := os.MkdirAll(sm.container, 0o700); err != nil {
			return fmt.Errorf("create shared files dir: %w", err)
		}
		if err := chownTree(sm.container, m.cfg.PicoclawUser); err != nil {
			return fmt.Errorf("chown shared files dir: %w", err)
		}
	}
	if err := m.ensureManagedContent(); err != nil {
		return err
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
		Binds:   ganglionBinds(m.cfg, key, hostDir, m.ganglionProjectIDs(key)),
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
		ganglionBindDrift(m.cfg, m.ganglionProjectIDs(key), st.Binds) ||
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
