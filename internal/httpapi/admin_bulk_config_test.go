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
)

// bulkConfigServer gives each agent a Template that DIFFERS from its key.
// `template` is a config.yaml field distinct from the agent key — two agents may
// share one — so a handler that passes the key where a template name belongs would
// read the wrong directory, or none. With key == template that bug is invisible.
func bulkConfigServer(orch Orchestrator, logs *[]string) *Server {
	cfg := &config.Config{
		ContainerDataRoot: "/tmp",
		Agents: map[string]config.Agent{
			"alpha": {Key: "alpha", ServiceName: "picoclaw-alpha", ResolvedToken: "bearer",
				Template: "alpha-tpl", Mode: config.ModeScaleToZero},
			"beta": {Key: "beta", ServiceName: "picoclaw-beta", ResolvedToken: "bearer",
				Template: "shared-tpl", Mode: config.ModeScaleToZero},
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

func bulkPath(route, agent, extra string) string {
	p := "/v1/admin/scope/config" + route + "?tenant_id=" + tenantT + "&subs_acc_id=" + subsX +
		"&agent=" + agent
	if extra != "" {
		p += "&" + extra
	}
	return p
}

// A subscriptions-manager: the tier AuthorizeUserManagement accepts, and the tier
// this feature is actually for.
func adminHeaders(t *testing.T) map[string]string {
	t.Helper()
	return headersFor(t, subsManagerProfile())
}

// --- T6: scope guard and the keys endpoint ---

func TestScopeConfigKeysPassesTheTemplateNotTheAgentKey(t *testing.T) {
	orch := newFakeOrch()
	orch.bulkCatalog = docker.TemplateCatalog{Template: "alpha-tpl", TemplateRevision: "sha256:abc"}
	s := bulkConfigServer(orch, nil)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, bulkPath("/keys", "alpha", ""), adminHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if len(orch.bulkCatalogNames) != 1 || orch.bulkCatalogNames[0] != "alpha-tpl" {
		t.Errorf("catalog asked for %v, want [alpha-tpl] — the agent's TEMPLATE, not its key",
			orch.bulkCatalogNames)
	}
}

func TestScopeConfigRejectsBadParameters(t *testing.T) {
	orch := newFakeOrch()
	s := bulkConfigServer(orch, nil)

	cases := map[string]string{
		"non-uuid tenant": "/v1/admin/scope/config/keys?tenant_id=nope&subs_acc_id=" + subsX + "&agent=alpha",
		"missing subs":    "/v1/admin/scope/config/keys?tenant_id=" + tenantT + "&agent=alpha",
		"unknown agent":   bulkPath("/keys", "ghost", ""),
		// "all" is not a target here: the template is per-agent and the workspace
		// enumeration filters by agent, so an all-agents form has nothing to mean.
		"agent=all": bulkPath("/keys", "all", ""),
		"no agent":  "/v1/admin/scope/config/keys?tenant_id=" + tenantT + "&subs_acc_id=" + subsX,
	}
	for name, path := range cases {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet, path, adminHeaders(t)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, w.Code)
		}
	}
	if len(orch.bulkCatalogNames) != 0 {
		t.Errorf("a rejected request reached the manager: %v", orch.bulkCatalogNames)
	}
}

// DEC-1's ceiling is one subscription, and it holds because there is no tenant FORM
// of the request — not because a check rejects one. A `scope=tenant` parameter is
// simply not read, so the request still resolves as a subscription.
func TestScopeConfigIgnoresAScopeParameter(t *testing.T) {
	orch := newFakeOrch()
	s := bulkConfigServer(orch, nil)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet,
		bulkPath("/inspect", "alpha", "key=heartbeat.interval&scope=tenant"), adminHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if len(orch.bulkInspectScope) != 1 {
		t.Fatalf("inspect calls = %d, want 1", len(orch.bulkInspectScope))
	}
	got := orch.bulkInspectScope[0]
	if got.Kind != docker.ScopeSubscription {
		t.Errorf("scope kind = %v, want subscription even with scope=tenant on the query", got.Kind)
	}
	if got.SubsAccID != subsX || got.AgentKey != "alpha" {
		t.Errorf("scope = %+v, want subs=%s agent=alpha", got, subsX)
	}
}

func TestScopeConfigRequiresUserManagement(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"keys", http.MethodGet, bulkPath("/keys", "alpha", ""), ""},
		{"inspect", http.MethodGet, bulkPath("/inspect", "alpha", "key=version"), ""},
		{"apply", http.MethodPut, bulkPath("", "alpha", ""), `{"key":"version","value":3}`},
	} {
		orch := newFakeOrch()
		var logs []string
		s := bulkConfigServer(orch, &logs)
		// A member of the subscription, not a manager of it.
		headers := headersFor(t, userProfile())

		w := httptest.NewRecorder()
		if tc.body == "" {
			s.Handler().ServeHTTP(w, adminReq(t, tc.method, tc.path, headers))
		} else {
			s.Handler().ServeHTTP(w, putReq(t, tc.path, tc.body, headers))
		}
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", tc.name, w.Code)
		}
		if len(orch.bulkApplied) != 0 || len(orch.bulkCatalogNames) != 0 || len(orch.bulkInspectKeys) != 0 {
			t.Errorf("%s: a refused caller reached the manager", tc.name)
		}
		// A refused request is audited: FR-6.4 leans on the audit to justify the
		// authority tier, so an unlogged 403 is the case that would undermine it.
		found := false
		for _, l := range logs {
			if strings.Contains(l, "bulk config") && strings.Contains(l, "result=rejected") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: refusal not audited; logs = %v", tc.name, logs)
		}
	}
}

