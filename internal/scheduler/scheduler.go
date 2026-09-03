// Package scheduler is the scheduler loop.
//
// Core model:
//   - The database is the schedule.
//   - There is no in-memory heap.
//   - There is no reload protocol.
//   - Wake interrupts sleep after CLI mutations.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/mahibulhaque/reloop/internal/reloop"
	"github.com/mahibulhaque/reloop/internal/store"
)

// errRecordTimedOut is the cause used when run recording exceeds
// Config.recordTimeout.
var errRecordTimedOut = errors.New("scheduler: record-run timeout")

// jobGracePeriod is how long a job's process gets to exit after
// SIGTERM before it is killed.
const jobGracePeriod = 5 * time.Second

// Config customises a [Scheduler]. Zero values are filled in by [New].
type Config struct {
	// maxConcurrent caps how many jobs run at once. The check happens
	// inside the claim transaction. A job skipped at full capacity
	// stays due and is retried on a later pass. Default: 100.
	maxConcurrent int

	// maxSleep bounds the sleep between store checks. It caps three
	// delays:
	//
	//  1. How long a job skipped at full capacity waits for a retry.
	//  2. How late a fire lands after a laptop wakes from sleep,
	//     because Go timers pause while the machine sleeps.
	//  3. How long the daemon oversleeps if the wall clock jumps
	//     backward.
	//
	// Default: 1 minute.
	maxSleep time.Duration

	// gcInterval is how often run history is trimmed. The first pass
	// runs at startup. Default: 1 hour.
	gcInterval time.Duration

	// recordTimeout bounds the write that records a finished run.
	// The write ignores shutdown cancellation so killed runs still
	// get recorded. It outlasts the 30s busy_timeout so a slow
	// writer cannot strand the run row. Default: 35 seconds.
	recordTimeout time.Duration

	// now provides the current time. Override for deterministic tests.
	now func() time.Time

	// runner executes individual jobs. Default: [execRunner].
	runner runner

	// Logger is used for warnings. nil uses slog.Default().
	Logger *slog.Logger
}

// Scheduler drives the scheduler loop.
//
// Lifecycle:
//   - Start blocks until its context is cancelled.
//   - Cancelling that context is the stop signal.
//   - Wake can be called from any goroutine.
type Scheduler struct {
	store *store.Store
	cfg   Config

	// wake means recheck the store now. Sends never block and pending
	// signals collapse into one.
	wake chan struct{}

	wg sync.WaitGroup
}

