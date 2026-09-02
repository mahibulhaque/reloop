package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mahibulhaque/repeat/internal/repeat"
	"github.com/mahibulhaque/repeat/internal/store"
)

// errTestCtxEnded is the cause attached to test-scoped contexts so
// a context-cancel failure surfaces a recognisable error in t.Fatal
// output instead of the generic context.Canceled/DeadlineExceeded.
var errTestCtxEnded = errors.New("test context ended")

func newStore(t *testing.T) *store.Store {
	t.Helper()
	r, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// recRunner records invocations and blocks on a per-call gate so tests
// can hold a job "running" for as long as they need to observe overlap
// behaviour.
type recRunner struct {
	mu      sync.Mutex
	calls   atomic.Int64
	wait    chan struct{}
	written []string
}

func (r *recRunner) Run(ctx context.Context, job repeat.Job, out io.Writer) (int, error) {
	r.calls.Add(1)
	r.mu.Lock()
	r.written = append(r.written, job.Name)
	r.mu.Unlock()
	if _, err := io.WriteString(out, "hi from "+job.Name+"\n"); err != nil {
		return -1, err
	}
	if r.wait != nil {
		select {
		case <-r.wait:
		case <-ctx.Done():
			return -1, ctx.Err()
		}
	}
	return 0, nil
}

// TestFireSkipsDeletedJob injects the claim race: the job is removed
// between the due scan and the claim, so the command must not run.
func TestFireSkipsDeletedJob(t *testing.T) {
	st := newStore(t)
	rr := &recRunner{}
	s := New(st, Config{runner: rr})
	ctx := t.Context()

	job, err := st.AddJob(ctx, repeat.JobSpec{Name: "gone", Command: []string{"x"}, Cron: "@every 1s"}, time.Now())
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if err := st.DeleteJob(ctx, job.ID); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}

	s.fire(ctx, time.Now(), job)
	s.wg.Wait()
	if got := rr.calls.Load(); got != 0 {
		t.Errorf("runner calls = %d, want 0 for a job deleted before the claim", got)
	}
}

