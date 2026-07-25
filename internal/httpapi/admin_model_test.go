package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
)

// modelAgent builds the "alpha" agent config used by the admin-model-override
// tests: a default fallback model plus one selectable model, each carrying a
// distinct fake api key so a leak into any response is detectable.
func modelAgent() config.Agent {
	return config.Agent{
		Key: "alpha", ServiceName: "picoclaw-alpha", ResolvedToken: "bearer", Mode: config.ModeScaleToZero,
		Model: &config.ModelConfig{Provider: "deepseek", Name: "deepseek-chat", APIKey: "sk-default-secret"},
		Models: []*config.ModelConfig{
			{Provider: "openai", Name: "gpt-4o", APIKey: "sk-openai-should-not-leak", APIKeyEnv: "OPENAI_KEY"},
		},
	}
}

// TestAdminModelsListSelectableNoKeyLeak proves GET /v1/admin/models returns
// only {provider,name} for every selectable model (default + Models) and NEVER
// an api key or apiKeyEnv byte anywhere in the response (CTX-AMO-06).
func TestAdminModelsListSelectableNoKeyLeak(t *testing.T) {
	s := testServer(newFakeOrch(), &fakeTurner{})
	s.Cfg.Agents["alpha"] = modelAgent()

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, "/v1/admin/models", headersFor(t, instanceProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"provider":"openai"`, `"name":"gpt-4o"`, `"provider":"deepseek"`, `"name":"deepseek-chat"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q: %s", want, body)
		}
	}
	for _, leak := range []string{
		"sk-default-secret", "sk-openai-should-not-leak", "OPENAI_KEY",
		"apiKeyEnv", "api_key", "apiKey", "APIKey",
	} {
		if strings.Contains(body, leak) {
			t.Errorf("models list leaked %q: %s", leak, body)
		}
	}
}

