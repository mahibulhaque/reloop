package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

func newDebugCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "debug",
		Short: "Inspection and debug helpers.",
		Long: `Subcommands for inspecting repeat's internals. Not part of the stable
user-facing API. Tools and messages here can change without notice.`,
		Example: "  repeat debug db                # open a sqlite shell against repeat's database",
		// Without an explicit RunE, Cobra silently prints parent help and
		// exits 0 for unknown subcommands like `repeat debug bogus`. We want
		// that to surface as a usage error (exit 2) instead.
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return usageErrf("unknown subcommand %q for %q", args[0], cmd.CommandPath())
		},
	}
	cmd.AddCommand(newDebugDBCmd())
	return cmd
}

func newDebugDBCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "db",
		Short: "Open the SQLite shell against repeat's database.",
		Long: `Open sqlite3 on repeat's database file. A starter query prints the 10
most recently created jobs. After that it is a regular interactive
sqlite3 session. Requires sqlite3 on PATH.`,
		Example: "  repeat debug db",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sqlite, err := exec.LookPath("sqlite3")
			if err != nil {
				return fmt.Errorf("sqlite3 not on PATH: %w, install it to use this command", err)
			}
			dir, err := dataDir()
			if err != nil {
				return err
			}
			dbPath := filepath.Join(dir, "repeat.db")
			if _, err := os.Stat(dbPath); err != nil {
				return fmt.Errorf("database not found at %s: %w", dbPath, err)
			}
			// Convert milli-epoch values to human dates. NULLIF keeps
			// never-run jobs blank instead of 1970. Column mode and
			// headers keep the starter table legible.
			args := []string{
				"-cmd", ".headers on",
				"-cmd", ".mode column",
				"-cmd", "SELECT id, name, kind, status, last_status, " +
					"datetime(NULLIF(last_run_at, 0)/1000, 'unixepoch', 'localtime') AS last_run, " +
					"datetime(created_at/1000, 'unixepoch', 'localtime') AS created " +
					"FROM jobs ORDER BY created_at DESC LIMIT 10;",
				dbPath,
			}
			ex := exec.Command(sqlite, args...)
			ex.Stdin = os.Stdin
			ex.Stdout = cmd.OutOrStdout()
			ex.Stderr = stderr
			return ex.Run()
		},
	}
}
