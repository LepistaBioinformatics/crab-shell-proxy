package docker

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// PERSONA: the identity files picoclaw reads from its workspace root.
//
// They used to be COPIED out of the agent template on first provision and never
// touched again, which meant a user could rewrite the agent's identity and its
// recurring task list, and an operator editing the template reached only users
// provisioned after the edit. Now they are delivered as root-owned read-only bind
// mounts, resolved from an admin cascade that falls back to the template — so
// they are read-only whether or not an admin ever injects one.

var ErrNotPersonaFile = errors.New("not a persona file")

// The files this feature knows about.
var PersonaFiles = []string{"AGENT.md", "SOUL.md", "HEARTBEAT.md", "USER.md"}

// The files that are MOUNTED, i.e. actually made read-only.
//
// USER.md is deliberately absent. The agent accumulates what it learns about the
// user there — the template ships it as a form (Preferences / Personal
// Information / Learning Goals) — so mounting it read-only would silently disable
// that write. What an operator controls for USER.md is the content it is SEEDED
// from; see personaSeedSource.
var PersonaMounted = []string{"AGENT.md", "SOUL.md", "HEARTBEAT.md"}

func IsPersonaFile(name string) bool {
	for _, f := range PersonaFiles {
		if f == name {
			return true
		}
	}
	return false
}

func isPersonaMounted(name string) bool {
	for _, f := range PersonaMounted {
		if f == name {
			return true
		}
	}
	return false
}

// resolvePersonaSources returns, per persona file, the path the content comes
// from — the first that exists in precedence order:
//
//	subscription+agent  →  tenant+agent  →  the agent template
//
// PRECEDENCE, not merge: two AGENT.md files cannot be merged into one identity.
// A file absent from all three is absent from the result, which is what keeps a
// bind from being emitted for it (see personaBinds).
//
// Pure apart from the os.Stat probes, so the rule is testable with plain temp
// dirs — no Docker, no root.
func resolvePersonaSources(cfg *config.Config, key WorkspaceKey, templateDir string) map[string]string {
	layers := []string{
		config.SubscriptionAgentPersonaDir(cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role),
		config.TenantAgentPersonaDir(cfg.ContainerDataRoot, key.TenantID, key.Role),
		filepath.Join(templateDir, "workspace"),
	}
	out := make(map[string]string, len(PersonaFiles))
	for _, name := range PersonaFiles {
		for _, dir := range layers {
			candidate := filepath.Join(dir, name)
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
				out[name] = candidate
				break
			}
		}
	}
	return out
}

type personaBind struct {
	name string
	bind string
}

// personaBinds is the read-only bind set for one workspace: one entry per MOUNTED
// persona file that the effective dir actually holds.
//
// A file with no source anywhere yields NO bind, and that matters: Docker invents
// an empty DIRECTORY at the destination when a bind source is missing, so a
// missing AGENT.md would become a directory named AGENT.md in the workspace root
// — worse than its absence.
//
// Pure so the bind strings are testable without Docker, the way sharedFileBinds
// already is.
func personaBinds(cfg *config.Config, key WorkspaceKey, mountDest string) []personaBind {
	containerDir := config.EffectivePersonaDir(cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role)
	hostDir := config.EffectivePersonaDir(cfg.HostDataRoot, key.TenantID, key.SubsAccID, key.Role)
	out := make([]personaBind, 0, len(PersonaMounted))
	for _, name := range PersonaMounted {
		if info, err := os.Stat(filepath.Join(containerDir, name)); err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, personaBind{
			name: name,
			bind: filepath.Join(hostDir, name) + ":" + mountDest + "/workspace/" + name + ":ro",
		})
	}
	return out
}

// personaBindStrings is personaBinds reduced to the bind strings the container
// spec takes.
func personaBindStrings(cfg *config.Config, key WorkspaceKey, mountDest string) []string {
	binds := personaBinds(cfg, key, mountDest)
	out := make([]string, 0, len(binds))
	for _, b := range binds {
		out = append(out, b.bind)
	}
	return out
}

// personaMountDests is the set of persona destinations inside a container that a
// bind list currently covers — the DESTINATION side only, and only the paths this
// feature owns.
//
// Destinations, never whole bind strings: a bind embeds HostDataRoot and
// PicoclawHome, so comparing strings would read an operator changing either as
// fleet-wide drift and recreate every container — mass session truncation from a
// settings edit. The destination answers the question that actually matters: is
// this file mounted at all.
func personaMountDests(binds []string, mountDest string) map[string]bool {
	prefix := mountDest + "/workspace/"
	out := map[string]bool{}
	for _, b := range binds {
		parts := strings.Split(b, ":")
		if len(parts) < 2 {
			continue
		}
		dest := parts[1]
		name, ok := strings.CutPrefix(dest, prefix)
		if ok && isPersonaMounted(name) {
			out[dest] = true
		}
	}
	return out
}

