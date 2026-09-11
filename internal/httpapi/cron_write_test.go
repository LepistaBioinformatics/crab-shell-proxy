package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/config"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/cron"
	"github.com/LepistaBioinformatics/crab-shell-proxy/internal/projects"
)

// ganglionCronServer is cronServer with the agent the headers resolve to running
// the ganglion harness, which is the only one these routes serve.
func ganglionCronServer(t *testing.T) (*Server, string) {
	t.Helper()
	s, root := cronServer(t)
	agent := s.Cfg.Agents["alpha"]
	agent.Harness = config.HarnessGanglion
	s.Cfg.Agents["alpha"] = agent
	return s, root
}

func cronWrite(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	for k, v := range goodHeaders(t) {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

func decodeJob(t *testing.T, rec *httptest.ResponseRecorder) cron.Job {
	t.Helper()
	var j cron.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil {
		t.Fatalf("decode job: %v (%s)", err, rec.Body)
	}
	return j
}

func listTasks(t *testing.T, s *Server, query string) []cron.Job {
	t.Helper()
	rec := do(t, s, "/v1/cron/tasks?"+query)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		Tasks []cron.Job `json:"tasks"`
		Fires bool       `json:"fires"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	return got.Tasks
}

// The whole member-facing lifecycle over one store.
func TestCronTaskCreateThenListThenPatchThenDelete(t *testing.T) {
	s, _ := ganglionCronServer(t)

	rec := cronWrite(t, s, http.MethodPost, "/v1/cron/tasks?"+cronQuery,
		`{"name":"resumo","message":"resuma o dia","schedule":{"kind":"cron","expr":"0 9 * * *"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body)
	}
	created := decodeJob(t, rec)
	if created.ID == "" || !created.Enabled {
		t.Fatalf("created record is not usable: %+v", created)
	}
	// Answered with a next run already computed, so the panel can say when it runs
	// without waiting for the scheduler's first pass.
	if created.State.NextRunAtMs == 0 {
		t.Error("created task carries no next run")
	}

	tasks := listTasks(t, s, cronQuery)
	if len(tasks) != 1 || tasks[0].ID != created.ID {
		t.Fatalf("the created task is not listed: %+v", tasks)
	}

	rec = cronWrite(t, s, http.MethodPatch, "/v1/cron/tasks?"+cronQuery,
		`{"id":"`+created.ID+`","enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d: %s", rec.Code, rec.Body)
	}
	if paused := decodeJob(t, rec); paused.Enabled || paused.Name != "resumo" {
		t.Fatalf("pausing changed the wrong thing: %+v", paused)
	}

	rec = cronWrite(t, s, http.MethodDelete, "/v1/cron/tasks?"+cronQuery+"&id="+created.ID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204: %s", rec.Code, rec.Body)
	}
	if tasks := listTasks(t, s, cronQuery); len(tasks) != 0 {
		t.Fatalf("the task survived its deletion: %+v", tasks)
	}
}

// A task belongs to the scope it was filed under: its runs are written into that
// project's sessions dir, so listing it anywhere else would offer to open
// transcripts that live somewhere it is not looking.
func TestCronTaskIsScopedToItsProject(t *testing.T) {
	s, _ := ganglionCronServer(t)
	s.Mgr.(*fakeOrch).projects = []projects.Project{{ID: "seedtrial", Name: "Seed trial"}}

	rec := cronWrite(t, s, http.MethodPost, "/v1/cron/tasks?"+cronQuery+"&project=seedtrial",
		`{"message":"resuma o ensaio","schedule":{"kind":"every","everyMs":3600000}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body)
	}
	created := decodeJob(t, rec)
	if created.Project != "seedtrial" {
		t.Fatalf("project not recorded: %+v", created)
	}

	if tasks := listTasks(t, s, cronQuery+"&project=seedtrial"); len(tasks) != 1 {
		t.Fatalf("the project's own panel does not list it: %+v", tasks)
	}
	// The global panel lists what belongs to no project, plus jobs whose project is
	// gone. This one's project is very much alive.
	if tasks := listTasks(t, s, cronQuery); len(tasks) != 0 {
		t.Fatalf("a project task leaked into the global list: %+v", tasks)
	}
	// And it cannot be edited from the wrong scope.
	rec = cronWrite(t, s, http.MethodPatch, "/v1/cron/tasks?"+cronQuery,
		`{"id":"`+created.ID+`","enabled":false}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("patch from the wrong scope = %d, want 404", rec.Code)
	}
}

// An unknown project is refused before anything is stored, exactly as it is on
// every other project-scoped route.
func TestCronTaskCreateRefusesAnUnknownProject(t *testing.T) {
	s, _ := ganglionCronServer(t)
	rec := cronWrite(t, s, http.MethodPost, "/v1/cron/tasks?"+cronQuery+"&project=nope",
		`{"message":"x","schedule":{"kind":"cron","expr":"0 9 * * *"}}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

// picoclaw holds its own timers in its own memory. Writing its jobs.json from
// here would produce a record the member can see and a schedule that never
// changed, which is the silent-success failure the harness gate exists to stop.
func TestCronTaskWritesAreRefusedOnPicoclaw(t *testing.T) {
	s, _ := cronServer(t) // agent alpha, picoclaw
	for _, c := range []struct {
		method, path, body string
	}{
		{http.MethodPost, "/v1/cron/tasks?" + cronQuery, `{"message":"x","schedule":{"kind":"cron","expr":"0 9 * * *"}}`},
		{http.MethodPatch, "/v1/cron/tasks?" + cronQuery, `{"id":"a","enabled":false}`},
		{http.MethodDelete, "/v1/cron/tasks?" + cronQuery + "&id=a", ""},
	} {
		rec := cronWrite(t, s, c.method, c.path, c.body)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s = %d, want 501: %s", c.method, rec.Code, rec.Body)
		}
	}
}

func TestCronTaskCreateRefusesAnUnschedulableTask(t *testing.T) {
	s, _ := ganglionCronServer(t)
	for name, body := range map[string]string{
		"no message":           `{"schedule":{"kind":"cron","expr":"0 9 * * *"}}`,
		"bad expression":       `{"message":"x","schedule":{"kind":"cron","expr":"every friday"}}`,
		"interval too fine":    `{"message":"x","schedule":{"kind":"every","everyMs":1000}}`,
		"one-shot in the past": `{"message":"x","schedule":{"kind":"at","atMs":1}}`,
	} {
		rec := cronWrite(t, s, http.MethodPost, "/v1/cron/tasks?"+cronQuery, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", name, rec.Code, rec.Body)
		}
	}
}

// A ganglion schedule is held by the proxy, which is always up and which STARTS
// the container to fire a job. Reporting it as inert because the agent scales to
// zero would be false, and scale-to-zero is what it was designed around.
func TestCronTasksFiresIsTrueForAScaleToZeroGanglion(t *testing.T) {
	s, _ := ganglionCronServer(t)
	rec := do(t, s, "/v1/cron/tasks?"+cronQuery)
	var got struct {
		Fires bool `json:"fires"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Fires {
		t.Error("fires = false for a ganglion agent whose schedules the proxy holds")
	}
}

// The store is the proxy's own file above the workspace bind, never picoclaw's
// path inside it. Writing the wrong one would put a schedule where the agent can
// edit it -- and leave the panel reading a file nothing creates.
func TestCronTaskIsStoredAboveTheWorkspaceBind(t *testing.T) {
	s, root := ganglionCronServer(t)
	rec := cronWrite(t, s, http.MethodPost, "/v1/cron/tasks?"+cronQuery,
		`{"message":"x","schedule":{"kind":"cron","expr":"0 9 * * *"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body)
	}
	jobs, err := cron.Load(config.SchedulesFile(root, tenantT, subsX, "alpha", accAlice))
	if err != nil || len(jobs) != 1 {
		t.Fatalf("the proxy-owned store does not hold the task: %v / %+v", err, jobs)
	}
	cronFile, _ := cronPaths(root)
	if picoJobs, _ := cron.Load(cronFile); len(picoJobs) != 0 {
		t.Fatalf("the task was written into picoclaw's store: %+v", picoJobs)
	}
}
