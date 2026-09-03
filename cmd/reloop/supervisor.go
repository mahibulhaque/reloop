package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/mahibulhaque/reloop/internal/daemon"
	"github.com/spf13/cobra"
)

func newInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Register the daemon with launchd (macOS) or systemd --user (Linux).",
		Long: `Write the platform's supervisor unit and bootstrap it so the reloop daemon
starts on login and respawns on crash. Idempotent and self-healing:
re-running rewrites and reloads the unit when the binary path, data
dir, or unit format changed (e.g. after a brew upgrade moved the
binary), and is a no-op otherwise. A --data-dir flag given here is
baked into the unit so the supervised daemon serves the same data
dir as the CLI.`,
		Example: "  reloop install\n  reloop install   # re-run after upgrading reloop to refresh the unit",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := dataDir()
			if err != nil {
				return err
			}
			bin, err := os.Executable()
			if err != nil {
				return fmt.Errorf("install: locate self: %w", err)
			}
			// Bake the absolutized dir, not the raw flag. The daemon
			// runs from its own working directory, so a relative flag
			// would resolve to a different data dir than the CLI used.
			var daemonArgs []string
			if globalFlags.dataDir != "" {
				daemonArgs = []string{"--data-dir", dir}
			}
			installed, err := daemon.Install(bin, dir, daemonArgs...)
			if errors.Is(err, exec.ErrNotFound) {
				// No launchctl/systemctl on this box. A raw exec error
				// would leave the user without the working path.
				return fmt.Errorf("%w: this system has no launchd/systemd, run the daemon directly (see 'reloop daemon --help')", err)
			}
			if err != nil {
				return err
			}
			if installed {
				fmt.Fprintln(cmd.OutOrStdout(), "installed and started supervisor")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "supervisor unit up to date, no changes")
			}
			return nil
		},
	}
}

func newUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the launchd/systemd supervisor unit.",
		Long: `Unload and delete the supervisor unit written by 'reloop install'. Leaves
the database and binary intact; for a full purge use 'reloop seppuku'.
Idempotent: a second 'reloop uninstall' reports nothing to remove.`,
		Example: "  reloop uninstall",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			removed, err := daemon.Uninstall()
			if err != nil {
				return err
			}
			if removed {
				fmt.Fprintln(cmd.OutOrStdout(), "supervisor removed")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "no supervisor installed")
			}
			return nil
		},
	}
}

// stopTimeout bounds a graceful daemon stop, shared by stop and
// seppuku. Longer than the daemon's worst-case drain (5s job grace +
// 5s record timeout): a SIGKILL exit reads as a crash to the
// supervisor, which would respawn the daemon we just stopped.
const stopTimeout = 15 * time.Second

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Signal the running daemon to exit.",
		Long: `Send SIGTERM to the running daemon and wait for it to release the
single-instance lock. Escalates to SIGKILL after 15 seconds. Returns
success either way (idempotent), printing a note when no daemon was
running to begin with.`,
		Example: "  reloop stop",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := dataDir()
			if err != nil {
				return err
			}
			running, graceful, err := daemon.StopDaemon(dir, stopTimeout)
			if err != nil {
				return fmt.Errorf("stop: %w", err)
			}
			switch {
			case !running:
				fmt.Fprintln(cmd.OutOrStdout(), "no daemon running")
			case graceful:
				fmt.Fprintln(cmd.OutOrStdout(), "daemon stopped")
			default:
				fmt.Fprintln(cmd.OutOrStdout(), "daemon stopped (forced)")
			}
			return nil
		},
	}
}
