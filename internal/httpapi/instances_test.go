package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
)

const telemetryTok = "tele-secret"

// instancesServer builds a server whose telemetry token is `tok`. An empty tok
// is the not-configured deployment, which must not register the route at all.
func instancesServer(orch Orchestrator, tok string) *Server {
	cfg := &config.Config{
		ContainerDataRoot:      "/tmp",
		ResolvedTelemetryToken: tok,
		Agents: map[string]config.Agent{
			"alpha": {Key: "alpha", ServiceName: "picoclaw-alpha", ResolvedToken: "bearer",
				Mode: config.ModeContinuous},
		},
	}
	return &Server{Cfg: cfg, Resolver: identity.NewSDKResolver(), Mgr: orch, Pico: &fakeTurner{}}
}

func instancesReq(auth string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/instances", nil)
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	return r
}

// The route must be ABSENT, not 401, when no token is configured. A deployment
// that has not opted in grows no new surface — this endpoint discloses the whole
// tenant/subscription/user topology.
func TestInstancesRouteAbsentWhenTokenUnset(t *testing.T) {
	s := instancesServer(newFakeOrch(), "")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, instancesReq("Bearer "+telemetryTok))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (route must not be registered): %s", w.Code, w.Body.String())
	}
}

func TestInstancesRejectsWrongOrMissingToken(t *testing.T) {
	for _, tc := range []struct{ name, auth string }{
		{"missing", ""},
		{"wrong", "Bearer nope"},
		{"bare value without scheme", telemetryTok},
		// The agent token is a DIFFERENT credential and must not open this route.
		// It is the gate on chatting as any member; a watcher must never hold one.
		{"an agent token", "Bearer bearer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := instancesServer(newFakeOrch(), telemetryTok)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, instancesReq(tc.auth))
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", w.Code, w.Body.String())
			}
		})
	}
}

// FR-P2: the inventory is a read. A telemetry poll that cold-starts a member's
// agent would be a defect, and the failure mode is silent — nothing about a 200
// reveals that a container was created to produce it.
func TestInstancesHasNoContainerSideEffects(t *testing.T) {
	orch := newFakeOrch()
	s := instancesServer(orch, telemetryTok)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, instancesReq("Bearer "+telemetryTok))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(orch.keys) != 0 {
		t.Errorf("EnsureRunning was called %d time(s); the inventory must not start containers", len(orch.keys))
	}
	if orch.armed != 0 {
		t.Errorf("ArmIdle was called %d time(s); the inventory must not touch lifecycle timers", orch.armed)
	}
	if len(orch.restarts) != 0 {
		t.Errorf("RestartWorkspace was called %d time(s)", len(orch.restarts))
	}
}

// An empty inventory is an initialised array. A consumer that has to branch on
// null before it can iterate is one that will forget to.
func TestInstancesEmptyIsArrayNotNull(t *testing.T) {
	s := instancesServer(newFakeOrch(), telemetryTok)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, instancesReq("Bearer "+telemetryTok))
	if !strings.Contains(w.Body.String(), `"instances":[]`) {
		t.Errorf("empty inventory should be [], got %s", w.Body.String())
	}
}

// All four states survive the trip, including the two that are not a container
// status. Those are the reason the set is closed rather than an engine string.
func TestInstancesReportsEveryState(t *testing.T) {
	orch := newFakeOrch()
	orch.instances = []docker.Instance{
		{TenantID: "t1", SubsAccID: "s1", Agent: "alpha", UserAccID: "u1",
			ContainerName: "crabshell-alpha-aaaa", Mode: "continuous", State: docker.InstanceRunning},
		{TenantID: "t1", SubsAccID: "s1", Agent: "alpha", UserAccID: "u2",
			ContainerName: "crabshell-alpha-bbbb", Mode: "continuous", State: docker.InstanceStopped},
		// Provisioned by POST /v1/accounts and never started: a real state, and
		// the one that would be lost if absent workspaces were simply omitted.
		{TenantID: "t1", SubsAccID: "s1", Agent: "alpha", UserAccID: "u3",
			ContainerName: "crabshell-alpha-cccc", Mode: "continuous", State: docker.InstanceProvisioned},
		// A container with no directory: a stack fault that must surface as one.
		{TenantID: "t2", SubsAccID: "s2", Agent: "beta", UserAccID: "u4",
			ContainerName: "crabshell-beta-dddd", Mode: "scale-to-zero", State: docker.InstanceOrphaned},
	}
	s := instancesServer(orch, telemetryTok)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, instancesReq("Bearer "+telemetryTok))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got instancesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if len(got.Instances) != 4 {
		t.Fatalf("got %d instances, want 4: %+v", len(got.Instances), got.Instances)
	}
	wantStates := []string{"running", "stopped", "provisioned", "orphaned"}
	for i, want := range wantStates {
		if got.Instances[i].State != want {
			t.Errorf("instance %d state = %q, want %q", i, got.Instances[i].State, want)
		}
	}
	// The tuple is the whole point: without it a caller cannot attribute a
	// container, because the name hashes the tuple one way.
	first := got.Instances[0]
	if first.TenantID != "t1" || first.SubsAccID != "s1" || first.Agent != "alpha" || first.UserAccID != "u1" {
		t.Errorf("tuple did not survive: %+v", first)
	}
	if first.ContainerName != "crabshell-alpha-aaaa" {
		t.Errorf("container name = %q", first.ContainerName)
	}
}

// A failure to read Docker is a 502, not an empty inventory. "Nothing is
// running" and "I could not find out" must not look the same to a watcher —
// that is exactly the confusion this endpoint exists to remove.
func TestInstancesFailureIsNotAnEmptyInventory(t *testing.T) {
	orch := newFakeOrch()
	orch.instancesErr = errors.New("docker unreachable")
	s := instancesServer(orch, telemetryTok)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, instancesReq("Bearer "+telemetryTok))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"instances"`) {
		t.Errorf("a failure must not be served as an inventory: %s", w.Body.String())
	}
}
