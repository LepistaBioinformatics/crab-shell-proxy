package docker

import (
	"context"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"sort"
)

// InstanceState is the closed set of states one workspace can be observed in.
//
// Closed on purpose. The alternative — passing the Docker engine's own status
// string through — would make every consumer pattern-match strings the engine is
// free to change, and would give them no vocabulary at all for the two states
// that are not a container status: a workspace provisioned on disk that has never
// had a container, and a container whose directory is gone.
type InstanceState string

const (
	// InstanceRunning: container exists and is running.
	InstanceRunning InstanceState = "running"
	// InstanceStopped: container exists and is not running (exited, created,
	// paused — the distinction is the engine's business, not a consumer's).
	InstanceStopped InstanceState = "stopped"
	// InstanceProvisioned: the workspace directory exists but no container does.
	// This is the ordinary outcome of POST /v1/accounts, which scaffolds
	// directories and creates nothing — a real state, not a gap in the answer.
	InstanceProvisioned InstanceState = "provisioned"
	// InstanceOrphaned: a managed container whose workspace directory is missing.
	// A stack fault. Reported rather than dropped, because silence here is
	// indistinguishable from the proxy having failed to answer.
	InstanceOrphaned InstanceState = "orphaned"
)

// Instance is one workspace as the inventory sees it.
//
// The tuple is carried explicitly because it cannot be recovered from
// ContainerName: that name hashes (tenant::subs::user) one way, and this type
// exists precisely so a caller without the Docker socket does not have to try.
type Instance struct {
	TenantID      string        `json:"tenant_id"`
	SubsAccID     string        `json:"subs_acc_id"`
	Agent         string        `json:"agent"`
	UserAccID     string        `json:"user_acc_id"`
	ContainerName string        `json:"container_name"`
	Mode          string        `json:"mode"`
	State         InstanceState `json:"state"`
}

// Instances returns every managed workspace: the union of the containers Docker
// reports and the workspace directories on disk.
//
// READ-ONLY, and that is a requirement rather than a property of this
// implementation. It must never reach EnsureRunning, provision,
// resolveAndMaterialize, or anything that can create, start, stop or remove a
// container: a telemetry read that cold-starts a member's agent is a defect.
//
// Both halves already exist for boot reconciliation — the same label filter
// Reconcile trusts, and the same glob existingWorkspaces walks — so this is a
// read-out over machinery that is already there. If this ever seems to need a
// registry, a cache or a background scan, that is the wrong turn.
func (m *Manager) Instances(ctx context.Context) ([]Instance, error) {
	summaries, err := m.docker.List(ctx, LabelManaged+"=true")
	if err != nil {
		return nil, err
	}

	// Keyed by the isolation tuple so the two halves can be matched. The
	// container name would work equally well, but only because it is derived
	// from the tuple — keying on the tuple says why.
	byKey := make(map[WorkspaceKey]Instance, len(summaries))
	for _, s := range summaries {
		key := WorkspaceKey{
			TenantID:  s.Labels[LabelTenant],
			SubsAccID: s.Labels[LabelSubscription],
			Role:      s.Labels[LabelAgent],
			UserAccID: s.Labels[LabelUser],
		}
		state := InstanceStopped
		if s.State == "running" {
			state = InstanceRunning
		}
		name := trimName(s.Names)
		if name == "" {
			name = m.ContainerName(key)
		}
		byKey[key] = Instance{
			TenantID:      key.TenantID,
			SubsAccID:     key.SubsAccID,
			Agent:         key.Role,
			UserAccID:     key.UserAccID,
			ContainerName: name,
			// RESOLVED, not read from LabelMode.
			//
			// The label is stamped at create time and a per-instance override
			// changes nothing about a container that already exists, so the
			// label goes stale the moment an admin changes the setting. It is
			// still written on create -- it costs nothing and reads well in
			// `docker inspect` -- but nothing behavioural may branch on it,
			// which TestNothingBranchesOnTheModeLabel enforces.
			Mode:  string(m.modeForLabelled(key)),
			State: state,
		}
	}

	// The disk half. Every key found here that Docker did not report is
	// provisioned-without-container; every key it did report is confirmed to have
	// a directory, which is what leaves the orphans behind in the map.
	onDisk := make(map[WorkspaceKey]bool)
	for _, agent := range m.cfg.Agents {
		for _, key := range m.existingWorkspaces(agent.Key) {
			onDisk[key] = true
			if _, ok := byKey[key]; ok {
				continue
			}
			byKey[key] = Instance{
				TenantID:      key.TenantID,
				SubsAccID:     key.SubsAccID,
				Agent:         key.Role,
				UserAccID:     key.UserAccID,
				ContainerName: m.ContainerName(key),
				Mode:          string(m.ModeFor(agent, key)),
				State:         InstanceProvisioned,
			}
		}
	}

	out := make([]Instance, 0, len(byKey))
	for key, inst := range byKey {
		if inst.State != InstanceProvisioned && !onDisk[key] {
			inst.State = InstanceOrphaned
		}
		out = append(out, inst)
	}

	// Deterministic order. Not cosmetic: an unordered inventory makes a diff
	// between two polls unreadable, and makes a test assert on map iteration.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.TenantID != b.TenantID {
			return a.TenantID < b.TenantID
		}
		if a.SubsAccID != b.SubsAccID {
			return a.SubsAccID < b.SubsAccID
		}
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		return a.UserAccID < b.UserAccID
	})
	return out, nil
}

// modeForLabelled resolves a mode for a key discovered from container labels,
// where the agent may no longer be configured. An unknown agent has no default
// to fall back to, so the override is reported alone rather than guessed at.
func (m *Manager) modeForLabelled(key WorkspaceKey) config.Mode {
	if agent, ok := m.cfg.Agents[key.Role]; ok {
		return m.ModeFor(agent, key)
	}
	return m.ModeOverride(key)
}
