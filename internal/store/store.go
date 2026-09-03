// Package store persists jobs and run history in SQLite.
//
// The query pattern, used everywhere:
//
//  1. Every SQL statement is a function-local constant written out
//     in full. No query is assembled from fragments.
//  2. A single-row query scans QueryRowContext through scanJob or
//     scanRun and maps sql.ErrNoRows inline.
//  3. A multi-row query loops over QueryContext rows the plain way.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mahibulhaque/reloop/internal/reloop"
	"modernc.org/sqlite"
)

// Timestamp columns hold Unix milliseconds. The schema is idempotent and
// re-run on every open, so a new index reaches existing databases
// without a version bump. A table shape change bumps schemaVersion,
// which deletes and recreates the file.
//
// Every index exists for a specific query:
//
//  1. jobs_next_fire is for the due scan and the soonest deadline.
//  2. jobs_name is for lookup by name. The lowest ID wins.
//  3. runs_job_started is for per-job run history.
//  4. runs_running only holds the open running rows. Those are the
//     concurrency slots, so counting them is an index seek.
const schema = `
CREATE TABLE IF NOT EXISTS jobs (
    id            TEXT    PRIMARY KEY,
    kind          TEXT    NOT NULL CHECK(kind IN ('cron','oneshot')),
    name          TEXT    NOT NULL,
    command_json  TEXT    NOT NULL,
    env_json      TEXT    NOT NULL DEFAULT '[]',
    cron_expression     TEXT    NOT NULL DEFAULT '',
    fire_at       INTEGER NOT NULL DEFAULT 0,
    status        TEXT    NOT NULL CHECK(status IN ('enabled','disabled','done')),
    last_run_at   INTEGER NOT NULL DEFAULT 0,
    last_status   TEXT    NOT NULL DEFAULT '',
    next_fire_at  INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS jobs_next_fire ON jobs(status, next_fire_at);
CREATE INDEX IF NOT EXISTS jobs_name      ON jobs(name, id);

CREATE TABLE IF NOT EXISTS runs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id       TEXT    NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    started_at   INTEGER NOT NULL,
    finished_at  INTEGER NOT NULL DEFAULT 0,
    exit_code    INTEGER NOT NULL DEFAULT 0,
    status       TEXT    NOT NULL,
    output       BLOB    NOT NULL DEFAULT x''
) STRICT;
CREATE INDEX IF NOT EXISTS runs_job_started ON runs(job_id, started_at DESC);
CREATE INDEX IF NOT EXISTS runs_running    ON runs(job_id) WHERE status = 'running';
`

// Default retention applied by Store.GC.
const (
	// RetentionPerJob is the number of most-recent runs kept per job.
	RetentionPerJob = 100

	// RetentionMaxAge is the maximum age of retained run history.
	RetentionMaxAge = 100 * 24 * time.Hour

	// RetentionMaxTotal caps the whole runs table.
	RetentionMaxTotal = 144_000
)

// DefaultListLimit caps Store.ListJobs output when no Limit is set.
const DefaultListLimit = 100

// ListOpts filters Store.ListJobs results. The zero value returns
// every job ordered by created_at descending.
type ListOpts struct {
	Kind   reloop.JobKind   // empty = both kinds
	Status reloop.JobStatus // empty = all statuses
	Limit  int              // <=0 means no cap at the store layer
	// Offset skips rows. LIMIT/OFFSET paging can skip or reloop rows
	// under concurrent writes, which is fine for a single-user store.
	Offset int
}

// MaxOutputBytes caps the captured stdout+stderr per run.
const MaxOutputBytes = 100 * 1024

// Store holds SQLite-backed jobs and run history. Concurrent callers
// against the same data dir are safe: transactions begin immediate,
// so writers queue inside SQLite instead of failing.
type Store struct {
	db      *sql.DB
	dataDir string
	dbPath  string
}

