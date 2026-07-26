// Package restart holds the restart-notice state and answers the one question
// the rest of the proxy asks of it: does this workspace still need a container
// bounce?
//
// The answer is DERIVED, never stored per user:
//
//	pending(workspace) ⇔ lastRestartAt(workspace) < noticeAt(nearest scope notice)
//
// That matters because an admin action targets a SCOPE (tenant / subscription,
// optionally narrowed to one agent) while the workspaces under it may be running,
// scaled to zero, or not yet created. Fanning a flag out at admin-action time
// could never be complete. Deriving it means a container created after the change
// shows no notice (its marker is stamped at create, and a fresh container has by
// definition already applied everything), and a scaled-to-zero one clears itself
// on its next cold start.
//
// State lives outside the tenant tree (see config.RestartRoot): the whole
// UserWorkspace is bind-mounted into the agent container, so a marker kept there
// would be readable and writable by the agent itself.
package restart

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// Reason is the closed set of causes a notice can carry. The proxy ships the
// enum, not a sentence: the phrasing (and its translation) belongs to the
// frontend.
type Reason string

const (
	ReasonSharedSecret Reason = "shared-secret"
	ReasonSharedSkills Reason = "shared-skills"
	ReasonSharedFiles  Reason = "shared-files"
	ReasonModel        Reason = "model"
	ReasonOwnSecret    Reason = "own-secret"
	ReasonAdminRequest Reason = "admin-request"
)

// AllAgents is the record key meaning "every agent in this scope", mirroring
// docker.Scope.AgentKey == "".
const AllAgents = "*"

// Notice is one scope's pending-restart record.
type Notice struct {
	// NoticeAt is when the change that needs a bounce was applied. A workspace
	// whose marker predates it is pending.
	NoticeAt time.Time `json:"noticeAt"`
	// ScheduledAt, when set, is when the proxy will bounce the scope itself.
	ScheduledAt *time.Time `json:"scheduledAt,omitempty"`
	Reason      Reason     `json:"reason"`
	Note        string     `json:"note,omitempty"`
	// By is the requesting admin's email, for traceability only.
	By string `json:"by,omitempty"`
}

// scopeRecord is the on-disk shape: agent key ("*" for all) -> notice.
type scopeRecord struct {
	Agents map[string]Notice `json:"agents"`
}

// marker is the on-disk shape of a workspace's restart state: when it last
// restarted, plus any notice that concerns only this member.
//
// The per-workspace notice lives here rather than in a scope record because
// some changes affect an exact, computed set of workspaces rather than a scope:
// a member's own secret write (only them), and a model re-apply (which skips
// workspaces carrying an explicit pin). Raising a scope notice for either would
// banner people whose instance did not change. It is compared exactly like a
// scope notice — newest wins.
type marker struct {
	LastRestartAt     time.Time `json:"lastRestartAt"`
	WorkspaceNoticeAt time.Time `json:"workspaceNoticeAt,omitempty"`
	WorkspaceReason   Reason    `json:"workspaceReason,omitempty"`
}

// Status is the derived answer handed to a member.
type Status struct {
	Pending       bool       `json:"pending"`
	Reason        Reason     `json:"reason,omitempty"`
	Note          string     `json:"note,omitempty"`
	NoticeAt      *time.Time `json:"noticeAt,omitempty"`
	ScheduledAt   *time.Time `json:"scheduledAt,omitempty"`
	LastRestartAt *time.Time `json:"lastRestartAt,omitempty"`
}

// ScheduleRef is one armed schedule found on disk, the input to the boot-time
// re-arm.
type ScheduleRef struct {
	TenantID  string
	SubsAccID string
	AgentKey  string // "" for all agents
	At        time.Time
	Notice    Notice
}

// Store reads and writes the restart state under a data root. Records are tiny
// and written at human frequency, so a single mutex serializing whole-file
// read-modify-write is enough — no per-file locking to get wrong.
type Store struct {
	root string
	mu   sync.Mutex
}

// NewStore builds a store over the proxy's container data root.
func NewStore(root string) *Store { return &Store{root: root} }

