package httpapi

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/sgelias/crab-shell-proxy/internal/config"
	"github.com/sgelias/crab-shell-proxy/internal/docker"
	"github.com/sgelias/crab-shell-proxy/internal/identity"
)

// Fixed UUIDs used across the authorization table tests.
const (
	accAlice = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	accBob   = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	tenantT  = "11111111-1111-1111-1111-111111111111"
	subsX    = "22222222-2222-2222-2222-222222222222"
	tenantU  = "33333333-3333-3333-3333-333333333333"
)

type fakeOrch struct {
	ensureErr  error
	armed      int
	scaffolded map[string]bool
	keys       []docker.WorkspaceKey

	writeErr   error
	deleteErr  error
	writes     []secretWrite
	deletes    []secretWrite
	restarts   []docker.WorkspaceKey
	listResult docker.SecretNames
}

type secretWrite struct {
	key                 docker.WorkspaceKey
	format, name, value string
}

func newFakeOrch() *fakeOrch { return &fakeOrch{scaffolded: map[string]bool{}} }

func (f *fakeOrch) WriteSecret(_ config.Agent, key docker.WorkspaceKey, format, name, value string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes = append(f.writes, secretWrite{key, format, name, value})
	return nil
}

func (f *fakeOrch) ListSecrets(docker.WorkspaceKey) (docker.SecretNames, error) {
	return f.listResult, nil
}

func (f *fakeOrch) DeleteSecret(key docker.WorkspaceKey, format, name string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletes = append(f.deletes, secretWrite{key: key, format: format, name: name})
	return nil
}

func (f *fakeOrch) RestartWorkspace(key docker.WorkspaceKey) error {
	f.restarts = append(f.restarts, key)
	return nil
}

func (f *fakeOrch) StoreMedia(_ docker.WorkspaceKey, rawName string, r io.Reader) (docker.StoredMedia, error) {
	n, _ := io.Copy(io.Discard, r)
	return docker.StoredMedia{Path: "uploads/test-" + rawName, Name: rawName, Size: n}, nil
}

func (f *fakeOrch) ListMedia(docker.WorkspaceKey) ([]docker.StoredMedia, error) {
	return nil, nil
}

func skey(tenantID, subsAccID string) string { return tenantID + "/" + subsAccID }

func (f *fakeOrch) EnsureRunning(_ context.Context, _ config.Agent, key docker.WorkspaceKey, _ string) (docker.Target, error) {
	if f.ensureErr != nil {
		return docker.Target{}, f.ensureErr
	}
	f.keys = append(f.keys, key)
	return docker.Target{Name: "picoclaw-alpha-h", WSEndpoint: "ws://x:1/pico/ws", PicoToken: "t"}, nil
}
func (f *fakeOrch) ArmIdle(config.Agent, docker.WorkspaceKey) { f.armed++ }
func (f *fakeOrch) ScaffoldSubscription(tenantID, subsAccID string) (bool, error) {
	k := skey(tenantID, subsAccID)
	if f.scaffolded[k] {
		return false, nil
	}
	f.scaffolded[k] = true
	return true, nil
}
func (f *fakeOrch) SubscriptionScaffolded(tenantID, subsAccID string) bool {
	return f.scaffolded[skey(tenantID, subsAccID)]
}

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

// licensedProfile builds a profile JSON for accId with a single licensed
// resource record {accId=subsAccID, tenantId, role, perm, verified}.
func licensedProfile(accID, tenantID, subsAccID, role, perm string, verified bool) string {
	v := "false"
	if verified {
		v = "true"
	}
	return `{"accId":"` + accID + `","owners":[{"email":"u@x","isPrincipal":true}],` +
		`"licensedResources":{"records":[{"accId":"` + subsAccID + `","tenantId":"` + tenantID +
		`","role":"` + role + `","perm":"` + perm + `","verified":` + v + `}]}}`
}

