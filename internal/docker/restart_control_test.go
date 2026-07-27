package docker

import (
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/restart"
)

// FR-1.4: a member who presses the button while their container is scaled to
// zero must still get their notice cleared — the next cold start applies
// everything, so there is nothing left pending. Without the stamp the banner
// would be unclearable by the only action offered.
func TestRestartWorkspaceStampsWhenContainerAbsent(t *testing.T) {
	f := newFakeDocker()
	m, _ := testManager(t, config.ModeScaleToZero, f)
	key := wk("absent")

	if err := m.Restarts().Raise(key.TenantID, key.SubsAccID, "",
		restart.Notice{NoticeAt: time.Now().Add(-time.Minute), Reason: restart.ReasonOwnSecret}); err != nil {
		t.Fatalf("Raise: %v", err)
	}

	if err := m.RestartWorkspace(key); err != nil {
		t.Fatalf("RestartWorkspace on an absent container: %v", err)
	}
	if f.stopN != 0 || f.startN != 0 {
		t.Errorf("absent container was touched: stop=%d start=%d", f.stopN, f.startN)
	}

	st, err := m.Restarts().Status(key.TenantID, key.SubsAccID, key.Role, key.UserAccID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Pending {
		t.Error("notice still pending after a no-op restart — the member cannot clear it")
	}
}

// FR-6.3: re-scheduling the same scope replaces the pending timer rather than
// stacking a second one.
func TestArmScheduledBounceReplacesPendingTimer(t *testing.T) {
	m, _ := testManager(t, config.ModeContinuous, newFakeDocker())
	scope := Scope{Kind: ScopeSubscription, TenantID: "t1", SubsAccID: "s1"}

	m.ArmScheduledBounce(scope, time.Now().Add(time.Hour))
	m.ArmScheduledBounce(scope, time.Now().Add(2*time.Hour))

	m.schedMu.Lock()
	n := len(m.sched)
	m.schedMu.Unlock()
	if n != 1 {
		t.Fatalf("armed timers = %d, want exactly 1 (re-arming must not stack)", n)
	}
	m.CancelScheduledBounce(scope)
}

// An admin withdrawing a scheduled restart must actually disarm it.
func TestCancelScheduledBounceDisarms(t *testing.T) {
	m, _ := testManager(t, config.ModeContinuous, newFakeDocker())
	scope := Scope{Kind: ScopeTenant, TenantID: "t1"}

	m.ArmScheduledBounce(scope, time.Now().Add(time.Hour))
	m.CancelScheduledBounce(scope)

	m.schedMu.Lock()
	n := len(m.sched)
	m.schedMu.Unlock()
	if n != 0 {
		t.Fatalf("armed timers = %d after cancel, want 0", n)
	}
}

// FR-6.1: when a scheduled bounce fires it clears ScheduledAt but keeps
// NoticeAt, so a container that was down at the appointed time still shows the
// notice until its own cold start stamps its marker.
func TestScheduledBounceFiresAndClearsOnlyTheSchedule(t *testing.T) {
	m, _ := testManager(t, config.ModeContinuous, newFakeDocker())
	scope := Scope{Kind: ScopeSubscription, TenantID: "t1", SubsAccID: "s1"}

	noticeAt := time.Now().Add(-time.Minute).UTC()
	when := time.Now().Add(5 * time.Millisecond)
	if err := m.Restarts().Raise(scope.TenantID, scope.SubsAccID, "",
		restart.Notice{NoticeAt: noticeAt, ScheduledAt: &when, Reason: restart.ReasonModel}); err != nil {
		t.Fatalf("Raise: %v", err)
	}

	m.ArmScheduledBounce(scope, when)

	deadline := time.Now().Add(2 * time.Second)
	for {
		n, ok, err := m.Restarts().Get(scope.TenantID, scope.SubsAccID, "")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if ok && n.ScheduledAt == nil {
			if !n.NoticeAt.Equal(noticeAt) {
				t.Fatalf("NoticeAt = %v, want it preserved at %v", n.NoticeAt, noticeAt)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("scheduled bounce did not fire within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// FR-6.2: a schedule persisted on disk survives a proxy restart. RearmSchedules
// is what Reconcile calls; an already-elapsed schedule fires immediately rather
// than being dropped.
func TestRearmSchedulesPicksUpPersistedSchedules(t *testing.T) {
	m, _ := testManager(t, config.ModeContinuous, newFakeDocker())

	future := time.Now().Add(time.Hour)
	if err := m.Restarts().Raise("t1", "s1", "alpha",
		restart.Notice{NoticeAt: time.Now(), ScheduledAt: &future, Reason: restart.ReasonModel}); err != nil {
		t.Fatalf("Raise: %v", err)
	}

	m.RearmSchedules()

	m.schedMu.Lock()
	n := len(m.sched)
	m.schedMu.Unlock()
	if n != 1 {
		t.Fatalf("re-armed timers = %d, want 1", n)
	}
	m.CancelScheduledBounce(Scope{Kind: ScopeSubscription, TenantID: "t1", SubsAccID: "s1", AgentKey: "alpha"})
}

// The split in design §4: BounceScope must not touch a container that is not
// running, and must restart the ones that are.
func TestBounceScopeOnlyTouchesRunningContainers(t *testing.T) {
	f := newFakeDocker()
	m, _ := testManager(t, config.ModeContinuous, f)

	runningKey := wk("up")
	runningName := m.ContainerName(runningKey)
	f.exists[runningName] = true
	f.running[runningName] = true

	stoppedKey := wk("down")
	stoppedName := m.ContainerName(stoppedKey)
	f.exists[stoppedName] = true

	f.listResult = []ContainerSummary{
		{Names: []string{"/" + runningName}, State: "running", Labels: map[string]string{
			LabelTenant: "t1", LabelSubscription: "s1", LabelAgent: "alpha", LabelUser: "up"}},
		{Names: []string{"/" + stoppedName}, State: "exited", Labels: map[string]string{
			LabelTenant: "t1", LabelSubscription: "s1", LabelAgent: "alpha", LabelUser: "down"}},
	}

	if err := m.BounceScope(Scope{Kind: ScopeSubscription, TenantID: "t1", SubsAccID: "s1"}); err != nil {
		t.Fatalf("BounceScope: %v", err)
	}
	if f.stopN != 1 || f.startN != 1 {
		t.Fatalf("stop/start = %d/%d, want 1/1 (only the running container)", f.stopN, f.startN)
	}
	if f.running[stoppedName] {
		t.Error("a stopped container was started by the bounce")
	}
}