// Open returns a Store with its schema applied. An empty dataDir
// means an in-memory database for tests. ctx scopes schema setup
// only, not the Store's lifetime.
func Open(ctx context.Context, dataDir string) (*Store, error) {
	var dbPath, dsn string
	switch dataDir {
	case "":
		dbPath = ":memory:"
		// A unique name per Open keeps test stores isolated.
		// cache=shared keeps the database alive across pool reconnects.
		dsn = fmt.Sprintf("file:reloopmem%d?mode=memory&cache=shared&_txlock=immediate&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)",
			rand.Uint64())
	default:
		// 0700: env snapshots in this directory routinely carry secrets.
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return nil, fmt.Errorf("mkdir data dir: %w", err)
		}
		dbPath = filepath.Join(dataDir, "reloop.db")
		// The settings apply in DSN order.
		//
		//  1. _txlock=immediate takes the write lock at BEGIN, so
		//     writers queue inside SQLite instead of failing busy.
		//  2. busy_timeout goes first. WAL setup on a fresh file
		//     needs to wait, not fail.
		//  3. journal_size_limit caps the WAL file after a burst.
		//  4. temp_store keeps sort scratch in memory.
		dsn = "file:" + url.PathEscape(dbPath) +
			"?_txlock=immediate" +
			"&_pragma=busy_timeout(30000)" +
			"&_pragma=journal_mode(WAL)" +
			"&_pragma=journal_size_limit(33554432)" +
			"&_pragma=synchronous(NORMAL)" +
			"&_pragma=foreign_keys(1)" +
			"&_pragma=temp_store(2)"
	}

	open := func() (*sql.DB, error) {
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			return nil, fmt.Errorf("open sqlite: %w", err)
		}
		db.SetMaxOpenConns(1)
		if err := applySchema(ctx, db); err != nil {
			_ = db.Close()
			return nil, err
		}
		return db, nil
	}

	db, err := open()
	if errors.Is(err, errFormatMismatch) {
		// No migrations. A database in another format is deleted and
		// recreated. Corrupt files and I/O errors still fail the open.
		for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
			rmErr := os.Remove(p)
			if rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
				return nil, fmt.Errorf("remove old-format database: %w", rmErr)
			}
		}
		db, err = open()
	}
	if err != nil {
		return nil, err
	}
	if dataDir != "" {
		// Keep the database, its WAL sidecars, and the directory
		// user-only. Best effort: the sidecars may not exist yet.
		_ = os.Chmod(dataDir, 0o700)
		_ = os.Chmod(dbPath, 0o600)
		_ = os.Chmod(dbPath+"-wal", 0o600)
		_ = os.Chmod(dbPath+"-shm", 0o600)
	}

	return &Store{db: db, dataDir: dataDir, dbPath: dbPath}, nil
}

// schemaVersion is the storage format, stamped in PRAGMA user_version.
// Open deletes and recreates a database in any other format.
const schemaVersion = 2

// errFormatMismatch reports a database written in a different format.
var errFormatMismatch = errors.New("storage format mismatch")

// applySchema brings the database to this binary's format.
//
//  1. Read the stamped version. Any other stamped version is a
//     format mismatch.
//  2. Version 0 with a jobs table is an old unstamped database.
//     Also a mismatch.
//  3. Run the idempotent schema and stamp the version. On a current
//     database this also creates any index added since.
func applySchema(ctx context.Context, db *sql.DB) error {
	const hasJobsSQL = `
		SELECT EXISTS(
			SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'jobs'
		)`

	// Keep the name list in sync with the schema above.
	const indexCountSQL = `
		SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name IN
			('jobs_next_fire', 'jobs_name', 'runs_job_started', 'runs_running')`

	// Fast path. A current database needs no writes to open, so
	// bursts of concurrent CLI opens read in parallel instead of
	// queueing on the write lock.
	var v, indexes int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err == nil && v == schemaVersion {
		if err := db.QueryRowContext(ctx, indexCountSQL).Scan(&indexes); err == nil && indexes == 4 {
			return nil
		}
	}

	// retryOnBusy covers concurrent first opens racing WAL setup.
	return retryOnBusy(ctx, func() error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("schema tx: %w", err)
		}
		defer tx.Rollback()

		var v int
		if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
			return fmt.Errorf("read schema version: %w", err)
		}
		if v != 0 && v != schemaVersion {
			return fmt.Errorf("%w: database format v%d, this binary writes v%d", errFormatMismatch, v, schemaVersion)
		}
		if v == 0 {
			var hasJobs bool
			if err := tx.QueryRowContext(ctx, hasJobsSQL).Scan(&hasJobs); err != nil {
				return fmt.Errorf("inspect schema: %w", err)
			}
			if hasJobs {
				return fmt.Errorf("%w: database written by reloop v0.1.x", errFormatMismatch)
			}
		}
		if _, err := tx.ExecContext(ctx, schema); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
			return fmt.Errorf("stamp schema version: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("schema commit: %w", err)
		}
		return nil
	})
}