func testServer(orch Orchestrator, turner Turner) *Server {
	res := identity.NewSDKResolver()
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

func headersFor(t *testing.T, profileJSON string) map[string]string {
	return map[string]string{
		identity.ServiceNameHeader: "picoclaw-alpha",
		"Authorization":            "Bearer bearer",
		identity.ProfileHeader:     encodeProfile(t, profileJSON),
	}
}

// goodHeaders carries alice, licensed (write, verified, role alpha) into subsX
// under tenantT.
func goodHeaders(t *testing.T) map[string]string {
	return headersFor(t, licensedProfile(accAlice, tenantT, subsX, "alpha", "write", true))
}

// goodBody is an authorized chat body targeting tenantT / subsX.
const goodBody = `{"messages":[{"role":"user","content":"hi"}],"session_id":"s","tenant_id":"` +
	tenantT + `","subs_acc_id":"` + subsX + `"}`

// scaffoldedOrch returns a fakeOrch with tenantT/subsX already scaffolded.
func scaffoldedOrch() *fakeOrch {
	o := newFakeOrch()
	o.scaffolded[skey(tenantT, subsX)] = true
	return o
}

func TestChatUnknownService(t *testing.T) {
	s := testServer(newFakeOrch(), &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, `{}`, nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestChatBadToken(t *testing.T) {
	s := testServer(newFakeOrch(), &fakeTurner{})
	h := goodHeaders(t)
	h["Authorization"] = "Bearer wrong"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, goodBody, h))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestChatNoProfile(t *testing.T) {
	s := testServer(newFakeOrch(), &fakeTurner{})
	h := goodHeaders(t)
	delete(h, identity.ProfileHeader)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, goodBody, h))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestChatNoSessionID(t *testing.T) {
	s := testServer(newFakeOrch(), &fakeTurner{})
	body := `{"messages":[{"role":"user","content":"hi"}],"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX + `"}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, body, goodHeaders(t)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestChatMissingTenantOrSubs(t *testing.T) {
	s := testServer(scaffoldedOrch(), &fakeTurner{})
	// Missing tenant_id.
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t,
		`{"messages":[{"role":"user","content":"hi"}],"session_id":"s","subs_acc_id":"`+subsX+`"}`, goodHeaders(t)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing tenant_id: status = %d, want 400", w.Code)
	}
	// Missing subs_acc_id.
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t,
		`{"messages":[{"role":"user","content":"hi"}],"session_id":"s","tenant_id":"`+tenantT+`"}`, goodHeaders(t)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing subs_acc_id: status = %d, want 400", w.Code)
	}
}

func TestChatOKJSON(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{content: "hello back"})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, goodBody, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hello back") {
		t.Errorf("body missing content: %s", w.Body.String())
	}
	if orch.armed != 1 {
		t.Errorf("ArmIdle called %d times, want 1", orch.armed)
	}
	// Routed to alice's own leaf under the targeted subscription.
	if len(orch.keys) != 1 || orch.keys[0].UserAccID != accAlice ||
		orch.keys[0].SubsAccID != subsX || orch.keys[0].TenantID != tenantT || orch.keys[0].Role != "alpha" {
		t.Errorf("routed key = %+v", orch.keys)
	}
}

func TestChatScaffoldsOnDemand(t *testing.T) {
	// Authorized chat against a not-yet-scaffolded subscription: the root is
	// created on demand (no 409), then the turn runs.
	orch := newFakeOrch()
	s := testServer(orch, &fakeTurner{content: "hi"})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, goodBody, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !orch.SubscriptionScaffolded(tenantT, subsX) {
		t.Error("subscription was not scaffolded on demand")
	}
}

