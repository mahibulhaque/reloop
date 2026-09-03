package main

import "github.com/spf13/cobra"

func newShowCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:     "show ID|NAME",
		Aliases: []string{"get"},
		Short:   "Show one job's details.",
		Long: `Show full detail for the job identified by its 5-char ID or its
exact name. With --json, emit the job as a single JSON object suitable
for programmatic use.`,
		Example: `  reloop show 7K3px
  reloop show backup --json | jq .cron`,
		Args: jobArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withService(cmd, func(s *service) error {
				warnIfDaemonDown(s)
				job, err := s.Resolve(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if jsonOut {
					return writeJSON(cmd.OutOrStdout(), job)
				}
				writeJobDetail(cmd.OutOrStdout(), job)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the job as a JSON object")
	return cmd
}
