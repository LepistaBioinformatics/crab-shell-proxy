package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
)

// cronServer is testServer over a temp data root, so these tests run the same
// authorization chain as every other route test and read real files from the
// layout the agent container actually gets bind-mounted.
func cronServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	s := testServer(scaffoldedOrch(), &fakeTurner{})
	s.Cfg = &config.Config{ContainerDataRoot: root, Agents: s.Cfg.Agents}
	return s, root
}

// The scope goodHeaders resolves to: role "alpha" from the service-name header,
// user from the profile's own accId.
func cronPaths(root string) (cronFile, sessionsDir string) {
	return config.CronFile(root, tenantT, subsX, "alpha", accAlice, config.MainWorkspace),
		config.SessionsDir(root, tenantT, subsX, "alpha", accAlice, config.MainWorkspace)
}

func seedFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const cronQuery = "tenant_id=" + tenantT + "&subs_acc_id=" + subsX

func cronReq(t *testing.T, path string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range goodHeaders(t) {
		r.Header.Set(k, v)
	}
	return r
}

func do(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, cronReq(t, path))
	return rec
}

// A workspace whose agent never scheduled anything has no cron dir and no
// sessions dir. That is the ordinary state, not an error, and the panel must be
// able to render it.
func TestCronTasksEmptyWorkspace(t *testing.T) {
	s, _ := cronServer(t)
	rec := do(t, s, "/v1/cron/tasks?"+cronQuery)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var got struct {
		Tasks   []json.RawMessage `json:"tasks"`
		Orphans []json.RawMessage `json:"orphans"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Tasks) != 0 || len(got.Orphans) != 0 {
		t.Errorf("got %d tasks / %d orphans, want none", len(got.Tasks), len(got.Orphans))
	}
	if !json.Valid(rec.Body.Bytes()) {
		t.Error("response is not valid JSON")
	}
}

// The join, plus the case that motivated the orphan grouping: a one-shot job
// deletes itself from the store after running but leaves its transcript behind.
func TestCronTasksJoinsRunsAndGroupsOrphans(t *testing.T) {
	s, root := cronServer(t)
	cronFile, sessionsDir := cronPaths(root)

	seedFile(t, cronFile, `{"version":1,"jobs":[
		{"id":"e520b224e7714d16","name":"Relatorio diario","enabled":true,
		 "schedule":{"kind":"cron","expr":"0 18 * * *"},
		 "payload":{"kind":"agent_turn","message":"gerar relatorio"},
		 "state":{"nextRunAtMs":1785780000000,"lastRunAtMs":1785693600000,"lastStatus":"ok"},
		 "deleteAfterRun":false},
		{"id":"2ef133eeeaa15cfe","name":"Ping","enabled":false,
		 "schedule":{"kind":"every","everyMs":300000},
		 "payload":{"kind":"agent_turn","message":"heartbeat"},
		 "state":{},"deleteAfterRun":false}]}`)

	// Two runs of the live daily job.
	for _, r := range []struct{ run, started string }{
		{"e520b224e7714d16-5e055123", "2026-08-02T20:58:00Z"},
		{"e520b224e7714d16-3a5a895e", "2026-08-01T20:58:00Z"},
	} {
		seedFile(t, filepath.Join(sessionsDir, "agent_cron-"+r.run+".meta.json"),
			`{"key":"agent:cron-`+r.run+`","count":34,"created_at":"`+r.started+
				`","updated_at":"`+r.started+`","scope":{"values":{"chat":"direct:pico:abc"}}}`)
		seedFile(t, filepath.Join(sessionsDir, "agent_cron-"+r.run+".jsonl"),
			`{"role":"user","content":"gerar relatorio","created_at":"`+r.started+`"}`+"\n")
	}
	// A run whose job is gone (deleteAfterRun).
	seedFile(t, filepath.Join(sessionsDir, "agent_cron-4daaedfb795f4be8-9a26b122.meta.json"),
		`{"key":"agent:cron-4daaedfb795f4be8-9a26b122","count":9,"created_at":"2026-07-31T21:00:00Z",`+
			`"updated_at":"2026-07-31T21:02:00Z","scope":{"values":{"chat":"direct:pico:abc"}}}`)
	seedFile(t, filepath.Join(sessionsDir, "agent_cron-4daaedfb795f4be8-9a26b122.jsonl"),
		`{"role":"user","content":"tarefa de uma vez","created_at":"2026-07-31T21:00:00Z"}`+"\n")

	rec := do(t, s, "/v1/cron/tasks?"+cronQuery)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var got struct {
		Tasks []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Enabled  bool   `json:"enabled"`
			Schedule struct {
				Kind    string `json:"kind"`
				Expr    string `json:"expr"`
				EveryMs int64  `json:"everyMs"`
			} `json:"schedule"`
			State struct {
				LastStatus  string `json:"lastStatus"`
				NextRunAtMs int64  `json:"nextRunAtMs"`
			} `json:"state"`
			Runs []struct {
				RunID    string `json:"runId"`
				Basename string `json:"basename"`
				Count    int    `json:"count"`
				Prompt   string `json:"prompt"`
			} `json:"runs"`
		} `json:"tasks"`
		Orphans []struct {
			JobID string `json:"jobId"`
			Runs  []struct {
				Prompt string `json:"prompt"`
			} `json:"runs"`
		} `json:"orphans"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(got.Tasks) != 2 {
		t.Fatalf("got %d tasks, want 2: %s", len(got.Tasks), rec.Body)
	}
	// Store order is preserved, so the panel shows what picoclaw shows.
	daily := got.Tasks[0]
	if daily.ID != "e520b224e7714d16" || daily.Name != "Relatorio diario" || !daily.Enabled {
		t.Errorf("tasks[0] identity wrong: %+v", daily)
	}
	if daily.Schedule.Kind != "cron" || daily.Schedule.Expr != "0 18 * * *" {
		t.Errorf("tasks[0] schedule = %+v", daily.Schedule)
	}
	if daily.State.LastStatus != "ok" || daily.State.NextRunAtMs != 1785780000000 {
		t.Errorf("tasks[0] state = %+v", daily.State)
	}
	if len(daily.Runs) != 2 {
		t.Fatalf("tasks[0] has %d runs, want its 2", len(daily.Runs))
	}
	if daily.Runs[0].RunID != "5e055123" {
		t.Errorf("runs[0].RunID = %q, want the newest run first", daily.Runs[0].RunID)
	}
	if daily.Runs[0].Count != 34 || daily.Runs[0].Prompt != "gerar relatorio" {
		t.Errorf("runs[0] = %+v, want the meta count and the transcript's first entry", daily.Runs[0])
	}

	ping := got.Tasks[1]
	if ping.Enabled || ping.Schedule.EveryMs != 300000 {
		t.Errorf("tasks[1] = %+v, want the disabled every-job", ping)
	}
	if ping.Runs == nil {
		t.Error("tasks[1].runs = null, want an empty array so the client can iterate it")
	}

	if len(got.Orphans) != 1 {
		t.Fatalf("got %d orphan groups, want 1: %s", len(got.Orphans), rec.Body)
	}
	if got.Orphans[0].JobID != "4daaedfb795f4be8" {
		t.Errorf("orphan jobId = %q, want the deleted job", got.Orphans[0].JobID)
	}
	if len(got.Orphans[0].Runs) != 1 || got.Orphans[0].Runs[0].Prompt != "tarefa de uma vez" {
		t.Errorf("orphan runs = %+v, want the surviving transcript named by its prompt", got.Orphans[0].Runs)
	}
}

// Refusing an unfamiliar store layout beats rendering a partial one: a silent
// mis-parse would look like tasks that lost their schedule.
func TestCronTasksRejectsForeignStoreVersion(t *testing.T) {
	s, root := cronServer(t)
	cronFile, _ := cronPaths(root)
	seedFile(t, cronFile, `{"version":99,"jobs":[]}`)

	rec := do(t, s, "/v1/cron/tasks?"+cronQuery)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500: %s", rec.Code, rec.Body)
	}
}

func TestCronRoutesRequireScopeParams(t *testing.T) {
	s, _ := cronServer(t)
	for name, path := range map[string]string{
		"tasks without tenant":  "/v1/cron/tasks?subs_acc_id=" + subsX,
		"tasks with bad tenant": "/v1/cron/tasks?tenant_id=nope&subs_acc_id=" + subsX,
		"tasks without subs":    "/v1/cron/tasks?tenant_id=" + tenantT,
		"runs without tenant":   "/v1/cron/runs?subs_acc_id=" + subsX + "&run=agent_cron-a-b",
		"runs without subs":     "/v1/cron/runs?tenant_id=" + tenantT + "&run=agent_cron-a-b",
	} {
		t.Run(name, func(t *testing.T) {
			if rec := do(t, s, path); rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestCronRunReturnsTranscriptWithToolActivity(t *testing.T) {
	s, root := cronServer(t)
	_, sessionsDir := cronPaths(root)

	seedFile(t, filepath.Join(sessionsDir, "agent_cron-abc123def456abcd-run1.meta.json"),
		`{"key":"agent:cron-abc123def456abcd-run1","count":4,"created_at":"2026-08-02T20:58:00Z",`+
			`"updated_at":"2026-08-02T21:00:00Z","scope":{"values":{"chat":"direct:pico:abc"}}}`)
	seedFile(t, filepath.Join(sessionsDir, "agent_cron-abc123def456abcd-run1.jsonl"),
		`{"role":"user","content":"buscar noticias","created_at":"t0"}`+"\n"+
			`{"role":"assistant","content":"vou buscar","model_name":"DeepSeek-V4-Pro-Azure","created_at":"t1",`+
			`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"web_search","arguments":"{}"}}]}`+"\n"+
			`{"role":"tool","content":"Results for: iran","created_at":"t2","tool_call_id":"call_1"}`+"\n"+
			`{"role":"assistant","content":"pronto","created_at":"t3"}`+"\n")

	rec := do(t, s, "/v1/cron/runs?"+cronQuery+"&run=agent_cron-abc123def456abcd-run1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var got struct {
		Entries []struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ModelName string `json:"model_name"`
			ToolCalls []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Entries) != 4 {
		t.Fatalf("got %d entries, want all 4 including the tool entry: %s", len(got.Entries), rec.Body)
	}
	if got.Entries[2].Role != "tool" {
		t.Errorf("entries[2].role = %q, want the tool entry served", got.Entries[2].Role)
	}
	if len(got.Entries[1].ToolCalls) != 1 || got.Entries[1].ToolCalls[0].Function.Name != "web_search" {
		t.Errorf("entries[1].tool_calls = %+v, want the web_search call", got.Entries[1].ToolCalls)
	}
	if got.Entries[1].ModelName != "DeepSeek-V4-Pro-Azure" {
		t.Errorf("entries[1].model_name = %q, want it served", got.Entries[1].ModelName)
	}
}

// The run parameter names a file, so it is resolved against the runs actually
// discovered in the caller's OWN sessions dir rather than joined into a path.
// Traversal and cross-workspace reads are then impossible by construction, not by
// sanitisation.
func TestCronRunRejectsUnknownRun(t *testing.T) {
	s, root := cronServer(t)
	_, sessionsDir := cronPaths(root)
	seedFile(t, filepath.Join(sessionsDir, "agent_cron-abc123def456abcd-run1.meta.json"),
		`{"key":"agent:cron-abc123def456abcd-run1","count":1,"created_at":"t","updated_at":"t",`+
			`"scope":{"values":{"chat":"direct:pico:abc"}}}`)
	seedFile(t, filepath.Join(sessionsDir, "..", "..", "elsewhere.jsonl"), `{"role":"user","content":"secret"}`)

	for name, run := range map[string]string{
		"traversal":      "../../elsewhere",
		"absolute":       "/etc/passwd",
		"a user session": "sk_v1_7f7c41b4",
		"nonexistent":    "agent_cron-0000000000000000-nope",
		"empty":          "",
	} {
		t.Run(name, func(t *testing.T) {
			rec := do(t, s, "/v1/cron/runs?"+cronQuery+"&run="+run)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 for run=%q: %s", rec.Code, run, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "secret") {
				t.Error("the response leaked content from outside the sessions dir")
			}
		})
	}
}
