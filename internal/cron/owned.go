package cron

// The writable half of this package: the store the PROXY owns, for harnesses
// that have no scheduler of their own.
//
// Everything here refuses to be pointed at picoclaw's file by accident. Owner
// takes a path and does not compose one, and the only path a caller can get is
// config.SchedulesFile — CronFile is reached by Load, which cannot write.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/adhocore/gronx"
)

// MinEveryMs is the floor on an "every" schedule.
//
// Not arbitrary and not about load on the proxy: every fire may COLD-START a
// container that scale-to-zero has stopped, so a ten-second interval is a request
// to keep an agent permanently warm by the back door. A minute is the finest
// granularity a cron expression can express anyway, so this refuses nothing the
// other kind allows.
const MinEveryMs = 60_000

// Kinds of schedule and payload this store accepts. picoclaw's vocabulary,
// because the record is picoclaw's.
const (
	KindCron  = "cron"
	KindEvery = "every"
	KindAt    = "at"

	PayloadAgentTurn = "agent_turn"
)

// The values written into State.LastStatus by the proxy's own scheduler.
//
// picoclaw's field is a plain string and no value of it was ever observed, which
// is why every reader displays it verbatim and none branches on it. These are the
// first values anything in this stack actually writes; they stay a closed pair so
// the panel can eventually colour them, and nothing here or upstream may assume
// a picoclaw job carries either.
const (
	StatusOK    = "ok"
	StatusError = "error"
)

// ErrNotFound is returned by Mutate's callback helpers when the job is absent.
var ErrNotFound = errors.New("scheduled task not found")

// Owner reads and writes proxy-owned stores.
//
// One per process. The lock is per PATH rather than one global one, because the
// scheduler writes a job's state right after firing it while an unrelated
// member's request may be editing their own file; serialising those two would be
// a shared bottleneck for no safety gained. Two writers on the SAME file are
// real, though — the scheduler and the member's own PATCH — which is what these
// locks are for. Nothing outside this process writes these files.
type Owner struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewOwner() *Owner { return &Owner{locks: map[string]*sync.Mutex{}} }

func (o *Owner) lockFor(path string) *sync.Mutex {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.locks == nil {
		o.locks = map[string]*sync.Mutex{}
	}
	l, ok := o.locks[path]
	if !ok {
		l = &sync.Mutex{}
		o.locks[path] = l
	}
	return l
}

// List returns the jobs in a proxy-owned store. A missing file is an empty list,
// not an error: the store appears with the member's first task.
func (o *Owner) List(path string) ([]Job, error) {
	l := o.lockFor(path)
	l.Lock()
	defer l.Unlock()
	return Load(path)
}

// Mutate applies fn to the store under its lock and writes the result back.
//
// fn returning an error leaves the file untouched, which is what makes a
// validation failure inside a mutation safe to express as a plain return.
func (o *Owner) Mutate(path string, fn func([]Job) ([]Job, error)) error {
	l := o.lockFor(path)
	l.Lock()
	defer l.Unlock()

	jobs, err := Load(path)
	if err != nil {
		return err
	}
	next, err := fn(jobs)
	if err != nil {
		return err
	}
	return writeStore(path, next)
}

// writeStore persists the store atomically: a temp file in the same directory, then a
// rename. A partial write here is not a lost edit, it is a store that fails to
// parse — after which every one of the member's schedules is invisible and inert.
func writeStore(path string, jobs []Job) error {
	if jobs == nil {
		jobs = []Job{}
	}
	raw, err := json.MarshalIndent(store{Version: storeVersion, Jobs: jobs}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".schedules-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// NewID returns an id for a new job.
//
// 8 bytes of hex and NO dash, which is load-bearing rather than cosmetic: a run's
// session key is "agent:cron-<jobID>-<runID>" and history.splitCronKey separates
// the two at the FIRST dash. A job id containing one would list every run of that
// job under a truncated id.
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any platform this runs on; falling back to
		// a timestamp keeps a failure from being silently non-unique.
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Validate checks a job the member is trying to store.
//
// Deliberately strict about the schedule and permissive about everything else:
// an unschedulable job is one that never fires and never says why, while an
// unfamiliar payload is the forward-compatibility this record shape was copied
// for.
func Validate(j Job) error {
	if j.Payload.Kind != "" && j.Payload.Kind != PayloadAgentTurn {
		return fmt.Errorf("unsupported payload kind %q", j.Payload.Kind)
	}
	if j.Payload.Message == "" {
		return errors.New("payload.message is required")
	}
	switch j.Schedule.Kind {
	case KindCron:
		if j.Schedule.Expr == "" {
			return errors.New("schedule.expr is required for a cron schedule")
		}
		if _, err := location(j.Schedule.Tz); err != nil {
			return err
		}
		if !gronx.IsValid(j.Schedule.Expr) {
			return fmt.Errorf("invalid cron expression %q", j.Schedule.Expr)
		}
	case KindEvery:
		if j.Schedule.EveryMs < MinEveryMs {
			return fmt.Errorf("schedule.everyMs must be at least %d", MinEveryMs)
		}
	case KindAt:
		if j.Schedule.AtMs <= 0 {
			return errors.New("schedule.atMs is required for a one-shot schedule")
		}
	default:
		return fmt.Errorf("unsupported schedule kind %q", j.Schedule.Kind)
	}
	return nil
}

// location resolves a schedule's zone. Empty is UTC.
func location(tz string) (*time.Location, error) {
	if tz == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q", tz)
	}
	return loc, nil
}

// NextRun reports when a job runs next, strictly after `after`.
//
// The second return is false when the job has no next run — a one-shot whose
// instant has passed. That is a terminal state, not an error: the scheduler
// disables or deletes such a job rather than asking again every tick.
func NextRun(s Schedule, after time.Time) (time.Time, bool) {
	switch s.Kind {
	case KindCron:
		loc, err := location(s.Tz)
		if err != nil {
			return time.Time{}, false
		}
		// gronx reads the expression in the reference time's own zone, so the zone
		// is applied by converting the reference rather than by any option of its
		// own. "0 9 * * *" with tz America/Sao_Paulo means 9am there.
		next, err := gronx.NextTickAfter(s.Expr, after.In(loc), false)
		if err != nil {
			return time.Time{}, false
		}
		return next.UTC(), true
	case KindEvery:
		if s.EveryMs <= 0 {
			return time.Time{}, false
		}
		return after.Add(time.Duration(s.EveryMs) * time.Millisecond).UTC(), true
	case KindAt:
		at := time.UnixMilli(s.AtMs).UTC()
		if !at.After(after) {
			return time.Time{}, false
		}
		return at, true
	}
	return time.Time{}, false
}
