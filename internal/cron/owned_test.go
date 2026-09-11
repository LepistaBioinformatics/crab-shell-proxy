package cron

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ownedPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".schedules.json")
}

func TestOwnerMutateRoundTripsThroughLoad(t *testing.T) {
	o := NewOwner()
	path := ownedPath(t)

	if err := o.Mutate(path, func(jobs []Job) ([]Job, error) {
		return append(jobs, Job{ID: "a1", Name: "daily", Enabled: true, Project: "seedtrial"}), nil
	}); err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	got, err := o.List(path)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a1" || got[0].Project != "seedtrial" {
		t.Fatalf("round trip lost the record: %+v", got)
	}
	// The envelope has to be the one Load accepts, or the read side stops
	// understanding a store the write side produced.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"version": 1`) {
		t.Fatalf("store envelope is not version 1: %s", raw)
	}
}

// A callback that fails must leave the file exactly as it was, or a refused edit
// becomes a corrupted schedule.
func TestOwnerMutateLeavesTheStoreAloneWhenTheCallbackFails(t *testing.T) {
	o := NewOwner()
	path := ownedPath(t)
	if err := o.Mutate(path, func(jobs []Job) ([]Job, error) {
		return append(jobs, Job{ID: "keep"}), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := o.Mutate(path, func(jobs []Job) ([]Job, error) {
		return nil, ErrNotFound
	}); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	got, err := o.List(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "keep" {
		t.Fatalf("a refused mutation changed the store: %+v", got)
	}
}

// history.splitCronKey cuts the run key at the FIRST dash, so a job id carrying
// one would file every run of that job under a truncated id.
func TestNewIDCarriesNoDash(t *testing.T) {
	for i := 0; i < 64; i++ {
		if id := NewID(); strings.Contains(id, "-") {
			t.Fatalf("job id %q contains a dash", id)
		}
	}
}

func TestJobProjectPrefersTheExplicitField(t *testing.T) {
	// A proxy-written job has no originating conversation to derive from.
	if got := JobProject(Job{Project: "seedtrial"}); got != "seedtrial" {
		t.Fatalf("explicit project ignored: %q", got)
	}
	// A picoclaw job still derives, unchanged.
	if got := JobProject(Job{Payload: Payload{To: "pico:p.seedtrial.abc"}}); got != "seedtrial" {
		t.Fatalf("picoclaw derivation broke: %q", got)
	}
}

func TestNextRun(t *testing.T) {
	base := time.Date(2026, 9, 10, 8, 30, 0, 0, time.UTC)

	t.Run("cron reads the expression in the schedule's own zone", func(t *testing.T) {
		// 09:00 in Sao Paulo (UTC-3) is 12:00 UTC. A schedule read in UTC would
		// answer 09:00 UTC, three hours early, every day, silently.
		at, ok := NextRun(Schedule{Kind: KindCron, Expr: "0 9 * * *", Tz: "America/Sao_Paulo"}, base)
		if !ok {
			t.Fatal("no next run")
		}
		if at.Hour() != 12 || at.Day() != 10 {
			t.Fatalf("want 12:00 UTC on the 10th, got %s", at)
		}
	})

	t.Run("cron with no zone is UTC", func(t *testing.T) {
		at, ok := NextRun(Schedule{Kind: KindCron, Expr: "0 9 * * *"}, base)
		if !ok || at.Hour() != 9 {
			t.Fatalf("want 09:00 UTC, got %s (ok=%v)", at, ok)
		}
	})

	t.Run("every counts from the reference", func(t *testing.T) {
		at, ok := NextRun(Schedule{Kind: KindEvery, EveryMs: 3600_000}, base)
		if !ok || !at.Equal(base.Add(time.Hour)) {
			t.Fatalf("want +1h, got %s (ok=%v)", at, ok)
		}
	})

	t.Run("a one-shot already past has no next run", func(t *testing.T) {
		if _, ok := NextRun(Schedule{Kind: KindAt, AtMs: base.Add(-time.Hour).UnixMilli()}, base); ok {
			t.Fatal("a past one-shot reported a next run")
		}
	})
}

func TestValidate(t *testing.T) {
	ok := Job{Payload: Payload{Message: "resumo diario"},
		Schedule: Schedule{Kind: KindCron, Expr: "0 9 * * *"}}
	if err := Validate(ok); err != nil {
		t.Fatalf("a valid job was refused: %v", err)
	}

	cases := map[string]Job{
		"no message": {Schedule: Schedule{Kind: KindCron, Expr: "0 9 * * *"}},
		"unknown schedule kind": {Payload: Payload{Message: "x"},
			Schedule: Schedule{Kind: "whenever"}},
		"invalid expression": {Payload: Payload{Message: "x"},
			Schedule: Schedule{Kind: KindCron, Expr: "not a cron"}},
		"unknown timezone": {Payload: Payload{Message: "x"},
			Schedule: Schedule{Kind: KindCron, Expr: "0 9 * * *", Tz: "Mars/Olympus"}},
		// Below the floor, every fire may cold-start a stopped container.
		"interval under the floor": {Payload: Payload{Message: "x"},
			Schedule: Schedule{Kind: KindEvery, EveryMs: 5_000}},
		"one-shot with no instant": {Payload: Payload{Message: "x"},
			Schedule: Schedule{Kind: KindAt}},
		"unsupported payload": {Payload: Payload{Kind: "shell", Message: "x"},
			Schedule: Schedule{Kind: KindCron, Expr: "0 9 * * *"}},
	}
	for name, j := range cases {
		if err := Validate(j); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