// --- T7: the inspect endpoint ---

func TestScopeConfigInspectMapsKeyRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"invalid", fmt.Errorf("inspect: %w", docker.ErrInvalidConfigKey), "invalid_key"},
		{"managed", fmt.Errorf("inspect: %w", docker.ErrManagedConfigPath), "managed_path"},
	} {
		orch := newFakeOrch()
		orch.bulkInspectErr = tc.err
		s := bulkConfigServer(orch, nil)

		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet,
			bulkPath("/inspect", "alpha", "key=model_list"), adminHeaders(t)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.name, w.Code)
		}
		if !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s: body = %s, want it to carry %q so the UI can put it on the key field",
				tc.name, w.Body.String(), tc.want)
		}
	}
}

// A subscription with no provisioned instances of that agent is an empty answer,
// not a missing resource: both the subscription and the agent exist.
func TestScopeConfigInspectEmptyScopeIsTwoHundred(t *testing.T) {
	orch := newFakeOrch()
	orch.bulkInspect = docker.ScopeConfigInspection{Key: "version", Agent: "alpha", Total: 0}
	s := bulkConfigServer(orch, nil)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet,
		bulkPath("/inspect", "alpha", "key=version"), adminHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got docker.ScopeConfigInspection
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Total != 0 || len(got.Buckets) != 0 {
		t.Errorf("got %+v, want an empty inspection", got)
	}
}

// --- T8: the apply endpoint ---

func mixedResult() docker.ScopeConfigResult {
	return docker.ScopeConfigResult{
		Key: "tools.web.brave.enabled",
		Outcomes: []docker.InstanceOutcome{
			{UserAccID: "u-applied", Outcome: docker.OutcomeApplied},
			{UserAccID: "u-unchanged", Outcome: docker.OutcomeUnchanged},
			{UserAccID: "u-stale", Outcome: docker.OutcomeStale},
			{UserAccID: "u-unreadable", Outcome: docker.OutcomeUnreadable},
		},
		Summary: map[string]int{"applied": 1, "unchanged": 1, "stale": 1, "unreadable": 1},
	}
}

func applyBody(value string) string {
	return `{"key":"tools.web.brave.enabled","value":` + value + `,"revisions":{"u-applied":"sha256:a"}}`
}

// DEC-8: only the instances that CHANGED are touched. A scope bounce would restart
// the members reported unchanged, stale and unreadable too, because it selects
// containers by label and cannot know what this apply wrote.
func TestScopeConfigPutRestartsOnlyTheChangedInstances(t *testing.T) {
	orch := newFakeOrch()
	orch.bulkResult = mixedResult()
	s := bulkConfigServer(orch, nil)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, bulkPath("", "alpha", "restart=now"), applyBody("true"), adminHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if len(orch.restarts) != 1 {
		t.Fatalf("restarts = %d (%v), want exactly 1 — only the applied instance",
			len(orch.restarts), orch.restarts)
	}
	got := orch.restarts[0]
	if got.UserAccID != "u-applied" || got.Role != "alpha" || got.SubsAccID != subsX {
		t.Errorf("restarted %+v, want the applied member's workspace", got)
	}
}