// personaBindDrift reports whether a container's persona mounts still match the
// effective set, and is the answer to "an admin saved an identity file and it
// never reached the instances, even after a restart".
//
// A bind set is fixed when the container is CREATED. BounceScope is stop+start by
// design (a recreate truncates the live transcript), so a container created before
// a persona file existed — including every container created by an image predating
// this feature — has no mount for it, and no rewrite of the effective file can ever
// arrive. Nothing short of a recreate fixes that.
//
// Equality, not subset, and safe because the comparison is scoped to the persona
// destinations: a file no layer provides is in neither set. That makes the reverse
// case work too — a cleared injection whose template also lacks the file leaves a
// bind whose source is gone, and Docker then recreates that source as an empty
// DIRECTORY named AGENT.md in the workspace root.
//
// Only meaningful where a successful syncEffectivePersona just ran: expected is
// read off the effective dir, so a transiently empty one would otherwise read as
// "strip the mounts".
func personaBindDrift(cfg *config.Config, key WorkspaceKey, mountDest string, actual []string) bool {
	expected := map[string]bool{}
	for _, b := range personaBinds(cfg, key, mountDest) {
		expected[mountDest+"/workspace/"+b.name] = true
	}
	have := personaMountDests(actual, mountDest)
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

// syncEffectivePersona materializes the resolved persona set into the effective
// dir that the binds read from.
func (m *Manager) syncEffectivePersona(key WorkspaceKey, templateDir string) error {
	dir := config.EffectivePersonaDir(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create effective persona dir: %w", err)
	}
	resolved := resolvePersonaSources(m.cfg, key, templateDir)
	for _, name := range PersonaFiles {
		dst := filepath.Join(dir, name)
		src, ok := resolved[name]
		if !ok {
			// Nothing provides it any more (an injection was deleted and the template
			// has none): drop the stale copy so no bind is emitted for it.
			if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("drop stale persona %s: %w", name, err)
			}
			continue
		}
		if err := writeInPlace(dst, src); err != nil {
			return fmt.Errorf("materialize persona %s: %w", name, err)
		}
	}
	return chownTree(dir, m.cfg.PicoclawUser)
}

// writeInPlace copies src over dst KEEPING dst's inode.
//
// This is load-bearing, not a style choice. A file bind mount pins the inode it
// was created against: write via temp-file-plus-rename and the host gets a new
// inode while the container keeps reading the old one for the whole life of the
// container — an admin's edit would appear to save and never arrive.
//
// The cost is that a reader can observe a partially written file. For identity
// markdown read at agent start that is acceptable, and any write that matters is
// followed by the caller's restart policy anyway.
func writeInPlace(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// SyncEffectivePersonaForScope re-resolves the cascade for every workspace an
// admin write at this scope reaches.
//
// EnsureRunning already re-resolves on every request, so a container picks a new
// injection up on its own. That is NOT enough: the admin handler fires the
// restart policy right after the write, so the container is bounced BEFORE the
// next EnsureRunning — picoclaw would boot reading the previous effective file
// and hold the stale identity until something restarted it again. Re-materialize
// first, then bounce.
func (m *Manager) SyncEffectivePersonaForScope(scope Scope) error {
	subsIDs := []string{scope.SubsAccID}
	if scope.Kind == ScopeTenant {
		all, err := m.ListTenantSubscriptions(scope.TenantID)
		if err != nil {
			return err
		}
		subsIDs = all
	}
	for _, s := range subsIDs {
		for _, agentKey := range m.agentsInScope(scope) {
			agent, ok := m.cfg.Agents[agentKey]
			if !ok {
				continue
			}
			key := WorkspaceKey{TenantID: scope.TenantID, SubsAccID: s, Role: agentKey}
			templateDir := config.TemplatesDir(m.cfg.ContainerDataRoot, agent.Template)
			if err := m.syncEffectivePersona(key, templateDir); err != nil {
				return err
			}
		}
	}
	return nil
}

// personaSeedSource is where a first provision reads USER.md from: the resolved
// cascade rather than the template alone, so an operator's injection becomes the
// starting content. Empty when nothing provides it.
//
// USER.md only. The mounted files are never seeded — see WorkspaceSeed.
func personaSeedSource(cfg *config.Config, key WorkspaceKey, templateDir, name string) string {
	if isPersonaMounted(name) {
		return ""
	}
	return resolvePersonaSources(cfg, key, templateDir)[name]
}

// --- admin CRUD over one scope's persona store ---

func (m *Manager) personaDir(scope Scope) string {
	root := m.cfg.ContainerDataRoot
	if scope.Kind == ScopeTenant {
		return config.TenantAgentPersonaDir(root, scope.TenantID, scope.AgentKey)
	}
	return config.SubscriptionAgentPersonaDir(root, scope.TenantID, scope.SubsAccID, scope.AgentKey)
}

// PersonaEntry is one injected file's metadata. Content is fetched separately.
type PersonaEntry struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modifiedAt"`
}