// New constructs a Scheduler without starting it.
func New(st *store.Store, cfg Config) *Scheduler {
	if cfg.maxConcurrent <= 0 {
		cfg.maxConcurrent = 100
	}
	if cfg.maxSleep <= 0 {
		cfg.maxSleep = time.Minute
	}
	if cfg.gcInterval <= 0 {
		cfg.gcInterval = time.Hour
	}
	if cfg.recordTimeout <= 0 {
		cfg.recordTimeout = 35 * time.Second
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.runner == nil {
		cfg.runner = execRunner{gracePeriod: jobGracePeriod}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Scheduler{
		store: st,
		cfg:   cfg,
		wake:  make(chan struct{}, 1),
	}
}

// Wake interrupts the current sleep.
// Use it after a write that may have produced a sooner deadline.
// Non-blocking. Multiple wakeups collapse into one pending signal.
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Start runs until ctx is cancelled and drains in-flight runners
// before returning. GC runs at startup and then on every
// Config.gcInterval tick.
func (s *Scheduler) Start(ctx context.Context) {
	defer s.wg.Wait()
	// Nothing is in flight yet, so any row still marked running was
	// left by a daemon that died. Close those before the loop starts.
	if n, err := s.store.Recover(ctx, s.cfg.now()); err != nil {
		s.warn("recover interrupted runs", err)
	} else if n > 0 {
		s.cfg.Logger.Info("resolved runs interrupted by an earlier daemon exit", "count", n)
	}
	s.wg.Go(func() { s.gcLoop(ctx) })

	for {
		if ctx.Err() != nil {
			return
		}

		now := s.cfg.now()
		due, err := s.store.DueJobs(ctx, now)
		s.warn("due jobs", err)
		for _, job := range due {
			s.fire(ctx, now, job)
		}

		s.sleep(ctx, now)
	}
}

// fire takes one due job through the state machine.
//
//  1. Claim it. The claim makes every should-this-run decision,
//     including the concurrency cap.
//  2. If the claim says no, do nothing. A job skipped at full
//     capacity stays due and a later pass retries it.
//  3. Otherwise run it in the background and record the result.
func (s *Scheduler) fire(ctx context.Context, now time.Time, job reloop.Job) {
	// A fired one-shot never fires again, so only crons get a next
	// deadline.
	var next time.Time
	if job.Kind == reloop.KindCron {
		next = reloop.NextFire(job, now)
		if next.IsZero() {
			// A cron with no computable next fire must not be claimed.
			// Claiming it with a zero deadline would mark it done at
			// finish, silently retiring a recurring job.
			s.warn("cron has no next fire, job left due", errors.New("unparseable or matchless cron"), "job", job.ID)
			return
		}
	}
	runID, run, err := s.store.Claim(ctx, job.ID, job.NextFireAt, next, now, s.cfg.maxConcurrent)
	if err != nil {
		s.warn("claim job", err, "job", job.ID)
		return
	}
	if !run {
		return
	}
	s.wg.Go(func() { s.runJob(ctx, runID, job) })
}

// sleep blocks until a deadline, Wake, or cancellation.
// The loop observes cancellation on the next ctx.Err check.
func (s *Scheduler) sleep(ctx context.Context, now time.Time) {
	timer := time.NewTimer(s.nextSleep(ctx, now))
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-s.wake:
	case <-timer.C:
	}
}

// nextSleep returns how long to sleep before the next store check.
// At least 1ms to avoid busy-spins. At most maxSleep.
func (s *Scheduler) nextSleep(ctx context.Context, now time.Time) time.Duration {
	soonest, err := s.store.SoonestDeadline(ctx, now)
	if err != nil {
		s.warn("soonest", err)
		// Falling through to maxSleep here would turn a passing DB
		// error into a long stall past due jobs. Retry soon instead.
		return time.Second
	}
	d := s.cfg.maxSleep
	if !soonest.IsZero() {
		until := soonest.Sub(now)
		if until < d {
			d = until
		}
	}
	if d < time.Millisecond {
		d = time.Millisecond
	}
	return d
}

// runJob executes one claimed job and records the result.
// The claim already advanced the deadline, so a crash mid-run never
// replays the fire. Recover closes the row as interrupted instead.
func (s *Scheduler) runJob(ctx context.Context, runID int64, job reloop.Job) {
	buf := newCappedBuf(store.MaxOutputBytes)
	exitCode, runErr := s.cfg.runner.Run(ctx, job, buf)

	outcome := reloop.RunOK
	if runErr != nil || exitCode != 0 {
		outcome = reloop.RunFail
	}

	// This write ignores shutdown cancellation because a killed run
	// still needs its record. The timeout keeps a stuck write from
	// blocking shutdown.
	writeCtx, cancel := context.WithTimeoutCause(
		context.WithoutCancel(ctx), s.cfg.recordTimeout, errRecordTimedOut)
	defer cancel()

	err := s.store.Finish(writeCtx, runID, job.ID, exitCode, outcome, buf.Bytes(), s.cfg.now())
	if err == nil {
		return
	}
	if errors.Is(err, reloop.ErrNotFound) {
		// The job was deleted while it ran. Deleting a job drops its
		// history too, so dropping this record is expected.
		s.cfg.Logger.Info("job deleted mid-run, run record dropped", "job", job.ID)
		return
	}
	// The row stays open until the next daemon start recovers it, and
	// an open row blocks this job and holds a concurrency slot.
	s.cfg.Logger.Error("finish run failed, run row stays open until the next daemon start",
		"job", job.ID, "run", runID, "err", err)
}

func (s *Scheduler) gcLoop(ctx context.Context) {
	gc := func() {
		s.warn("gc", s.store.GC(ctx, s.cfg.now(), store.RetentionPerJob, store.RetentionMaxAge, store.RetentionMaxTotal))
	}
	gc()
	ticker := time.NewTicker(s.cfg.gcInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			gc()
		}
	}
}

// warn logs a non-nil err with optional extra attributes.
func (s *Scheduler) warn(op string, err error, attrs ...any) {
	if err != nil {
		s.cfg.Logger.Warn(op, append(attrs, "err", err)...)
	}
}