func TestChatForbiddenDenyPaths(t *testing.T) {
	cases := []struct {
		name    string
		profile string
	}{
		{"unlicensed", `{"accId":"` + accAlice + `","owners":[{"email":"u@x","isPrincipal":true}]}`},
		{"read-only", licensedProfile(accAlice, tenantT, subsX, "alpha", "read", true)},
		{"wrong-tenant", licensedProfile(accAlice, tenantU, subsX, "alpha", "write", true)},
		{"missing-role", licensedProfile(accAlice, tenantT, subsX, "beta", "write", true)},
		{"acc-equals-subs", licensedProfile(subsX, tenantT, subsX, "alpha", "write", true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(scaffoldedOrch(), &fakeTurner{content: "x"})
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, chatReq(t, goodBody, headersFor(t, tc.profile)))
			if w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403 (%s): %s", w.Code, tc.name, w.Body.String())
			}
		})
	}
}

// TestChatUnverifiedAccepted locks the user decision (2026-07-16) that `verified`
// is NOT enforced: an otherwise-valid grant (write, right tenant/role/account)
// that is unverified still authorizes the chat.
func TestChatUnverifiedAccepted(t *testing.T) {
	profile := licensedProfile(accAlice, tenantT, subsX, "alpha", "write", false)
	s := testServer(scaffoldedOrch(), &fakeTurner{content: "ok"})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, goodBody, headersFor(t, profile)))
	if w.Code != http.StatusOK {
		t.Errorf("unverified grant: status = %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestChatStaffShortCircuit(t *testing.T) {
	// A staff profile with NO licensed resources still passes the chain.
	profile := `{"accId":"` + accAlice + `","isStaff":true,"owners":[{"email":"u@x","isPrincipal":true}]}`
	s := testServer(scaffoldedOrch(), &fakeTurner{content: "ok"})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, goodBody, headersFor(t, profile)))
	if w.Code != http.StatusOK {
		t.Errorf("staff short-circuit: status = %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestChatTwoMembersIsolate(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{content: "x"})
	// Alice.
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, goodBody, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("alice status = %d", w.Code)
	}
	// Bob, licensed into the same subscription.
	bob := headersFor(t, licensedProfile(accBob, tenantT, subsX, "alpha", "write", true))
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, goodBody, bob))
	if w.Code != http.StatusOK {
		t.Fatalf("bob status = %d", w.Code)
	}
	if len(orch.keys) != 2 {
		t.Fatalf("want 2 routed keys, got %d", len(orch.keys))
	}
	if orch.keys[0].UserAccID == orch.keys[1].UserAccID {
		t.Errorf("two members collapsed to the same user dir: %v", orch.keys)
	}
	if orch.keys[0].UserAccID != accAlice || orch.keys[1].UserAccID != accBob {
		t.Errorf("routed keys = %+v", orch.keys)
	}
}

