package httpapi

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/sgelias/crab-shell-proxy/internal/config"
	"github.com/sgelias/crab-shell-proxy/internal/docker"
	"github.com/sgelias/crab-shell-proxy/internal/identity"
)

type fakeOrch struct {
	ensureErr error
	armed     int
}

func (f *fakeOrch) EnsureRunning(context.Context, config.Agent, string) (docker.Target, error) {
	if f.ensureErr != nil {
		return docker.Target{}, f.ensureErr
	}
	return docker.Target{Name: "picoclaw-alpha-h", WSEndpoint: "ws://x:1/pico/ws", PicoToken: "t"}, nil
}
func (f *fakeOrch) ArmIdle(config.Agent, string) { f.armed++ }

type fakeTurner struct {
	content string
	err     error
	deltas  []string
}

func (f *fakeTurner) RunTurn(_ context.Context, _, _, _, _ string, onDelta func(string)) (string, error) {
	for _, d := range f.deltas {
		if onDelta != nil {
			onDelta(d)
		}
	}
	return f.content, f.err
}

func encodeProfile(t *testing.T, body string) string {
	t.Helper()
	enc, _ := zstd.NewWriter(nil)
	out := enc.EncodeAll([]byte(body), nil)
	_ = enc.Close()
	return base64.StdEncoding.EncodeToString(out)
}

func testServer(orch Orchestrator, turner Turner) *Server {
	res, _ := identity.NewFallbackResolver()
	cfg := &config.Config{
		ContainerDataRoot: "/tmp",
		Agents: map[string]config.Agent{
			"alpha": {Key: "alpha", ServiceName: "picoclaw-alpha", ResolvedToken: "bearer",
				Mode: config.ModeScaleToZero},
		},
	}
	return &Server{Cfg: cfg, Resolver: res, Mgr: orch, Pico: turner}
}

func chatReq(t *testing.T, body string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func goodHeaders(t *testing.T) map[string]string {
	return map[string]string{
		identity.ServiceNameHeader: "picoclaw-alpha",
		"Authorization":            "Bearer bearer",
		identity.ProfileHeader:     encodeProfile(t, `{"owners":[{"email":"alice@x","isPrincipal":true}]}`),
	}
}

func TestChatUnknownService(t *testing.T) {
	s := testServer(&fakeOrch{}, &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, `{}`, nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestChatBadToken(t *testing.T) {
	s := testServer(&fakeOrch{}, &fakeTurner{})
	h := goodHeaders(t)
	h["Authorization"] = "Bearer wrong"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, `{"messages":[{"role":"user","content":"hi"}],"session_id":"s"}`, h))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestChatNoProfile(t *testing.T) {
	s := testServer(&fakeOrch{}, &fakeTurner{})
	h := goodHeaders(t)
	delete(h, identity.ProfileHeader)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, `{"messages":[{"role":"user","content":"hi"}],"session_id":"s"}`, h))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestChatNoSessionID(t *testing.T) {
	s := testServer(&fakeOrch{}, &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, `{"messages":[{"role":"user","content":"hi"}]}`, goodHeaders(t)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestChatOKJSON(t *testing.T) {
	orch := &fakeOrch{}
	s := testServer(orch, &fakeTurner{content: "hello back"})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t,
		`{"messages":[{"role":"user","content":"hi"}],"session_id":"s"}`, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hello back") {
		t.Errorf("body missing content: %s", w.Body.String())
	}
	if orch.armed != 1 {
		t.Errorf("ArmIdle called %d times, want 1", orch.armed)
	}
}

func TestChatManagerErrorIs502(t *testing.T) {
	s := testServer(&fakeOrch{ensureErr: context.DeadlineExceeded}, &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t,
		`{"messages":[{"role":"user","content":"hi"}],"session_id":"s"}`, goodHeaders(t)))
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestChatStreamFraming(t *testing.T) {
	s := testServer(&fakeOrch{}, &fakeTurner{deltas: []string{"Hel", "lo"}})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t,
		`{"messages":[{"role":"user","content":"hi"}],"session_id":"s","stream":true}`, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"role":"assistant"`) {
		t.Error("missing initial role chunk")
	}
	if !strings.Contains(body, `"content":"Hel"`) || !strings.Contains(body, `"content":"lo"`) {
		t.Errorf("missing streamed deltas: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("missing [DONE] terminator")
	}
}

func TestModelsRequiresAuth(t *testing.T) {
	s := testServer(&fakeOrch{}, &fakeTurner{})
	// no headers -> unknown service
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	// good headers -> 200 list
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	for k, v := range goodHeaders(t) {
		r.Header.Set(k, v)
	}
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "picoclaw") {
		t.Errorf("models: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestHealthzUnauthenticated(t *testing.T) {
	s := testServer(&fakeOrch{}, &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}
