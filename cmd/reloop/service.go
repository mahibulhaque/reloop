package main

import (
	"context"
	"regexp"
	"syscall"
	"time"

	"github.com/mahibulhaque/reloop/internal/daemon"
	"github.com/mahibulhaque/reloop/internal/reloop"
	"github.com/mahibulhaque/reloop/internal/store"
)

// service is the CLI's composition root.
//
// It wires the store and daemon-state probe into command operations.
// Every mutation sends SIGHUP to a running daemon.
// That wakes the scheduler so it re-evaluates immediately.
type service struct {
	st *store.Store
}

func newService(st *store.Store) *service { return &service{st: st} }

// notify sends SIGHUP to a running daemon so it interrupts its sleep
// and re-evaluates the schedule. Silent no-op when no daemon is up.
func (s *service) notify() {
	_, _ = daemon.SignalDaemon(s.st.DataDir(), syscall.SIGHUP)
}

// Add inserts a job. now comes from the command so parsing --at and
// validating it use the same clock reading.
func (s *service) Add(ctx context.Context, spec reloop.JobSpec, now time.Time) (reloop.Job, error) {
	// Validation happens inside AddJob so the store is the single gate.
	job, err := s.st.AddJob(ctx, spec, now)
	if err != nil {
		return reloop.Job{}, err
	}
	s.notify()
	return job, nil
}

// idShape matches the 5-char alphanumeric handle produced by the
// store. Anything else is treated as a name when resolving CLI input.
var idShape = regexp.MustCompile(`^[0-9A-Za-z]{5}$`)

// Resolve looks up a job by ID or name.
//
// Lookup order:
//   - A 5-char alphanumeric arg is tried as an ID first.
//   - If that misses, it falls back to name lookup.
//   - Anything else is treated as a name only.
//
// The returned Job carries the real ID for follow-on writes.
func (s *service) Resolve(ctx context.Context, arg string) (reloop.Job, error) {
	if idShape.MatchString(arg) {
		job, err := s.st.Job(ctx, reloop.JobID(arg))
		if err == nil {
			return job, nil
		}
		// Fall through to name lookup so an unlucky name that happens
		// to look like an ID still resolves.
	}
	return s.st.JobByName(ctx, arg)
}

// List returns jobs matching opts.
//
// Limit behavior:
//   - Limit == 0 uses store.DefaultListLimit.
//   - Limit < 0 disables the cap.
//
// The bool result is true when more rows were available.
// Front-ends use that to show a "more available" hint.
func (s *service) List(ctx context.Context, opts store.ListOpts) ([]reloop.Job, bool, error) {
	limit := opts.Limit
	if limit == 0 {
		limit = store.DefaultListLimit
	}
	// Ask for one more row than the cap so we can tell the caller
	// whether anything got trimmed without running a count query.
	if limit > 0 {
		opts.Limit = limit + 1
	} else {
		opts.Limit = 0
	}
	jobs, err := s.st.ListJobs(ctx, opts)
	if err != nil {
		return nil, false, err
	}
	hasMore := limit > 0 && len(jobs) > limit
	if hasMore {
		jobs = jobs[:limit]
	}
	return jobs, hasMore, nil
}

func (s *service) Delete(ctx context.Context, job reloop.Job) error {
	if err := s.st.DeleteJob(ctx, job.ID); err != nil {
		return err
	}
	s.notify()
	return nil
}

// Enable re-activates a disabled job from now. Disabled intervals are
// not backfilled. The store guards a done or already-claimed one-shot
// inside the update itself.
func (s *service) Enable(ctx context.Context, job reloop.Job) error {
	now := time.Now()
	if err := s.st.EnableJob(ctx, job.ID, reloop.NextFire(job, now), now); err != nil {
		return err
	}
	s.notify()
	return nil
}

func (s *service) Disable(ctx context.Context, job reloop.Job) error {
	if err := s.st.DisableJob(ctx, job.ID, time.Now()); err != nil {
		return err
	}
	s.notify()
	return nil
}

func (s *service) Status(ctx context.Context) (reloop.Status, error) {
	counts, err := s.st.Counts(ctx)
	if err != nil {
		return reloop.Status{}, err
	}
	return reloop.Status{
		Daemon:  s.DaemonState(),
		DataDir: s.st.DataDir(),
		DBPath:  s.st.DBPath(),
		Jobs:    counts,
	}, nil
}

// DaemonState probes the single-instance lock.
//
// The daemon holds the lock for its lifetime.
// The OS releases it on exit, so stale state is impossible.
func (s *service) DaemonState() reloop.DaemonStatus {
	pid, startedAt, running, _ := daemon.ProbeRunLock(s.st.DataDir())
	st := reloop.DaemonStatus{Supervised: daemon.IsSupervised(s.st.DataDir())}
	if running {
		st.Running = true
		st.PID = pid
		st.StartedAt = startedAt
	}
	return st
}
