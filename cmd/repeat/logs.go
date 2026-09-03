package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/mahibulhaque/repeat/internal/repeat"
	"github.com/mahibulhaque/repeat/internal/store"
	"github.com/spf13/cobra"
)

// logOpts controls the streaming behaviour of `repeat logs`.
type logOpts struct {
	Follow bool          // when true, blocks waiting for new completed runs
	Lines  int           // last N lines from each emitted run (0 = all)
	Since  time.Duration // only runs started within this window (0 = no limit)
}

// pollInterval is how often --follow polls while runs are appearing.
const pollInterval = 500 * time.Millisecond

// idlePollInterval is the slower --follow poll while nothing new is
// happening. The trade is up to this much extra latency on the first
// output of a fresh run.
const idlePollInterval = 2 * time.Second

func newLogsCmd() *cobra.Command {
	var (
		follow bool
		lines  int
		since  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "logs ID|NAME",
		Short: "Print the captured output of a job's runs.",
		Long: `Print the captured stdout+stderr of one or more of a job's runs.

  -f, --follow     stream completed runs as they appear. Blocks until
                   interrupted.
  -n, --lines N    only the last N lines of each emitted run.
  --since DUR      include all runs that started within the window
                   (e.g. 10m, 2h, 30s). Default: just the most recent.

Behaviour matrix:

  (no flags)            most recent run, raw output, no header.
  --lines N             same, trimmed to last N lines.
  --since DUR           every completed run in the window, oldest-first,
                        each preceded by a one-line header.
  --follow              most recent run with header, then new runs as they
                        complete, until interrupted.
  --since + --follow    runs in the window, then new ones as they appear.

Header format: '==> run #ID status exit=N finished=TIME'.`,
		Example: `  repeat logs backup
  repeat logs backup -n 50
  repeat logs 7K3px -f
  repeat logs backup --since 1h
  repeat logs backup --since 30m -f`,
		Args: jobArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			if lines < 0 {
				return usageErrf("--lines must be non-negative")
			}
			if since < 0 {
				return usageErrf("--since must be non-negative")
			}
			return withService(cmd, func(s *service) error {
				warnIfDaemonDown(s)
				job, err := s.Resolve(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				return streamLogs(cmd.Context(), s.st, job.ID, logOpts{
					Follow: follow,
					Lines:  lines,
					Since:  since,
				}, cmd.OutOrStdout())
			})
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new completed runs as they appear")
	cmd.Flags().IntVarP(&lines, "lines", "n", 0, "only the last N lines of each emitted run")
	cmd.Flags().DurationVar(&since, "since", 0, "include runs that started within this window (e.g. 10m, 2h)")
	return cmd
}

// streamLogs writes the requested run output to w.
//
//  1. With no flags, emit the latest run raw and return.
//  2. With --since, emit every finished run in the window, each with
//     a header.
//  3. With --follow, keep polling by run ID and emit runs as they
//     finish, until ctx is cancelled. The cursor never moves past an
//     open running row, or its output would be lost when it closes.
func streamLogs(ctx context.Context, st *store.Store, jobID repeat.JobID, opts logOpts, w io.Writer) error {
	started := time.Now()
	if opts.Since == 0 && !opts.Follow {
		// No flags: print the latest completed run raw. A job with no
		// runs prints nothing and exits 0.
		run, err := st.LatestRun(ctx, jobID)
		if errors.Is(err, repeat.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return emitRunOutput(ctx, st, run.ID, opts.Lines, w)
	}

	var openID int64
	if opts.Follow {
		open, err := st.OpenRun(ctx, jobID)
		if err == nil {
			openID = open.ID
		}
	}

	var lastEmitted int64
	switch {
	case opts.Since > 0:
		runs, err := st.ListRunsSince(ctx, jobID, started.Add(-opts.Since))
		if err != nil {
			return err
		}
		for _, r := range runs {
			if openID > 0 && r.ID > openID {
				break
			}
			if r.Status == repeat.RunRunning {
				continue
			}
			if err := emitRunWithHeader(ctx, st, r, opts.Lines, w); err != nil {
				return err
			}
			lastEmitted = r.ID
		}
		if openID > 0 {
			// Park just before the open record: it streams the moment
			// it closes, and nothing older is replayed.
			lastEmitted = openID - 1
			break
		}
		// When --since excludes old history, start following after the
		// newest pre-existing run instead of replaying it on the first
		// poll.
		latest, err := st.LatestRun(ctx, jobID)
		if err != nil {
			break
		}
		preexisting := !latest.StartedAt.After(started)
		if preexisting && latest.ID > lastEmitted {
			lastEmitted = latest.ID
		}
	case openID > 0:
		// A run is executing right now. Show the newest completed run
		// first, as documented, unless it closed after the open run
		// started (an overlap skip) and would replay from the cursor.
		latest, err := st.LatestRun(ctx, jobID)
		if err == nil && latest.ID < openID {
			if err := emitRunWithHeader(ctx, st, latest, opts.Lines, w); err != nil {
				return err
			}
		}
		// Park the cursor so the open run streams the moment it closes.
		lastEmitted = openID - 1
	default:
		latest, err := st.LatestRun(ctx, jobID)
		if err != nil {
			break
		}
		if err := emitRunWithHeader(ctx, st, latest, opts.Lines, w); err != nil {
			return err
		}
		lastEmitted = latest.ID
	}

	if !opts.Follow {
		return nil
	}

	t := time.NewTimer(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		runs, err := st.ListRunsAfter(ctx, jobID, lastEmitted)
		if err != nil {
			t.Reset(pollInterval)
			continue
		}
		for _, run := range runs {
			if run.Status == repeat.RunRunning {
				// Still open: stop here rather than skip, so the
				// cursor stays behind it until it closes.
				break
			}
			if err := emitRunWithHeader(ctx, st, run, opts.Lines, w); err != nil {
				return err
			}
			lastEmitted = run.ID
		}
		// Poll fast only while runs are appearing.
		next := idlePollInterval
		if len(runs) > 0 {
			next = pollInterval
		}
		t.Reset(next)
	}
}

func emitRunWithHeader(ctx context.Context, st *store.Store, run repeat.Run, lines int, w io.Writer) error {
	header := fmt.Sprintf("==> run #%d %s exit=%d finished=%s\n",
		run.ID, run.Status, run.ExitCode, fmtLocal(run.FinishedAt))
	if _, err := io.WriteString(w, header); err != nil {
		return err
	}
	return emitRunOutput(ctx, st, run.ID, lines, w)
}

func emitRunOutput(ctx context.Context, st *store.Store, runID int64, lines int, w io.Writer) error {
	out, err := st.RunLog(ctx, runID)
	if err != nil {
		return err
	}
	if lines > 0 {
		out = tailLines(out, lines)
	}
	_, err = w.Write(out)
	return err
}

// tailLines returns at most n trailing newline-delimited lines of buf.
func tailLines(buf []byte, n int) []byte {
	if n <= 0 || len(buf) == 0 {
		return buf
	}
	count := 0
	i := len(buf)
	if buf[i-1] == '\n' {
		i--
	}
	for ; i > 0; i-- {
		if buf[i-1] == '\n' {
			count++
			if count == n {
				return buf[i:]
			}
		}
	}
	return buf
}
