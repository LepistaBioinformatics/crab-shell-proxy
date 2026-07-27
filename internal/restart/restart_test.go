package restart

import (
	"testing"
	"time"
)

const (
	tenant = "11111111-1111-1111-1111-111111111111"
	subs   = "22222222-2222-2222-2222-222222222222"
	user   = "33333333-3333-3333-3333-333333333333"
	agent  = "alpha"
)

func at(min int) time.Time {
	return time.Date(2026, 7, 26, 12, min, 0, 0, time.UTC)
}

func newStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(t.TempDir())
}

// A workspace with no marker and no notice is simply not pending.
func TestStatusCleanWhenNoNotice(t *testing.T) {
	s := newStore(t)
	st, err := s.Status(tenant, subs, agent, user)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Pending {
		t.Fatal("no notice raised, yet pending")
	}
}

// FR-3.3: a missing marker reads as older than any notice, so the notice is
// live. This is the safe direction — a spurious banner, never a skipped
// restart.
func TestStatusPendingWhenMarkerMissing(t *testing.T) {
	s := newStore(t)
	if err := s.Raise(tenant, subs, "", Notice{NoticeAt: at(10), Reason: ReasonSharedSecret}); err != nil {
		t.Fatalf("Raise: %v", err)
	}
	st, err := s.Status(tenant, subs, agent, user)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Pending {
		t.Fatal("notice raised with no marker, expected pending")
	}
	if st.Reason != ReasonSharedSecret {
		t.Fatalf("reason = %q, want %q", st.Reason, ReasonSharedSecret)
	}
}

// FR-3.4: any restart advances the marker past the notice and clears it, with
// no per-user record to delete.
func TestStampClearsPending(t *testing.T) {
	s := newStore(t)
	if err := s.Raise(tenant, subs, "", Notice{NoticeAt: at(10), Reason: ReasonModel}); err != nil {
		t.Fatalf("Raise: %v", err)
	}
	if err := s.Stamp(tenant, subs, agent, user, at(11)); err != nil {
		t.Fatalf("Stamp: %v", err)
	}
	st, err := s.Status(tenant, subs, agent, user)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Pending {
		t.Fatal("marker is newer than the notice, expected not pending")
	}
	if st.LastRestartAt == nil || !st.LastRestartAt.Equal(at(11)) {
		t.Fatalf("lastRestartAt = %v, want %v", st.LastRestartAt, at(11))
	}
}

// A marker predating the notice leaves it live: the workspace restarted before
// the change, so it has not applied it.
func TestStampBeforeNoticeStaysPending(t *testing.T) {
	s := newStore(t)
	if err := s.Stamp(tenant, subs, agent, user, at(5)); err != nil {
		t.Fatalf("Stamp: %v", err)
	}
	if err := s.Raise(tenant, subs, "", Notice{NoticeAt: at(10), Reason: ReasonModel}); err != nil {
		t.Fatalf("Raise: %v", err)
	}
	st, err := s.Status(tenant, subs, agent, user)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Pending {
		t.Fatal("marker predates the notice, expected pending")
	}
}

// FR-2.4: each of the four cascade positions must reach the workspace.
func TestResolveReachesEveryCascadePosition(t *testing.T) {
	cases := []struct {
		name             string
		tenantID, subsID string
		agentKey         string
	}{
		{"tenant, all agents", tenant, "", ""},
		{"tenant, this agent", tenant, "", agent},
		{"subscription, all agents", tenant, subs, ""},
		{"subscription, this agent", tenant, subs, agent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			if err := s.Raise(tc.tenantID, tc.subsID, tc.agentKey,
				Notice{NoticeAt: at(10), Reason: ReasonAdminRequest}); err != nil {
				t.Fatalf("Raise: %v", err)
			}
			st, err := s.Status(tenant, subs, agent, user)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if !st.Pending {
				t.Fatalf("notice at %s did not reach the workspace", tc.name)
			}
		})
	}
}

// An agent-scoped notice must NOT leak to a different agent (spec FR-2.4's
// "only that agent's workspaces").
func TestAgentScopedNoticeDoesNotLeak(t *testing.T) {
	s := newStore(t)
	if err := s.Raise(tenant, subs, "beta", Notice{NoticeAt: at(10), Reason: ReasonModel}); err != nil {
		t.Fatalf("Raise: %v", err)
	}
	st, err := s.Status(tenant, subs, agent, user)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Pending {
		t.Fatal("a beta-scoped notice reached an alpha workspace")
	}
}

// Newest wins, not narrowest: a tenant-wide notice raised after a subscription
// one must not be masked by the older, narrower record.
func TestResolveNewestWinsOverNarrowest(t *testing.T) {
	s := newStore(t)
	if err := s.Raise(tenant, subs, agent, Notice{NoticeAt: at(10), Reason: ReasonModel}); err != nil {
		t.Fatalf("Raise narrow: %v", err)
	}
	if err := s.Raise(tenant, "", "", Notice{NoticeAt: at(20), Reason: ReasonSharedSecret}); err != nil {
		t.Fatalf("Raise wide: %v", err)
	}
	n, found, err := s.Resolve(tenant, subs, agent)
	if err != nil || !found {
		t.Fatalf("Resolve: %v found=%v", err, found)
	}
	if n.Reason != ReasonSharedSecret {
		t.Fatalf("reason = %q, want the newer tenant-wide %q", n.Reason, ReasonSharedSecret)
	}
}

