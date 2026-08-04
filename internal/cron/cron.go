// Package cron reads picoclaw's scheduled-job store.
//
// The store is picoclaw's, not the proxy's: picoclaw creates, schedules and
// deletes jobs, and holds the live schedule in memory. This package therefore
// exposes no writer. Editing the file from outside would diverge from those
// in-memory timers with no way to tell the user, so the member surface stays
// read-only until that reload behavior is verified.
//
// The record shape below was taken from sipeed/picoclaw:latest by creating jobs
// and dumping the file, not from documentation.
package cron

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// storeVersion is the only layout this package claims to understand. Anything
// else is refused rather than parsed optimistically: a silent mis-parse would
// surface to the user as tasks that mysteriously lost their schedule.
const storeVersion = 1

// Schedule is when a job runs. Exactly one parameter is meaningful, selected by
// Kind: "cron" uses Expr, "every" uses EveryMs, "at" is the one-shot kind and
// uses AtMs. An unrecognised Kind keeps its string and carries no parameter, so
// one unfamiliar job renders as best it can instead of failing the whole list.
type Schedule struct {
	Kind    string `json:"kind"`
	Expr    string `json:"expr,omitempty"`
	EveryMs int64  `json:"everyMs,omitempty"`
	AtMs    int64  `json:"atMs,omitempty"`
}

// Payload is what the job does when it fires. Only "agent_turn" has been
// observed; picoclaw's feature config also carries an allow_command switch, so
// other kinds are plausible and handled the same way as unknown schedules.
// Channel and To appear only on jobs created with delivery flags.
type Payload struct {
	Kind    string `json:"kind"`
	Message string `json:"message,omitempty"`
	Channel string `json:"channel,omitempty"`
	To      string `json:"to,omitempty"`
}

// State is picoclaw's own bookkeeping for a job.
//
// LastStatus is deliberately a plain string that callers display verbatim: no
// real value has ever been observed, so nothing here or upstream may branch on
// one. It also describes only the most recent run — per-run outcomes are not
// recorded anywhere, which is why run listings carry no success mark.
type State struct {
	NextRunAtMs int64  `json:"nextRunAtMs,omitempty"`
	LastRunAtMs int64  `json:"lastRunAtMs,omitempty"`
	LastStatus  string `json:"lastStatus,omitempty"`
	LastError   string `json:"lastError,omitempty"`
}

// Job is one scheduled task.
//
// DeleteAfterRun is load-bearing for anything joining jobs to their run
// transcripts: a one-shot job removes itself from this store once it has run,
// while its transcript stays on disk. Runs whose job is absent here are a normal
// outcome, not a missing record.
type Job struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Enabled        bool     `json:"enabled"`
	Schedule       Schedule `json:"schedule"`
	Payload        Payload  `json:"payload"`
	State          State    `json:"state"`
	CreatedAtMs    int64    `json:"createdAtMs"`
	UpdatedAtMs    int64    `json:"updatedAtMs"`
	DeleteAfterRun bool     `json:"deleteAfterRun"`
}

type store struct {
	Version int   `json:"version"`
	Jobs    []Job `json:"jobs"`
}

// Load reads the job store at path.
//
// A missing file returns no jobs and no error: the store appears only once the
// agent schedules its first task, so its absence is the ordinary state of a
// workspace that has never used cron.
func Load(path string) ([]Job, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var s store
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse cron store: %w", err)
	}
	if s.Version != storeVersion {
		return nil, fmt.Errorf("unsupported cron store version %d, want %d", s.Version, storeVersion)
	}
	return s.Jobs, nil
}
