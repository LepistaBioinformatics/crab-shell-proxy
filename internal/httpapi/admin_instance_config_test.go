package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/docker"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/identity"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/restart"
)

// instanceConfigServer is testServer plus a SECOND agent. The vehicle-vs-target
// distinction (an `agent` parameter naming beta while the request is routed as
// alpha) cannot be asserted with only one agent configured.
func instanceConfigServer(orch Orchestrator, logs *[]string) *Server {
	cfg := &config.Config{
		ContainerDataRoot: "/tmp",
		Agents: map[string]config.Agent{
			"alpha": {Key: "alpha", ServiceName: "picoclaw-alpha", ResolvedToken: "bearer",
				Mode: config.ModeScaleToZero},
			"beta": {Key: "beta", ServiceName: "picoclaw-beta", ResolvedToken: "bearer",
				Mode: config.ModeScaleToZero},
		},
	}
	s := &Server{Cfg: cfg, Resolver: identity.NewSDKResolver(), Mgr: orch, Pico: &fakeTurner{}}
	if logs != nil {
		s.Logf = func(format string, args ...any) {
			*logs = append(*logs, fmt.Sprintf(format, args...))
		}
	}
	return s
}

func configPath(agent string) string {
	return "/v1/admin/users/config?tenant_id=" + tenantT + "&subs_acc_id=" + subsX +
		"&user_acc_id=" + accBob + "&agent=" + agent
}

func putReq(t *testing.T, path, body string, headers map[string]string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestInstanceConfigRequiresUserManagement(t *testing.T) {
	orch := newFakeOrch()
	var logs []string
	s := instanceConfigServer(orch, &logs)
	// A member of the subscription, not a manager of it.
	headers := headersFor(t, userProfile())

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, configPath("alpha"), headers))
	if w.Code != http.StatusForbidden {
		t.Errorf("GET status = %d, want 403", w.Code)
	}

	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, configPath("alpha"), `{"raw":"{}"}`, headers))
	if w.Code != http.StatusForbidden {
		t.Errorf("PUT status = %d, want 403", w.Code)
	}
	if orch.instanceConfigWritten != "" {
		t.Error("a refused caller reached the write")
	}

	// A REFUSED write is audited too. A 403 on an endpoint that can move the
	// agent's sandbox boundary is the line an operator most wants to find, and it
	// happens before the key exists — so it has to be logged from the handler.
	var audit string
	for _, l := range logs {
		if strings.Contains(l, "instance config write") {
			audit = l
		}
	}
	if audit == "" {
		t.Fatalf("a refused PUT left no audit line; logs = %v", logs)
	}
	for _, want := range []string{"by=", "user=" + accBob, "agent=alpha", "result=rejected"} {
		if !strings.Contains(audit, want) {
			t.Errorf("refusal audit missing %q: %s", want, audit)
		}
	}
}

func TestInstanceConfigRejectsBadIDs(t *testing.T) {
	s := instanceConfigServer(newFakeOrch(), nil)
	bad := "/v1/admin/users/config?tenant_id=not-a-uuid&subs_acc_id=" + subsX +
		"&user_acc_id=" + accBob + "&agent=alpha"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, bad, headersFor(t, instanceProfile())))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// The agent is required and may not be the "all" sentinel: a workspace has
// exactly one role, so there is no such thing as an all-agents config.json.
func TestInstanceConfigRequiresExplicitAgent(t *testing.T) {
	s := instanceConfigServer(newFakeOrch(), nil)
	base := "/v1/admin/users/config?tenant_id=" + tenantT + "&subs_acc_id=" + subsX +
		"&user_acc_id=" + accBob
	for _, path := range []string{base, base + "&agent=all", base + "&agent=nope"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, path, headersFor(t, instanceProfile())))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, w.Code)
		}
	}
}

