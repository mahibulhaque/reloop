// Tests for the run state machine in lifecycle.go, in transition
// order: Claim, then Finish, then Recover.

package store

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/mahibulhaque/reloop/internal/reloop"
)

// mustClaim drives job through the same claim transaction production
// uses: deadline cleared (one-shot form), running row opened.
func mustClaim(t *testing.T, r *Store, job reloop.Job, now time.Time) int64 {
	t.Helper()
	runID, run, err := r.Claim(t.Context(), job.ID, job.NextFireAt, time.Time{}, now, 100)
	if err != nil || !run {
		t.Fatalf("Claim(%s) = (run=%v, %v), want a claimed run", job.ID, run, err)
	}
	return runID
}

// mustFinish closes a claimed run as ok through the production path.
func mustFinish(t *testing.T, r *Store, runID int64, id reloop.JobID, now time.Time) {
	t.Helper()
	if err := r.Finish(t.Context(), runID, id, 0, reloop.RunOK, nil, now); err != nil {
		t.Fatalf("Finish(%s): %v", id, err)
	}
}

// TestClaim checks the conditional claim: a job that was removed,
// disabled, or rescheduled since the due scan must not be claimable,
// and a failed claim must leave no row behind.
func TestClaim(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")

	job, err := r.AddJob(ctx, reloop.JobSpec{Name: "c", Command: []string{"x"}, Cron: "@hourly"}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	next := now.Add(2 * time.Hour)

	runID, run, err := r.Claim(ctx, job.ID, job.NextFireAt, next, now, 100)
	if err != nil || !run || runID == 0 {
		t.Fatalf("Claim = (%d, run=%v, %v), want a claimed run", runID, run, err)
	}
	mustFinish(t, r, runID, job.ID, now)

	// A second claim against the stale deadline loses.
	if _, run, err := r.Claim(ctx, job.ID, job.NextFireAt, next, now, 100); err != nil || run {
		t.Fatalf("stale Claim = (run=%v, %v), want (false, nil)", run, err)
	}
	// A disabled job is not claimable even with the right deadline.
	if err := r.DisableJob(ctx, job.ID, now); err != nil {
		t.Fatalf("DisableJob: %v", err)
	}
	if _, run, err := r.Claim(ctx, job.ID, next, next.Add(time.Hour), now, 100); err != nil || run {
		t.Fatalf("disabled Claim = (run=%v, %v), want (false, nil)", run, err)
	}
	// Neither is a deleted one.
	if err := r.DeleteJob(ctx, job.ID); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if _, run, err := r.Claim(ctx, job.ID, next, next.Add(time.Hour), now, 100); err != nil || run {
		t.Fatalf("deleted Claim = (run=%v, %v), want (false, nil)", run, err)
	}
}

// TestClaimRefusesWhenSaturated checks the in-transaction capacity cap:
// the open running rows are the slots, so a claim past maxRunning is
// refused with nothing written, and closing a row frees the slot.
func TestClaimRefusesWhenSaturated(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")

	holder, err := r.AddJob(ctx, reloop.JobSpec{Name: "holder", Command: []string{"x"}, FireAt: now.Add(time.Hour)}, now)
	if err != nil {
		t.Fatalf("AddJob holder: %v", err)
	}
	starved, err := r.AddJob(ctx, reloop.JobSpec{Name: "starved", Command: []string{"x"}, FireAt: now.Add(time.Hour)}, now)
	if err != nil {
		t.Fatalf("AddJob starved: %v", err)
	}
	holderRun, run, err := r.Claim(ctx, holder.ID, holder.NextFireAt, time.Time{}, now, 1)
	if err != nil || !run {
		t.Fatalf("holder Claim = (run=%v, %v), want a claimed run", run, err)
	}

	if _, run, err := r.Claim(ctx, starved.ID, starved.NextFireAt, time.Time{}, now, 1); err != nil || run {
		t.Fatalf("saturated Claim = (run=%v, %v), want (false, nil)", run, err)
	}
	got, err := r.Job(ctx, starved.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if !got.NextFireAt.Equal(starved.NextFireAt) || got.Status != reloop.StatusEnabled {
		t.Errorf("starved job: status=%s next=%s, want enabled with its deadline intact",
			got.Status, got.NextFireAt)
	}
	if runs := listRuns(t, r, starved.ID); len(runs) != 0 {
		t.Fatalf("starved job runs = %d, want none while saturated", len(runs))
	}

	mustFinish(t, r, holderRun, holder.ID, now)
	if _, run, err := r.Claim(ctx, starved.ID, starved.NextFireAt, time.Time{}, now, 1); err != nil || !run {
		t.Fatalf("post-finish Claim = (run=%v, %v), want a claimed run", run, err)
	}
}

// TestClaimRecordsOverlap checks the overlap path of the claim. While
// a previous run is still going, the next fire is recorded as a skip
// in the same transaction and the job summary is left alone because
// nothing ran.
func TestClaimRecordsOverlap(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")

	job, err := r.AddJob(ctx, reloop.JobSpec{Name: "c", Command: []string{"echo"}, Cron: "@hourly"}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	first := now.Add(time.Hour)
	if _, run, err := r.Claim(ctx, job.ID, job.NextFireAt, first, now, 100); err != nil || !run {
		t.Fatalf("first Claim = (run=%v, %v), want a claimed run", run, err)
	}

	second := now.Add(2 * time.Hour)
	runID, run, err := r.Claim(ctx, job.ID, first, second, now.Add(time.Hour), 100)
	if err != nil || run || runID != 0 {
		t.Fatalf("overlapping Claim = (%d, run=%v, %v), want (0, false, nil)", runID, run, err)
	}

	runs := listRuns(t, r, job.ID)
	if diff := cmp.Diff([]reloop.RunStatus{reloop.RunSkippedOverlap, reloop.RunRunning}, runStatuses(runs)); diff != "" {
		t.Errorf("run statuses mismatch (-want +got):\n%s", diff)
	}
	got, err := r.Job(ctx, job.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if !got.NextFireAt.Equal(second) || got.LastStatus != "" {
		t.Errorf("after overlap: next=%s last=%q, want %s with an untouched summary",
			got.NextFireAt, got.LastStatus, second)
	}
}

// TestStoreRunLifecycleAndLog walks one run through the full
// state machine: claim opens a visible running row, finish closes
// it and updates the job summary, and the captured output reads back.
func TestStoreRunLifecycleAndLog(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")

	job, err := r.AddJob(ctx, reloop.JobSpec{Name: "c", Command: []string{"echo"}, Cron: "@hourly"}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	next := now.Add(2 * time.Hour)
	runID, run, err := r.Claim(ctx, job.ID, job.NextFireAt, next, now, 100)
	if err != nil || !run {
		t.Fatalf("Claim = (run=%v, %v), want a claimed run", run, err)
	}

	// The claim is visible mid-execution: an open running row and
	// the advanced deadline. LatestRun only sees completed runs, so
	// the open row is found through OpenRun.
	open, err := r.OpenRun(ctx, job.ID)
	if err != nil {
		t.Fatalf("OpenRun mid-run: %v", err)
	}
	if open.ID != runID || open.Status != reloop.RunRunning || !open.FinishedAt.IsZero() {
		t.Errorf("mid-run row = %+v, want open running row %d", open, runID)
	}
	if _, err := r.LatestRun(ctx, job.ID); !errors.Is(err, reloop.ErrNotFound) {
		t.Errorf("LatestRun mid-run = %v, want ErrNotFound while only an open row exists", err)
	}
	mid, err := r.Job(ctx, job.ID)
	if err != nil {
		t.Fatalf("Job mid-run: %v", err)
	}
	if !mid.NextFireAt.Equal(next) {
		t.Errorf("mid-run next_fire_at = %s, want %s", mid.NextFireAt, next)
	}

	finishedAt := now.Add(time.Second)
	if err := r.Finish(ctx, runID, job.ID, 0, reloop.RunOK, []byte("hello\n"), finishedAt); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	latest, err := r.LatestRun(ctx, job.ID)
	if err != nil {
		t.Fatalf("LatestRun: %v", err)
	}
	if latest.Status != reloop.RunOK || latest.ExitCode != 0 || !latest.FinishedAt.Equal(finishedAt) {
		t.Errorf("LatestRun = %+v, want ok/0 finished at %s", latest, finishedAt)
	}
	got, err := r.Job(ctx, job.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if got.LastStatus != reloop.RunOK || !got.LastRunAt.Equal(finishedAt) || got.Status != reloop.StatusEnabled {
		t.Errorf("job summary = last=%s at=%s status=%s, want ok/%s/enabled",
			got.LastStatus, got.LastRunAt, got.Status, finishedAt)
	}

	out, err := r.RunLog(ctx, latest.ID)
	if err != nil {
		t.Fatalf("RunLog: %v", err)
	}
	if string(out) != "hello\n" {
		t.Errorf("log content = %q, want %q", out, "hello\n")
	}
}

// TestFinishAfterDeleteIsNotFound checks the delete-while-running path:
// rm cascades over the open row, so the finish write must surface
// as ErrNotFound, which the scheduler treats as benign.
func TestFinishAfterDeleteIsNotFound(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")

	job, err := r.AddJob(ctx, reloop.JobSpec{Name: "victim", Command: []string{"x"}, FireAt: now.Add(time.Hour)}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	runID := mustClaim(t, r, job, now)
	if err := r.DeleteJob(ctx, job.ID); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	err = r.Finish(ctx, runID, job.ID, 0, reloop.RunOK, []byte("out"), now.Add(time.Second))
	if !errors.Is(err, reloop.ErrNotFound) {
		t.Fatalf("Finish after delete = %v, want errors.Is(ErrNotFound)", err)
	}
}

// TestRecoverClosesRunningRecords checks the crash-recovery path for
// both kinds: a row left running by a dead daemon is closed as
// interrupted, the owning one-shot becomes done, the owning cron stays
// enabled on its already-advanced deadline, and untouched jobs stay
// untouched.
func TestRecoverClosesRunningRecords(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")
	future := now.Add(time.Hour)

	cron, err := r.AddJob(ctx, reloop.JobSpec{Name: "cron", Command: []string{"x"}, Cron: "@hourly"}, now)
	if err != nil {
		t.Fatalf("AddJob cron: %v", err)
	}
	pending, err := r.AddJob(ctx, reloop.JobSpec{Name: "pending", Command: []string{"x"}, FireAt: future}, now)
	if err != nil {
		t.Fatalf("AddJob pending: %v", err)
	}
	oneshot, err := r.AddJob(ctx, reloop.JobSpec{Name: "oneshot", Command: []string{"x"}, FireAt: future}, now)
	if err != nil {
		t.Fatalf("AddJob oneshot: %v", err)
	}
	// Simulate the crash: both jobs claimed (running rows open),
	// daemon died before finishing either.
	mustClaim(t, r, oneshot, now)
	cronNext := now.Add(2 * time.Hour)
	if _, run, err := r.Claim(ctx, cron.ID, cron.NextFireAt, cronNext, now, 100); err != nil || !run {
		t.Fatalf("Claim cron = (run=%v, %v), want a claimed run", run, err)
	}

	n, err := r.Recover(ctx, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 2 {
		t.Fatalf("recovered %d runs, want 2", n)
	}

	got, err := r.Job(ctx, oneshot.ID)
	if err != nil {
		t.Fatalf("Job oneshot: %v", err)
	}
	if got.Status != reloop.StatusDone || got.LastStatus != reloop.RunInterrupted {
		t.Errorf("oneshot after recover: status=%s last=%s, want done/interrupted", got.Status, got.LastStatus)
	}
	run, err := r.LatestRun(ctx, oneshot.ID)
	if err != nil {
		t.Fatalf("LatestRun oneshot: %v", err)
	}
	if run.Status != reloop.RunInterrupted || run.ExitCode != -1 || run.FinishedAt.IsZero() {
		t.Errorf("recovered run = %+v, want closed interrupted/-1", run)
	}

	// The cron keeps firing: enabled, deadline where the claim put it,
	// only its summary records the interruption.
	got, err = r.Job(ctx, cron.ID)
	if err != nil {
		t.Fatalf("Job cron: %v", err)
	}
	if got.Status != reloop.StatusEnabled || !got.NextFireAt.Equal(cronNext) || got.LastStatus != reloop.RunInterrupted {
		t.Errorf("cron after recover: status=%s next=%s last=%s, want enabled/%s/interrupted",
			got.Status, got.NextFireAt, got.LastStatus, cronNext)
	}

	if got, err := r.Job(ctx, pending.ID); err != nil || got.Status != reloop.StatusEnabled {
		t.Errorf("pending job after recover: %+v err=%v, want still enabled", got, err)
	}

	// A second recover finds nothing.
	if n, err := r.Recover(ctx, now.Add(2*time.Minute)); err != nil || n != 0 {
		t.Errorf("second recover = %d, %v; want 0, nil", n, err)
	}
}

// TestRecoverDisabledClaimedOneshot checks that a one-shot disabled
// after being claimed recovers like an enabled one. Disabling does not
// stop the running command, so the stranded state is the same.
// Re-enabling would otherwise fire the command a second time.
func TestRecoverDisabledClaimedOneshot(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")

	job, err := r.AddJob(ctx, reloop.JobSpec{Name: "j", Command: []string{"x"}, FireAt: now.Add(time.Hour)}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	mustClaim(t, r, job, now)
	if err := r.DisableJob(ctx, job.ID, now); err != nil {
		t.Fatalf("DisableJob: %v", err)
	}

	if n, err := r.Recover(ctx, now.Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("recover = %d, %v; want 1, nil", n, err)
	}
	got, err := r.Job(ctx, job.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if got.Status != reloop.StatusDone || got.LastStatus != reloop.RunInterrupted {
		t.Errorf("after recover: status=%s last=%s, want done/interrupted", got.Status, got.LastStatus)
	}
}

func runStatuses(runs []reloop.Run) []reloop.RunStatus {
	statuses := make([]reloop.RunStatus, 0, len(runs))
	for _, run := range runs {
		statuses = append(statuses, run.Status)
	}
	return statuses
}