// FR-3.5: withdrawing removes the record, and the last withdrawal removes the
// file so an empty tree stays empty.
func TestWithdrawRemovesNotice(t *testing.T) {
	s := newStore(t)
	if err := s.Raise(tenant, subs, "", Notice{NoticeAt: at(10), Reason: ReasonModel}); err != nil {
		t.Fatalf("Raise: %v", err)
	}
	if err := s.Withdraw(tenant, subs, ""); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if _, found, err := s.Resolve(tenant, subs, agent); err != nil || found {
		t.Fatalf("after withdraw: found=%v err=%v", found, err)
	}
}

// Two agents under the same subscription share one file; withdrawing one must
// not disturb the other.
func TestWithdrawKeepsSiblingAgent(t *testing.T) {
	s := newStore(t)
	if err := s.Raise(tenant, subs, "alpha", Notice{NoticeAt: at(10), Reason: ReasonModel}); err != nil {
		t.Fatalf("Raise alpha: %v", err)
	}
	if err := s.Raise(tenant, subs, "beta", Notice{NoticeAt: at(11), Reason: ReasonModel}); err != nil {
		t.Fatalf("Raise beta: %v", err)
	}
	if err := s.Withdraw(tenant, subs, "alpha"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if _, ok, err := s.Get(tenant, subs, "beta"); err != nil || !ok {
		t.Fatalf("beta lost: ok=%v err=%v", ok, err)
	}
}

// FR-6.2: Schedules must return both future and already-elapsed entries — an
// elapsed one fires immediately at boot rather than being dropped.
func TestSchedulesReturnsFutureAndElapsed(t *testing.T) {
	s := newStore(t)
	past, future := at(5), at(90)
	if err := s.Raise(tenant, subs, "alpha", Notice{NoticeAt: at(1), ScheduledAt: &past}); err != nil {
		t.Fatalf("Raise past: %v", err)
	}
	if err := s.Raise(tenant, "", "", Notice{NoticeAt: at(2), ScheduledAt: &future}); err != nil {
		t.Fatalf("Raise future: %v", err)
	}
	if err := s.Raise(tenant, subs, "beta", Notice{NoticeAt: at(3)}); err != nil {
		t.Fatalf("Raise unscheduled: %v", err)
	}

	got, err := s.Schedules()
	if err != nil {
		t.Fatalf("Schedules: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d schedules, want 2 (the unscheduled notice must not appear): %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, ref := range got {
		seen[ref.TenantID+"|"+ref.SubsAccID+"|"+ref.AgentKey] = true
	}
	if !seen[tenant+"|"+subs+"|alpha"] {
		t.Errorf("missing the subscription+agent schedule: %+v", got)
	}
	if !seen[tenant+"||"] {
		t.Errorf("missing the tenant-wide schedule: %+v", got)
	}
}

// Schedules over an empty tree is an empty result, not an error — the normal
// state at first boot.
func TestSchedulesEmptyTree(t *testing.T) {
	s := newStore(t)
	got, err := s.Schedules()
	if err != nil {
		t.Fatalf("Schedules on an empty tree: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d schedules, want 0", len(got))
	}
}

// FR-6.1: firing a scheduled bounce clears ScheduledAt but keeps NoticeAt, so a
// container that was down still shows the notice until its cold start stamps it.
func TestClearScheduleKeepsNotice(t *testing.T) {
	s := newStore(t)
	when := at(90)
	if err := s.Raise(tenant, subs, "", Notice{NoticeAt: at(10), ScheduledAt: &when, Reason: ReasonModel}); err != nil {
		t.Fatalf("Raise: %v", err)
	}
	if err := s.ClearSchedule(tenant, subs, ""); err != nil {
		t.Fatalf("ClearSchedule: %v", err)
	}
	n, ok, err := s.Get(tenant, subs, "")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if n.ScheduledAt != nil {
		t.Error("ScheduledAt should be cleared")
	}
	if !n.NoticeAt.Equal(at(10)) {
		t.Errorf("NoticeAt = %v, want it preserved at %v", n.NoticeAt, at(10))
	}
}

// A scheduled notice surfaces its time to the member (FR-7.2 needs it).
func TestStatusCarriesScheduledAt(t *testing.T) {
	s := newStore(t)
	when := at(90)
	if err := s.Raise(tenant, subs, "", Notice{NoticeAt: at(10), ScheduledAt: &when, Reason: ReasonSharedSkills}); err != nil {
		t.Fatalf("Raise: %v", err)
	}
	st, err := s.Status(tenant, subs, agent, user)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.ScheduledAt == nil || !st.ScheduledAt.Equal(when) {
		t.Fatalf("scheduledAt = %v, want %v", st.ScheduledAt, when)
	}
}