func TestChatManagerErrorIs502(t *testing.T) {
	orch := scaffoldedOrch()
	orch.ensureErr = context.DeadlineExceeded
	s := testServer(orch, &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, goodBody, goodHeaders(t)))
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestChatStreamFraming(t *testing.T) {
	s := testServer(scaffoldedOrch(), &fakeTurner{deltas: []string{"Hel", "lo"}})
	body := `{"messages":[{"role":"user","content":"hi"}],"session_id":"s","stream":true,"tenant_id":"` +
		tenantT + `","subs_acc_id":"` + subsX + `"}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, chatReq(t, body, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	out := w.Body.String()
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Error("missing initial role chunk")
	}
	if !strings.Contains(out, `"content":"Hel"`) || !strings.Contains(out, `"content":"lo"`) {
		t.Errorf("missing streamed deltas: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Error("missing [DONE] terminator")
	}
}

// realMgrServer builds a Server backed by a real *docker.Manager over a temp
// data root, for the pure-filesystem scaffold endpoints. The Docker handle is
// nil because these endpoints never touch it.
func realMgrServer(t *testing.T, webhookSecret string) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{
		ContainerDataRoot: root, ResolvedWebhookSecret: webhookSecret,
		Agents: map[string]config.Agent{
			"alpha": {Key: "alpha", ServiceName: "picoclaw-alpha", ResolvedToken: "bearer",
				Mode: config.ModeScaleToZero},
		},
	}
	mgr := docker.NewManager(cfg, nil, func(context.Context, string, int) error { return nil }, nil)
	return &Server{Cfg: cfg, Resolver: identity.NewSDKResolver(), Mgr: mgr}, root
}

const accountBody = `{"id":"` + subsX + `","accountType":{"subscription":{"tenantId":"` + tenantT + `"}}}`

func accountReq(t *testing.T, body, auth string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/accounts", strings.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	return r
}

func TestAccountsScaffold201Then200(t *testing.T) {
	s, root := realMgrServer(t, "wh-secret")
	// First create -> 201 and the scaffold dir appears.
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, accountReq(t, accountBody, "Bearer wh-secret"))
	if w.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(config.SubscriptionRoot(root, tenantT, subsX)); err != nil {
		t.Fatalf("scaffold dir not present: %v", err)
	}
	// Retry -> 200 (idempotent).
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, accountReq(t, accountBody, "Bearer wh-secret"))
	if w.Code != http.StatusOK {
		t.Errorf("retry status = %d, want 200", w.Code)
	}
}

func TestAccountsBadSecret401(t *testing.T) {
	s, root := realMgrServer(t, "wh-secret")
	for _, auth := range []string{"", "Bearer wrong"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, accountReq(t, accountBody, auth))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("auth %q: status = %d, want 401", auth, w.Code)
		}
	}
	if _, err := os.Stat(config.SubscriptionRoot(root, tenantT, subsX)); err == nil {
		t.Error("scaffold created despite bad secret")
	}
}

func TestAccountsEmptyConfiguredSecretRejects(t *testing.T) {
	s, _ := realMgrServer(t, "") // no secret configured
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, accountReq(t, accountBody, "Bearer "))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 when no secret configured", w.Code)
	}
}

func TestAccountsBadBody400(t *testing.T) {
	s, _ := realMgrServer(t, "wh-secret")
	cases := []string{
		`{"accountType":{"subscription":{"tenantId":"` + tenantT + `"}}}`, // missing id
		`{"id":"` + subsX + `","accountType":{"user":{}}}`,                // not a subscription
		`{"id":"not-a-uuid","accountType":{"subscription":{"tenantId":"` + tenantT + `"}}}`,
		`{"id":"` + subsX + `","accountType":{"subscription":{"tenantId":"nope"}}}`,
	}
	for _, body := range cases {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, accountReq(t, body, "Bearer wh-secret"))
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, w.Code)
		}
	}
}

func subsReq(t *testing.T, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/subscriptions", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestSubscriptionsList(t *testing.T) {
	s, _ := realMgrServer(t, "wh-secret")
	h := map[string]string{identity.ProfileHeader: encodeProfile(t,
		licensedProfile(accAlice, tenantT, subsX, "alpha", "write", true))}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, subsReq(t, h))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, subsX) || !strings.Contains(body, tenantT) ||
		!strings.Contains(body, `"role":"alpha"`) || !strings.Contains(body, `"perm":"write"`) {
		t.Errorf("subscription tuple missing: %s", body)
	}
	if !strings.Contains(body, `"scaffolded":false`) {
		t.Errorf("expected scaffolded=false annotation: %s", body)
	}
}

func TestSubscriptionsEmpty(t *testing.T) {
	s, _ := realMgrServer(t, "wh-secret")
	h := map[string]string{identity.ProfileHeader: encodeProfile(t,
		`{"accId":"`+accAlice+`","owners":[{"email":"u@x","isPrincipal":true}]}`)}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, subsReq(t, h))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"subscriptions":[]`) {
		t.Errorf("expected empty list: %s", w.Body.String())
	}
}