// Raise writes (replacing) the notice for a scope + agent. An empty agentKey
// means every agent in the scope.
func (s *Store) Raise(tenantID, subsAccID, agentKey string, n Notice) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := config.RestartScopeFile(s.root, tenantID, subsAccID)
	rec, err := readScope(path)
	if err != nil {
		return err
	}
	if rec.Agents == nil {
		rec.Agents = map[string]Notice{}
	}
	rec.Agents[agentOrAll(agentKey)] = n
	return writeScope(path, rec)
}

// Withdraw removes the scope + agent notice. Removing the last entry removes
// the file, so an empty tree stays empty.
func (s *Store) Withdraw(tenantID, subsAccID, agentKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := config.RestartScopeFile(s.root, tenantID, subsAccID)
	rec, err := readScope(path)
	if err != nil {
		return err
	}
	delete(rec.Agents, agentOrAll(agentKey))
	if len(rec.Agents) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return writeScope(path, rec)
}

// Get returns the notice recorded at exactly this scope + agent (no cascade) —
// what an admin screen shows and amends.
func (s *Store) Get(tenantID, subsAccID, agentKey string) (Notice, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := readScope(config.RestartScopeFile(s.root, tenantID, subsAccID))
	if err != nil {
		return Notice{}, false, err
	}
	n, ok := rec.Agents[agentOrAll(agentKey)]
	return n, ok, nil
}

// Resolve walks the four cascade positions a workspace sits under — the same
// positions sharedSecretsCascade uses — and returns the NEWEST notice among
// them. Newest rather than narrowest: a tenant-wide notice raised after a
// subscription one must not be masked by the older, narrower record.
func (s *Store) Resolve(tenantID, subsAccID, agentKey string) (Notice, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolveLocked(tenantID, subsAccID, agentKey)
}

func (s *Store) resolveLocked(tenantID, subsAccID, agentKey string) (Notice, bool, error) {
	tenantRec, err := readScope(config.RestartScopeFile(s.root, tenantID, ""))
	if err != nil {
		return Notice{}, false, err
	}
	subsRec, err := readScope(config.RestartScopeFile(s.root, tenantID, subsAccID))
	if err != nil {
		return Notice{}, false, err
	}

	var best Notice
	var found bool
	for _, candidate := range []Notice{
		tenantRec.Agents[AllAgents],
		tenantRec.Agents[agentKey],
		subsRec.Agents[AllAgents],
		subsRec.Agents[agentKey],
	} {
		if candidate.NoticeAt.IsZero() {
			continue
		}
		if !found || candidate.NoticeAt.After(best.NoticeAt) {
			best, found = candidate, true
		}
	}
	return best, found, nil
}

// Stamp records that a workspace has just applied everything pending. The
// workspace-notice fields are preserved, not cleared: the timestamp comparison
// already resolves them, and rewriting history would lose the reason if a
// second notice lands in the same instant.
func (s *Store) Stamp(tenantID, subsAccID, role, userAccID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := config.RestartWorkspaceFile(s.root, tenantID, subsAccID, role, userAccID)
	m, err := readMarker(path)
	if err != nil {
		return err
	}
	m.LastRestartAt = at.UTC()
	return writeJSON(path, m)
}

// RaiseWorkspace records a notice concerning exactly one workspace — a member's
// own secret write (DEC-3), or a model re-apply that touched this workspace and
// not its neighbours. It never reaches anyone else.
func (s *Store) RaiseWorkspace(tenantID, subsAccID, role, userAccID string, reason Reason, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := config.RestartWorkspaceFile(s.root, tenantID, subsAccID, role, userAccID)
	m, err := readMarker(path)
	if err != nil {
		return err
	}
	m.WorkspaceNoticeAt = at.UTC()
	m.WorkspaceReason = reason
	return writeJSON(path, m)
}

// LastRestart returns when the workspace last applied everything. A missing
// marker is the zero time, which reads as "older than any notice" — the safe
// direction: a spurious banner, never a silently skipped restart.
func (s *Store) LastRestart(tenantID, subsAccID, role, userAccID string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := readMarker(config.RestartWorkspaceFile(s.root, tenantID, subsAccID, role, userAccID))
	if err != nil {
		return time.Time{}, err
	}
	return m.LastRestartAt, nil
}

