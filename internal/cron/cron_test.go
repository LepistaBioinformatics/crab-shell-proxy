package cron

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write drops a jobs.json into a temp dir and returns its path.
func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jobs.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The three schedule kinds picoclaw writes. "cron" and "every" were produced and
// observed by driving the CLI; "at" is the one-shot kind that exists in the job
// system but has no CLI flag today, so its shape follows the same pattern.
func TestLoadScheduleKinds(t *testing.T) {
	cases := []struct {
		name    string
		sched   string
		wantKnd string
		check   func(*testing.T, Schedule)
	}{
		{
			name:    "cron",
			sched:   `{"kind":"cron","expr":"0 18 * * *"}`,
			wantKnd: "cron",
			check: func(t *testing.T, s Schedule) {
				if s.Expr != "0 18 * * *" {
					t.Errorf("Expr = %q, want the cron expression", s.Expr)
				}
			},
		},
		{
			name:    "every",
			sched:   `{"kind":"every","everyMs":300000}`,
			wantKnd: "every",
			check: func(t *testing.T, s Schedule) {
				if s.EveryMs != 300000 {
					t.Errorf("EveryMs = %d, want 300000", s.EveryMs)
				}
			},
		},
		{
			name:    "at",
			sched:   `{"kind":"at","atMs":1785780000000}`,
			wantKnd: "at",
			check: func(t *testing.T, s Schedule) {
				if s.AtMs != 1785780000000 {
					t.Errorf("AtMs = %d, want the one-shot instant", s.AtMs)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := write(t, `{"version":1,"jobs":[{"id":"abc","schedule":`+tc.sched+`}]}`)
			jobs, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(jobs) != 1 {
				t.Fatalf("len(jobs) = %d, want 1", len(jobs))
			}
			if jobs[0].Schedule.Kind != tc.wantKnd {
				t.Errorf("Kind = %q, want %q", jobs[0].Schedule.Kind, tc.wantKnd)
			}
			tc.check(t, jobs[0].Schedule)
		})
	}
}

// A kind this proxy has never seen must survive as its own string with no
// parameter invented for it, so the caller can render what it has instead of the
// whole list failing over one unfamiliar job.
func TestLoadUnknownKindsDegrade(t *testing.T) {
	path := write(t, `{"version":1,"jobs":[{
		"id":"abc",
		"schedule":{"kind":"solar_eclipse","orbitDays":3},
		"payload":{"kind":"shell_command","command":"ls"}}]}`)

	jobs, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if jobs[0].Schedule.Kind != "solar_eclipse" {
		t.Errorf("Schedule.Kind = %q, want it preserved verbatim", jobs[0].Schedule.Kind)
	}
	if jobs[0].Schedule.Expr != "" || jobs[0].Schedule.EveryMs != 0 || jobs[0].Schedule.AtMs != 0 {
		t.Errorf("unknown schedule kind gained a parameter: %+v", jobs[0].Schedule)
	}
	if jobs[0].Payload.Kind != "shell_command" {
		t.Errorf("Payload.Kind = %q, want it preserved verbatim", jobs[0].Payload.Kind)
	}
}

// The full record as observed from sipeed/picoclaw:latest, plus a field this
// version of the proxy does not know about.
func TestLoadFullRecord(t *testing.T) {
	path := write(t, `{"version":1,"jobs":[{
		"id":"9abd3e01bd0a082a",
		"name":"Daily summary",
		"enabled":true,
		"schedule":{"kind":"cron","expr":"0 18 * * *"},
		"payload":{"kind":"agent_turn","message":"Summarize logs","channel":"pico","to":"pico:deadbeef"},
		"state":{"nextRunAtMs":1785780000000,"lastRunAtMs":1785693600000,"lastStatus":"ok","lastError":""},
		"createdAtMs":1785766614402,
		"updatedAtMs":1785766614402,
		"deleteAfterRun":false,
		"someFieldFromAFuturePicoclaw":42}]}`)

	jobs, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	j := jobs[0]
	if j.ID != "9abd3e01bd0a082a" || j.Name != "Daily summary" || !j.Enabled {
		t.Errorf("identity fields wrong: %+v", j)
	}
	if j.Payload.Message != "Summarize logs" || j.Payload.Channel != "pico" || j.Payload.To != "pico:deadbeef" {
		t.Errorf("payload wrong: %+v", j.Payload)
	}
	if j.State.NextRunAtMs != 1785780000000 || j.State.LastRunAtMs != 1785693600000 {
		t.Errorf("state instants wrong: %+v", j.State)
	}
	if j.State.LastStatus != "ok" {
		t.Errorf("LastStatus = %q, want it carried verbatim", j.State.LastStatus)
	}
	if j.CreatedAtMs != 1785766614402 || j.UpdatedAtMs != 1785766614402 || j.DeleteAfterRun {
		t.Errorf("bookkeeping fields wrong: %+v", j)
	}
}

// An agent that has never scheduled anything has an empty store, and one that
// never ran the cron CLI has no file at all. Neither is a failure.
func TestLoadEmptyAndMissing(t *testing.T) {
	t.Run("empty jobs array", func(t *testing.T) {
		jobs, err := Load(write(t, `{"version":1,"jobs":[]}`))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(jobs) != 0 {
			t.Errorf("len(jobs) = %d, want 0", len(jobs))
		}
	})

	t.Run("missing file", func(t *testing.T) {
		jobs, err := Load(filepath.Join(t.TempDir(), "cron", "jobs.json"))
		if err != nil {
			t.Fatalf("a workspace with no cron store must not error: %v", err)
		}
		if jobs != nil {
			t.Errorf("jobs = %+v, want nil", jobs)
		}
	})
}

// Guessing at an unfamiliar layout is worse than refusing to read it: a silent
// mis-parse would surface as tasks that quietly lost their schedule.
func TestLoadRejectsForeignVersion(t *testing.T) {
	for _, body := range []string{
		`{"version":2,"jobs":[]}`,
		`{"jobs":[]}`,
	} {
		_, err := Load(write(t, body))
		if err == nil {
			t.Fatalf("Load(%s) = nil error, want a version error", body)
		}
		if !strings.Contains(err.Error(), "version") {
			t.Errorf("error %q does not name the version it found", err)
		}
	}
}

func TestLoadRejectsMalformedJSON(t *testing.T) {
	if _, err := Load(write(t, `{"version":1,"jobs":[`)); err == nil {
		t.Error("Load on truncated JSON = nil error, want a parse error")
	}
}
