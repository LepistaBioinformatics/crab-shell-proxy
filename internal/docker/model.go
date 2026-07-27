package docker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/registry"
)

// This file holds the re-apply entry points only. Model RESOLUTION lives in
// internal/registry and nowhere else: the previous version of this file resolved
// models from config.yaml while registered_models.go resolved them from disk, and
// neither knew about the other — so a scope re-apply silently overwrote a per-user
// assignment with no error and no way to recover the lost choice.

// ReapplyModelScope re-materializes the established workspaces under scope that a
// scope-default change actually affects, and restarts only those.
//
// A workspace with an EXPLICIT per-user pin is skipped entirely: its resolution did
// not change, so re-materializing would be a no-op — but restarting it would not.
// Bouncing someone's agent because a sibling's default changed is exactly what
// "workspaces with an explicit assignment are untouched" forbids. A workspace whose
// pinned MODEL changed is handled by ReapplyModelForModel, not by this path.
//
// A workspace that was never provisioned is skipped too: resolution already applies
// at its first provision.
//
// Per-workspace failures are logged and skipped rather than returned, so one bad
// workspace does not block the pass for the others.
func (m *Manager) ReapplyModelScope(scope Scope) error {
	keys, err := m.scopeWorkspaceKeys(scope)
	if err != nil {
		return err
	}
	for _, key := range keys {
		// "No pin" and "could not read the pin" are different answers. Treating a
		// read failure as "no pin" would re-materialize and RESTART a workspace
		// whose deliberate per-user choice this pass is supposed to leave alone, so
		// an unreadable record skips the workspace instead of overriding it.
		switch a, err := m.reg.GetAssignment(m.workspaceRef(key)); {
		case err == nil:
			if a.Source == registry.SourceExplicit {
				continue
			}
		case errors.Is(err, registry.ErrNotFound):
			// Never materialized, or no record yet: nothing pinned to protect.
		default:
			m.logf("reapply model scope: workspace %+v: read assignment: %v — skipped", key, err)
			continue
		}
		if err := m.reapplyWorkspace(key); err != nil {
			m.logf("reapply model scope: workspace %+v: %v", key, err)
			continue
		}
		if err := m.RestartWorkspace(key); err != nil {
			m.logf("reapply model scope: restart %+v: %v", key, err)
		}
	}
	return nil
}

// ReapplyModelUser re-materializes one workspace and restarts it if running
// (RestartWorkspace is a no-op when it is not — the next cold start picks up what
// is already on disk).
func (m *Manager) ReapplyModelUser(key WorkspaceKey) error {
	if err := m.reapplyWorkspace(key); err != nil {
		return err
	}
	return m.RestartWorkspace(key)
}

// ReapplyModelForModel re-materializes every workspace whose materialized set
// contains modelName — as primary OR as a chain member — and restarts them.
//
// The chain half is load-bearing: an api_base or key edit that reached only
// primaries would leave every workspace holding the model as a fallback on a
// stale or revoked credential, which is exactly the failure fallback exists to
// prevent.
func (m *Manager) ReapplyModelForModel(modelName string) error {
	refs, err := m.reg.WorkspacesUsing(modelName)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		key := WorkspaceKey{
			TenantID: ref.TenantID, SubsAccID: ref.SubsAccID,
			Role: ref.Agent, UserAccID: ref.UserAccID,
		}
		if err := m.reapplyWorkspace(key); err != nil {
			m.logf("reapply model %q: workspace %+v: %v", modelName, key, err)
			continue
		}
		if err := m.RestartWorkspace(key); err != nil {
			m.logf("reapply model %q: restart %+v: %v", modelName, key, err)
		}
	}
	return nil
}

// reapplyWorkspace re-materializes one ALREADY-PROVISIONED workspace. A missing
// config.json means it has never been provisioned, which is a no-op rather than
// an error.
func (m *Manager) reapplyWorkspace(key WorkspaceKey) error {
	userDir := config.UserWorkspace(m.cfg.ContainerDataRoot, key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if _, err := os.Stat(filepath.Join(userDir, "config.json")); err != nil {
		return nil
	}
	return m.resolveAndMaterialize(key, userDir)
}

// scopeWorkspaceKeys enumerates every discovered WorkspaceKey under scope:
// ListSubscriptionUsers for a single subscription, or the
// tenants/<t>/subscriptions/*/agents/*/users/* glob for a whole tenant.
func (m *Manager) scopeWorkspaceKeys(scope Scope) ([]WorkspaceKey, error) {
	if scope.Kind == ScopeSubscription {
		users, err := m.ListSubscriptionUsers(scope.TenantID, scope.SubsAccID)
		if err != nil {
			return nil, err
		}
		keys := make([]WorkspaceKey, 0, len(users))
		for _, u := range users {
			keys = append(keys, WorkspaceKey{TenantID: scope.TenantID, SubsAccID: scope.SubsAccID, Role: u.Role, UserAccID: u.AccID})
		}
		return keys, nil
	}
	pattern := filepath.Join(m.cfg.ContainerDataRoot, "tenants", identity.SanitizeID(scope.TenantID),
		"subscriptions", "*", "agents", "*", "users", "*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	var keys []WorkspaceKey
	for _, uw := range matches {
		fi, statErr := os.Stat(uw)
		if statErr != nil || !fi.IsDir() {
			continue
		}
		rel, err := filepath.Rel(m.cfg.ContainerDataRoot, uw)
		if err != nil {
			continue
		}
		// rel = tenants/<t>/subscriptions/<s>/agents/<role>/users/<u>
		parts := strings.Split(rel, string(os.PathSeparator))
		if len(parts) != 8 {
			continue
		}
		keys = append(keys, WorkspaceKey{TenantID: parts[1], SubsAccID: parts[3], Role: parts[5], UserAccID: parts[7]})
	}
	return keys, nil
}

// SetModelAssignment pins one workspace to a model and re-materializes it. The
// pin is EXPLICIT, which is what makes it survive later scope-default changes.
func (m *Manager) SetModelAssignment(key WorkspaceKey, modelName string) error {
	ref := m.workspaceRef(key)
	if err := m.reg.PutAssignment(ref, registry.Assignment{
		ModelName: modelName, Source: registry.SourceExplicit,
	}); err != nil {
		return err
	}
	return m.ReapplyModelUser(key)
}

// ClearModelAssignment drops a per-user pin so the workspace falls back to its
// scope default, then re-materializes it. The assignment is re-created as
// INHERITED by the re-materialization, which is how the inventory keeps knowing
// what this workspace runs.
func (m *Manager) ClearModelAssignment(key WorkspaceKey) error {
	if err := m.reg.DeleteAssignment(m.workspaceRef(key)); err != nil {
		return err
	}
	return m.ReapplyModelUser(key)
}

// setModelListEntry upserts model_list[name] = {api_keys: [apiKey]} into the
// parsed .security.yml, creating model_list only if absent and leaving every
// sibling key untouched. materializeModels is its only caller.
func setModelListEntry(sec map[string]any, name, apiKey string) {
	ml, ok := sec["model_list"].(map[string]any)
	if !ok {
		ml = map[string]any{}
		sec["model_list"] = ml
	}
	ml[name] = map[string]any{"api_keys": []string{apiKey}}
}