// TestFireSkipsDisabledJob is the same race with a disable instead of
// a remove. The disabled job's deadline must also stay untouched.
func TestFireSkipsDisabledJob(t *testing.T) {
	st := newStore(t)
	rr := &recRunner{}
	s := New(st, Config{runner: rr})
	ctx := t.Context()

	job, err := st.AddJob(ctx, repeat.JobSpec{Name: "paused", Command: []string{"x"}, Cron: "@every 1s"}, time.Now())
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if err := st.DisableJob(ctx, job.ID, time.Now()); err != nil {
		t.Fatalf("DisableJob: %v", err)
	}

	s.fire(ctx, time.Now(), job)
	s.wg.Wait()
	if got := rr.calls.Load(); got != 0 {
		t.Errorf("runner calls = %d, want 0 for a job disabled before the claim", got)
	}
	after, err := st.Job(ctx, job.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if !after.NextFireAt.Equal(job.NextFireAt) {
		t.Errorf("NextFireAt = %v, want untouched %v", after.NextFireAt, job.NextFireAt)
	}
}

// TestFireSkipsCronWithNoNextFire checks the guard for a cron whose
// expression yields no next fire (hand-edited database, parser
// change): fire must warn and not claim, because a zero deadline
// reaching Claim would mark the job done at finish.
func TestFireSkipsCronWithNoNextFire(t *testing.T) {
	st := newStore(t)
	rr := &recRunner{}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	s := New(st, Config{runner: rr, Logger: logger})

	job := repeat.Job{ID: "AAAAA", Kind: repeat.KindCron, Cron: "not a cron", Status: repeat.StatusEnabled}
	s.fire(t.Context(), time.Now(), job)
	s.wg.Wait()

	if got := rr.calls.Load(); got != 0 {
		t.Errorf("runner calls = %d, want 0 for a cron with no next fire", got)
	}
	if !strings.Contains(logBuf.String(), "cron has no next fire") {
		t.Errorf("log = %q, want the no-next-fire warning", logBuf.String())
	}
}

// TestFireConcurrentOneshotClaimRunsOnce hammers the conditional claim
// with simultaneous fire attempts for the same due one-shot. Exactly
// one may win. A second run of a one-shot is the bug the claim
// protocol exists to prevent.
func TestFireConcurrentOneshotClaimRunsOnce(t *testing.T) {
	st := newStore(t)
	rr := &recRunner{}
	s := New(st, Config{runner: rr})
	ctx := t.Context()

	now := time.Now()
	job, err := st.AddJob(ctx, repeat.JobSpec{
		Name: "once", Command: []string{"true"}, FireAt: now.Add(time.Hour),
	}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() { s.fire(ctx, job.FireAt, job) })
	}
	wg.Wait()
	s.wg.Wait()

	if got := rr.calls.Load(); got != 1 {
		t.Errorf("runner calls = %d, want exactly 1 winner out of 32 concurrent claims", got)
	}
	after, err := st.Job(ctx, job.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if after.Status != repeat.StatusDone || !after.NextFireAt.IsZero() {
		t.Errorf("after concurrent fires: status=%s next=%s, want done/zero", after.Status, after.NextFireAt)
	}
}

// TestFireConcurrentCronClaimAdvancesOnce is the cron variant: all
// contenders compute the same next deadline from the same snapshot,
// so the conditional write must let exactly one through.
func TestFireConcurrentCronClaimAdvancesOnce(t *testing.T) {
	st := newStore(t)
	rr := &recRunner{}
	s := New(st, Config{runner: rr})
	ctx := t.Context()

	job, err := st.AddJob(ctx, repeat.JobSpec{
		Name: "tick", Command: []string{"true"}, Cron: "@hourly",
	}, time.Now())
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	due := job.NextFireAt.Add(time.Second)

	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() { s.fire(ctx, due, job) })
	}
	wg.Wait()
	s.wg.Wait()

	if got := rr.calls.Load(); got != 1 {
		t.Errorf("runner calls = %d, want exactly 1 winner out of 32 concurrent claims", got)
	}
	after, err := st.Job(ctx, job.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	want := repeat.NextFire(job, due)
	if !after.NextFireAt.Equal(want) {
		t.Errorf("NextFireAt = %s, want advanced exactly once to %s", after.NextFireAt, want)
	}
}

// TestFireDisableRaceInterleavings replays the fire-vs-disable race
// many times so the race detector explores interleavings. Whichever
// write wins, the outcome must be one of exactly two consistent
// states: the job ran to done, or it never ran and kept its deadline.
func TestFireDisableRaceInterleavings(t *testing.T) {
	st := newStore(t)
	rr := &recRunner{}
	s := New(st, Config{runner: rr})
	ctx := t.Context()

	for i := range 50 {
		now := time.Now()
		job, err := st.AddJob(ctx, repeat.JobSpec{
			Name: fmt.Sprintf("racy-%d", i), Command: []string{"true"}, FireAt: now.Add(time.Hour),
		}, now)
		if err != nil {
			t.Fatalf("AddJob: %v", err)
		}

		before := rr.calls.Load()
		var wg sync.WaitGroup
		wg.Go(func() { s.fire(ctx, job.FireAt, job) })
		wg.Go(func() { _ = st.DisableJob(ctx, job.ID, now) })
		wg.Wait()
		s.wg.Wait()

		after, err := st.Job(ctx, job.ID)
		if err != nil {
			t.Fatalf("Job: %v", err)
		}
		switch rr.calls.Load() - before {
		case 1:
			if after.NextFireAt.IsZero() == false {
				t.Fatalf("iteration %d: ran but deadline %s not cleared", i, after.NextFireAt)
			}
		case 0:
			if !after.NextFireAt.Equal(job.NextFireAt) || after.Status != repeat.StatusDisabled {
				t.Fatalf("iteration %d: no run, but status=%s next=%s; want disabled with the original deadline",
					i, after.Status, after.NextFireAt)
			}
		default:
			t.Fatalf("iteration %d: %d runner calls for one fire", i, rr.calls.Load()-before)
		}
	}
}

