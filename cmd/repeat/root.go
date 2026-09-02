package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/mahibulhaque/repeat/internal/daemon"
	"github.com/mahibulhaque/repeat/internal/repeat"
	"github.com/mahibulhaque/repeat/internal/store"
	"github.com/spf13/cobra"
)

// Global flags shared by every subcommand. Resolved in PersistentPreRunE.
type rootFlags struct {
	dataDir string
	quiet   bool
}

var globalFlags rootFlags

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:               "repeat",
		CompletionOptions: cobra.CompletionOptions{HiddenDefaultCmd: true},
		Short:             "Cron and one-shot job scheduler that runs in-process.",
		Long: `repeat schedules cron-style recurring jobs and one-shot jobs at a wall-clock
time and executes them inside its own daemon. It does not use the system
cron or at daemons, so it behaves identically on macOS and Linux.

State lives in a single SQLite file under the platform's data directory
(~/Library/Application Support/repeat on macOS, $XDG_DATA_HOME/repeat on Linux).
Captured output for the last 100 runs per job is retained for 100 days.

Run 'repeat install' once to register the daemon as a launchd LaunchAgent
or systemd --user unit. The supervisor then starts the daemon at login
and restarts the daemon after a crash. 'repeat stop' stops the daemon
until the next login or the next 'repeat install'.

Exit codes: 0=ok 1=err 2=usage 3=not-found 4=conflict 5=precondition.
Output: pass --json to ls, show, status, prune, or add for stable,
machine-readable output on stdout. Errors and warnings go to stderr.`,
		Example: `  repeat install
  repeat add --cron '@hourly' --name backup -- ./bk.sh
  repeat ls --json`,
		Args:          rootArgs,
		RunE:          rootHelp,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&globalFlags.dataDir, "data-dir", "",
		"override the data directory (defaults to the platform standard)")
	root.PersistentFlags().BoolVar(&globalFlags.quiet, "quiet", false,
		"suppress the daemon-down warning written to stderr")

	root.AddCommand(
		newAddCmd(),
		newListCmd(),
		newPruneCmd(),
		newEnableCmd(),
		newDisableCmd(),
		newRemoveCmd(),
		newDaemonCmd(),
		newStatusCmd(),
		newDebugCmd(),
	)
	tagUsageErrors(root)
	return root
}

func rootArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())
}

func rootHelp(cmd *cobra.Command, _ []string) error {
	return cmd.Help()
}

// tagUsageErrors wires the command tree so flag-parse and positional-
// arg violations propagate as errUsage-wrapped errors. Without this,
// cobra surfaces those errors with no sentinel and the exit-code
// mapper falls through to 1 (unexpected), but per contract they are
// usage errors (exit 2).
func tagUsageErrors(root *cobra.Command) {
	// Cobra inherits FlagErrorFunc from the root, so one registration
	// covers every command.
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		if cmd.HasSubCommands() && !cmd.HasParent() {
			args := cmd.Flags().Args()
			if len(args) > 0 && !hasSubcommand(cmd, args[0]) {
				return fmt.Errorf("%w: unknown command %q for %q", errUsage, args[0], cmd.CommandPath())
			}
		}
		return fmt.Errorf("%w: %v", errUsage, err)
	})
	wrapArgsErrors(root)
}

// wrapArgsErrors wraps each command's Args validator with errUsage.
func wrapArgsErrors(c *cobra.Command) {
	if c.Args != nil {
		inner := c.Args
		c.Args = func(cmd *cobra.Command, args []string) error {
			if err := inner(cmd, args); err != nil {
				return fmt.Errorf("%w: %v", errUsage, err)
			}
			return nil
		}
	}
	for _, child := range c.Commands() {
		wrapArgsErrors(child)
	}
}

func hasSubcommand(cmd *cobra.Command, name string) bool {
	for _, child := range cmd.Commands() {
		if child.Name() == name || child.HasAlias(name) {
			return true
		}
	}
	return false
}

// dataDir returns the --data-dir flag made absolute, or the platform
// default. Absolute because the flag can land in a launchd or systemd
// unit and the daemon runs from a different working directory.
func dataDir() (string, error) {
	if globalFlags.dataDir != "" {
		return filepath.Abs(globalFlags.dataDir)
	}
	return daemon.DataDir()
}

// openService opens the store and wraps it in a service.
//
// ctx scopes schema setup.
// Commands should defer the returned cleanup.
func openService(ctx context.Context) (*service, func(), error) {
	dir, err := dataDir()
	if err != nil {
		return nil, func() {}, err
	}
	st, err := store.Open(ctx, dir)
	if err != nil {
		return nil, func() {}, err
	}
	return newService(st), func() { _ = st.Close() }, nil
}

// withService opens the store, runs fn, and closes it. Commands must
// validate their input before calling this: opening the store creates
// the data dir as a side effect.
func withService(cmd *cobra.Command, fn func(*service) error) error {
	s, cleanup, err := openService(cmd.Context())
	if err != nil {
		return err
	}
	defer cleanup()
	return fn(s)
}

// warnIfDaemonDown writes a single stderr line when no daemon is
// running. The check is done against the OS-level flock at
// $DATA/repeat.lock, so a crashed daemon is instantly visible because
// the kernel released the lock.
//
// A dead daemon with a supervisor installed gets its own message.
// That state usually means the unit points at a binary an upgrade
// removed, which only the user can fix.
func warnIfDaemonDown(s *service) {
	if globalFlags.quiet {
		return
	}
	state := s.DaemonState()
	if state.Running {
		return
	}
	if state.Supervised {
		fmt.Fprintln(stderr, "warning: repeatd is not running even though a supervisor is installed. Run 'repeat install' to refresh it, or check repeat.log in the data dir.")
		return
	}
	fmt.Fprintln(stderr, "warning: repeatd is not running. Jobs will not fire until you run 'repeat install', or start it directly with 'repeat daemon &' on systems without launchd/systemd.")
}

// exitCode maps a returned error to a stable shell exit code. Callers
// (main) os.Exit on the result.
func exitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		return 2
	case errors.Is(err, repeat.ErrNotFound):
		return 3
	case errors.Is(err, repeat.ErrConflict), errors.Is(err, repeat.ErrDaemonUp):
		return 4
	case errors.Is(err, repeat.ErrInvalidSpec),
		errors.Is(err, repeat.ErrInvalidCron),
		errors.Is(err, repeat.ErrInvalidTime),
		errors.Is(err, repeat.ErrUnsupportedOS):
		return 5
	default:
		return 1
	}
}

// errUsage is sentineled so we can map invalid-flag/arg situations to
// exit code 2 without depending on Cobra's error strings.
var errUsage = errors.New("usage error")

func usageErrf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{errUsage}, args...)...)
}

// stderr is a thin wrapper for test injection.
// Real invocations write to os.Stderr.
var stderr io.Writer = os.Stderr