// DEC-9: an ABSENT restart parameter means notice here, where every sibling means
// now. The default would otherwise be "N members lose their running agent because
// nobody chose anything".
func TestScopeConfigPutDefaultsToNotice(t *testing.T) {
	orch := newFakeOrch()
	orch.bulkResult = mixedResult()
	s := bulkConfigServer(orch, nil)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, bulkPath("", "alpha", ""), applyBody("true"), adminHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if len(orch.restarts) != 0 {
		t.Errorf("restarts = %v, want none: the default is notice", orch.restarts)
	}
	if len(orch.workspaceNotices) != 1 || orch.workspaceNotices[0].UserAccID != "u-applied" {
		t.Errorf("notices = %v, want one for the applied member", orch.workspaceNotices)
	}
}

// The local default must stay LOCAL. parsePolicyFields is shared, so flipping it
// there would silently change every sibling endpoint.
func TestSharedRestartDefaultIsStillNow(t *testing.T) {
	p, err := parsePolicyFields("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != PolicyNow {
		t.Errorf("shared default = %q, want %q — DEC-9's substitution belongs in the bulk handler alone",
			p.Mode, PolicyNow)
	}
}

// FR-5.1: the key is audited, the value never is. A hand-typed dotted path can
// address a credential-bearing field.
func TestScopeConfigPutNeverLogsTheValue(t *testing.T) {
	const secret = "sk-must-not-appear-in-logs"

	orch := newFakeOrch()
	orch.bulkResult = mixedResult()
	var logs []string
	s := bulkConfigServer(orch, &logs)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, bulkPath("", "alpha", ""), applyBody(`"`+secret+`"`), adminHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	// Refused too, so both sides of the branch are covered.
	wr := httptest.NewRecorder()
	s.Handler().ServeHTTP(wr, putReq(t, bulkPath("", "alpha", ""), applyBody(`"`+secret+`"`),
		headersFor(t, userProfile())))

	joined := strings.Join(logs, "\n")
	if strings.Contains(joined, secret) {
		t.Errorf("the submitted value reached the log:\n%s", joined)
	}
	if !strings.Contains(joined, "tools.web.brave.enabled") {
		t.Errorf("the KEY should be audited; logs:\n%s", joined)
	}
}

func TestScopeConfigPutTemplateIsSeparatelyReportedAndLogged(t *testing.T) {
	orch := newFakeOrch()
	orch.bulkResult = mixedResult()
	orch.bulkTemplate = docker.TemplateResult{OK: true, Migration: "20260731T000000Z-x.json"}
	var logs []string
	s := bulkConfigServer(orch, &logs)

	body := `{"key":"tools.web.brave.enabled","value":true,"revisions":{"u-applied":"sha256:a"},` +
		`"alsoTemplate":true,"templateRevision":"sha256:tpl"}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, bulkPath("", "alpha", ""), body, adminHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}

	if len(orch.bulkTemplateArgs) != 1 {
		t.Fatalf("template calls = %d, want 1", len(orch.bulkTemplateArgs))
	}
	call := orch.bulkTemplateArgs[0]
	if call.Template != "alpha-tpl" {
		t.Errorf("template = %q, want alpha-tpl", call.Template)
	}
	if call.Revision != "sha256:tpl" {
		t.Errorf("template revision = %q, want the submitted token", call.Revision)
	}
	// The value arrives DECODED, which is what the template writer takes.
	if call.Value != true {
		t.Errorf("template value = %#v, want the decoded true", call.Value)
	}

	var got scopeConfigApplyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Template == nil || !got.Template.OK {
		t.Errorf("template result = %+v, want a reported success", got.Template)
	}
	if len(got.Outcomes) != 4 {
		t.Errorf("outcomes = %d, want the instance results alongside the template", len(got.Outcomes))
	}

	// A separate line: the template's blast radius differs in KIND — it seeds
	// future members of every subscription, and of every agent sharing it.
	found := false
	for _, l := range logs {
		if strings.Contains(l, "template write") && strings.Contains(l, "alpha-tpl") {
			found = true
		}
	}
	if !found {
		t.Errorf("template write not logged separately; logs = %v", logs)
	}
}

func TestScopeConfigPutTemplateFailureLeavesInstanceOutcomes(t *testing.T) {
	orch := newFakeOrch()
	orch.bulkResult = mixedResult()
	orch.bulkTemplateErr = docker.ErrStaleRevision
	s := bulkConfigServer(orch, nil)

	body := `{"key":"tools.web.brave.enabled","value":true,"revisions":{"u-applied":"sha256:a"},` +
		`"alsoTemplate":true,"templateRevision":"stale"}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, bulkPath("", "alpha", ""), body, adminHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the instance writes landed", w.Code)
	}
	var got scopeConfigApplyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Template == nil || got.Template.OK {
		t.Errorf("template = %+v, want ok:false", got.Template)
	}
	if got.Summary["applied"] != 1 {
		t.Errorf("summary = %v, want the instance outcomes intact", got.Summary)
	}
}