func TestSchedulerFiresCronJob(t *testing.T) {
	st := newStore(t)
	rr := &recRunner{}
	s := New(st, Config{
		maxConcurrent: 4,
		runner:        rr,
	})

	ctx, cancel := context.WithTimeoutCause(t.Context(), 3*time.Second, errTestCtxEnded)
	defer cancel()

	now := time.Now()
	if _, err := st.AddJob(ctx, repeat.JobSpec{
		Name: "tick", Command: []string{"true"}, Cron: "@every 1s",
	}, now); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	go s.Start(ctx)

	if !waitFor(2500*time.Millisecond, func() bool { return rr.calls.Load() >= 2 }) {
		t.Fatalf("runner calls = %d within 2.5s, want at least 2", rr.calls.Load())
	}
}

func TestSchedulerSkipsOverlap(t *testing.T) {
	st := newStore(t)
	gate := make(chan struct{})
	rr := &recRunner{wait: gate}
	s := New(st, Config{
		maxConcurrent: 4,
		runner:        rr,
	})

	ctx, cancel := context.WithTimeoutCause(t.Context(), 5*time.Second, errTestCtxEnded)
	defer cancel()

	now := time.Now()
	job, err := st.AddJob(ctx, repeat.JobSpec{
		Name: "slow", Command: []string{"true"}, Cron: "@every 1s",
	}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	go s.Start(ctx)
	// recRunner's wait-loop already selects on ctx.Done(), so the
	// gated runner unblocks when the test's defer cancel() fires.

	// Wait until the first run has actually been started, then give
	// the scheduler enough wall time to attempt at least one more
	// firing while the first is still held by the gate.
	if !waitFor(2*time.Second, func() bool { return rr.calls.Load() >= 1 }) {
		t.Fatalf("runner calls = %d, want at least 1", rr.calls.Load())
	}
	sleep(1500 * time.Millisecond)

	runs, err := st.ListRunsAfter(ctx, job.ID, 0)
	if err != nil {
		t.Fatalf("ListRunsAfter: %v", err)
	}
	var overlaps int
	for _, r := range runs {
		if r.Status == repeat.RunSkippedOverlap {
			overlaps++
		}
	}
	if overlaps == 0 {
		t.Fatalf("overlap rows = %d, want at least 1; runs=%+v", overlaps, runs)
	}
}

func TestSchedulerHonoursDisabled(t *testing.T) {
	st := newStore(t)
	rr := &recRunner{}
	s := New(st, Config{
		runner: rr,
	})

	ctx, cancel := context.WithTimeoutCause(t.Context(), 2*time.Second, errTestCtxEnded)
	defer cancel()

	now := time.Now()
	job, err := st.AddJob(ctx, repeat.JobSpec{
		Name: "off", Command: []string{"true"}, Cron: "@every 1s",
	}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if err := st.DisableJob(ctx, job.ID, now); err != nil {
		t.Fatalf("DisableJob: %v", err)
	}

	go s.Start(ctx)

	// Past the first @every 1s tick: an enabled job would have fired.
	sleep(1300 * time.Millisecond)
	if got := rr.calls.Load(); got != 0 {
		t.Errorf("disabled job fired %d times, want 0", got)
	}
}

func TestSchedulerWakePicksUpNewJob(t *testing.T) {
	// Scheduler starts with no jobs.
	// It sleeps on maxSleep.
	// AddJob updates next_fire_at.
	// Wake interrupts sleep so the scheduler sees the new row quickly.
	st := newStore(t)
	rr := &recRunner{}
	s := New(st, Config{runner: rr})

	ctx, cancel := context.WithTimeoutCause(t.Context(), 3*time.Second, errTestCtxEnded)
	defer cancel()

	go s.Start(ctx)

	// Let the scheduler settle into its first sleep.
	sleep(100 * time.Millisecond)

	now := time.Now()
	if _, err := st.AddJob(ctx, repeat.JobSpec{
		Name: "late", Command: []string{"true"}, Cron: "@every 1s",
	}, now); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	// In real CLI usage, service.notify sends SIGHUP to the daemon.
	// The daemon then calls Wake.
	// This test reaches under that and calls Wake directly.
	s.Wake()

	if !waitFor(2*time.Second, func() bool { return rr.calls.Load() >= 1 }) {
		t.Fatalf("scheduler did not pick up the new job; calls=%d", rr.calls.Load())
	}
}

func TestSchedulerNextSleepUsesConfiguredClock(t *testing.T) {
	st := newStore(t)
	fakeNow := time.Date(2040, 5, 13, 10, 0, 0, 0, time.UTC)
	if _, err := st.AddJob(t.Context(), repeat.JobSpec{
		Name: "future", Command: []string{"true"}, FireAt: fakeNow.Add(250 * time.Millisecond),
	}, fakeNow); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	s := New(st, Config{
		maxSleep: 10 * time.Second,
		now:      func() time.Time { return fakeNow },
		runner:   &recRunner{},
	})
	got := s.nextSleep(t.Context(), fakeNow)
	if got < 200*time.Millisecond || got > 300*time.Millisecond {
		t.Errorf("nextSleep = %v, want about 250ms from configured now", got)
	}
}

func TestSchedulerOneshotClaimDoesNotOverlapWhileRunning(t *testing.T) {
	st := newStore(t)
	gate := make(chan struct{})
	rr := &recRunner{wait: gate}
	s := New(st, Config{
		maxSleep:      5 * time.Millisecond,
		maxConcurrent: 4,
		runner:        rr,
	})

	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)

	now := time.Now()
	job, err := st.AddJob(ctx, repeat.JobSpec{
		Name: "once-slow", Command: []string{"true"}, FireAt: now.Add(20 * time.Millisecond),
	}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	done := make(chan struct{})
	go func() { defer close(done); s.Start(ctx) }()

	if !waitFor(2*time.Second, func() bool { return rr.calls.Load() >= 1 }) {
		t.Fatal("oneshot never started")
	}
	sleep(75 * time.Millisecond)

	runs, err := st.ListRunsAfter(ctx, job.ID, 0)
	if err != nil {
		t.Fatalf("ListRunsAfter: %v", err)
	}
	for _, run := range runs {
		if run.Status == repeat.RunSkippedOverlap {
			t.Errorf("oneshot recorded overlap while first run was still active: %+v", runs)
		}
	}

	close(gate)
	cancel(errTestCtxEnded)
	<-done
}

func TestSchedulerMissedOneshotFiresOnStartup(t *testing.T) {
	// A one-shot whose FireAt is in the past (because the daemon was
	// down at the scheduled time) must fire as soon as the scheduler
	// comes up. The previous behaviour silently dropped it.
	st := newStore(t)
	rr := &recRunner{}
	s := New(st, Config{
		runner: rr,
	})

	ctx, cancel := context.WithTimeoutCause(t.Context(), 2*time.Second, errTestCtxEnded)
	defer cancel()

	// Model "the daemon was down": the job was added two hours ago with
	// a fire time one hour ago. Insert with the historical now so the
	// fire time is valid at add time and past at scheduler startup.
	addedAt := time.Now().Add(-2 * time.Hour)
	if _, err := st.AddJob(ctx, repeat.JobSpec{
		Name: "missed", Command: []string{"true"}, FireAt: addedAt.Add(1 * time.Hour),
	}, addedAt); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	go s.Start(ctx)

	if !waitFor(1500*time.Millisecond, func() bool { return rr.calls.Load() == 1 }) {
		t.Fatalf("missed oneshot did not fire on startup")
	}
}

func TestSchedulerRecoversInterruptedOneshotOnStartup(t *testing.T) {
	// A one-shot claimed by a previous daemon that died mid-run (open
	// running record, deadline cleared) must not re-fire. Startup
	// resolves it: done, with the record closed as interrupted.
	st := newStore(t)
	rr := &recRunner{}
	s := New(st, Config{runner: rr})

	ctx, cancel := context.WithTimeoutCause(t.Context(), 2*time.Second, errTestCtxEnded)
	defer cancel()

	addedAt := time.Now().Add(-time.Hour)
	job, err := st.AddJob(ctx, repeat.JobSpec{
		Name: "orphan", Command: []string{"true"}, FireAt: addedAt.Add(time.Minute),
	}, addedAt)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	// The crashed daemon claimed the fire but never finished the run.
	if _, run, err := st.Claim(ctx, job.ID, job.NextFireAt, time.Time{}, addedAt.Add(time.Minute), 100); err != nil || !run {
		t.Fatalf("Claim = (run=%v, %v), want a claimed run", run, err)
	}

	go s.Start(ctx)

	if !waitFor(1500*time.Millisecond, func() bool {
		got, err := st.Job(ctx, job.ID)
		return err == nil && got.Status == repeat.StatusDone
	}) {
		t.Fatalf("interrupted oneshot was not recovered to done")
	}
	got, err := st.Job(ctx, job.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if got.LastStatus != repeat.RunInterrupted {
		t.Errorf("last status = %s, want %s", got.LastStatus, repeat.RunInterrupted)
	}
	run, err := st.LatestRun(ctx, job.ID)
	if err != nil {
		t.Fatalf("LatestRun: %v", err)
	}
	if run.Status != repeat.RunInterrupted || run.FinishedAt.IsZero() {
		t.Errorf("recovered record = %+v, want closed as interrupted", run)
	}
	if calls := rr.calls.Load(); calls != 0 {
		t.Errorf("interrupted oneshot ran %d times, want 0", calls)
	}
}

// TestSchedulerLeavesDueJobWhenSaturated checks the capacity
// rule: at the cap, a due job must NOT be claimed. Claiming
// and queueing would strand it if the daemon stopped while queued.
// Leaving it due loses nothing, and a later loop pass picks it up
// once a slot frees. maxSleep is tightened so those passes happen
// inside the test window.
func TestSchedulerLeavesDueJobWhenSaturated(t *testing.T) {
	st := newStore(t)
	gate := make(chan struct{})
	rr := &recRunner{wait: gate}
	s := New(st, Config{maxConcurrent: 1, maxSleep: 25 * time.Millisecond, runner: rr})

	ctx, cancel := context.WithTimeoutCause(t.Context(), 10*time.Second, errTestCtxEnded)
	defer cancel()

	now := time.Now()
	if _, err := st.AddJob(ctx, repeat.JobSpec{
		Name: "holder", Command: []string{"true"}, FireAt: now.Add(10 * time.Millisecond),
	}, now); err != nil {
		t.Fatalf("AddJob holder: %v", err)
	}
	starved, err := st.AddJob(ctx, repeat.JobSpec{
		Name: "starved", Command: []string{"true"}, FireAt: now.Add(50 * time.Millisecond),
	}, now)
	if err != nil {
		t.Fatalf("AddJob starved: %v", err)
	}

	go s.Start(ctx)

	if !waitFor(2*time.Second, func() bool { return rr.calls.Load() >= 1 }) {
		t.Fatal("holder never started")
	}
	// Give the loop time to pass the starved deadline while the only
	// slot is held. The job must stay claimable: deadline intact, no
	// run row written.
	sleep(300 * time.Millisecond)
	got, err := st.Job(ctx, starved.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if got.NextFireAt.IsZero() || got.Status != repeat.StatusEnabled {
		t.Fatalf("starved job: status=%s next=%s, want enabled with its deadline intact",
			got.Status, got.NextFireAt)
	}
	if runs, err := st.ListRunsAfter(ctx, starved.ID, 0); err != nil || len(runs) != 0 {
		t.Fatalf("starved job runs = %d (%v), want none while saturated", len(runs), err)
	}

	// Once the holder finishes, the next pass claims the starved job.
	close(gate)
	if !waitFor(2*time.Second, func() bool {
		got, err := st.Job(ctx, starved.ID)
		return err == nil && got.Status == repeat.StatusDone
	}) {
		t.Fatal("starved occurrence did not run after a slot freed")
	}
	if got := rr.calls.Load(); got != 2 {
		t.Errorf("runner calls = %d, want 2", got)
	}
}

func TestSchedulerOneshotMarksDone(t *testing.T) {
	st := newStore(t)
	rr := &recRunner{}
	s := New(st, Config{
		runner: rr,
	})

	ctx, cancel := context.WithTimeoutCause(t.Context(), 2*time.Second, errTestCtxEnded)
	defer cancel()

	now := time.Now()
	job, err := st.AddJob(ctx, repeat.JobSpec{
		Name: "once", Command: []string{"true"}, FireAt: now.Add(200 * time.Millisecond),
	}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	go s.Start(ctx)

	if !waitFor(1500*time.Millisecond, func() bool { return rr.calls.Load() == 1 }) {
		t.Fatalf("oneshot did not fire; calls=%d", rr.calls.Load())
	}
	if !waitFor(500*time.Millisecond, func() bool {
		got, err := st.Job(ctx, job.ID)
		return err == nil && got.Status == repeat.StatusDone
	}) {
		t.Fatalf("oneshot not marked done")
	}
}

func TestSchedulerCapsLargeOutput(t *testing.T) {
	st := newStore(t)
	s := New(st, Config{
		// Use the real exec runner so we genuinely exercise the
		// capping path with a chatty process.
	})

	ctx, cancel := context.WithTimeoutCause(t.Context(), 5*time.Second, errTestCtxEnded)
	defer cancel()

	now := time.Now()
	job, err := st.AddJob(ctx, repeat.JobSpec{
		Name:    "fat",
		Command: []string{"/bin/sh", "-c", "yes A | head -c 200000"},
		FireAt:  now.Add(200 * time.Millisecond),
	}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	go s.Start(ctx)

	var runID int64
	if !waitFor(3*time.Second, func() bool {
		run, err := st.LatestRun(ctx, job.ID)
		if err != nil {
			return false
		}
		runID = run.ID
		return !run.FinishedAt.IsZero()
	}) {
		t.Fatalf("fat job did not complete")
	}

	buf, err := st.RunLog(ctx, runID)
	if err != nil {
		t.Fatalf("RunLog: %v", err)
	}
	if len(buf) > store.MaxOutputBytes+128 {
		t.Errorf("RunLog() bytes = %d, want <= MaxOutputBytes(%d)+marker", len(buf), store.MaxOutputBytes)
	}
	if !bytes.Contains(buf, []byte("output truncated")) {
		t.Errorf("RunLog() tail = %q, want truncation marker", buf[max(0, len(buf)-64):])
	}
}

func TestSchedulerRecordsRunDespiteCancel(t *testing.T) {
	// A long-running job is started, then the daemon ctx is cancelled
	// to simulate SIGTERM.
	//
	// The runner returns once the child is killed.
	// Recording must still produce a runs row.
	// Otherwise shutdown loses the audit trail for in-flight jobs.
	// The write uses writeCtx so it survives parent cancellation.
	st := newStore(t)
	now := time.Now()
	gate := make(chan struct{})
	rr := &recRunner{wait: gate}

	job, err := st.AddJob(t.Context(), repeat.JobSpec{
		Name: "long", Command: []string{"true"}, Cron: "@every 1s",
	}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	s := New(st, Config{
		maxConcurrent: 4,
		runner:        rr,
		recordTimeout: 2 * time.Second,
	})

	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	done := make(chan struct{})
	go func() { defer close(done); s.Start(ctx) }()

	if !waitFor(2*time.Second, func() bool { return rr.calls.Load() >= 1 }) {
		t.Fatal("runner never fired")
	}

	cancel(errTestCtxEnded)
	close(gate)
	<-done

	runs, err := st.ListRunsAfter(t.Context(), job.ID, 0)
	if err != nil {
		t.Fatalf("ListRunsAfter: %v", err)
	}
	if len(runs) == 0 {
		t.Error("no runs row recorded; cancellation lost the audit trail")
	}
}

func TestSchedulerGCRunsPeriodically(t *testing.T) {
	st := newStore(t)
	now := time.Now()

	// Cron that won't fire during the test window.
	job, err := st.AddJob(t.Context(), repeat.JobSpec{
		Name: "c", Command: []string{"true"}, Cron: "0 0 1 1 *",
	}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	s := New(st, Config{
		maxConcurrent: 1,
		gcInterval:    100 * time.Millisecond,
		runner:        &recRunner{},
	})

	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	done := make(chan struct{})
	go func() { defer close(done); s.Start(ctx) }()

	// Wait for the startup GC pass to settle so the post-startup
	// insert below exercises only the ticker, not the startup pass.
	sleep(200 * time.Millisecond)

	// Seed an ancient finished run through the real state machine:
	// claim at the ancient instant, finish a second later. The claim
	// rewrites the same far-future deadline, so the job stays not due.
	ancient := now.Add(-2 * store.RetentionMaxAge)
	runID, ran, err := st.Claim(t.Context(), job.ID, job.NextFireAt, job.NextFireAt, ancient, 100)
	if err != nil || !ran {
		t.Fatalf("Claim = (run=%v, %v), want a claimed run", ran, err)
	}
	if err := st.Finish(t.Context(), runID, job.ID, 0, repeat.RunOK, nil, ancient.Add(time.Second)); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	ok := waitFor(2*time.Second, func() bool {
		runs, err := st.ListRunsAfter(t.Context(), job.ID, 0)
		return err == nil && len(runs) == 0
	})
	if !ok {
		t.Error("periodic GC did not remove stale run within deadline")
	}

	cancel(errTestCtxEnded)
	<-done
}

// timeScale scales every real-clock wait in these tests. Default 1x.
// Set REPEAT_TEST_TIME_MULTIPLIER to widen margins on slow CI. Resolved
// once, so there is no mutable package state.
var timeScale = sync.OnceValue(func() time.Duration {
	n, err := strconv.Atoi(os.Getenv("REPEAT_TEST_TIME_MULTIPLIER"))
	if err != nil || n < 1 {
		n = 1
	}
	return time.Duration(n)
})

// waitFor polls pred until it holds or the scaled deadline elapses.
// Prefer it over a fixed sleep when waiting for an observable state.
func waitFor(d time.Duration, pred func() bool) bool {
	deadline := time.Now().Add(d * timeScale())
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return pred()
}

// sleep blocks for the scaled duration. Use only when real time must
// pass (observing an extra firing or an absence). Else use waitFor.
func sleep(d time.Duration) { time.Sleep(d * timeScale()) }

// TestStartReturnsWhenCancelled checks the shutdown path: a cancelled
// context stops the loop after recovery, even before the first scan.
func TestStartReturnsWhenCancelled(t *testing.T) {
	st := newStore(t)
	s := New(st, Config{runner: &recRunner{}})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan struct{})
	go func() {
		s.Start(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("Start did not return after cancellation")
	}
}

// TestFireWarnsWhenClaimFails checks the claim error path: a store
// failure must skip the job, not run it or crash the loop.
func TestFireWarnsWhenClaimFails(t *testing.T) {
	st := newStore(t)
	job, err := st.AddJob(t.Context(), repeat.JobSpec{
		Name: "doomed", Command: []string{"x"}, Cron: "@every 1s",
	}, time.Now())
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	_ = st.Close()

	rr := &recRunner{}
	s := New(st, Config{runner: rr})
	s.fire(t.Context(), time.Now(), job)
	s.wg.Wait()
	if got := rr.calls.Load(); got != 0 {
		t.Errorf("runner calls = %d, want 0 when the claim fails", got)
	}
}

// TestNextSleepRetriesSoonOnStoreError checks the short retry: a store
// error must not stall the loop for the full maxSleep.
func TestNextSleepRetriesSoonOnStoreError(t *testing.T) {
	st := newStore(t)
	_ = st.Close()

	s := New(st, Config{})
	if got := s.nextSleep(t.Context(), time.Now()); got != time.Second {
		t.Errorf("nextSleep = %v, want 1s after a store error", got)
	}
}

// TestNextSleepClampsImminentDeadline checks the busy-spin floor.
func TestNextSleepClampsImminentDeadline(t *testing.T) {
	st := newStore(t)
	job, err := st.AddJob(t.Context(), repeat.JobSpec{
		Name: "soon", Command: []string{"x"}, FireAt: time.Now().Add(time.Hour),
	}, time.Now())
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	s := New(st, Config{})
	got := s.nextSleep(t.Context(), job.NextFireAt.Add(-100*time.Microsecond))
	if got != time.Millisecond {
		t.Errorf("nextSleep = %v, want the 1ms floor", got)
	}
}

// TestRunJobDropsRecordWhenJobDeletedMidRun checks the delete race: the
// finish write hits a gone job and the record is dropped on purpose.
func TestRunJobDropsRecordWhenJobDeletedMidRun(t *testing.T) {
	st := newStore(t)
	ctx := t.Context()
	job, err := st.AddJob(ctx, repeat.JobSpec{
		Name: "vanishing", Command: []string{"x"}, Cron: "@every 1s",
	}, time.Now())
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	runID, ran, err := st.Claim(ctx, job.ID, job.NextFireAt, time.Now().Add(time.Hour), time.Now(), 100)
	if err != nil || !ran {
		t.Fatalf("Claim = (run=%v, %v), want a claimed run", ran, err)
	}
	if err := st.DeleteJob(ctx, job.ID); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	s := New(st, Config{runner: &recRunner{}, Logger: logger})
	s.runJob(ctx, runID, job)
	if !strings.Contains(logBuf.String(), "run record dropped") {
		t.Errorf("log = %q, want the dropped-record note", logBuf.String())
	}
}

// TestRunJobLogsWhenFinishFails checks the stranded-row warning: a
// failed finish leaves the row open for the next start to recover.
func TestRunJobLogsWhenFinishFails(t *testing.T) {
	st := newStore(t)
	ctx := t.Context()
	job, err := st.AddJob(ctx, repeat.JobSpec{
		Name: "stranded", Command: []string{"x"}, Cron: "@every 1s",
	}, time.Now())
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	runID, ran, err := st.Claim(ctx, job.ID, job.NextFireAt, time.Now().Add(time.Hour), time.Now(), 100)
	if err != nil || !ran {
		t.Fatalf("Claim = (run=%v, %v), want a claimed run", ran, err)
	}
	_ = st.Close()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	s := New(st, Config{runner: &recRunner{}, Logger: logger})
	s.runJob(ctx, runID, job)
	if !strings.Contains(logBuf.String(), "finish run failed") {
		t.Errorf("log = %q, want the finish-failed note", logBuf.String())
	}
}