// The target agent comes from the `agent` parameter, NOT from the addressed
// service. The webapp routes every admin call through picoclaw-alpha, so
// inheriting the vehicle would repair alpha's config while the admin believes
// they are fixing beta's.
func TestInstanceConfigTargetsTheNamedAgentNotTheVehicle(t *testing.T) {
	orch := newFakeOrch()
	orch.instanceConfig = docker.InstanceConfig{Raw: "{}", Valid: true, Revision: "sha256:x"}
	s := instanceConfigServer(orch, nil)

	w := httptest.NewRecorder()
	// Routed as alpha (headersFor sets picoclaw-alpha), targeting beta.
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, configPath("beta"), headersFor(t, instanceProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(orch.instanceConfigReadKeys) != 1 {
		t.Fatalf("read keys = %v", orch.instanceConfigReadKeys)
	}
	if got := orch.instanceConfigReadKeys[0].Role; got != "beta" {
		t.Errorf("Role = %q, want beta — the parameter, not the vehicle", got)
	}
}

func TestInstanceConfigGetIncludesManagedPaths(t *testing.T) {
	orch := newFakeOrch()
	orch.instanceConfig = docker.InstanceConfig{
		Raw: "{}", Valid: true, Revision: "sha256:x", ManagedPaths: docker.ManagedConfigPaths,
	}
	s := instanceConfigServer(orch, nil)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, configPath("alpha"), headersFor(t, instanceProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var got docker.InstanceConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.ManagedPaths) != len(docker.ManagedConfigPaths) {
		t.Errorf("managedPaths = %v, want docker.ManagedConfigPaths", got.ManagedPaths)
	}
	// The UI renders these read-only from the response; a missing one would look
	// admin-editable.
	for _, p := range docker.ManagedConfigPaths {
		if !strings.Contains(w.Body.String(), p) {
			t.Errorf("managed path %q missing from the response", p)
		}
	}
}

// A broken config.json is data the admin needs, not an error: 200 with
// valid=false. Returning a parsed object (or a 4xx) would make the endpoint
// unable to open the exact files it exists for.
func TestInstanceConfigGetReturnsBrokenFileAs200(t *testing.T) {
	orch := newFakeOrch()
	orch.instanceConfig = docker.InstanceConfig{
		Raw: `{"version": 3, "agents": {`, Valid: false,
		ParseError: "unexpected end of JSON input", Offset: 26, Revision: "sha256:x",
	}
	s := instanceConfigServer(orch, nil)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, configPath("alpha"), headersFor(t, instanceProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a broken file: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"valid":false`) {
		t.Errorf("body does not report the parse failure: %s", w.Body.String())
	}
}

func TestInstanceConfigGetNotProvisioned(t *testing.T) {
	orch := newFakeOrch()
	orch.instanceConfigErr = docker.ErrNotProvisioned
	s := instanceConfigServer(orch, nil)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, configPath("alpha"), headersFor(t, instanceProfile())))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "not_provisioned") {
		t.Errorf("body = %s, want a not_provisioned code the UI can branch on", w.Body.String())
	}
}

func TestInstanceConfigPutStaleRevision(t *testing.T) {
	orch := newFakeOrch()
	orch.instanceConfigWriteErr = docker.ErrStaleRevision
	s := instanceConfigServer(orch, nil)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, configPath("alpha"),
		`{"raw":"{}","revision":"sha256:old"}`, headersFor(t, instanceProfile())))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), "stale_revision") {
		t.Errorf("body = %s", w.Body.String())
	}
	if orch.instanceConfigRevision != "sha256:old" {
		t.Errorf("revision = %q, want the one the client sent", orch.instanceConfigRevision)
	}
}

func TestInstanceConfigPutInvalidJSON(t *testing.T) {
	orch := newFakeOrch()
	orch.instanceConfigWriteErr = docker.ErrConfigNotObject
	s := instanceConfigServer(orch, nil)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, configPath("alpha"),
		`{"raw":"[]"}`, headersFor(t, instanceProfile())))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid_json") {
		t.Errorf("body = %s", w.Body.String())
	}
}

// The default policy bounces the workspace; picoclaw reads config.json only at
// boot, so without a bounce nothing the admin fixed is in effect.
func TestInstanceConfigPutRestartNow(t *testing.T) {
	orch := newFakeOrch()
	orch.instanceConfigAfterWrite = docker.InstanceConfig{Raw: `{"version":4}`, Valid: true, Revision: "sha256:new"}
	orch.instanceConfigReapply = docker.ReapplyResult{OK: true}
	s := instanceConfigServer(orch, nil)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, configPath("alpha"),
		`{"raw":"{\"version\":4}","revision":"sha256:x"}`, headersFor(t, instanceProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if len(orch.restarts) != 1 {
		t.Fatalf("restarts = %v, want exactly the edited workspace", orch.restarts)
	}
	if orch.restarts[0].Role != "alpha" || orch.restarts[0].UserAccID != accBob {
		t.Errorf("bounced %+v, want the edited workspace", orch.restarts[0])
	}
	if len(orch.workspaceNotices) != 0 {
		t.Error("an immediate bounce also raised a notice")
	}
}

func TestInstanceConfigPutRestartNotice(t *testing.T) {
	orch := newFakeOrch()
	orch.instanceConfigAfterWrite = docker.InstanceConfig{Raw: "{}", Valid: true, Revision: "sha256:new"}
	s := instanceConfigServer(orch, nil)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, configPath("alpha")+"&restart=notice",
		`{"raw":"{}","revision":"sha256:x"}`, headersFor(t, instanceProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if len(orch.restarts) != 0 {
		t.Errorf("notice policy bounced anyway: %v", orch.restarts)
	}
	if len(orch.workspaceNotices) != 1 {
		t.Fatalf("workspace notices = %v, want one", orch.workspaceNotices)
	}
	st, err := orch.RestartStatus(orch.workspaceNotices[0])
	if err != nil {
		t.Fatal(err)
	}
	if st.Reason != restart.ReasonConfig {
		t.Errorf("reason = %q, want %q", st.Reason, restart.ReasonConfig)
	}
}

// `schedule` behaves as `notice` here, which is what bounceNow does at every
// per-workspace site: the scheduler arms per SCOPE, and scheduling one member's
// config change would bounce every member under it.
func TestInstanceConfigPutSchedulesAsNotice(t *testing.T) {
	orch := newFakeOrch()
	orch.instanceConfigAfterWrite = docker.InstanceConfig{Raw: "{}", Valid: true}
	s := instanceConfigServer(orch, nil)

	// A valid window (the policy parser still enforces future-and-within-7-days,
	// so the value has to be relative to now).
	at := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, configPath("alpha")+"&restart=schedule&restart_at="+url.QueryEscape(at),
		`{"raw":"{}"}`, headersFor(t, instanceProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if len(orch.armedSchedules) != 0 {
		t.Errorf("a scope schedule was armed: %v", orch.armedSchedules)
	}
	if len(orch.workspaceNotices) != 1 {
		t.Errorf("workspace notices = %v, want one", orch.workspaceNotices)
	}
}

// The response is the POST-materialization document, not the submitted bytes: an
// admin who edited a proxy-owned key must see it revert rather than believe the
// edit stuck.
func TestInstanceConfigPutReturnsPostReapplyState(t *testing.T) {
	orch := newFakeOrch()
	orch.instanceConfigAfterWrite = docker.InstanceConfig{
		Raw: `{"agents":{"defaults":{"model_name":"registry-owned"}}}`, Valid: true, Revision: "sha256:new",
	}
	orch.instanceConfigReapply = docker.ReapplyResult{OK: false, Detail: "no active model"}
	s := instanceConfigServer(orch, nil)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, configPath("alpha"),
		`{"raw":"{\"agents\":{\"defaults\":{\"model_name\":\"hand-edited\"}}}"}`,
		headersFor(t, instanceProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "hand-edited") {
		t.Errorf("response echoed the submitted bytes: %s", body)
	}
	if !strings.Contains(body, "registry-owned") {
		t.Errorf("response is not the post-reapply document: %s", body)
	}
	// A failed reapply is reported, never presented as a failed save.
	if !strings.Contains(body, `"ok":false`) || !strings.Contains(body, "no active model") {
		t.Errorf("reapply outcome missing: %s", body)
	}
}

// Write access to config.json is write access to the agent's sandbox boundary, so
// every attempt is logged — and the document is never in the log, because a
// legacy layout can still carry credentials in it.
func TestInstanceConfigPutLogsWithoutBody(t *testing.T) {
	var logs []string
	orch := newFakeOrch()
	orch.instanceConfig = docker.InstanceConfig{Raw: "{}", Valid: true, Size: 2}
	orch.instanceConfigAfterWrite = docker.InstanceConfig{Raw: `{"a":1}`, Valid: true, Size: 7}
	orch.instanceConfigReapply = docker.ReapplyResult{OK: true}
	s := instanceConfigServer(orch, &logs)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, configPath("alpha"),
		`{"raw":"{\"secret_looking\":\"sk-live-nope\"}"}`, headersFor(t, instanceProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	var audit string
	for _, l := range logs {
		if strings.Contains(l, "instance config write") {
			audit = l
		}
	}
	if audit == "" {
		t.Fatalf("no audit line; logs = %v", logs)
	}
	for _, want := range []string{"by=", "user=" + accBob, "agent=alpha", "before=2B", "after=7B", "result=ok"} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit line missing %q: %s", want, audit)
		}
	}
	for _, l := range logs {
		if strings.Contains(l, "sk-live-nope") || strings.Contains(l, "secret_looking") {
			t.Errorf("the document reached the logs: %s", l)
		}
	}
}

// FR-1.4: the route addresses config.json and nothing else. A caller-supplied
// name/path parameter is not read, so it cannot redirect the read.
func TestInstanceConfigNoFileNameParameter(t *testing.T) {
	orch := newFakeOrch()
	orch.instanceConfig = docker.InstanceConfig{Raw: "{}", Valid: true}
	s := instanceConfigServer(orch, nil)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet,
		configPath("alpha")+"&name=.security.yml&path=../../etc/passwd&file=x",
		headersFor(t, instanceProfile())))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if len(orch.instanceConfigReadKeys) != 1 {
		t.Fatalf("read keys = %v", orch.instanceConfigReadKeys)
	}
	// The key is the only thing the manager gets; there is no file argument in
	// the signature at all, so no parameter could have reached one.
	got := orch.instanceConfigReadKeys[0]
	if got.TenantID != tenantT || got.SubsAccID != subsX || got.UserAccID != accBob || got.Role != "alpha" {
		t.Errorf("key = %+v, want the addressed workspace", got)
	}
}

// There is no DELETE or POST on this route: replacing the document is the whole
// surface, and a delete would leave an unprovisioned workspace behind.
func TestInstanceConfigNoDeleteOrPost(t *testing.T) {
	s := instanceConfigServer(newFakeOrch(), nil)
	for _, method := range []string{http.MethodDelete, http.MethodPost} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, adminReq(t, method, configPath("alpha"), headersFor(t, instanceProfile())))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, w.Code)
		}
	}
}
