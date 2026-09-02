// Tests for store.go, in source order: Open and schema, jobs, due
// scans, run history, GC, and counts. The state-machine tests live in
// lifecycle_test.go.

package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/mahibulhaque/repeat/internal/repeat"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	r, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	got, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	return got
}

// insertRun seeds one finished run row directly. Production rows go
// through Claim and Finish. Tests need arbitrary timestamps.
func insertRun(t *testing.T, r *Store, jobID repeat.JobID, started, finished time.Time) int64 {
	t.Helper()
	const q = `
		INSERT INTO runs (job_id, started_at, finished_at, exit_code, status, output)
		VALUES (?, ?, ?, 0, 'ok', x'')`
	res, err := r.db.ExecContext(t.Context(), q, string(jobID), started.UnixMilli(), finished.UnixMilli())
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("run id: %v", err)
	}
	return id
}

// listRuns returns every run for jobID, newest first.
func listRuns(t *testing.T, r *Store, jobID repeat.JobID) []repeat.Run {
	t.Helper()
	const q = `
		SELECT id, job_id, started_at, finished_at, exit_code, status
		FROM runs
		WHERE job_id = ?
		ORDER BY started_at DESC, id DESC`
	rows, err := r.db.QueryContext(t.Context(), q, string(jobID))
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	defer rows.Close()
	runs := make([]repeat.Run, 0)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			t.Fatalf("scan run: %v", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	return runs
}

// TestOpenStampsSchemaVersion checks that a fresh database is stamped
// with the current format version, so later opens can gate on it.
func TestOpenStampsSchemaVersion(t *testing.T) {
	r := newStore(t)
	var v int
	if err := r.db.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
}

// TestOpenRecreatesLegacyDatabase checks the pre-1.0 storage policy: a
// database with tables but no version stamp is deleted and
// recreated, never migrated and never misread.
func TestOpenRecreatesLegacyDatabase(t *testing.T) {
	dir := t.TempDir()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "repeat.db"))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(schema); err != nil {
		t.Fatalf("apply legacy schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO jobs
		(id, kind, name, command_json, env_json, cron_expression, fire_at, status, next_fire_at, created_at, updated_at)
		VALUES ('AAAAA', 'oneshot', 'legacy', '["x"]', '[]', '', 1, 'enabled', 1, 1, 1)`); err != nil {
		t.Fatalf("insert legacy job: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	st, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open legacy db: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if _, err := st.Job(t.Context(), "AAAAA"); !errors.Is(err, repeat.ErrNotFound) {
		t.Errorf("legacy job survived recreation: err=%v, want ErrNotFound", err)
	}
	var v int
	if err := st.db.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if v != schemaVersion {
		t.Errorf("user_version after recreation = %d, want %d", v, schemaVersion)
	}
}

// TestOpenRecreatesMismatchedVersion is the other direction: a stamp
// from a different (for example newer) format is also blown away
// rather than half-read.
func TestOpenRecreatesMismatchedVersion(t *testing.T) {
	dir := t.TempDir()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "repeat.db"))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatalf("stamp future version: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	st, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open future db: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	jobs, err := st.ListJobs(t.Context(), ListOpts{})
	if err != nil || len(jobs) != 0 {
		t.Errorf("recreated db: jobs=%d err=%v, want an empty fresh database", len(jobs), err)
	}
	var v int
	if err := st.db.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if v != schemaVersion {
		t.Errorf("user_version after recreation = %d, want %d", v, schemaVersion)
	}
}

// TestOpenHealsMissingIndex checks the idempotent schema: an index this
// binary expects but the database lacks appears on the next open,
// with no version bump and no data loss.
func TestOpenHealsMissingIndex(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	job, err := st.AddJob(t.Context(), repeat.JobSpec{
		Name: "keep", Command: []string{"true"}, Cron: "@hourly",
	}, mustTime(t, "2026-05-13T10:00:00Z"))
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if _, err := st.db.ExecContext(t.Context(), `DROP INDEX runs_running`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	re, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = re.Close() })
	var n int
	const q = `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'runs_running'`
	if err := re.db.QueryRowContext(t.Context(), q).Scan(&n); err != nil {
		t.Fatalf("count index: %v", err)
	}
	if n != 1 {
		t.Errorf("runs_running index count = %d after reopen, want 1", n)
	}
	if _, err := re.Job(t.Context(), job.ID); err != nil {
		t.Errorf("job lost across the index heal: %v", err)
	}
}

// TestOpenRejectsCorruptDatabase feeds Open a file that is not SQLite.
// The pitch is "restarts lose nothing", so the failure mode before a
// restart matters: an error, never a panic or a silent re-init that
// would discard the schedule.
func TestOpenRejectsCorruptDatabase(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "repeat.db"), []byte("this is not a database"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	st, err := Open(t.Context(), dir)
	if err == nil {
		_ = st.Close()
		t.Fatal("Open on a corrupt database = nil error, want failure")
	}
}

// TestOpenUnwritableDataDir covers the data dir losing write permission
// (chmod, restored backup, wrong user). Open must fail with an error
// instead of panicking partway through schema setup.
func TestOpenUnwritableDataDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	st, err := Open(t.Context(), dir)
	if err == nil {
		_ = st.Close()
		t.Fatal("Open in an unwritable dir = nil error, want failure")
	}
}

func TestStoreJobRoundtrip(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")

	spec := repeat.JobSpec{Name: "ping", Command: []string{"echo", "hi"}, Cron: "@hourly"}
	got, err := r.AddJob(ctx, spec, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if got.ID == "" || got.Kind != repeat.KindCron || got.Status != repeat.StatusEnabled {
		t.Errorf("AddJob returned %+v", got)
	}
	if diff := cmp.Diff([]string{"echo", "hi"}, got.Command); diff != "" {
		t.Errorf("AddJob command mismatch (-want +got):\n%s", diff)
	}

	fetched, err := r.Job(ctx, got.ID)
	if err != nil {
		t.Fatalf("Job lookup: %v", err)
	}
	if fetched.Name != "ping" {
		t.Errorf("Job(name) = %q, want %q", fetched.Name, "ping")
	}

	if err := r.DeleteJob(ctx, got.ID); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if _, err := r.Job(ctx, got.ID); !errors.Is(err, repeat.ErrNotFound) {
		t.Errorf("Job(after delete) = %v, want ErrNotFound", err)
	}
}

// TestStoreListJobsOrder checks list ordering.
//
// Jobs should sort reverse-chronologically by creation time.
// ID order is meaningless because IDs are random.
// Users expect the most-recently-added job at the top.
func TestStoreListJobsOrder(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	base := mustTime(t, "2026-05-13T10:00:00Z")

	for i, name := range []string{"oldest", "middle", "newest"} {
		when := base.Add(time.Duration(i) * time.Minute)
		if _, err := r.AddJob(ctx, repeat.JobSpec{
			Name: name, Command: []string{"echo"}, Cron: "@hourly",
		}, when); err != nil {
			t.Fatalf("AddJob %q: %v", name, err)
		}
	}
	jobs, err := r.ListJobs(ctx, ListOpts{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 3 {
		t.Errorf("ListJobs() returned %d jobs, want 3", len(jobs))
	}
	want := []string{"newest", "middle", "oldest"}
	if diff := cmp.Diff(want, jobNames(jobs)); diff != "" {
		t.Errorf("ListJobs() names mismatch (-want +got):\n%s", diff)
	}
}

func TestStoreListFilters(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")
	future := now.Add(time.Hour)

	_, err := r.AddJob(ctx, repeat.JobSpec{Name: "c1", Command: []string{"echo"}, Cron: "@hourly"}, now)
	if err != nil {
		t.Fatalf("AddJob c1: %v", err)
	}
	one, err := r.AddJob(ctx, repeat.JobSpec{Name: "o1", Command: []string{"echo"}, FireAt: future}, now)
	if err != nil {
		t.Fatalf("AddJob o1: %v", err)
	}
	if err := r.DisableJob(ctx, one.ID, now); err != nil {
		t.Fatalf("DisableJob o1: %v", err)
	}

	all, err := r.ListJobs(ctx, ListOpts{})
	if err != nil {
		t.Fatalf("ListJobs all: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("ListJobs all returned %d jobs, want 2", len(all))
	}
	cronOnly, err := r.ListJobs(ctx, ListOpts{Kind: repeat.KindCron})
	if err != nil {
		t.Fatalf("ListJobs cron: %v", err)
	}
	if diff := cmp.Diff([]string{"c1"}, jobNames(cronOnly)); diff != "" {
		t.Errorf("ListJobs cron names mismatch (-want +got):\n%s", diff)
	}
	enabledOnly, err := r.ListJobs(ctx, ListOpts{Status: repeat.StatusEnabled})
	if err != nil {
		t.Fatalf("ListJobs enabled: %v", err)
	}
	if diff := cmp.Diff([]string{"c1"}, jobNames(enabledOnly)); diff != "" {
		t.Errorf("ListJobs enabled names mismatch (-want +got):\n%s", diff)
	}
}

// TestListJobsOffsetWithoutLimit checks that an uncapped listing still
// honors Offset: SQLite needs an explicit LIMIT -1 for a bare OFFSET.
func TestListJobsOffsetWithoutLimit(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")
	for i, name := range []string{"a", "b", "c"} {
		if _, err := r.AddJob(ctx, repeat.JobSpec{Name: name, Command: []string{"x"}, Cron: "@hourly"},
			now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("AddJob %s: %v", name, err)
		}
	}
	got, err := r.ListJobs(ctx, ListOpts{Offset: 2})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if diff := cmp.Diff([]string{"a"}, jobNames(got)); diff != "" {
		t.Errorf("offset names mismatch (-want +got):\n%s", diff)
	}
}

// TestDisableJobSkipsDone checks done as terminal. A done one-shot
// flipped to disabled would leave the status partition counting it as
// in flight forever, so the update must refuse instead.
func TestDisableJobSkipsDone(t *testing.T) {
	r := newStore(t)
	now := mustTime(t, "2026-01-02T10:00:00Z")

	job, err := r.AddJob(t.Context(), repeat.JobSpec{
		Name: "ran", Command: []string{"true"}, FireAt: now.Add(time.Minute),
	}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	mustFinish(t, r, mustClaim(t, r, job, now.Add(time.Minute)), job.ID, now.Add(2*time.Minute))

	err = r.DisableJob(t.Context(), job.ID, now.Add(3*time.Minute))
	if !errors.Is(err, repeat.ErrConflict) {
		t.Fatalf("DisableJob(done) = %v, want errors.Is(ErrConflict)", err)
	}
	got, err := r.Job(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if got.Status != repeat.StatusDone {
		t.Errorf("status = %s, want done to stay done", got.Status)
	}
}

// TestStoreDueJobsRespectsStatus ensures disabled rows never surface in
// DueJobs even when next_fire_at is in the past. Otherwise the scheduler
// would fire a job the user just disabled.
func TestStoreDueJobsRespectsStatus(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")

	j, err := r.AddJob(ctx, repeat.JobSpec{
		Name: "j", Command: []string{"true"}, Cron: "@hourly",
	}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	runID, run, err := r.Claim(ctx, j.ID, j.NextFireAt, now.Add(-time.Minute), now, 100)
	if err != nil || !run {
		t.Fatalf("Claim = (run=%v, %v), want a claimed run", run, err)
	}
	mustFinish(t, r, runID, j.ID, now)
	// Confirm it would be due.
	due, err := r.DueJobs(ctx, now)
	if err != nil {
		t.Fatalf("DueJobs before disable: %v", err)
	}
	if len(due) != 1 {
		t.Errorf("due before disable: %d jobs, want 1", len(due))
	}
	// Disable the job.
	// It must drop out of DueJobs even with past next_fire_at.
	if err := r.DisableJob(ctx, j.ID, now); err != nil {
		t.Fatalf("DisableJob disabled: %v", err)
	}
	due, err = r.DueJobs(ctx, now)
	if err != nil {
		t.Fatalf("DueJobs after disable: %v", err)
	}
	if len(due) != 0 {
		t.Errorf("due after disable: %d jobs, want 0", len(due))
	}
}

// TestStoreNextFireAtInvariant checks the store-side contract the scheduler
// relies on: AddJob writes next_fire_at, a finished one-shot keeps it at
// zero, and Claim moves are reflected in DueJobs and SoonestDeadline.
func TestStoreNextFireAtInvariant(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")

	// AddJob computes next_fire_at on insert.
	cron, err := r.AddJob(ctx, repeat.JobSpec{
		Name: "c", Command: []string{"true"}, Cron: "@hourly",
	}, now)
	if err != nil {
		t.Fatalf("AddJob cron: %v", err)
	}
	if cron.NextFireAt.IsZero() {
		t.Errorf("cron next_fire_at is zero, want non-zero")
	}
	if !cron.NextFireAt.After(now) {
		t.Errorf("next_fire_at = %v, want > now (%v)", cron.NextFireAt, now)
	}

	one, err := r.AddJob(ctx, repeat.JobSpec{
		Name: "o", Command: []string{"true"}, FireAt: now.Add(time.Hour),
	}, now)
	if err != nil {
		t.Fatalf("AddJob oneshot: %v", err)
	}
	if !one.NextFireAt.Equal(now.Add(time.Hour)) {
		t.Errorf("oneshot next_fire_at = %v, want %v", one.NextFireAt, now.Add(time.Hour))
	}

	// DueJobs at now sees nothing because both deadlines are future.
	due, err := r.DueJobs(ctx, now)
	if err != nil {
		t.Fatalf("DueJobs: %v", err)
	}
	if len(due) != 0 {
		t.Errorf("DueJobs(now) returned %d jobs, want 0", len(due))
	}

	// SoonestDeadline returns the cron job's deadline because it is sooner.
	soonest, err := r.SoonestDeadline(ctx, now)
	if err != nil {
		t.Fatalf("SoonestDeadline: %v", err)
	}
	if !soonest.Equal(cron.NextFireAt) {
		t.Errorf("SoonestDeadline = %v, want %v", soonest, cron.NextFireAt)
	}

	// Advancing the cron past now+2h and claiming the one-shot (deadline
	// zeroed) moves both out of the due window.
	if _, run, err := r.Claim(ctx, cron.ID, cron.NextFireAt, now.Add(2*time.Hour), now, 100); err != nil || !run {
		t.Fatalf("Claim cron = (run=%v, %v), want a claimed run", run, err)
	}
	oneRun := mustClaim(t, r, one, now)
	due, err = r.DueJobs(ctx, now)
	if err != nil {
		t.Fatalf("DueJobs after advance: %v", err)
	}
	if len(due) != 0 {
		t.Errorf("after advance, DueJobs returned %d jobs, want 0", len(due))
	}

	// Finishing the one-shot leaves next_fire_at at zero: done.
	mustFinish(t, r, oneRun, one.ID, now)
	got, err := r.Job(ctx, one.ID)
	if err != nil {
		t.Fatalf("Job oneshot: %v", err)
	}
	if !got.NextFireAt.IsZero() || got.Status != repeat.StatusDone {
		t.Errorf("finished one-shot: next=%v status=%s, want zero/done", got.NextFireAt, got.Status)
	}

	// SoonestDeadline now skips the done one-shot and sees the cron at +2h.
	soonest, err = r.SoonestDeadline(ctx, now)
	if err != nil {
		t.Fatalf("SoonestDeadline after done: %v", err)
	}
	if !soonest.Equal(now.Add(2 * time.Hour)) {
		t.Errorf("SoonestDeadline after done = %v, want %v", soonest, now.Add(2*time.Hour))
	}
}

func TestStoreListRunsSince(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")

	job, err := r.AddJob(ctx, repeat.JobSpec{Name: "c", Command: []string{"x"}, Cron: "@hourly"}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	starts := []time.Time{
		now.Add(-4 * time.Hour),
		now.Add(-2 * time.Hour),
		now.Add(-30 * time.Minute),
		now.Add(-1 * time.Minute),
	}
	for _, s := range starts {
		insertRun(t, r, job.ID, s, s.Add(time.Second))
	}

	runs, err := r.ListRunsSince(ctx, job.ID, now.Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("ListRunsSince: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("ListRunsSince() returned %d runs, want 2", len(runs))
	}
	// Oldest-first ordering.
	if !runs[0].StartedAt.Before(runs[1].StartedAt) {
		t.Fatalf("ListRunsSince() returned starts %v then %v, want oldest first", runs[0].StartedAt, runs[1].StartedAt)
	}
}

func TestStoreListRunsAfter(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")

	job, err := r.AddJob(ctx, repeat.JobSpec{Name: "c", Command: []string{"x"}, Cron: "@hourly"}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	first := insertRun(t, r, job.ID, now, now.Add(time.Second))
	second := insertRun(t, r, job.ID, now.Add(time.Second), now.Add(2*time.Second))
	third := insertRun(t, r, job.ID, now.Add(2*time.Second), now.Add(3*time.Second))

	runs, err := r.ListRunsAfter(ctx, job.ID, first)
	if err != nil {
		t.Fatalf("ListRunsAfter: %v", err)
	}
	if diff := cmp.Diff([]int64{second, third}, runIDs(runs)); diff != "" {
		t.Errorf("ListRunsAfter IDs mismatch (-want +got):\n%s", diff)
	}
}

func TestStoreGCEnforcesRetention(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")

	job, err := r.AddJob(ctx, repeat.JobSpec{Name: "c", Command: []string{"echo"}, Cron: "@hourly"}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Force a tight retention so we can exercise both axes.
	const perJob = 3

	// Five runs across two days.
	// GC should keep the 3 most recent.
	// The oldest run is still within 100 days.
	// That isolates the per-job axis.
	starts := []time.Time{
		now.Add(-4 * time.Hour),
		now.Add(-3 * time.Hour),
		now.Add(-2 * time.Hour),
		now.Add(-1 * time.Hour),
		now,
	}
	for _, s := range starts {
		insertRun(t, r, job.ID, s, s.Add(time.Second))
	}

	if err := r.GC(ctx, now, perJob, RetentionMaxAge, 0); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if runs := listRuns(t, r, job.ID); len(runs) != 3 {
		t.Errorf("after GC: %d runs, want 3", len(runs))
	}
}

func TestStoreGCEnforcesGlobalCap(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")

	// Two jobs, three runs each, spaced so global ordering by started_at
	// is unambiguous. With perJob=10 and maxAge huge, only the global cap
	// should trim. Cap of 4 drops the 2 oldest.
	jobA, err := r.AddJob(ctx, repeat.JobSpec{Name: "a", Command: []string{"x"}, Cron: "@hourly"}, now)
	if err != nil {
		t.Fatalf("AddJob a: %v", err)
	}
	jobB, err := r.AddJob(ctx, repeat.JobSpec{Name: "b", Command: []string{"x"}, Cron: "@hourly"}, now)
	if err != nil {
		t.Fatalf("AddJob b: %v", err)
	}
	type rec struct {
		job repeat.JobID
		t   time.Time
	}
	recs := []rec{
		{jobA.ID, now.Add(-6 * time.Minute)},
		{jobB.ID, now.Add(-5 * time.Minute)},
		{jobA.ID, now.Add(-4 * time.Minute)},
		{jobB.ID, now.Add(-3 * time.Minute)},
		{jobA.ID, now.Add(-2 * time.Minute)},
		{jobB.ID, now.Add(-1 * time.Minute)},
	}
	for _, e := range recs {
		insertRun(t, r, e.job, e.t, e.t.Add(time.Second))
	}

	if err := r.GC(ctx, now, 10, RetentionMaxAge, 4); err != nil {
		t.Fatalf("GC: %v", err)
	}

	runsA := listRuns(t, r, jobA.ID)
	runsB := listRuns(t, r, jobB.ID)
	total := len(runsA) + len(runsB)
	if total != 4 {
		t.Errorf("after GC: %d runs total, want 4", total)
	}
	// The two oldest were a@-6m and b@-5m.
	// Both should be gone.
	for _, r := range runsA {
		if !r.StartedAt.After(now.Add(-5 * time.Minute)) {
			t.Errorf("jobA run at %v survived global cap", r.StartedAt)
		}
	}
	for _, r := range runsB {
		if !r.StartedAt.After(now.Add(-5 * time.Minute)) {
			t.Errorf("jobB run at %v survived global cap", r.StartedAt)
		}
	}
}

func TestStoreGCKeepsNewestRunsWhenTimestampsTie(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")

	job, err := r.AddJob(ctx, repeat.JobSpec{Name: "ties", Command: []string{"x"}, Cron: "@hourly"}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	for range 5 {
		insertRun(t, r, job.ID, now, now.Add(time.Second))
	}

	if err := r.GC(ctx, now, 2, RetentionMaxAge, 0); err != nil {
		t.Fatalf("GC: %v", err)
	}
	runs := listRuns(t, r, job.ID)
	if len(runs) != 2 {
		t.Errorf("after GC: %d runs, want 2; runs=%+v", len(runs), runs)
	}
	if diff := cmp.Diff([]int64{5, 4}, runIDs(runs)); diff != "" {
		t.Errorf("kept run IDs mismatch (-want +got):\n%s", diff)
	}
}

// TestGCKeepsRunningRecords checks that GC never trims an open running
// row. It belongs to a command that is still executing and deleting
// it would break the finish write.
func TestGCKeepsRunningRecords(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")

	job, err := r.AddJob(ctx, repeat.JobSpec{Name: "slow", Command: []string{"x"}, Cron: "@hourly"}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	// A run "started" far past every retention bound, still going.
	ancient := now.Add(-2 * RetentionMaxAge)
	runID, run, err := r.Claim(ctx, job.ID, job.NextFireAt, now.Add(time.Hour), ancient, 100)
	if err != nil || !run {
		t.Fatalf("Claim = (run=%v, %v), want a claimed run", run, err)
	}
	insertRun(t, r, job.ID, now, now.Add(time.Second))

	// Tightest possible bounds on every axis.
	if err := r.GC(ctx, now, 1, RetentionMaxAge, 1); err != nil {
		t.Fatalf("GC: %v", err)
	}
	runs := listRuns(t, r, job.ID)
	var stillRunning bool
	for _, got := range runs {
		if got.ID == runID && got.Status == repeat.RunRunning {
			stillRunning = true
		}
	}
	if !stillRunning {
		t.Fatalf("GC trimmed the open running row; runs=%+v", runs)
	}
	// Finishing it must still work.
	if err := r.Finish(ctx, runID, job.ID, 0, repeat.RunOK, nil, now.Add(time.Minute)); err != nil {
		t.Fatalf("Finish after GC: %v", err)
	}
}

func TestStoreCounts(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")
	future := now.Add(time.Hour)

	if _, err := r.AddJob(ctx, repeat.JobSpec{Name: "c1", Command: []string{"x"}, Cron: "@hourly"}, now); err != nil {
		t.Fatalf("AddJob c1: %v", err)
	}
	if _, err := r.AddJob(ctx, repeat.JobSpec{Name: "c2", Command: []string{"x"}, Cron: "@daily"}, now); err != nil {
		t.Fatalf("AddJob c2: %v", err)
	}
	if _, err := r.AddJob(ctx, repeat.JobSpec{Name: "o1", Command: []string{"x"}, FireAt: future}, now); err != nil {
		t.Fatalf("AddJob o1: %v", err)
	}
	o2, err := r.AddJob(ctx, repeat.JobSpec{Name: "o2", Command: []string{"x"}, FireAt: future}, now)
	if err != nil {
		t.Fatalf("AddJob o2: %v", err)
	}
	mustFinish(t, r, mustClaim(t, r, o2, now), o2.ID, now)
	c3, err := r.AddJob(ctx, repeat.JobSpec{Name: "c3", Command: []string{"x"}, Cron: "@daily"}, now)
	if err != nil {
		t.Fatalf("AddJob c3: %v", err)
	}
	if err := r.DisableJob(ctx, c3.ID, now); err != nil {
		t.Fatalf("DisableJob c3: %v", err)
	}

	c, err := r.Counts(ctx)
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	want := repeat.JobCounts{Total: 5, Cron: 3, CronDisabled: 1, OneshotPending: 1, OneshotDone: 1}
	if diff := cmp.Diff(want, c); diff != "" {
		t.Errorf("Counts() mismatch (-want +got):\n%s", diff)
	}
}

// TestStoreCountsPartitionsOneshots checks that every one-shot lands in
// exactly one of pending/in-flight/done/disabled. A claimed one-shot is
// in flight even after a mid-run disable, matching startup recovery.
func TestStoreCountsPartitionsOneshots(t *testing.T) {
	r := newStore(t)
	ctx := t.Context()
	now := mustTime(t, "2026-05-13T10:00:00Z")
	future := now.Add(time.Hour)

	if _, err := r.AddJob(ctx, repeat.JobSpec{Name: "cron", Command: []string{"x"}, Cron: "@hourly"}, now); err != nil {
		t.Fatalf("AddJob cron: %v", err)
	}
	if _, err := r.AddJob(ctx, repeat.JobSpec{Name: "pending", Command: []string{"x"}, FireAt: future}, now); err != nil {
		t.Fatalf("AddJob pending: %v", err)
	}
	done, err := r.AddJob(ctx, repeat.JobSpec{Name: "done", Command: []string{"x"}, FireAt: future}, now)
	if err != nil {
		t.Fatalf("AddJob done: %v", err)
	}
	mustFinish(t, r, mustClaim(t, r, done, now), done.ID, now)
	// Claimed: next_fire_at cleared by the scheduler, not yet finished.
	claimed, err := r.AddJob(ctx, repeat.JobSpec{Name: "claimed", Command: []string{"x"}, FireAt: future}, now)
	if err != nil {
		t.Fatalf("AddJob claimed: %v", err)
	}
	mustClaim(t, r, claimed, now)
	// Disabled mid-run: still in flight, not in the disabled bucket.
	// Disabling does not stop a running command.
	midRun, err := r.AddJob(ctx, repeat.JobSpec{Name: "mid-run", Command: []string{"x"}, FireAt: future}, now)
	if err != nil {
		t.Fatalf("AddJob mid-run: %v", err)
	}
	mustClaim(t, r, midRun, now)
	if err := r.DisableJob(ctx, midRun.ID, now); err != nil {
		t.Fatalf("DisableJob mid-run: %v", err)
	}
	// Disabled before firing: the disabled bucket.
	parked, err := r.AddJob(ctx, repeat.JobSpec{Name: "parked", Command: []string{"x"}, FireAt: future}, now)
	if err != nil {
		t.Fatalf("AddJob parked: %v", err)
	}
	if err := r.DisableJob(ctx, parked.ID, now); err != nil {
		t.Fatalf("DisableJob parked: %v", err)
	}

	got, err := r.Counts(ctx)
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	want := repeat.JobCounts{
		Total: 6, Cron: 1,
		OneshotPending: 1, OneshotDone: 1, OneshotDisabled: 1, OneshotInFlight: 2,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Counts mismatch (-want +got):\n%s", diff)
	}
}

func jobNames(jobs []repeat.Job) []string {
	names := make([]string, 0, len(jobs))
	for _, job := range jobs {
		names = append(names, job.Name)
	}
	return names
}

func runIDs(runs []repeat.Run) []int64 {
	ids := make([]int64, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.ID)
	}
	return ids
}
