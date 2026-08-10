package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/projects"
)

// projectAgents rewrites config.json's agents.list and agents.dispatch from the
// project store. It is the ONLY writer of those two keys.
//
// REBUILD, never merge. Merging would accumulate entries for deleted projects
// and let config.json drift away from the store, which is the whole failure this
// design exists to prevent: the store is the source of truth precisely because
// materializeModels rewrites the entire config on every ensure, so a rule
// written straight into the file is erased on the user's next chat. Rebuilding
// from the store makes the rules self-healing instead.
//
// agents.defaults is deliberately untouched — it belongs to model
// materialization, and a projection that edited it would fight the model
// registry once per chat.
func projectAgents(cfg map[string]any, home string, list []projects.Project) {
	agents := childMap(cfg, "agents")

	// No projects: emit nothing, and clear anything a previous projection left.
	//
	// Not "emit a list holding only main". picoclaw already synthesizes an
	// identical implicit main when agents.list is absent, so writing one would add
	// a key to every existing user's config.json on their next chat — a fleet-wide
	// rewrite, and a drift signal, to say something that was already true.
	if len(list) == 0 {
		delete(agents, "list")
		delete(agents, "dispatch")
		return
	}

	// The explicit default entry is required from here on: a NON-EMPTY agents.list
	// removes picoclaw's implicit main agent, and the default would silently fall
	// to list[0] — the user's first project would answer every unrouted chat.
	entries := make([]any, 0, len(list)+1)
	entries = append(entries, map[string]any{"id": defaultAgentID, "default": true})

	rules := make([]any, 0, len(list))
	for _, p := range list {
		// id, name and workspace ONLY. No `model`, no `skills`, no `subagents`:
		// every one of those is inherited by being absent. agents.defaults cascades
		// into each list entry, and a nil skills filter means "all skills", which is
		// how a project picks up the admin shared-skills cascade for free. Writing
		// them would freeze the project against changes the parent still receives.
		entries = append(entries, map[string]any{
			"id":        p.ID,
			"name":      p.Name,
			"workspace": projectWorkspacePath(home, p.ID),
		})
		rules = append(rules, map[string]any{
			"name":  "proj-" + p.ID,
			"agent": p.ID,
			"when": map[string]any{
				"channel": "pico",
				"chat":    identity.ProjectChatPattern(p.ID),
			},
		})
	}

	agents["list"] = entries
	// No catch-all rule is emitted. A message matching nothing falls through to
	// the default agent on its own, which is what makes rule ORDER irrelevant here
	// — worth keeping, because picoclaw takes the first matching rule and an
	// ordering bug in generated config would be invisible until it misrouted
	// someone.
	agents["dispatch"] = map[string]any{"rules": rules}
}

// defaultAgentID mirrors picoclaw's routing.DefaultAgentID.
const defaultAgentID = "main"

// projectWorkspacePath is the in-container workspace of one project's agent.
//
// It must match the shape picoclaw derives for a named agent that declares no
// workspace (<defaults.workspace>/../workspace-<id>), because the agent's own
// tooling resolves paths independently of what we write here. Writing it
// explicitly keeps the two in agreement rather than relying on that derivation.
func projectWorkspacePath(home, projectID string) string {
	return home + "/.picoclaw/" + config.ProjectWorkspace(projectID)
}

// projectStore opens the project list for one workspace. Cheap and stateless —
// the Store is a path plus a lock, so there is nothing to cache, and reading
// fresh on every ensure is what keeps config.json converging on the store.
func (m *Manager) projectStore(key WorkspaceKey) *projects.Store {
	return projects.NewStore(config.ProjectsFile(
		m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID))
}

// projectWorkspaceDirs are created empty on the host at seed time. picoclaw
// would create the workspace root itself, but as root-owned and bare — these
// have to exist, and be owned by the agent uid, before it writes anything.
var projectWorkspaceDirs = []string{"sessions", MemoryDirName, "uploads"}

// projectPersonaRefreshed are re-copied into every project workspace on every
// ensure, so an admin persona change reaches project agents the same way it
// reaches the main one.
//
// Copies, not binds. The main workspace mounts these read-only, but a bind per
// file per project would multiply mounts with no matching benefit — they are
// small, and the read-only bind's effect (an agent edit does not survive) is
// reproduced by overwriting them here.
//
// AGENT.md is absent because a project's is COMPOSED rather than copied, and
// USER.md because it is the one file the agent OWNS — see projectPersonaSeeded.
var projectPersonaRefreshed = []string{"SOUL.md", "HEARTBEAT.md"}