func TestSubscriptionsNoProfile401(t *testing.T) {
	s, _ := realMgrServer(t, "wh-secret")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, subsReq(t, nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func historyReq(t *testing.T, query string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/sessions/history?"+query, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestHistoryOKNewPath(t *testing.T) {
	// A read from the new layout for the caller's own leaf; empty (no dir yet)
	// but 200 with a messages field, proving the path resolved without error.
	s := testServer(scaffoldedOrch(), &fakeTurner{})
	q := "session_id=s&tenant_id=" + tenantT + "&subs_acc_id=" + subsX
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, historyReq(t, q, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"messages"`) {
		t.Errorf("missing messages field: %s", w.Body.String())
	}
}

func TestHistoryRequiresTenantAndSubs(t *testing.T) {
	s := testServer(scaffoldedOrch(), &fakeTurner{})
	for _, q := range []string{
		"tenant_id=" + tenantT + "&subs_acc_id=" + subsX, // missing session_id
		"session_id=s&subs_acc_id=" + subsX,              // missing tenant_id
		"session_id=s&tenant_id=" + tenantT,              // missing subs_acc_id
	} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, historyReq(t, q, goodHeaders(t)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("query %q: status = %d, want 400", q, w.Code)
		}
	}
}

func TestHistoryAccountSwitchGuard(t *testing.T) {
	s := testServer(scaffoldedOrch(), &fakeTurner{})
	// profile accId == subs_acc_id -> 403.
	h := headersFor(t, licensedProfile(subsX, tenantT, subsX, "alpha", "write", true))
	q := "session_id=s&tenant_id=" + tenantT + "&subs_acc_id=" + subsX
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, historyReq(t, q, h))
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestModelsRequiresAuth(t *testing.T) {
	s := testServer(newFakeOrch(), &fakeTurner{})
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

func secretBody(format, name, value string) string {
	return `{"tenant_id":"` + tenantT + `","subs_acc_id":"` + subsX +
		`","format":"` + format + `","name":"` + name + `","value":"` + value + `"}`
}

func secretsPostReq(t *testing.T, body string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/secrets", strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func secretsReq(t *testing.T, method, query string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(method, "/v1/secrets?"+query, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestSecretsPostEachFormat(t *testing.T) {
	for _, format := range []string{"dotenv", "json", "file", "native"} {
		t.Run(format, func(t *testing.T) {
			orch := scaffoldedOrch()
			s := testServer(orch, &fakeTurner{})
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, secretsPostReq(t, secretBody(format, "web.brave", "sekret"), goodHeaders(t)))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
			if len(orch.writes) != 1 || orch.writes[0].format != format {
				t.Errorf("write not recorded: %+v", orch.writes)
			}
			// Routed to alice's own (user, agent) store, not the subscription.
			if orch.writes[0].key.UserAccID != accAlice || orch.writes[0].key.Role != "alpha" ||
				orch.writes[0].key.SubsAccID != subsX || orch.writes[0].key.TenantID != tenantT {
				t.Errorf("routed key = %+v", orch.writes[0].key)
			}
			if len(orch.restarts) != 1 {
				t.Errorf("restart invoked %d times, want 1", len(orch.restarts))
			}
		})
	}
}

func TestSecretsPostValidationMapsTo400(t *testing.T) {
	for _, sentinel := range []error{docker.ErrInvalidSecretName, docker.ErrUnknownNativeSlot} {
		orch := scaffoldedOrch()
		orch.writeErr = sentinel
		s := testServer(orch, &fakeTurner{})
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, secretsPostReq(t, secretBody("native", "bad.slot", "v"), goodHeaders(t)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("sentinel %v: status = %d, want 400", sentinel, w.Code)
		}
		if len(orch.restarts) != 0 {
			t.Errorf("sentinel %v: restart must not run when write is rejected", sentinel)
		}
	}
}

func TestSecretsPostUnknownFormat400(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, secretsPostReq(t, secretBody("yaml", "X", "v"), goodHeaders(t)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for unknown format", w.Code)
	}
	if len(orch.writes) != 0 {
		t.Error("write ran for an unknown format")
	}
}

func TestSecretsPostNoProfile401(t *testing.T) {
	s := testServer(scaffoldedOrch(), &fakeTurner{})
	h := goodHeaders(t)
	delete(h, identity.ProfileHeader)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, secretsPostReq(t, secretBody("dotenv", "A", "v"), h))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestSecretsPostForbidden(t *testing.T) {
	cases := []struct{ name, profile string }{
		{"unlicensed", `{"accId":"` + accAlice + `","owners":[{"email":"u@x","isPrincipal":true}]}`},
		{"read-only", licensedProfile(accAlice, tenantT, subsX, "alpha", "read", true)},
		{"wrong-tenant", licensedProfile(accAlice, tenantU, subsX, "alpha", "write", true)},
		{"missing-role", licensedProfile(accAlice, tenantT, subsX, "beta", "write", true)},
		{"acc-equals-subs", licensedProfile(subsX, tenantT, subsX, "alpha", "write", true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orch := scaffoldedOrch()
			s := testServer(orch, &fakeTurner{})
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, secretsPostReq(t, secretBody("dotenv", "A", "v"), headersFor(t, tc.profile)))
			if w.Code != http.StatusForbidden {
				t.Errorf("%s: status = %d, want 403", tc.name, w.Code)
			}
			if len(orch.writes) != 0 {
				t.Errorf("%s: write ran despite a 403", tc.name)
			}
		})
	}
}

func TestSecretsGetNamesOnly(t *testing.T) {
	orch := scaffoldedOrch()
	orch.listResult = docker.SecretNames{
		Dotenv: []string{"BRAVE_KEY"},
		JSON:   []string{"OPENAI_KEY"},
		Native: []string{"web.brave"},
		File:   []string{"token.pem"},
	}
	s := testServer(orch, &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, secretsReq(t, http.MethodGet, "tenant_id="+tenantT+"&subs_acc_id="+subsX, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, name := range []string{"BRAVE_KEY", "OPENAI_KEY", "web.brave", "token.pem"} {
		if !strings.Contains(body, name) {
			t.Errorf("missing name %q in listing: %s", name, body)
		}
	}
	for _, group := range []string{`"dotenv"`, `"json"`, `"native"`, `"file"`} {
		if !strings.Contains(body, group) {
			t.Errorf("missing format group %q: %s", group, body)
		}
	}
	// The response shape (SecretNames) carries names only; a value can never be
	// present. This sentinel is a value never placed into any name field.
	if strings.Contains(body, "SECRET-VALUE") {
		t.Errorf("listing leaked a value: %s", body)
	}
}

func TestSecretsGetForbidden(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{})
	// read-only grant -> 403 (same chain as chat).
	h := headersFor(t, licensedProfile(accAlice, tenantT, subsX, "alpha", "read", true))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, secretsReq(t, http.MethodGet, "tenant_id="+tenantT+"&subs_acc_id="+subsX, h))
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestSecretsDelete(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{})
	q := "tenant_id=" + tenantT + "&subs_acc_id=" + subsX + "&format=dotenv&name=BRAVE_KEY"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, secretsReq(t, http.MethodDelete, q, goodHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(orch.deletes) != 1 || orch.deletes[0].name != "BRAVE_KEY" || orch.deletes[0].format != "dotenv" {
		t.Errorf("delete not recorded: %+v", orch.deletes)
	}
	if len(orch.restarts) != 1 {
		t.Errorf("restart after delete invoked %d times, want 1", len(orch.restarts))
	}
}

func TestSecretsDeleteUnknownFormat400(t *testing.T) {
	orch := scaffoldedOrch()
	s := testServer(orch, &fakeTurner{})
	q := "tenant_id=" + tenantT + "&subs_acc_id=" + subsX + "&format=yaml&name=X"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, secretsReq(t, http.MethodDelete, q, goodHeaders(t)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if len(orch.deletes) != 0 {
		t.Error("delete ran for an unknown format")
	}
}

func TestHealthzUnauthenticated(t *testing.T) {
	s := testServer(newFakeOrch(), &fakeTurner{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}