// By and AppliedAt are stamped by the server and carry json:"-" for this reason:
// Go's field matching is case-insensitive, so without it a body could forge the
// provenance written into every migration record in the batch.
func TestScopeConfigPutIgnoresForgedProvenance(t *testing.T) {
	orch := newFakeOrch()
	orch.bulkResult = mixedResult()
	s := bulkConfigServer(orch, nil)

	body := `{"key":"version","value":3,"revisions":{},"by":"someone-else",` +
		`"appliedAt":"1999-01-01T00:00:00Z"}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, bulkPath("", "alpha", ""), body, adminHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if len(orch.bulkApplied) != 1 {
		t.Fatalf("applies = %d, want 1", len(orch.bulkApplied))
	}
	ch := orch.bulkApplied[0]
	if ch.By == "someone-else" {
		t.Error("the request body forged the migration record's author")
	}
	if ch.AppliedAt.Year() == 1999 {
		t.Error("the request body forged the migration record's timestamp")
	}
}

func TestScopeConfigPutRejectsAnOversizeEnvelope(t *testing.T) {
	orch := newFakeOrch()
	s := bulkConfigServer(orch, nil)

	// One value, padded past the envelope cap.
	body := `{"key":"version","value":"` + strings.Repeat("x", maxBulkConfigEnvelope+1) + `"}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, bulkPath("", "alpha", ""), body, adminHeaders(t)))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 (%s)", w.Code, w.Body.String())
	}
	if len(orch.bulkApplied) != 0 {
		t.Error("an oversize body reached the manager")
	}
}

func TestScopeConfigPutRejectsABadRestartPolicy(t *testing.T) {
	orch := newFakeOrch()
	s := bulkConfigServer(orch, nil)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, bulkPath("", "alpha", "restart=whenever"),
		applyBody("true"), adminHeaders(t)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	// Validated BEFORE the apply: a policy 400 must not report a change that
	// already landed.
	if len(orch.bulkApplied) != 0 {
		t.Error("a bad restart policy still reached the apply")
	}
}

// schedule degrades to notice, as it does at every other per-workspace site: the
// scheduler arms per SCOPE, and arming one would bounce members this apply never
// touched.
func TestScopeConfigPutScheduleBehavesAsNotice(t *testing.T) {
	orch := newFakeOrch()
	orch.bulkResult = mixedResult()
	s := bulkConfigServer(orch, nil)

	at := "restart=schedule&restart_at=" + url.QueryEscape(time.Now().UTC().Add(2*time.Hour).Format(time.RFC3339))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, bulkPath("", "alpha", at), applyBody("true"), adminHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if len(orch.restarts) != 0 {
		t.Errorf("restarts = %v, want none for schedule", orch.restarts)
	}
	if len(orch.workspaceNotices) != 1 {
		t.Errorf("notices = %v, want one", orch.workspaceNotices)
	}
}