// projectPersonaSeeded are written only when MISSING.
//
// USER.md is the one identity file the main workspace deliberately leaves
// writable — it is excluded from PersonaMounted precisely because the agent
// accumulates what it learns about the user in it, and provision.go seeds it
// once so "a returning user's evolved files are never clobbered".
//
// A project agent builds its own picture of the user, in its own workspace.
// Refreshing this file on every ensure — which is what an earlier version of
// this code did — would erase that on the user's next message, silently and
// repeatedly.
var projectPersonaSeeded = []string{"USER.md"}

// seedProjectWorkspace brings one project's workspace to its intended state.
// Idempotent, and called on every ensure rather than only at creation: the
// workspace is then self-healing, the way the dispatch rules are.
//
// effPersonaDir is the RESOLVED persona set (scope → tenant → template), already
// materialized by syncEffectivePersona.
// Every write below lands in a tree the project's own agent owns and can reshape
// between two ensures — the workspace is bind-mounted read-write into its
// container and chowned to its uid. So the whole seed runs inside an os.Root
// anchored at the project workspace: a `sessions`, `memory` or `AGENT.md`
// component replaced by a symlink fails the syscall rather than redirecting a
// root-owned write out of the workspace.
func seedProjectWorkspace(userDir, effPersonaDir string, p projects.Project, user string) error {
	rootPath := filepath.Join(userDir, config.ProjectWorkspace(p.ID))
	tree, err := openTree(rootPath)
	if err != nil {
		return err
	}
	defer tree.Close()

	for _, sub := range projectWorkspaceDirs {
		if err := tree.root.MkdirAll(sub, 0o700); err != nil {
			return fmt.Errorf("create project dir %s: %w", sub, err)
		}
	}

	for _, name := range projectPersonaRefreshed {
		src := filepath.Join(effPersonaDir, name)
		if !fileExists(src) {
			continue // nothing provides it; absent is a valid state
		}
		if err := copyIntoTree(tree, src, name); err != nil {
			return fmt.Errorf("refresh project persona %s: %w", name, err)
		}
	}
	for _, name := range projectPersonaSeeded {
		// Lstat, not Stat: a symlink standing where USER.md belongs must count as
		// "already there" for the seeding decision — following it to decide would
		// mean deciding based on a file outside the workspace.
		if _, statErr := tree.root.Lstat(name); statErr == nil {
			continue // the agent has been writing here; leave it alone
		}
		src := filepath.Join(effPersonaDir, name)
		if !fileExists(src) {
			continue
		}
		if err := copyIntoTree(tree, src, name); err != nil {
			return fmt.Errorf("seed project persona %s: %w", name, err)
		}
	}

	agentMD, err := composeProjectAgentMD(filepath.Join(effPersonaDir, "AGENT.md"), p)
	if err != nil {
		return err
	}
	if err := tree.root.WriteFile("AGENT.md", []byte(agentMD), 0o600); err != nil {
		return fmt.Errorf("write project AGENT.md: %w", err)
	}

	return chownTree(rootPath, user)
}