// TestAdminModelSetForbiddenNoWrite proves a caller with no authority over the
// target scope is denied (403) and the override is never written.
func TestAdminModelSetForbiddenNoWrite(t *testing.T) {
	orch := newFakeOrch()
	s := testServer(orch, &fakeTurner{})
	s.Cfg.Agents["alpha"] = modelAgent()

	body := `{"scope":"tenant","tenant_id":"` + tenantT + `","provider":"openai","name":"gpt-4o"}`
	r := httptest.NewRequest(http.MethodPut, "/v1/admin/model", strings.NewReader(body))
	for k, v := range headersFor(t, subsManagerProfile()) {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
	if len(orch.modelSets) != 0 {
		t.Errorf("model override written despite 403: %d", len(orch.modelSets))
	}
}

// TestAdminModelSetUnknownModel400 proves a {provider,name} outside the
// agent's selectable allowlist is rejected (400) and nothing is written, even
// for an otherwise fully-authorized (Instance) caller.
func TestAdminModelSetUnknownModel400(t *testing.T) {
	orch := newFakeOrch()
	s := testServer(orch, &fakeTurner{})
	s.Cfg.Agents["alpha"] = modelAgent()

	body := `{"scope":"tenant","tenant_id":"` + tenantT + `","provider":"mistral","name":"not-selectable"}`
	r := httptest.NewRequest(http.MethodPut, "/v1/admin/model", strings.NewReader(body))
	for k, v := range headersFor(t, instanceProfile()) {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if len(orch.modelSets) != 0 {
		t.Errorf("model override written despite 400: %d", len(orch.modelSets))
	}
}

// TestAdminModelUsersListIncludesEffectiveModel mirrors handleAdminUsersList
// but asserts each user's effective model {provider,name,level} is present and
// that no api key ever reaches the response (CTX-AMO-06).
func TestAdminModelUsersListIncludesEffectiveModel(t *testing.T) {
	orch := newFakeOrch()
	orch.users = []docker.UserRef{{AccID: accBob, Role: "alpha", Email: "bob@x"}}
	orch.effectiveModel = &config.ModelConfig{Provider: "openai", Name: "gpt-4o", APIKey: "sk-should-not-leak"}
	orch.effectiveLevel = "user"
	s := testServer(orch, &fakeTurner{})
	s.Cfg.Agents["alpha"] = modelAgent()

	path := "/v1/admin/model/users?tenant_id=" + tenantT + "&subs_acc_id=" + subsX
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, path, headersFor(t, instanceProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{accBob, `"provider":"openai"`, `"name":"gpt-4o"`, `"level":"user"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, "sk-should-not-leak") {
		t.Errorf("model users list leaked an api key: %s", body)
	}
}

// TestAdminModelUsersForbidden proves a caller with no authority over the
// subscription is denied.
func TestAdminModelUsersForbidden(t *testing.T) {
	s := testServer(newFakeOrch(), &fakeTurner{})
	path := "/v1/admin/model/users?tenant_id=" + tenantT + "&subs_acc_id=" + subsX
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, path, headersFor(t, userProfile())))
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
}

// --- real-Manager round trip: PUT writes the override file to disk, GET
// reflects it (+ level), DELETE clears it and falls back to the agent default.

// fakeAdminDocker is a no-op docker.Docker so ReapplyModelScope's trailing
// RestartScope call (which lists running containers) never touches a real
// daemon; List returning an empty slice means "nothing running to restart".
type fakeAdminDocker struct{}

func (fakeAdminDocker) Inspect(context.Context, string) (docker.ContainerState, error) {
	return docker.ContainerState{}, nil
}
func (fakeAdminDocker) EnsureImage(context.Context, string) error                 { return nil }
func (fakeAdminDocker) Create(context.Context, docker.CreateSpec) (string, error) { return "", nil }
func (fakeAdminDocker) Start(context.Context, string) error                       { return nil }
func (fakeAdminDocker) Stop(context.Context, string, time.Duration) error         { return nil }
func (fakeAdminDocker) Remove(context.Context, string) error                      { return nil }
func (fakeAdminDocker) List(context.Context, string) ([]docker.ContainerSummary, error) {
	return nil, nil
}

// realModelServer wires a Server over a REAL docker.Manager (temp data root,
// no-op Docker) so PUT/GET/DELETE /v1/admin/model exercise the actual
// override-file read/write/clear + reapply-and-restart path, not a fake.
func realModelServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{
		ContainerDataRoot: root,
		ContainerPrefix:   "picoclaw",
		Agents:            map[string]config.Agent{"alpha": modelAgent()},
	}
	mgr := docker.NewManager(cfg, fakeAdminDocker{}, func(context.Context, string, int) error { return nil }, nil, nil)
	return &Server{Cfg: cfg, Resolver: identity.NewSDKResolver(), Mgr: mgr}, root
}

func modelGetReq(t *testing.T, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet,
		"/v1/admin/model?scope=subscription&tenant_id="+tenantT+"&subs_acc_id="+subsX, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestAdminModelPutWritesOverrideFile(t *testing.T) {
	s, root := realModelServer(t)
	h := headersFor(t, instanceProfile())

	body := `{"scope":"subscription","tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX +
		`","provider":"openai","name":"gpt-4o"}`
	r := httptest.NewRequest(http.MethodPut, "/v1/admin/model", strings.NewReader(body))
	for k, v := range h {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sk-openai-should-not-leak") {
		t.Errorf("PUT response leaked an api key: %s", w.Body.String())
	}

	path := config.SubscriptionModelOverrideFile(root, tenantT, subsX)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("override file not written at %s: %v", path, err)
	}
}

// TestAdminModelPutGetDeleteRoundTrip proves GET reflects the override just
// set (+ its level), and that DELETE clears it and falls back to the agent's
// default model at level "default".
func TestAdminModelPutGetDeleteRoundTrip(t *testing.T) {
	s, _ := realModelServer(t)
	h := headersFor(t, instanceProfile())

	// No override yet -> default (agent.Model = deepseek/deepseek-chat).
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, modelGetReq(t, h))
	if w.Code != http.StatusOK {
		t.Fatalf("initial GET status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"level":"default"`) {
		t.Errorf("expected default level before any override: %s", w.Body.String())
	}

	// PUT a subscription-level override.
	putBody := `{"scope":"subscription","tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX +
		`","provider":"openai","name":"gpt-4o"}`
	putReq := httptest.NewRequest(http.MethodPut, "/v1/admin/model", strings.NewReader(putBody))
	for k, v := range h {
		putReq.Header.Set(k, v)
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200: %s", w.Code, w.Body.String())
	}

	// GET now reflects it, at level "subscription".
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, modelGetReq(t, h))
	body := w.Body.String()
	if !strings.Contains(body, `"provider":"openai"`) || !strings.Contains(body, `"name":"gpt-4o"`) ||
		!strings.Contains(body, `"level":"subscription"`) {
		t.Errorf("GET after PUT = %s, want openai/gpt-4o at subscription level", body)
	}

	// DELETE clears it -> falls back to the agent default.
	delReq := httptest.NewRequest(http.MethodDelete,
		"/v1/admin/model?scope=subscription&tenant_id="+tenantT+"&subs_acc_id="+subsX, nil)
	for k, v := range h {
		delReq.Header.Set(k, v)
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, delReq)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, modelGetReq(t, h))
	if !strings.Contains(w.Body.String(), `"level":"default"`) {
		t.Errorf("expected fallback to default after clear: %s", w.Body.String())
	}
}

// TestAdminModelPerUserPutGetRoundTrip exercises the user_acc_id branch of
// PUT/GET /v1/admin/model (untested by the scope-only cases above): the
// override lands in the per-user file (not the subscription one), and GET at
// the same user_acc_id reflects it at level "user".
func TestAdminModelPerUserPutGetRoundTrip(t *testing.T) {
	s, root := realModelServer(t)
	h := headersFor(t, instanceProfile())

	userGetReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet,
			"/v1/admin/model?tenant_id="+tenantT+"&subs_acc_id="+subsX+"&user_acc_id="+accBob, nil)
		for k, v := range h {
			r.Header.Set(k, v)
		}
		return r
	}

	putBody := `{"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX + `","user_acc_id":"` + accBob +
		`","provider":"openai","name":"gpt-4o"}`
	putReq := httptest.NewRequest(http.MethodPut, "/v1/admin/model", strings.NewReader(putBody))
	for k, v := range h {
		putReq.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200: %s", w.Code, w.Body.String())
	}

	userPath := config.UserModelOverrideFile(root, tenantT, subsX, "alpha", accBob)
	if _, err := os.Stat(userPath); err != nil {
		t.Fatalf("per-user override file not written at %s: %v", userPath, err)
	}
	// The subscription-level file must NOT have been touched by a user-scoped PUT.
	if _, err := os.Stat(config.SubscriptionModelOverrideFile(root, tenantT, subsX)); !os.IsNotExist(err) {
		t.Errorf("per-user PUT should not write the subscription-level override file, stat err = %v", err)
	}

	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, userGetReq())
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"provider":"openai"`) || !strings.Contains(body, `"name":"gpt-4o"`) ||
		!strings.Contains(body, `"level":"user"`) {
		t.Errorf("per-user GET = %s, want openai/gpt-4o at level user", body)
	}

	// DELETE the per-user override -> falls back to the agent default.
	delReq := httptest.NewRequest(http.MethodDelete,
		"/v1/admin/model?tenant_id="+tenantT+"&subs_acc_id="+subsX+"&user_acc_id="+accBob, nil)
	for k, v := range h {
		delReq.Header.Set(k, v)
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, delReq)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200: %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, userGetReq())
	if !strings.Contains(w.Body.String(), `"level":"default"`) {
		t.Errorf("expected fallback to default after per-user clear: %s", w.Body.String())
	}
}

// TestAdminModelPerUserForbidden proves a caller with no authority over the
// user's subscription is denied (403) and nothing is written.
func TestAdminModelPerUserForbidden(t *testing.T) {
	orch := newFakeOrch()
	s := testServer(orch, &fakeTurner{})
	s.Cfg.Agents["alpha"] = modelAgent()

	body := `{"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX + `","user_acc_id":"` + accBob +
		`","provider":"openai","name":"gpt-4o"}`
	r := httptest.NewRequest(http.MethodPut, "/v1/admin/model", strings.NewReader(body))
	for k, v := range headersFor(t, userProfile()) {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
	if len(orch.modelSets) != 0 {
		t.Errorf("model override written despite 403: %d", len(orch.modelSets))
	}
}
