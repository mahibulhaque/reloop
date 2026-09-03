package main

import (
	"errors"
	"testing"
	"time"

	"github.com/mahibulhaque/reloop/internal/reloop"
	"github.com/mahibulhaque/reloop/internal/store"
)

// newStore opens a throwaway store rooted in t.TempDir.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// markDone drives job through claim and finish, landing it done the
// same way production does.
func markDone(t *testing.T, st *store.Store, job reloop.Job) {
	t.Helper()
	runID, run, err := st.Claim(t.Context(), job.ID, job.NextFireAt, time.Time{}, time.Now(), 100)
	if err != nil || !run {
		t.Fatalf("Claim(%s) = (run=%v, %v), want a claimed run", job.ID, run, err)
	}
	if err := st.Finish(t.Context(), runID, job.ID, 0, reloop.RunOK, nil, time.Now()); err != nil {
		t.Fatalf("Finish(%s): %v", job.ID, err)
	}
}

func TestServiceEnableCronSchedulesFromNow(t *testing.T) {
	st := newStore(t)

	createdAt := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	job, err := st.AddJob(t.Context(), reloop.JobSpec{
		Name: "stale", Command: []string{"true"}, Cron: "@hourly",
	}, createdAt)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if err := st.DisableJob(t.Context(), job.ID, createdAt.Add(time.Minute)); err != nil {
		t.Fatalf("DisableJob: %v", err)
	}

	beforeEnable := time.Now()
	if err := newService(st).Enable(t.Context(), job); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	got, err := st.Job(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if !got.NextFireAt.After(beforeEnable) {
		t.Errorf("next_fire_at = %s, want a future fire after enable time %s",
			got.NextFireAt.Format(time.RFC3339Nano), beforeEnable.Format(time.RFC3339Nano))
	}
	if got.NextFireAt.After(beforeEnable.Add(time.Hour + 2*time.Second)) {
		t.Errorf("next_fire_at = %s, want next hourly fire near now",
			got.NextFireAt.Format(time.RFC3339Nano))
	}
}

// TestServiceEnableEnabledCronIsNoOp checks the "No-op if already
// enabled" contract. Rewriting the deadline would silently push the
// pending fire back a full interval.
func TestServiceEnableEnabledCronIsNoOp(t *testing.T) {
	st := newStore(t)

	job, err := st.AddJob(t.Context(), reloop.JobSpec{
		Name: "live", Command: []string{"true"}, Cron: "@hourly",
	}, time.Now().Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	if err := newService(st).Enable(t.Context(), job); err != nil {
		t.Fatalf("Enable(enabled cron) = %v, want nil", err)
	}
	got, err := st.Job(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if !got.NextFireAt.Equal(job.NextFireAt) {
		t.Errorf("next_fire_at = %s after no-op enable, want untouched %s",
			got.NextFireAt, job.NextFireAt)
	}
}

func TestServiceEnableDoneOneshotConflicts(t *testing.T) {
	// Re-enabling a completed one-shot must be a conflict. NextFire
	// treats a past FireAt as due now, so allowing it would refire
	// the command immediately.
	st := newStore(t)

	addedAt := time.Now().Add(-time.Hour)
	job, err := st.AddJob(t.Context(), reloop.JobSpec{
		Name: "ran", Command: []string{"true"}, FireAt: addedAt.Add(time.Minute),
	}, addedAt)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	markDone(t, st, job)

	err = newService(st).Enable(t.Context(), job)
	if !errors.Is(err, reloop.ErrConflict) {
		t.Fatalf("Enable(done oneshot) = %v, want errors.Is(ErrConflict)", err)
	}
	got, err := st.Job(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if got.Status != reloop.StatusDone || !got.NextFireAt.IsZero() {
		t.Errorf("after rejected enable: status=%s next=%s, want done/zero", got.Status, got.NextFireAt)
	}
}

func TestServiceEnableClaimedOneshotConflicts(t *testing.T) {
	// A claimed one-shot (deadline cleared, not done) is running or
	// stranded by a crash. Enable must not recompute its deadline and
	// fire the command a second time.
	st := newStore(t)

	now := time.Now()
	job, err := st.AddJob(t.Context(), reloop.JobSpec{
		Name: "claimed", Command: []string{"true"}, FireAt: now.Add(time.Hour),
	}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if _, run, err := st.Claim(t.Context(), job.ID, job.NextFireAt, time.Time{}, now, 100); err != nil || !run {
		t.Fatalf("Claim = (run=%v, %v), want a claimed run", run, err)
	}
	if err := st.DisableJob(t.Context(), job.ID, now); err != nil {
		t.Fatalf("DisableJob: %v", err)
	}

	err = newService(st).Enable(t.Context(), job)
	if !errors.Is(err, reloop.ErrConflict) {
		t.Fatalf("Enable(claimed oneshot) = %v, want errors.Is(ErrConflict)", err)
	}
	got, err := st.Job(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if !got.NextFireAt.IsZero() {
		t.Errorf("after rejected enable: next=%s, want zero (no refire)", got.NextFireAt)
	}
}

func TestServiceDisableDoneOneshotConflicts(t *testing.T) {
	// Disabling a done one-shot must be a conflict. Done is terminal,
	// and a done job flipped to disabled would sit in the in-flight
	// count with nothing running.
	st := newStore(t)

	now := time.Now()
	job, err := st.AddJob(t.Context(), reloop.JobSpec{
		Name: "ran", Command: []string{"true"}, FireAt: now.Add(time.Minute),
	}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	markDone(t, st, job)

	err = newService(st).Disable(t.Context(), job)
	if !errors.Is(err, reloop.ErrConflict) {
		t.Fatalf("Disable(done oneshot) = %v, want errors.Is(ErrConflict)", err)
	}
	counts, err := st.Counts(t.Context())
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if counts.OneshotDone != 1 || counts.OneshotInFlight != 0 {
		t.Errorf("counts after rejected disable: done=%d in_flight=%d, want 1/0",
			counts.OneshotDone, counts.OneshotInFlight)
	}
}
