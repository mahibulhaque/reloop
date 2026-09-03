package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/mahibulhaque/reloop/internal/reloop"
	"github.com/spf13/cobra"
)

// jobActionRunE is the shared body of rm/enable/disable: resolve the
// job, apply the verb, confirm on stdout.
func jobActionRunE(verb string, act func(*service, context.Context, reloop.Job) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		return withService(cmd, func(s *service) error {
			job, err := s.Resolve(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			warnIfDaemonDown(s)
			if err := act(s, cmd.Context(), job); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s job %s\n", verb, job.ID)
			return nil
		})
	}
}

// jobArg validates the ID|NAME positional before RunE runs, so a bad
// invocation never creates the data dir as a side effect.
var jobArg = cobra.MatchAll(cobra.ExactArgs(1), func(_ *cobra.Command, args []string) error {
	if args[0] == "" {
		return errors.New("empty job reference")
	}
	return nil
})

func newRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm ID|NAME",
		Aliases: []string{"delete"},
		Short:   "Delete a job and its run history.",
		Long:    "Delete the job identified by ID or by exact name. Run history rows cascade.",
		Example: "  reloop rm 7K3px\n  reloop rm backup\n",
		Args:    jobArg,
		RunE:    jobActionRunE("deleted", (*service).Delete),
	}
}

func newEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "enable ID|NAME",
		Short:   "Enable a previously disabled job.",
		Long:    "Mark the job as enabled so the scheduler resumes firing it on its schedule. No-op if already enabled. A one-shot that already ran or started stays put; re-enabling it is a conflict (exit 4), add a new job instead.",
		Example: "  reloop enable backup\n  reloop enable 7K3px",
		Args:    jobArg,
		RunE:    jobActionRunE("enabled", (*service).Enable),
	}
}

func newDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "disable ID|NAME",
		Short:   "Stop a job from firing without deleting it.",
		Long:    "Mark a job disabled so the scheduler stops firing it. The job stays in the store and can be re-enabled later with 'reloop enable'. No-op if already disabled.",
		Example: "  reloop disable backup\n  reloop disable 7K3px",
		Args:    jobArg,
		RunE:    jobActionRunE("disabled", (*service).Disable),
	}
}

// parseKindFlag validates --kind values. Empty input is allowed and
// returns the zero kind so callers can distinguish "no filter" from
// "bad filter".
func parseKindFlag(s string) (reloop.JobKind, error) {
	if s == "" {
		return "", nil
	}
	k := reloop.JobKind(s)
	if k != reloop.KindCron && k != reloop.KindOneshot {
		return "", usageErrf("--kind must be 'cron' or 'oneshot'")
	}
	return k, nil
}

// parseStatusFlag validates --status values. Empty input is allowed
// and returns the zero status so callers can distinguish "no filter"
// from "bad filter".
func parseStatusFlag(s string) (reloop.JobStatus, error) {
	if s == "" {
		return "", nil
	}
	st := reloop.JobStatus(s)
	switch st {
	case reloop.StatusEnabled, reloop.StatusDisabled, reloop.StatusDone:
		return st, nil
	}
	return "", usageErrf("--status must be 'enabled', 'disabled', or 'done'")
}