// Status is the derived answer for one workspace. The agent key doubles as the
// cascade's agent narrowing, which is why the workspace's role is passed to
// Resolve.
func (s *Store) Status(tenantID, subsAccID, role, userAccID string) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, found, err := s.resolveLocked(tenantID, subsAccID, role)
	if err != nil {
		return Status{}, err
	}
	m, err := readMarker(config.RestartWorkspaceFile(s.root, tenantID, subsAccID, role, userAccID))
	if err != nil {
		return Status{}, err
	}

	// The workspace's own notice competes with the scope's on the same footing:
	// newest wins, so a shared-secret change after their own secret write
	// replaces the reason they see.
	if !m.WorkspaceNoticeAt.IsZero() && (!found || m.WorkspaceNoticeAt.After(n.NoticeAt)) {
		n = Notice{NoticeAt: m.WorkspaceNoticeAt, Reason: m.WorkspaceReason}
		found = true
	}

	st := Status{}
	if !m.LastRestartAt.IsZero() {
		l := m.LastRestartAt
		st.LastRestartAt = &l
	}
	if !found || !m.LastRestartAt.Before(n.NoticeAt) {
		return st, nil
	}
	at := n.NoticeAt
	st.Pending = true
	st.Reason = n.Reason
	st.Note = n.Note
	st.NoticeAt = &at
	st.ScheduledAt = n.ScheduledAt
	return st, nil
}

// Schedules enumerates every scope record carrying a ScheduledAt — future OR
// already elapsed. The caller (Reconcile) arms the future ones and fires the
// elapsed ones immediately, so a schedule never disappears just because the
// proxy was down when it came due.
func (s *Store) Schedules() ([]ScheduleRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	scopesRoot := filepath.Join(config.RestartRoot(s.root), "scopes")
	var out []ScheduleRef
	err := filepath.WalkDir(scopesRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		rec, err := readScope(path)
		if err != nil {
			return err
		}
		tenantID := filepath.Base(filepath.Dir(path))
		subsAccID := ""
		if base := filepath.Base(path); base != "_tenant.json" {
			subsAccID = base[:len(base)-len(".json")]
		}
		for agentKey, n := range rec.Agents {
			if n.ScheduledAt == nil {
				continue
			}
			ref := ScheduleRef{
				TenantID: tenantID, SubsAccID: subsAccID,
				At: *n.ScheduledAt, Notice: n,
			}
			if agentKey != AllAgents {
				ref.AgentKey = agentKey
			}
			out = append(out, ref)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return out, nil
}

// ClearSchedule drops only the ScheduledAt of a scope + agent notice, keeping
// NoticeAt: the bounce fired, but a container that was down at the time still
// needs its marker to advance, which its cold start does.
func (s *Store) ClearSchedule(tenantID, subsAccID, agentKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := config.RestartScopeFile(s.root, tenantID, subsAccID)
	rec, err := readScope(path)
	if err != nil {
		return err
	}
	key := agentOrAll(agentKey)
	n, ok := rec.Agents[key]
	if !ok || n.ScheduledAt == nil {
		return nil
	}
	n.ScheduledAt = nil
	rec.Agents[key] = n
	return writeScope(path, rec)
}

func agentOrAll(agentKey string) string {
	if agentKey == "" {
		return AllAgents
	}
	return agentKey
}

// readScope returns an empty record for a missing file — an absent notice is
// the normal case, not an error.
func readScope(path string) (scopeRecord, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return scopeRecord{Agents: map[string]Notice{}}, nil
	}
	if err != nil {
		return scopeRecord{}, err
	}
	var rec scopeRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return scopeRecord{}, fmt.Errorf("restart: parse %s: %w", path, err)
	}
	if rec.Agents == nil {
		rec.Agents = map[string]Notice{}
	}
	return rec, nil
}

func readMarker(path string) (marker, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return marker{}, nil
	}
	if err != nil {
		return marker{}, err
	}
	var m marker
	if err := json.Unmarshal(b, &m); err != nil {
		return marker{}, fmt.Errorf("restart: parse %s: %w", path, err)
	}
	return m, nil
}

func writeScope(path string, rec scopeRecord) error { return writeJSON(path, rec) }

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
