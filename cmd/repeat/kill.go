package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/mahibulhaque/repeat/internal/daemon"
	"github.com/spf13/cobra"
)

func newKillCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "kill",
		Short: "Purge every trace of repeat from this machine.",
		Long: `Stop the running daemon, remove the launchd/systemd supervisor unit,
delete the data directory (database, lock files, log) and finally remove
the repeat binary itself.

Destructive and irreversible. Defaults to a dry run that prints the
plan without touching anything. Pass --force (-f) to actually do it.`,
		Example: "  repeat kill            # dry-run, show the plan\n  repeat kill --force    # actually wipe everything",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return kill(cmd, force)
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "actually perform the destructive operations (default is dry-run)")
	return cmd
}

// kill is the destructive cleanup driver.
//
// perform=false means dry-run.
// Every "would ..." line matches one real action.
func kill(cmd *cobra.Command, perform bool) error {
	out := cmd.OutOrStdout()
	dir, err := dataDir()
	if err != nil {
		return err
	}
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}

	if err := stopDaemon(out, dir, perform); err != nil {
		return err
	}

	if daemon.IsSupervised(dir) {
		err := step(out, perform,
			"removing supervisor unit",
			"would remove supervisor unit",
			func() error {
				_, err := daemon.Uninstall()
				return err
			})
		if err != nil {
			return err
		}
	}

	if _, err := os.Stat(dir); err == nil {
		err := step(out, perform,
			fmt.Sprintf("removing data dir %s", dir),
			fmt.Sprintf("would remove data dir %s", dir),
			func() error {
				if err := os.RemoveAll(dir); err != nil {
					return fmt.Errorf("remove data dir: %w", err)
				}
				return nil
			})
		if err != nil {
			return err
		}
	}

	// Removing the running binary is fine on Unix. The kernel keeps
	// the inode alive until this process exits, and the file is gone
	// the next time something looks for it.
	err = step(out, perform,
		fmt.Sprintf("removing binary %s", bin),
		fmt.Sprintf("would remove binary %s", bin),
		func() error {
			if err := os.Remove(bin); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove binary: %w", err)
			}
			return nil
		})
	if err != nil {
		return err
	}

	if perform {
		fmt.Fprintln(out, "done")
	} else {
		fmt.Fprintln(out, "(dry run, nothing was modified)")
		fmt.Fprintln(out, "Re-run with --force (-f) to actually perform the operations above.")
	}
	return nil
}

// step performs one destructive action after narrating it, or prints
// the planned mirror of the same message on a dry run.
func step(out io.Writer, perform bool, doing, planned string, fn func() error) error {
	if !perform {
		fmt.Fprintln(out, planned)
		return nil
	}
	fmt.Fprintln(out, doing)
	return fn()
}

// stopDaemon does what 'repeat stop' does.
//
// It stays silent when no daemon is running.
// kill does not need that line.
//
// A stop failure aborts the purge: deleting the database and binary
// under a live daemon would leave it running against removed state.
func stopDaemon(out io.Writer, dir string, perform bool) error {
	pid, _, running, _ := daemon.ProbeRunLock(dir)
	if !running {
		return nil
	}
	if !perform {
		fmt.Fprintf(out, "would stop daemon (pid %d)\n", pid)
		return nil
	}
	fmt.Fprintf(out, "stopping daemon (pid %d)\n", pid)
	if _, _, err := daemon.StopDaemon(dir, stopTimeout); err != nil {
		return fmt.Errorf("stop daemon: %w, aborting before deleting state", err)
	}
	return nil
}
