package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
)

// The admin half of the member-facing notice: someone reports "my scheduled
// tasks never run", and this is where an admin fixes it for THAT member --
// without moving the whole agent to continuous and paying for a container per
// member that never stops.

// modeServer mirrors instanceConfigServer, with an idleTimeout on alpha (so
// scale-to-zero is representable there) and none on beta (so it is not).
func modeServer(orch Orchestrator) *Server {
	cfg := &config.Config{
		ContainerDataRoot: "/tmp",
		Agents: map[string]config.Agent{
			"alpha": {Key: "alpha", ServiceName: "picoclaw-alpha", ResolvedToken: "bearer",
				Mode: config.ModeScaleToZero, IdleTimeout: config.Duration(15 * time.Minute)},
			"beta": {Key: "beta", ServiceName: "picoclaw-beta", ResolvedToken: "bearer",
				Mode: config.ModeContinuous},
		},
	}
	return &Server{Cfg: cfg, Resolver: identity.NewSDKResolver(), Mgr: orch, Pico: &fakeTurner{}}
}

func modePath(agent string) string {
	return "/v1/admin/users/mode?tenant_id=" + tenantT + "&subs_acc_id=" + subsX +
		"&user_acc_id=" + accBob + "&agent=" + agent
}

// Same gate as the sibling config editor. A member of the subscription is not a
// manager of it, and this endpoint decides whether a container may run forever.
func TestInstanceModeRequiresUserManagement(t *testing.T) {
	s := modeServer(newFakeOrch())
	headers := headersFor(t, userProfile())

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, modePath("alpha"), headers))
	if w.Code != http.StatusForbidden {
		t.Errorf("GET status = %d, want 403", w.Code)
	}

	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, modePath("alpha"), `{"mode":"continuous"}`, headers))
	if w.Code != http.StatusForbidden {
		t.Errorf("PUT status = %d, want 403", w.Code)
	}
}

// Inherited and pinned are the same value today and diverge the moment the
// agent default moves. An admin choosing between them has to see which they
// have, so the two travel in separate fields.
func TestInstanceModeReportsInheritedAndPinnedApart(t *testing.T) {
	s := modeServer(newFakeOrch())

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, modePath("alpha"), headersFor(t, instanceProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got instanceModeView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Effective != config.ModeScaleToZero {
		t.Errorf("Effective = %q, want the agent default", got.Effective)
	}
	if got.Override != "" {
		t.Errorf("Override = %q, want empty when the value is inherited", got.Override)
	}
	if got.Fires {
		t.Error("Fires is true on a scale-to-zero instance: the member's panel says the opposite")
	}
	if !got.ScaleToZeroAllowed {
		t.Error("ScaleToZeroAllowed is false for an agent that declares an idleTimeout")
	}
}

// An agent with no idleTimeout cannot represent scale-to-zero. The UI is told,
// rather than being left to offer a choice the write refuses.
func TestScaleToZeroIsReportedUnavailableWithoutAnIdleTimeout(t *testing.T) {
	s := modeServer(newFakeOrch())

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, modePath("beta"), headersFor(t, instanceProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var got instanceModeView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ScaleToZeroAllowed {
		t.Error("ScaleToZeroAllowed is true for an agent with no idleTimeout")
	}
}

func TestInstanceModePutWritesAndReturnsTheFreshView(t *testing.T) {
	orch := newFakeOrch()
	s := modeServer(orch)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, modePath("alpha"), `{"mode":"continuous"}`,
		headersFor(t, instanceProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got instanceModeView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// The fresh view, not an echo of the request: a CLEAR returns the agent
	// default, which is never what the caller sent.
	if got.Effective != config.ModeContinuous || got.Override != config.ModeContinuous {
		t.Errorf("view = %+v, want the continuous instance just written", got)
	}
	if !got.Fires {
		t.Error("Fires is false right after switching to continuous")
	}
	if orch.setModeCalls != 1 {
		t.Errorf("SetMode called %d times, want 1", orch.setModeCalls)
	}
}

// The failure mode this whole feature has to avoid is a setting that looks
// applied and is not, so a refused write is a 400 that names the cause.
func TestInstanceModePutSurfacesARefusal(t *testing.T) {
	orch := newFakeOrch()
	orch.setModeErr = errors.New(`agent "beta" has no idleTimeout, so its instances cannot be set to scale-to-zero`)
	s := modeServer(orch)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, modePath("beta"), `{"mode":"scale-to-zero"}`,
		headersFor(t, instanceProfile())))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "idleTimeout") {
		t.Errorf("the refusal does not name the cause: %s", w.Body.String())
	}
}

// An unknown agent is a 404, not a panic on a zero-valued config.Agent whose
// empty Mode would then be written as an override.
func TestInstanceModeRejectsAnUnknownAgent(t *testing.T) {
	s := modeServer(newFakeOrch())
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, modePath("nosuch"), headersFor(t, instanceProfile())))
	if w.Code == http.StatusOK {
		t.Errorf("an unknown agent answered 200: %s", w.Body.String())
	}
}
