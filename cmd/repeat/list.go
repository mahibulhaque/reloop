package main

import (
	"fmt"

	"github.com/mahibulhaque/repeat/internal/store"
	"github.com/spf13/cobra"
)

func newListCmd() *cobra.Command {
	var (
		jsonOut bool
		kind    string
		status  string
		limit   int
		offset  int
		all     bool
	)
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List jobs.",
		Long: `List jobs in the local store, most recent first. Filter by --kind
(cron|oneshot) or --status (enabled|disabled|done). The result is capped
at 100 rows by default; pass --all to disable the cap, or --limit/--offset
to page through. With --json the result is an array of job objects
suitable for jq/awk pipelines; truncation is reported on stderr.`,
		Example: `  repeat ls
  repeat ls --status enabled --json
  repeat ls --kind oneshot
  repeat ls --all
  repeat ls --limit 20 --offset 40`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validate all input before openService: a rejected
			// invocation must not create the data dir as a side effect.
			if limit < 0 {
				return usageErrf("--limit must be non-negative")
			}
			if offset < 0 {
				return usageErrf("--offset must be non-negative")
			}
			k, err := parseKindFlag(kind)
			if err != nil {
				return err
			}
			st, err := parseStatusFlag(status)
			if err != nil {
				return err
			}

			opts := store.ListOpts{Offset: offset, Kind: k, Status: st}
			switch {
			case all:
				opts.Limit = -1
			case limit > 0:
				opts.Limit = limit
			}

			return withService(cmd, func(s *service) error {
				warnIfDaemonDown(s)
				jobs, hasMore, err := s.List(cmd.Context(), opts)
				if err != nil {
					return err
				}
				if jsonOut {
					// store.ListJobs returns a non-nil slice: empty encodes as [], not null.
					if err := writeJSON(cmd.OutOrStdout(), jobs); err != nil {
						return err
					}
				} else {
					writeJobsTable(cmd.OutOrStdout(), jobs)
				}
				if hasMore && !globalFlags.quiet {
					fmt.Fprintf(stderr,
						"showing %d jobs, more available. Pass --all or use --limit/--offset to page.\n",
						len(jobs))
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit jobs as a JSON array")
	cmd.Flags().StringVar(&kind, "kind", "", "filter by kind: cron|oneshot")
	cmd.Flags().StringVar(&status, "status", "", "filter by status: enabled|disabled|done")
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows to return (0 = default 100)")
	cmd.Flags().IntVar(&offset, "offset", 0, "rows to skip before returning")
	cmd.Flags().BoolVar(&all, "all", false, "return every matching row (overrides --limit)")
	return cmd
}
