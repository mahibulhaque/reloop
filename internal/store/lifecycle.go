// This file is the run state machine. Every state change of a run
// happens here and nowhere else.
//
// The life of one run:
//
//  1. The job becomes due.
//  2. Claim advances the deadline and inserts a running row.
//  3. The command executes.
//  4. Finish closes the row and updates the job summary.
//  5. If the daemon dies before step 4, the next startup calls
//     Recover, which closes the row as interrupted.
//
// Cron and one-shot jobs take the same steps. A cron's claim writes
// the next fire time and a one-shot's writes zero. A job that
// finishes with a zero deadline is marked done.
//
// The running rows are also the concurrency slots. Claim refuses to
// insert a row past the cap and closing a row frees its slot.
//
// Each step is one transaction. A crash between steps leaves either
// a due job or a running row, never a half-written state.

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/mahibulhaque/repeat/internal/repeat"
)

// interruptedNote is stored as the output of a run whose real
// outcome is unknown because the daemon died.
const interruptedNote = "repeat: daemon exited before this run finished, outcome unknown\n"

// Claim takes a due job and opens its running row.
//
//  1. Advance the deadline to next (zero for one-shots). This fails
//     if the job changed since the due scan or all maxRunning slots
//     are busy, and then nothing runs.
//  2. If the job's previous run is still going, insert a
//     skipped_overlap row instead and nothing runs.
//  3. Otherwise insert the running row and return its ID with
//     run=true.
func (s *Store) Claim(ctx context.Context, id repeat.JobID, prev, next, now time.Time, maxRunning int) (runID int64, run bool, err error) {
	err = retryOnBusy(ctx, func() error {
		var cerr error
		runID, run, cerr = s.claim(ctx, id, prev, next, now, maxRunning)
		return cerr
	})
	return runID, run, err
}

func (s *Store) claim(ctx context.Context, id repeat.JobID, prev, next, now time.Time, maxRunning int) (int64, bool, error) {
	// Zero rows here means the job changed since the due scan or
	// every slot is busy.
	const claimDeadlineSQL = `
		UPDATE jobs SET next_fire_at = ?, updated_at = ?
		WHERE id = ? AND status = 'enabled' AND next_fire_at = ?
		  AND (SELECT COUNT(*) FROM runs WHERE status = 'running') < ?`

	const prevRunOpenSQL = `
		SELECT EXISTS(SELECT 1 FROM runs WHERE job_id = ? AND status = 'running')`

	// A skipped_overlap row starts and finishes at the same instant.
	const recordOverlapSQL = `
		INSERT INTO runs (job_id, started_at, finished_at, status)
		VALUES (?, ?, ?, ?)`

	const openRunSQL = `
		INSERT INTO runs (job_id, started_at, status)
		VALUES (?, ?, ?)`

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("claim tx: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, claimDeadlineSQL,
		msOrZero(next), now.UnixMilli(), string(id), prev.UnixMilli(), maxRunning)
	if err != nil {
		return 0, false, fmt.Errorf("claim deadline: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("claim rows affected: %w", err)
	}
	if n == 0 {
		return 0, false, nil
	}

	var overlapping bool
	if err := tx.QueryRowContext(ctx, prevRunOpenSQL, string(id)).Scan(&overlapping); err != nil {
		return 0, false, fmt.Errorf("claim overlap check: %w", err)
	}
	if overlapping {
		if _, err := tx.ExecContext(ctx, recordOverlapSQL,
			string(id), now.UnixMilli(), now.UnixMilli(), string(repeat.RunSkippedOverlap)); err != nil {
			return 0, false, fmt.Errorf("claim record overlap: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return 0, false, fmt.Errorf("claim commit: %w", err)
		}
		return 0, false, nil
	}

	res, err = tx.ExecContext(ctx, openRunSQL,
		string(id), now.UnixMilli(), string(repeat.RunRunning))
	if err != nil {
		return 0, false, fmt.Errorf("claim record run: %w", err)
	}
	runID, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("claim run id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("claim commit: %w", err)
	}
	return runID, true, nil
}

// Finish closes the running row and updates the job in one
// transaction.
//
//  1. Close the row with the exit code, outcome, and output.
//  2. Update the job's last-run summary. A job with no next deadline
//     is marked done.
//
// ErrNotFound means the job was deleted mid-run.
func (s *Store) Finish(ctx context.Context, runID int64, id repeat.JobID, exitCode int, outcome repeat.RunStatus, output []byte, now time.Time) error {
	if output == nil {
		output = []byte{}
	}
	return retryOnBusy(ctx, func() error {
		return s.finish(ctx, runID, id, exitCode, outcome, output, now)
	})
}

func (s *Store) finish(ctx context.Context, runID int64, id repeat.JobID, exitCode int, outcome repeat.RunStatus, output []byte, now time.Time) error {
	// Zero rows here means the row is not running anymore.
	const closeRunSQL = `
		UPDATE runs SET finished_at = ?, exit_code = ?, status = ?, output = ?
		WHERE id = ? AND status = 'running'`

	// Same summary write as recoverJobsSQL. A job with no next
	// deadline becomes done.
	const finishJobSQL = `
		UPDATE jobs SET last_run_at = ?, last_status = ?, updated_at = ?,
			status = CASE WHEN next_fire_at = 0 THEN 'done' ELSE status END
		WHERE id = ?`

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finish tx: %w", err)
	}
	defer tx.Rollback()

	ms := now.UnixMilli()
	res, err := tx.ExecContext(ctx, closeRunSQL,
		ms, exitCode, string(outcome), output, runID)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("finish rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: run %d of job %q is not running, the job was deleted mid-run", repeat.ErrNotFound, runID, id)
	}
	if _, err := tx.ExecContext(ctx, finishJobSQL,
		ms, string(outcome), ms, string(id)); err != nil {
		return fmt.Errorf("finish summary: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("finish commit: %w", err)
	}
	return nil
}

// Recover runs at daemon startup and closes every leftover running
// row as interrupted. The command may or may not have completed, so
// the run is never replayed. It returns how many rows it closed.
func (s *Store) Recover(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := retryOnBusy(ctx, func() error {
		var rerr error
		n, rerr = s.recoverInterrupted(ctx, now)
		return rerr
	})
	return n, err
}

func (s *Store) recoverInterrupted(ctx context.Context, now time.Time) (int, error) {
	// Same summary write as finishJobSQL. It must run before
	// recoverRunsSQL because the running rows are how the jobs are
	// found.
	const recoverJobsSQL = `
		UPDATE jobs SET last_run_at = ?, last_status = ?, updated_at = ?,
			status = CASE WHEN next_fire_at = 0 THEN 'done' ELSE status END
		WHERE id IN (SELECT job_id FROM runs WHERE status = 'running')`

	const recoverRunsSQL = `
		UPDATE runs SET status = ?, finished_at = ?, exit_code = -1, output = ?
		WHERE status = 'running'`

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("recover tx: %w", err)
	}
	defer tx.Rollback()

	ms := now.UnixMilli()
	if _, err := tx.ExecContext(ctx, recoverJobsSQL,
		ms, string(repeat.RunInterrupted), ms); err != nil {
		return 0, fmt.Errorf("recover jobs: %w", err)
	}
	res, err := tx.ExecContext(ctx, recoverRunsSQL,
		string(repeat.RunInterrupted), ms, []byte(interruptedNote))
	if err != nil {
		return 0, fmt.Errorf("recover runs: %w", err)
	}
	closed, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("recover rows affected: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("recover commit: %w", err)
	}
	return int(closed), nil
}