// SQLite primary result codes. Extended codes share the low byte.
const (
	sqliteBusy       = 5  // SQLITE_BUSY
	sqliteConstraint = 19 // SQLITE_CONSTRAINT
)

// sqliteCodeIs reports whether err carries the given SQLite code.
func sqliteCodeIs(err error, code int) bool {
	se, ok := errors.AsType[*sqlite.Error](err)
	return ok && se.Code()&0xff == code
}

// retryOnBusy invokes fn with backoff on SQLITE_BUSY. Busy reaches Go
// only when fresh-database WAL setup races another process, or when
// busy_timeout expires under a very long writer. Both fix themselves
// by rerunning the whole transaction.
func retryOnBusy(ctx context.Context, fn func() error) error {
	const maxAttempts = 8
	delay := 25 * time.Millisecond
	var lastErr error
	for range maxAttempts {
		err := fn()
		if err == nil {
			return nil
		}
		if !sqliteCodeIs(err, sqliteBusy) {
			return err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(delay):
		}
		if delay < time.Second {
			delay *= 2
		}
	}
	return lastErr
}

// DataDir returns the directory that contains the store database.
func (s *Store) DataDir() string { return s.dataDir }

// DBPath returns the SQLite database path, or :memory: for test stores.
func (s *Store) DBPath() string { return s.dbPath }

// Close releases the underlying SQLite handle. SQLite recommends
// PRAGMA optimize before closing. It keeps the query planner
// statistics fresh.
func (s *Store) Close() error {
	_, _ = s.db.Exec(`PRAGMA optimize`)
	return s.db.Close()
}

// AddJob validates and inserts a new enabled job. Validation lives
// here, not only in the CLI, so no caller can insert a job the
// scheduler can never fire.
func (s *Store) AddJob(ctx context.Context, spec reloop.JobSpec, now time.Time) (reloop.Job, error) {
	const insertJobSQL = `
		INSERT INTO jobs
			(id, kind, name, command_json, env_json, cron_expression, fire_at, status,
			 next_fire_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'enabled', ?, ?, ?)`

	// Store the trimmed expression. A stored newline would break
	// table output.
	spec.Cron = strings.TrimSpace(spec.Cron)
	if err := spec.Validate(now); err != nil {
		return reloop.Job{}, err
	}
	// Marshalling a []string cannot fail. nil still becomes '[]' so
	// the column never holds JSON null. Bound as strings because the
	// columns are TEXT and the tables are STRICT.
	cmdBytes, _ := json.Marshal(spec.Command)
	cmdJSON := string(cmdBytes)
	envJSON := "[]"
	if len(spec.Env) > 0 {
		envBytes, _ := json.Marshal(spec.Env)
		envJSON = string(envBytes)
	}

	kind := reloop.KindOneshot
	fireAt := int64(0)
	if spec.Cron != "" {
		kind = reloop.KindCron
	} else {
		fireAt = spec.FireAt.UnixMilli()
	}
	// Compute next_fire_at before insert so the scheduler finds the
	// new job through its indexed due scan.
	probe := reloop.Job{
		Kind:   kind,
		Cron:   spec.Cron,
		FireAt: spec.FireAt,
		Status: reloop.StatusEnabled,
	}
	nextFire := msOrZero(reloop.NextFire(probe, now))

	for range 8 {
		id := newJobID()
		// Random ID collisions are rare. Retry a few times before
		// surfacing a real insert error.
		_, err := s.db.ExecContext(ctx, insertJobSQL, string(id), kind, spec.Name, cmdJSON, envJSON,
			spec.Cron, fireAt, nextFire, now.UnixMilli(), now.UnixMilli())
		if err != nil {
			if sqliteCodeIs(err, sqliteConstraint) {
				continue
			}
			return reloop.Job{}, fmt.Errorf("insert job: %w", err)
		}
		// Return the row just written without re-reading it. The time
		// fields go through the same millisecond conversion a SELECT
		// would produce.
		return reloop.Job{
			ID:         id,
			Kind:       kind,
			Name:       spec.Name,
			Command:    spec.Command,
			Env:        spec.Env,
			Cron:       spec.Cron,
			FireAt:     timeFromMilli(fireAt),
			Status:     reloop.StatusEnabled,
			NextFireAt: timeFromMilli(nextFire),
			CreatedAt:  timeFromMilli(now.UnixMilli()),
			UpdatedAt:  timeFromMilli(now.UnixMilli()),
		}, nil
	}
	return reloop.Job{}, fmt.Errorf("insert job: exhausted ID retries")
}

const idAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func newJobID() reloop.JobID {
	var buf [5]byte
	for i := range buf {
		buf[i] = idAlphabet[rand.IntN(len(idAlphabet))]
	}
	return reloop.JobID(buf[:])
}

// Job returns the job with id.
func (s *Store) Job(ctx context.Context, id reloop.JobID) (reloop.Job, error) {
	const jobByIDSQL = `
		SELECT id, kind, name, command_json, env_json, cron_expression, fire_at,
		       status, last_run_at, last_status, next_fire_at, created_at, updated_at
		FROM jobs
		WHERE id = ?`
	job, err := scanJob(s.db.QueryRowContext(ctx, jobByIDSQL, string(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return reloop.Job{}, fmt.Errorf("%w: job %q", reloop.ErrNotFound, id)
	}
	return job, err
}

// JobByName returns the lowest-ID exact-name match. Names are not
// unique.
func (s *Store) JobByName(ctx context.Context, name string) (reloop.Job, error) {
	const jobByNameSQL = `
		SELECT id, kind, name, command_json, env_json, cron_expression, fire_at,
		       status, last_run_at, last_status, next_fire_at, created_at, updated_at
		FROM jobs
		WHERE name = ?
		ORDER BY id ASC
		LIMIT 1`
	job, err := scanJob(s.db.QueryRowContext(ctx, jobByNameSQL, name))
	if errors.Is(err, sql.ErrNoRows) {
		return reloop.Job{}, fmt.Errorf("%w: job %q", reloop.ErrNotFound, name)
	}
	return job, err
}

// ListJobs returns jobs matching opts, most recently created first.
// Env is left empty: no list consumer execs the job, and the env
// snapshot is by far the widest column.
func (s *Store) ListJobs(ctx context.Context, opts ListOpts) ([]reloop.Job, error) {
	// An empty filter argument matches every row, LIMIT -1 is
	// uncapped, and OFFSET 0 is a no-op, so one statement covers
	// every filter and paging combination.
	const listJobsSQL = `
		SELECT id, kind, name, command_json, '[]', cron_expression, fire_at,
		       status, last_run_at, last_status, next_fire_at, created_at, updated_at
		FROM jobs
		WHERE (?1 = '' OR kind = ?1)
		  AND (?2 = '' OR status = ?2)
		ORDER BY created_at DESC, id ASC
		LIMIT ?3 OFFSET ?4`

	limit := opts.Limit
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.db.QueryContext(ctx, listJobsSQL,
		string(opts.Kind), string(opts.Status), limit, opts.Offset)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	jobs := make([]reloop.Job, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("list jobs: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	return jobs, nil
}

// DeleteJob removes the job with id. Its run history cascades away.
func (s *Store) DeleteJob(ctx context.Context, id reloop.JobID) error {
	const deleteJobSQL = `DELETE FROM jobs WHERE id = ?`
	return s.execJob(ctx, "delete job", id, deleteJobSQL, string(id))
}

// DisableJob marks a job disabled. The done-job guard sits in the SQL
// so it cannot race the daemon marking the job done at the same
// moment. A done job is ErrConflict, a missing one ErrNotFound.
func (s *Store) DisableJob(ctx context.Context, id reloop.JobID, now time.Time) error {
	const disableJobSQL = `
		UPDATE jobs SET status = 'disabled', updated_at = ?
		WHERE id = ? AND status != 'done'`

	err := s.execJob(ctx, "disable job", id, disableJobSQL, now.UnixMilli(), string(id))
	if err == nil {
		return nil
	}
	if !errors.Is(err, reloop.ErrNotFound) {
		return err
	}
	// Zero rows means missing or already done. Look up which.
	job, jerr := s.Job(ctx, id)
	if jerr != nil {
		return err
	}
	if job.Status == reloop.StatusDone {
		return fmt.Errorf("%w: job %s already ran, a done one-shot has nothing left to disable",
			reloop.ErrConflict, id)
	}
	return err
}

// EnableJob enables a disabled job and writes its recomputed deadline
// in one statement. The guards sit in the SQL so they cannot race the
// daemon.
//
//  1. An already-enabled job is a no-op. Rewriting its deadline would
//     silently push the pending fire back.
//  2. A done or claimed one-shot stays put, because re-arming it
//     would fire the command a second time. Both are ErrConflict.
//  3. A missing job is ErrNotFound.
func (s *Store) EnableJob(ctx context.Context, id reloop.JobID, next time.Time, now time.Time) error {
	const enableJobSQL = `
		UPDATE jobs SET status = 'enabled', next_fire_at = ?, updated_at = ?
		WHERE id = ?
		  AND status != 'enabled'
		  AND NOT (kind = 'oneshot' AND (status = 'done' OR next_fire_at = 0))`

	err := s.execJob(ctx, "enable job", id, enableJobSQL, msOrZero(next), now.UnixMilli(), string(id))
	if err == nil {
		return nil
	}
	if !errors.Is(err, reloop.ErrNotFound) {
		return err
	}
	// Zero rows means missing or guarded. Look up which.
	job, jerr := s.Job(ctx, id)
	if jerr != nil {
		return err
	}
	if job.Status == reloop.StatusEnabled {
		return nil
	}
	if job.Status == reloop.StatusDone {
		return fmt.Errorf("%w: job %s already ran and one-shots fire once, add a new job to run it again",
			reloop.ErrConflict, id)
	}
	if job.NextFireAt.IsZero() {
		return fmt.Errorf("%w: job %s already started, its run is in flight or awaiting recovery at the next daemon start",
			reloop.ErrConflict, id)
	}
	return err
}

// DueJobs returns enabled jobs due at or before now, soonest first.
// The (status, next_fire_at) index keeps this a range scan.
func (s *Store) DueJobs(ctx context.Context, now time.Time) ([]reloop.Job, error) {
	const dueJobsSQL = `
		SELECT id, kind, name, command_json, env_json, cron_expression, fire_at,
		       status, last_run_at, last_status, next_fire_at, created_at, updated_at
		FROM jobs
		WHERE status = 'enabled' AND next_fire_at > 0 AND next_fire_at <= ?
		ORDER BY next_fire_at ASC`
	rows, err := s.db.QueryContext(ctx, dueJobsSQL, now.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("due jobs: %w", err)
	}
	defer rows.Close()
	jobs := make([]reloop.Job, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("due jobs: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("due jobs: %w", err)
	}
	return jobs, nil
}

// SoonestDeadline returns the next enabled deadline after now.
// Zero means there is no future fire.
func (s *Store) SoonestDeadline(ctx context.Context, now time.Time) (time.Time, error) {
	const soonestSQL = `
		SELECT next_fire_at
		FROM jobs
		WHERE status = 'enabled' AND next_fire_at > ?
		ORDER BY next_fire_at ASC
		LIMIT 1`
	var ms int64
	err := s.db.QueryRowContext(ctx, soonestSQL, now.UnixMilli()).Scan(&ms)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("soonest deadline: %w", err)
	}
	return timeFromMilli(ms), nil
}

// ListRunsSince returns runs started at or after since, oldest first.
func (s *Store) ListRunsSince(ctx context.Context, jobID reloop.JobID, since time.Time) ([]reloop.Run, error) {
	const runsSinceSQL = `
		SELECT id, job_id, started_at, finished_at, exit_code, status
		FROM runs
		WHERE job_id = ? AND started_at >= ?
		ORDER BY started_at ASC, id ASC`
	rows, err := s.db.QueryContext(ctx, runsSinceSQL, string(jobID), since.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("list runs since: %w", err)
	}
	defer rows.Close()
	runs := make([]reloop.Run, 0)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("list runs since: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list runs since: %w", err)
	}
	return runs, nil
}

// ListRunsAfter returns runs inserted after afterID, oldest first.
func (s *Store) ListRunsAfter(ctx context.Context, jobID reloop.JobID, afterID int64) ([]reloop.Run, error) {
	const runsAfterSQL = `
		SELECT id, job_id, started_at, finished_at, exit_code, status
		FROM runs
		WHERE job_id = ? AND id > ?
		ORDER BY id ASC`
	rows, err := s.db.QueryContext(ctx, runsAfterSQL, string(jobID), afterID)
	if err != nil {
		return nil, fmt.Errorf("list runs after: %w", err)
	}
	defer rows.Close()
	runs := make([]reloop.Run, 0)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("list runs after: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list runs after: %w", err)
	}
	return runs, nil
}

// LatestRun returns the newest completed run for jobID. Open running
// rows are excluded, so `reloop logs` never shows the empty output of a
// run that is still going. OpenRun finds those.
func (s *Store) LatestRun(ctx context.Context, jobID reloop.JobID) (reloop.Run, error) {
	const latestRunSQL = `
		SELECT id, job_id, started_at, finished_at, exit_code, status
		FROM runs
		WHERE job_id = ? AND status != 'running'
		ORDER BY started_at DESC, id DESC
		LIMIT 1`
	run, err := scanRun(s.db.QueryRowContext(ctx, latestRunSQL, string(jobID)))
	if errors.Is(err, sql.ErrNoRows) {
		return reloop.Run{}, fmt.Errorf("%w: no runs for job %q", reloop.ErrNotFound, jobID)
	}
	return run, err
}

// OpenRun returns the job's running row, if one is open. Claim allows
// at most one open row per job.
func (s *Store) OpenRun(ctx context.Context, jobID reloop.JobID) (reloop.Run, error) {
	const openRunSQL = `
		SELECT id, job_id, started_at, finished_at, exit_code, status
		FROM runs
		WHERE job_id = ? AND status = 'running'
		LIMIT 1`
	run, err := scanRun(s.db.QueryRowContext(ctx, openRunSQL, string(jobID)))
	if errors.Is(err, sql.ErrNoRows) {
		return reloop.Run{}, fmt.Errorf("%w: no open run for job %q", reloop.ErrNotFound, jobID)
	}
	return run, err
}

// RunLog returns the captured output for runID.
func (s *Store) RunLog(ctx context.Context, runID int64) ([]byte, error) {
	const runLogSQL = `SELECT output FROM runs WHERE id = ?`
	var out []byte
	err := s.db.QueryRowContext(ctx, runLogSQL, runID).Scan(&out)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: run %d", reloop.ErrNotFound, runID)
	}
	if err != nil {
		return nil, fmt.Errorf("read output: %w", err)
	}
	return out, nil
}

// GC trims run history in one transaction.
//
//  1. Drop runs older than maxAge.
//  2. Keep the perJob newest runs per job.
//  3. If the table still exceeds maxTotal, cap it globally.
//
// Running rows are never trimmed. Deleting one mid-run would break
// the finish write.
func (s *Store) GC(ctx context.Context, now time.Time, perJob int, maxAge time.Duration, maxTotal int) error {
	const gcAgeSQL = `DELETE FROM runs WHERE started_at < ? AND status != 'running'`

	// Ties on started_at break by run ID so the newest rows survive
	// deterministically.
	const gcPerJobSQL = `
		DELETE FROM runs
		WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (
					PARTITION BY job_id
					ORDER BY started_at DESC, id DESC
				) AS rn
				FROM runs
				WHERE status != 'running'
			)
			WHERE rn > ?
		)`

	const countRunsSQL = `SELECT COUNT(*) FROM runs`

	// The cap pass sorts the whole table, so the count guard skips it
	// whenever nothing would be deleted.
	const gcCapSQL = `
		DELETE FROM runs
		WHERE id IN (
			SELECT id FROM runs
			WHERE status != 'running'
			ORDER BY started_at DESC, id DESC
			LIMIT -1 OFFSET ?
		)`

	cutoff := now.Add(-maxAge).UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("gc tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, gcAgeSQL, cutoff); err != nil {
		return fmt.Errorf("gc by age: %w", err)
	}
	if _, err := tx.ExecContext(ctx, gcPerJobSQL, perJob); err != nil {
		return fmt.Errorf("gc by count: %w", err)
	}
	if maxTotal > 0 {
		var total int
		if err := tx.QueryRowContext(ctx, countRunsSQL).Scan(&total); err != nil {
			return fmt.Errorf("gc count runs: %w", err)
		}
		if total > maxTotal {
			if _, err := tx.ExecContext(ctx, gcCapSQL, maxTotal); err != nil {
				return fmt.Errorf("gc by total: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("gc commit: %w", err)
	}
	// GC runs hourly, so the periodic PRAGMA optimize lives here.
	_, _ = s.db.ExecContext(ctx, `PRAGMA optimize`)
	return nil
}

// Counts returns the aggregate used by reloop status, in one round-trip.
// In flight means the job has an open running row, the state
// machine's own encoding, so this count cannot drift from what Claim
// and Finish write.
func (s *Store) Counts(ctx context.Context) (reloop.JobCounts, error) {
	const countsSQL = `
		SELECT
			COUNT(*),
			COUNT(*) FILTER (WHERE kind = 'cron'),
			COUNT(*) FILTER (WHERE kind = 'cron' AND status = 'disabled'),
			COUNT(*) FILTER (WHERE kind = 'oneshot' AND status = 'enabled' AND next_fire_at > 0),
			COUNT(*) FILTER (WHERE kind = 'oneshot' AND status = 'done'),
			COUNT(*) FILTER (WHERE kind = 'oneshot' AND status = 'disabled' AND next_fire_at > 0),
			COUNT(*) FILTER (WHERE kind = 'oneshot' AND EXISTS (
				SELECT 1 FROM runs WHERE runs.job_id = jobs.id AND runs.status = 'running'))
		FROM jobs`
	var c reloop.JobCounts
	row := s.db.QueryRowContext(ctx, countsSQL)
	if err := row.Scan(&c.Total, &c.Cron, &c.CronDisabled,
		&c.OneshotPending, &c.OneshotDone, &c.OneshotDisabled, &c.OneshotInFlight); err != nil {
		return reloop.JobCounts{}, fmt.Errorf("counts: %w", err)
	}
	return c, nil
}

// execJob runs a single-job write, mapping zero affected rows to
// ErrNotFound.
func (s *Store) execJob(ctx context.Context, op string, id reloop.JobID, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", op, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: job %q", reloop.ErrNotFound, id)
	}
	return nil
}

// scanner is the piece of *sql.Row and *sql.Rows the scan functions
// need, so one scan function serves single-row and multi-row queries.
type scanner interface {
	Scan(dest ...any) error
}

// scanJob reads one jobs row. Every job SELECT must list the columns
// in this exact order.
func scanJob(s scanner) (reloop.Job, error) {
	var (
		j             reloop.Job
		kind          string
		cmdJSON       string
		envJSON       string
		status        string
		fireAtMilli   int64
		lastRunMilli  int64
		lastStatus    string
		nextFireMilli int64
		createdMilli  int64
		updatedMilli  int64
	)
	err := s.Scan(&j.ID, &kind, &j.Name, &cmdJSON, &envJSON, &j.Cron, &fireAtMilli,
		&status, &lastRunMilli, &lastStatus, &nextFireMilli, &createdMilli, &updatedMilli)
	if err != nil {
		return reloop.Job{}, err
	}
	j.Kind = reloop.JobKind(kind)
	j.Status = reloop.JobStatus(status)
	j.LastStatus = reloop.RunStatus(lastStatus)
	j.FireAt = timeFromMilli(fireAtMilli)
	j.LastRunAt = timeFromMilli(lastRunMilli)
	j.NextFireAt = timeFromMilli(nextFireMilli)
	j.CreatedAt = timeFromMilli(createdMilli)
	j.UpdatedAt = timeFromMilli(updatedMilli)
	if err := json.Unmarshal([]byte(cmdJSON), &j.Command); err != nil {
		return reloop.Job{}, fmt.Errorf("decode command: %w", err)
	}
	if err := json.Unmarshal([]byte(envJSON), &j.Env); err != nil {
		return reloop.Job{}, fmt.Errorf("decode env: %w", err)
	}
	return j, nil
}

// scanRun reads one runs row. Every run SELECT must list the columns
// in this exact order.
func scanRun(s scanner) (reloop.Run, error) {
	var (
		r             reloop.Run
		startedMilli  int64
		finishedMilli int64
		status        string
	)
	err := s.Scan(&r.ID, &r.JobID, &startedMilli, &finishedMilli, &r.ExitCode, &status)
	if err != nil {
		return reloop.Run{}, err
	}
	r.StartedAt = timeFromMilli(startedMilli)
	r.FinishedAt = timeFromMilli(finishedMilli)
	r.Status = reloop.RunStatus(status)
	return r, nil
}

// msOrZero converts a time to Unix milliseconds, the zero time to 0.
func msOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// timeFromMilli is the inverse of msOrZero: 0 maps back to the zero
// time, everything else to a UTC instant.
func timeFromMilli(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