// composeProjectAgentMD builds a project's AGENT.md: the parent's frontmatter
// verbatim, then the project's instructions as the body.
//
// Keeping the frontmatter is the inheritance. picoclaw parses it for the agent's
// declared tools, skills and model override, and a project that dropped it would
// quietly lose whatever the parent's identity declares — which is the opposite of
// "a project inherits everything from its parent".
//
// The file is fully DERIVED — frontmatter from the persona cascade, body from the
// project store — so it is recomposed on every ensure rather than written once.
// Nothing authored by a human lives only here. The tradeoff is that the agent can
// write to this path (it is a copy, where the main workspace uses a read-only
// bind) and such an edit is reverted on the next ensure. That is the same
// outcome the bind produces, reached a moment later.
func composeProjectAgentMD(parentPath string, p projects.Project) (string, error) {
	var frontmatter string
	raw, err := os.ReadFile(parentPath)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("read parent AGENT.md: %w", err)
	}
	if err == nil {
		frontmatter, _ = splitFrontmatter(string(raw))
	}

	frontmatter = withProjectMCPAllowlist(frontmatter, p.ID)

	var b strings.Builder
	if frontmatter != "" {
		b.WriteString(frontmatter)
		if !strings.HasSuffix(frontmatter, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("# " + p.Name + "\n\n")
	instructions := strings.TrimSpace(p.Instructions)
	if instructions == "" {
		// An empty project is legitimate — a user may create one and add
		// instructions later — but the agent should be told what it is looking at
		// rather than reading a bare heading.
		instructions = "This project has no instructions yet."
	}
	b.WriteString(instructions)
	b.WriteString("\n")
	return b.String(), nil
}

// splitFrontmatter separates a leading YAML frontmatter block from the body.
// Returns ("", whole) when there is none.
//
// The closing delimiter is searched for explicitly rather than assumed: a body
// that merely CONTAINS a "---" line (a markdown horizontal rule, or a project's
// own instructions) must not be mistaken for the end of a frontmatter block that
// never opened.
func splitFrontmatter(doc string) (frontmatter, body string) {
	const delim = "---"
	if !strings.HasPrefix(doc, delim+"\n") && !strings.HasPrefix(doc, delim+"\r\n") {
		return "", doc
	}
	rest := doc[len(delim):]
	rest = strings.TrimLeft(rest, "\r")
	rest = strings.TrimPrefix(rest, "\n")

	for offset := 0; offset < len(rest); {
		lineEnd := strings.IndexByte(rest[offset:], '\n')
		var line string
		if lineEnd < 0 {
			line = rest[offset:]
		} else {
			line = rest[offset : offset+lineEnd]
		}
		if strings.TrimRight(line, "\r") == delim {
			end := offset + len(line)
			if lineEnd >= 0 {
				end = offset + lineEnd + 1
			}
			return doc[:len(doc)-len(rest)+end], rest[end:]
		}
		if lineEnd < 0 {
			break
		}
		offset += lineEnd + 1
	}
	// Unterminated block: treat the whole document as body rather than swallowing
	// it as frontmatter.
	return "", doc
}

// removeProjectWorkspace deletes one project's workspace, transcripts included.
// Separate from the store delete on purpose: a failed removal must not leave a
// record pointing at a directory that is half gone.
func removeProjectWorkspace(userDir, projectID string) error {
	return os.RemoveAll(filepath.Join(userDir, config.ProjectWorkspace(projectID)))
}

// syncProjectWorkspaces brings every project workspace to its intended state,
// and removes the ones whose project is gone.
//
// Called on the same ensure path as syncEffectivePersona and for the same
// reason: the workspaces are derived state, so re-deriving them each time is what
// lets an admin persona change, a restored backup or a half-finished delete
// converge on their own.
func (m *Manager) syncProjectWorkspaces(key WorkspaceKey, userDir string) error {
	list, err := m.projectStore(key).List()
	if err != nil {
		return fmt.Errorf("read projects: %w", err)
	}

	effPersona := config.EffectivePersonaDir(
		m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role)

	live := make(map[string]bool, len(list))
	for _, p := range list {
		live[config.ProjectWorkspace(p.ID)] = true
		if err := seedProjectWorkspace(userDir, effPersona, p, m.cfg.PicoclawUser); err != nil {
			return fmt.Errorf("seed project %s: %w", p.ID, err)
		}
	}

	// Sweep orphans. A delete removes the record first and the directory second,
	// so a crash between the two leaves a workspace with no project — invisible to
	// the user, still holding their transcripts, and never cleaned up otherwise.
	entries, err := os.ReadDir(userDir)
	if err != nil {
		return fmt.Errorf("scan user dir: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !strings.HasPrefix(name, "workspace-") || live[name] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(userDir, name)); err != nil {
			return fmt.Errorf("remove orphaned %s: %w", name, err)
		}
		m.logf("workspace %s/%s: removed orphaned project dir %s", key.Role, key.UserAccID, name)
	}
	return nil
}

// projectSecretsBinds is one read-only .secrets mount per project, pointing at
// the SAME effective secrets dir the main workspace uses.
//
// Same source, several destinations: a project inherits its parent's
// credentials rather than owning any, so there is nothing per-project to sync.
// It has to be a bind and not a copy because syncEffectiveSecrets rewrites that
// directory on every ensure — a copy would be a snapshot that silently stopped
// tracking the cascade.
func projectSecretsBinds(effHost, mountDest string, list []projects.Project) []string {
	out := make([]string, 0, len(list))
	for _, p := range list {
		out = append(out, effHost+":"+projectSecretsDest(mountDest, p.ID)+":ro")
	}
	return out
}

func projectSecretsDest(mountDest, projectID string) string {
	return mountDest + "/" + config.ProjectWorkspace(projectID) + "/.secrets"
}

// projectBindDrift reports whether a container's bind set no longer matches the
// project list. Bind sets are fixed at create time, so a project created after
// the container was built has NO .secrets mount and never will — its agent runs
// without credentials, which reads as a picoclaw misconfiguration rather than as
// a missing mount. Only a recreate fixes it.
//
// Compares DESTINATIONS, not whole bind strings, mirroring personaMountDests for
// the same reason: a bind string embeds HostDataRoot, so comparing those would
// read an operator moving the data root as drift on every workspace at once and
// recreate the entire fleet.
func projectBindDrift(mountDest string, list []projects.Project, actual []string) bool {
	expected := make(map[string]bool, len(list))
	for _, p := range list {
		expected[projectSecretsDest(mountDest, p.ID)] = true
	}

	have := map[string]bool{}
	prefix := mountDest + "/workspace-"
	for _, b := range actual {
		parts := strings.Split(b, ":")
		if len(parts) < 2 {
			continue
		}
		if dest := parts[1]; strings.HasPrefix(dest, prefix) {
			have[dest] = true
		}
	}

	if len(expected) != len(have) {
		return true
	}
	for dest := range expected {
		if !have[dest] {
			return true
		}
	}
	return false
}

// projectBindDriftFor is the Manager-side wrapper: it reads the current project
// list and compares it against a container's actual binds.
//
// A store read failure returns FALSE rather than propagating. Drift detection is
// an optimization on top of a container that is already running; answering
// "recreate" on a transient read error would destroy a live conversation to fix
// something that may not be broken. The real read, the one that matters, happens
// in create().
// imageDrift reports whether a container is running something other than what
// PicoclawImage resolves to right now.
//
// Compares resolved IDS, not the tag. The tag is what create() was given and it
// does not change when the image behind it is rebuilt — which is the case that
// matters, because this stack builds its own harness image under a fixed tag
// (deploy/picoclaw-glob). A name comparison would have reported "no drift" for the
// exact upgrade this exists to catch.
//
// Answers FALSE rather than propagating whenever the desired id cannot be
// established — the image is not present locally, or the daemon errors. Recreating
// on "I don't know" would destroy a live conversation to install nothing: the
// container is already running a working image, and create() calls EnsureImage,
// which is where a genuinely missing image is pulled or fails loudly.
func (m *Manager) imageDrift(ctx context.Context, st ContainerState) bool {
	if st.Image == "" {
		return false // an older daemon, or a fake in a test that does not model it
	}
	want, err := m.docker.ImageID(ctx, m.cfg.PicoclawImage)
	if err != nil {
		m.logf("image drift check skipped for %s: %v", m.cfg.PicoclawImage, err)
		return false
	}
	if want == "" {
		return false // not present locally yet; EnsureImage owns that case
	}
	return st.Image != want
}

func (m *Manager) projectBindDriftFor(key WorkspaceKey, actual []string) bool {
	list, err := m.projectStore(key).List()
	if err != nil {
		m.logf("workspace %s/%s: project drift check skipped: %v", key.Role, key.UserAccID, err)
		return false
	}
	return projectBindDrift(m.picoclawMountDest(), list, actual)
}

// --- project operations (the surface the HTTP layer calls) ---

func (m *Manager) userDir(key WorkspaceKey) string {
	return config.UserWorkspace(m.cfg.ContainerDataRoot,
		key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
}

// ListProjects returns one workspace's projects.
func (m *Manager) ListProjects(key WorkspaceKey) ([]projects.Project, error) {
	return m.projectStore(key).List()
}

// CreateProject records the project and seeds its workspace.
//
// It does NOT bounce the container. A new project needs a new bind mount, which
// Docker cannot add to a running container — but projectBindDrift already
// detects exactly that, so the recreate happens on the user's next request,
// which is also the first moment the project could possibly be used. Forcing it
// here would interrupt a conversation the user is still having to prepare one
// they have not started.
func (m *Manager) CreateProject(key WorkspaceKey, name, instructions string) (projects.Project, error) {
	p, err := m.projectStore(key).Create(name, instructions, time.Now())
	if err != nil {
		return projects.Project{}, err
	}
	if err := m.seedOne(key, p); err != nil {
		return projects.Project{}, err
	}
	return p, nil
}

// RenameProject changes the display name. No bounce and no new mount: the id,
// the workspace path and the dispatch rule are all untouched.
func (m *Manager) RenameProject(key WorkspaceKey, id, name string) (projects.Project, error) {
	p, err := m.projectStore(key).Rename(id, name)
	if err != nil {
		return projects.Project{}, err
	}
	return p, m.seedOne(key, p)
}

// SetProjectInstructions rewrites the project's AGENT.md body. No bounce: the
// agent reads AGENT.md at turn time, so the change lands on the next message.
func (m *Manager) SetProjectInstructions(key WorkspaceKey, id, instructions string) (projects.Project, error) {
	p, err := m.projectStore(key).SetInstructions(id, instructions)
	if err != nil {
		return projects.Project{}, err
	}
	return p, m.seedOne(key, p)
}

// DeleteProject removes the record, then the workspace and its transcripts.
//
// Record first: an interrupted delete then leaves an orphaned directory, which
// syncProjectWorkspaces sweeps on the next ensure. The other order would leave a
// record pointing at a workspace that is already gone — a project that lists,
// routes, and fails on use.
func (m *Manager) DeleteProject(key WorkspaceKey, id string) error {
	if err := m.projectStore(key).Delete(id); err != nil {
		return err
	}
	return removeProjectWorkspace(m.userDir(key), id)
}

// HasProject reports whether the id exists. The HTTP layer checks this BEFORE
// starting a container: an unknown project must be a 404, not a silent fallback
// to the default agent, which would write the user's conversation into the main
// workspace and read afterwards as lost history rather than a bad request.
func (m *Manager) HasProject(key WorkspaceKey, id string) (bool, error) {
	if id == "" {
		return true, nil
	}
	if _, err := m.projectStore(key).Get(id); err != nil {
		if errors.Is(err, projects.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (m *Manager) seedOne(key WorkspaceKey, p projects.Project) error {
	effPersona := config.EffectivePersonaDir(
		m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role)
	if err := seedProjectWorkspace(m.userDir(key), effPersona, p, m.cfg.PicoclawUser); err != nil {
		return fmt.Errorf("seed project %s: %w", p.ID, err)
	}
	return nil
}

// withProjectMCPAllowlist adds `mcpServers: [memory-<id>]` to a project's
// frontmatter.
//
// THE ONE DELIBERATE EDIT to the inherited frontmatter, and it is here rather
// than anywhere else because nothing else can do the job. tools.mcp.servers is
// global to the container: every agent reads the same block, so a per-project
// knowledge graph cannot come from the token alone. picoclaw's per-agent lever is
// exactly this allowlist (pkg/agent/tool_allowlist.go resolveAgentMCPServerAllowlist,
// frontmatter field `mcpServers`), which filters servers BY NAME. Naming only its
// own server is what locks a project agent to its own graph.
//
// ASYMMETRIC, knowingly. AllowsMCPServer returns true when the allowlist is nil,
// and the MAIN agent's AGENT.md comes from the admin persona cascade — not this
// proxy's file to edit. So the main agent can still reach a project's memory
// server. That is a context boundary, not a security one: both agents belong to
// the same user, in the same workspace, holding the same credentials. Closing it
// would mean writing into the file an admin edits on the Identity screen.
//
// An existing `mcpServers:` line is REPLACED, not appended to. A parent that
// already restricts its servers would otherwise hand the project a list naming
// servers plus its own graph, and the project would read the parent's memory
// through them.
func withProjectMCPAllowlist(frontmatter, projectID string) string {
	entry := "mcpServers: [" + ProjectMCPServerName(projectID) + "]"

	if frontmatter == "" {
		return "---\n" + entry + "\n---\n"
	}

	lines := strings.Split(strings.TrimSuffix(frontmatter, "\n"), "\n")
	out := make([]string, 0, len(lines)+1)
	inserted := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "mcpServers:") {
			out = append(out, entry)
			inserted = true
			continue
		}
		// Before the closing delimiter, which is the last line.
		if i == len(lines)-1 && !inserted && strings.TrimSpace(line) == "---" {
			out = append(out, entry, line)
			inserted = true
			continue
		}
		out = append(out, line)
	}
	if !inserted {
		out = append(out, entry)
	}
	return strings.Join(out, "\n") + "\n"
}
