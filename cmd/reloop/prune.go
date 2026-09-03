package main

import (
	"fmt"

	"github.com/mahibulhaque/reloop/internal/reloop"
	"github.com/mahibulhaque/reloop/internal/store"
	"github.com/spf13/cobra"
)

func newPruneCmd() *cobra.Command {
	var (
		kind    string
		status  string
		force   bool
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete jobs that won't fire (done one-shots and disabled jobs).",
		Long: `Remove jobs from the store that the scheduler will never fire again:
done one-shots and disabled jobs. Defaults to a dry run that prints
the candidates without touching the database. Pass --force (-f) to
actually delete.

Filters mirror 'reloop ls'. With --status given, only that status is
pruned (overrides the default disabled+done set). With --kind, the
result is further narrowed to that kind. With --json, the candidate
set (dry-run) or the deleted IDs (--force) are emitted as JSON.`,
		Example: `  reloop prune                       # dry-run, show candidates
  reloop prune --force               # actually delete the candidates
  reloop prune --status done -f      # only done one-shots
  reloop prune --kind cron -f        # only cron jobs (the disabled ones)
  reloop prune --json | jq '.[].id'  # scripted candidate list`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Validate all input before the store opens: a rejected
			// invocation must not create the data dir as a side effect.
			k, err := parseKindFlag(kind)
			if err != nil {
				return err
			}
			st, err := parseStatusFlag(status)
			if err != nil {
				return err
			}
			return withService(cmd, func(s *service) error {
				all, _, err := s.List(cmd.Context(), store.ListOpts{Limit: -1, Kind: k, Status: st})
				if err != nil {
					return err
				}

				targets := all
				if status == "" {
					// Default scope: anything the scheduler won't fire.
					targets = []reloop.Job{}
					for _, j := range all {
						if j.Status == reloop.StatusDisabled || j.Status == reloop.StatusDone {
							targets = append(targets, j)
						}
					}
				}

				out := cmd.OutOrStdout()
				if !force {
					if jsonOut {
						return writeJSON(out, targets)
					}
					if len(targets) == 0 {
						fmt.Fprintln(out, "nothing to prune")
						return nil
					}
					fmt.Fprintf(out, "would prune %d job(s):\n", len(targets))
					for _, j := range targets {
						fmt.Fprintf(out, "  %s  %-8s  %-8s  %s\n", j.ID, j.Kind, j.Status, sanitize(j.Name))
					}
					fmt.Fprintln(out, "(dry run, nothing was modified)")
					fmt.Fprintln(out, "Re-run with --force (-f) to actually delete.")
					return nil
				}

				for _, j := range targets {
					if err := s.st.DeleteJob(cmd.Context(), j.ID); err != nil {
						return fmt.Errorf("delete %s: %w", j.ID, err)
					}
				}
				if len(targets) > 0 {
					// One wake covers the whole batch.
					s.notify()
				}
				if jsonOut {
					return writeJSON(out, targets)
				}
				if len(targets) == 0 {
					fmt.Fprintln(out, "nothing to prune")
					return nil
				}
				fmt.Fprintf(out, "pruned %d job(s)\n", len(targets))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "filter by kind: cron|oneshot")
	cmd.Flags().StringVar(&status, "status", "", "filter by status: enabled|disabled|done (default: disabled+done)")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "actually delete (default is dry-run)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the affected jobs as a JSON array (candidates on dry-run, deleted jobs with --force)")
	return cmd
}
