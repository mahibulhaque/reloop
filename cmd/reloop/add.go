package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mahibulhaque/reloop/internal/reloop"
	"github.com/spf13/cobra"
)

func newAddCmd() *cobra.Command {
	var (
		cronExpression string
		atExpression   string
		name           string
		jsonOut        bool
	)
	cmd := &cobra.Command{
		Use:     "add [--cron EXPR | --at TIME] [--name NAME] -- COMMAND [ARG...]",
		Aliases: []string{"create", "new"},
		Short:   "Create a new cron or one-shot job.",
		Long: `Create a new job. Exactly one of --cron or --at is required.

--name NAME    a short human label. Optional, defaults to the command.

The command MUST follow '--'. Two forms work and you can use whichever
fits your shell habits:

  -- "echo hello"        # single quoted string -> run via /bin/sh -c
  -- sh -c "echo hello"  # explicit shell invocation -> exec directly
  -- /bin/echo hello     # multi-word argv      -> exec directly

One positional after '--' runs as a shell line through sh -c. Two or
more positionals run directly as the program and its arguments. Use
the shell form for pipes, redirects, or paths with spaces.

Put every reloop flag (--cron, --at, --name, --json) BEFORE '--'. Anything
after '--' belongs to the job and is not parsed by reloop.

Absolute-path binaries (e.g. /usr/bin/python3) are checked for existence
at add time so a typo'd path doesn't silently produce a job that fails
on every fire. Relative paths and bare names aren't checked because
they resolve against the daemon's CWD/PATH at fire time.

CRON SYNTAX (--cron)

A 5-field crontab expression or one of the @descriptor shortcuts.

  5-field layout:  MINUTE HOUR DAY-OF-MONTH MONTH DAY-OF-WEEK

    MINUTE         0-59
    HOUR           0-23
    DAY-OF-MONTH   1-31
    MONTH          1-12 or jan,feb,mar,apr,may,jun,jul,aug,sep,oct,nov,dec
    DAY-OF-WEEK    0-6 (0 = Sunday) or sun,mon,tue,wed,thu,fri,sat

  Each field accepts:
    *              every value
    N              the value N
    N-M            range N through M (inclusive)
    A,B,C          a list of values
    */N            every N units (step), starting from the minimum
    A-B/N          step over a range

  Examples:
    "0 9 * * 1-5"        09:00 on weekdays
    "*/15 * * * *"       every 15 minutes
    "0 0 1 jan,jul *"    midnight on Jan 1 and Jul 1
    "30 2 * * sun"       02:30 every Sunday

  Descriptor shortcuts:
    @yearly  / @annually    midnight on Jan 1   (0 0 1 1 *)
    @monthly                midnight on the 1st (0 0 1 * *)
    @weekly                 midnight on Sunday  (0 0 * * 0)
    @daily   / @midnight    midnight            (0 0 * * *)
    @hourly                 top of each hour    (0 * * * *)

  Interval shortcut:
    @every DURATION         fire every DURATION (Go-style: 30s, 5m, 2h, 24h)

  NOT supported:
    @reboot      reloop's daemon does not see reboot events directly.
                 Use 'reloop install' to register a launchd/systemd unit
                 that starts the daemon at boot, then schedule normally.
    seconds      the parser works per minute. Use @every 30s for
                 sub-minute intervals.

ONE-SHOT TIME (--at)

A wall-clock time in the future. All forms below are accepted.

  RFC3339              "2026-05-12T15:30:00-07:00"
                       The machine form with an explicit timezone.

  Relative offset      "+30s"      seconds
                       "+30m"      minutes
                       "+2h"       hours
                       "+3d"       days (whole days only, not composable)
                       "+1h30m"    any Go duration (h/m/s/ms/us/ns/composed)

  Today at TIME        "today 17:00"        24-hour clock
                       "today 5:30pm"       12-hour with am/pm
                       "today 9am"          minutes default to :00
                       The time must still be in the future.

  Tomorrow at TIME     "tomorrow 9am"
                       "tomorrow 23:59"
                       "tomorrow 12am"      midnight
                       "tomorrow 12pm"      noon

The time must be in the future. A past time is rejected with exit
code 5. The latest accepted time is 9999-12-31T09:59:59Z, so the
year has four digits in any timezone.`,
		Example: `  reloop add --cron "@hourly" --name backup -- /usr/local/bin/backup.sh
  reloop add --cron "0 9 * * 1-5" -- 'say "stand up"'
  reloop add --cron "@every 30s" -- /bin/echo tick
  reloop add --at "tomorrow 9am" --name morning -- 'say "good morning"'
  reloop add --at "+30m" -- "echo reminder | mail me@example.com"
  reloop add --at "2026-12-31T23:59:00-08:00" --name year-end -- ./fireworks`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validate all input before openService: a rejected
			// invocation must not create the data dir as a side effect.
			if (cronExpression == "") == (atExpression == "") {
				return usageErrf("provide exactly one of --cron or --at")
			}
			if cmd.ArgsLenAtDash() < 0 {
				return usageErrf("place the command after '--', for example `reloop add --cron '@hourly' -- /bin/echo hi`")
			}
			if strings.TrimSpace(args[0]) == "" {
				return usageErrf("empty command after '--'")
			}
			if err := checkAbsExecutable(args); err != nil {
				return err
			}

			spec := reloop.JobSpec{Command: wrapCommand(args), Name: name}
			if spec.Name == "" {
				// A derived name must fit the store's cap. Only an
				// explicit --name is rejected for length.
				spec.Name = truncate(strings.Join(args, " "), reloop.MaxNameRunes)
			}
			// Snapshot the user's environment so the daemon's minimal
			// launchd/systemd PATH doesn't break commands that "just
			// work" in the shell. Same model as at(1).
			spec.Env = os.Environ()
			// One clock reading for parsing --at and validating it, so
			// a fire time can never expire between the two.
			now := time.Now()
			if cronExpression != "" {
				spec.Cron = cronExpression
			} else {
				fireAt, err := reloop.ParseAt(atExpression, now)
				if err != nil {
					return err
				}
				spec.FireAt = fireAt
			}

			return withService(cmd, func(s *service) error {
				warnIfDaemonDown(s)
				job, err := s.Add(cmd.Context(), spec, now)
				if err != nil {
					return err
				}
				if jsonOut {
					return writeJSON(cmd.OutOrStdout(), job)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "added job %s (%s, %s)\n",
					job.ID, job.Kind, scheduleSummary(job))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&cronExpression, "cron", "", "cron expression (mutually exclusive with --at)")
	cmd.Flags().StringVar(&atExpression, "at", "", "fire time for a one-shot job; wall-clock (mutually exclusive with --cron)")
	cmd.Flags().StringVar(&name, "name", "", "human label (defaults to the command)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the created job as JSON on stdout")
	return cmd
}

// wrapCommand turns positional argv into the scheduler command.
//
// Rules:
//   - One positional is treated as a shell line.
//   - Two or more positionals are treated as explicit argv.
func wrapCommand(args []string) []string {
	if len(args) == 1 {
		return []string{"/bin/sh", "-c", args[0]}
	}
	return args
}

// checkAbsExecutable rejects an add whose command is an absolute path
// that doesn't exist or isn't executable. Relative paths and bare names
// are deliberately not checked: they resolve against the daemon's CWD
// and snapshotted PATH at fire time, neither of which the CLI can
// observe reliably.
func checkAbsExecutable(args []string) error {
	bin := absExecutableTarget(args)
	if bin == "" {
		return nil
	}
	info, err := os.Stat(bin)
	if errors.Is(err, fs.ErrNotExist) {
		return usageErrf("command not found: %s, use a path that exists or a bare name on PATH", bin)
	}
	if err != nil {
		return usageErrf("command %s: %v", bin, err)
	}
	if info.IsDir() {
		return usageErrf("command %s is a directory, not an executable", bin)
	}
	if info.Mode()&0o111 == 0 {
		return usageErrf("command %s is not executable, run chmod +x first", bin)
	}
	return nil
}

// absExecutableTarget returns the absolute path that will be exec'd,
// or "" if the command can't be validated from the CLI side. The
// shell-form (one positional) is opaque once it contains any shell
// metacharacter, so only a bare absolute path qualifies.
func absExecutableTarget(args []string) string {
	if len(args) >= 2 {
		if filepath.IsAbs(args[0]) {
			return args[0]
		}
		return ""
	}
	s := args[0]
	if !filepath.IsAbs(s) {
		return ""
	}
	if strings.ContainsAny(s, " \t\n|&;<>()$`\\\"'*?[]{}~#=!") {
		return ""
	}
	return s
}