// ListPersona returns the files injected AT THIS SCOPE — not the resolved
// cascade. An operator editing a scope needs to see what that scope contributes,
// not what a workspace ends up with.
func (m *Manager) ListPersona(scope Scope) ([]PersonaEntry, error) {
	dir := m.personaDir(scope)
	out := []PersonaEntry{}
	for _, name := range PersonaFiles {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, PersonaEntry{
			Name: name, Size: info.Size(), ModifiedAt: modTime(info),
		})
	}
	return out, nil
}

// personaLayer is one step of the read cascade: where to look, and what to call
// the answer if it is found there.
type personaLayer struct {
	source string
	dir    string
}

// personaScopeLayers is the read cascade for a SCOPE — the same precedence
// resolvePersonaSources applies to a workspace, minus the workspace dimension a
// scope does not have:
//
//	subscription scope:  this scope  →  the tenant's injection  →  the template
//	tenant scope:        this scope  →  the template
//
// The template layer is the point: an editor opened where nothing is injected has
// to start from the content the agent actually runs today, not from a blank page,
// or the admin's first save silently replaces an identity they never saw.
func (m *Manager) personaScopeLayers(scope Scope) []personaLayer {
	root := m.cfg.ContainerDataRoot
	layers := make([]personaLayer, 0, 3)
	if scope.Kind == ScopeSubscription {
		layers = append(layers,
			personaLayer{PersonaSourceScope, config.SubscriptionAgentPersonaDir(root, scope.TenantID, scope.SubsAccID, scope.AgentKey)},
			personaLayer{PersonaSourceTenant, config.TenantAgentPersonaDir(root, scope.TenantID, scope.AgentKey)})
	} else {
		layers = append(layers,
			personaLayer{PersonaSourceScope, config.TenantAgentPersonaDir(root, scope.TenantID, scope.AgentKey)})
	}
	// A template layer only exists for a configured agent, and only once that
	// template has been materialized on disk (first provision does it). Absent, the
	// cascade simply ends earlier and the caller gets "nothing anywhere".
	if agent, ok := m.cfg.Agents[scope.AgentKey]; ok {
		layers = append(layers,
			personaLayer{PersonaSourceTemplate, filepath.Join(config.TemplatesDir(root, agent.Template), "workspace")})
	}
	return layers
}

// Where a resolved persona read came from. Reported to the caller because the
// content alone is ambiguous: the admin screen has to be able to say "this is the
// agent template's text, saving it creates an injection here" rather than letting
// one layer's identity pass for another's.
const (
	PersonaSourceScope    = "scope"
	PersonaSourceTenant   = "tenant"
	PersonaSourceTemplate = "template"
)

// ReadPersona returns the content an editor at this scope should start from and
// which layer produced it. os.ErrNotExist when no layer has the file, which the
// handler maps to a 404 exactly as the scope-only read used to.
func (m *Manager) ReadPersona(scope Scope, name string) (string, string, error) {
	if !IsPersonaFile(name) {
		return "", "", ErrNotPersonaFile
	}
	for _, layer := range m.personaScopeLayers(scope) {
		raw, err := os.ReadFile(filepath.Join(layer.dir, name))
		if err == nil {
			return string(raw), layer.source, nil
		}
		if !os.IsNotExist(err) {
			return "", "", err
		}
	}
	return "", "", os.ErrNotExist
}

func (m *Manager) WritePersona(scope Scope, name, body string) error {
	if !IsPersonaFile(name) {
		return ErrNotPersonaFile
	}
	dir := m.personaDir(scope)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create persona dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		return fmt.Errorf("write persona %s: %w", name, err)
	}
	return chownTree(dir, m.cfg.PicoclawUser)
}

// DeletePersona drops the injection so the next cascade layer takes over. It is
// idempotent: removing what is not there is the state the caller asked for.
func (m *Manager) DeletePersona(scope Scope, name string) error {
	if !IsPersonaFile(name) {
		return ErrNotPersonaFile
	}
	err := os.Remove(filepath.Join(m.personaDir(scope), name))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