// The catalog is a SUGGESTION list, not a whitelist: a key that no template
// carries — a newer picoclaw's field, or one an earlier repair added — must still
// inspect and apply. Nothing on either path may consult the catalog first.
func TestScopeConfigAcceptsAKeyTheCatalogDoesNotCarry(t *testing.T) {
	orch := newFakeOrch()
	// An empty catalog: whatever the admin typed cannot possibly be in it.
	orch.bulkCatalog = docker.TemplateCatalog{Template: "alpha-tpl"}
	orch.bulkResult = mixedResult()
	s := bulkConfigServer(orch, nil)

	const typed = "tools.web.some_future_provider.enabled"

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adminReq(t, http.MethodGet,
		bulkPath("/inspect", "alpha", "key="+typed), adminHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("inspect status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if len(orch.bulkInspectKeys) != 1 || orch.bulkInspectKeys[0] != typed {
		t.Errorf("inspected %v, want the typed key passed through verbatim", orch.bulkInspectKeys)
	}

	wp := httptest.NewRecorder()
	body := `{"key":"` + typed + `","value":true,"revisions":{"u-applied":"sha256:a"}}`
	s.Handler().ServeHTTP(wp, putReq(t, bulkPath("", "alpha", ""), body, adminHeaders(t)))
	if wp.Code != http.StatusOK {
		t.Fatalf("apply status = %d, want 200 (%s)", wp.Code, wp.Body.String())
	}
	if len(orch.bulkApplied) != 1 || orch.bulkApplied[0].Key != typed {
		t.Errorf("applied %v, want the typed key", orch.bulkApplied)
	}
	if len(orch.bulkCatalogNames) != 0 {
		t.Error("inspect or apply consulted the catalog; it is a suggestion list, not a whitelist")
	}
}

// The scoped seed is the answer to "which subscription does this apply to", which a
// TEMPLATE record legitimately cannot answer. Both halves may be requested, and each
// is reported on its own because they reach different populations.
func TestScopeConfigPutScopesFutureMembersToOneSubscription(t *testing.T) {
	orch := newFakeOrch()
	orch.bulkResult = mixedResult()
	orch.bulkOverlay = docker.OverlayResult{OK: true, Migration: "20260801T100000Z-evolution.json"}
	var logs []string
	s := bulkConfigServer(orch, &logs)

	body := `{"key":"evolution.min_task_count","value":25,"revisions":{"u-applied":"sha256:a"},` +
		`"alsoSubscription":true}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, bulkPath("", "alpha", ""), body, adminHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}

	if len(orch.bulkOverlayArgs) != 1 {
		t.Fatalf("overlay calls = %d, want 1", len(orch.bulkOverlayArgs))
	}
	call := orch.bulkOverlayArgs[0]
	if call.Scope.SubsAccID != subsX || call.Scope.AgentKey != "alpha" {
		t.Errorf("overlay scope = %+v, want this subscription and agent", call.Scope)
	}
	if call.Key != "evolution.min_task_count" || call.Value != "25" {
		t.Errorf("overlay got key=%q value=%s", call.Key, call.Value)
	}
	// The template must NOT be touched: scoping is the whole point.
	if len(orch.bulkTemplateArgs) != 0 {
		t.Error("a scoped request wrote the agent template as well")
	}

	var got scopeConfigApplyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Subscription == nil || !got.Subscription.OK {
		t.Errorf("subscription result = %+v, want a reported success", got.Subscription)
	}
	if got.Template != nil {
		t.Errorf("template result = %+v, want absent", got.Template)
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "scoped seed") && strings.Contains(l, subsX) {
			found = true
		}
	}
	if !found {
		t.Errorf("scoped seed not logged with its subscription; logs = %v", logs)
	}
}

func TestScopeConfigPutOverlayFailureLeavesInstanceOutcomes(t *testing.T) {
	orch := newFakeOrch()
	orch.bulkResult = mixedResult()
	orch.bulkOverlayErr = docker.ErrManagedConfigPath
	s := bulkConfigServer(orch, nil)

	body := `{"key":"model_list","value":25,"revisions":{"u-applied":"sha256:a"},"alsoSubscription":true}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, putReq(t, bulkPath("", "alpha", ""), body, adminHeaders(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the instance writes landed", w.Code)
	}
	var got scopeConfigApplyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Subscription == nil || got.Subscription.OK {
		t.Errorf("subscription = %+v, want ok:false", got.Subscription)
	}
	if got.Summary["applied"] != 1 {
		t.Errorf("summary = %v, want the instance outcomes intact", got.Summary)
	}
}
