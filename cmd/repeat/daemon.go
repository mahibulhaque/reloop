package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mahibulhaque/repeat/internal/daemon"
	"github.com/mahibulhaque/repeat/internal/repeat"
	"github.com/mahibulhaque/repeat/internal/scheduler"
	"github.com/mahibulhaque/repeat/internal/store"
	"github.com/spf13/cobra"
)

func newDaemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "Run the scheduler in the foreground until SIGTERM.",
		Long: `Run the scheduler in the foreground. 'repeat install' registers this
process with launchd or systemd. On systems without either (Alpine,
WSL, containers), run the daemon in a loop. The loop restarts the
daemon after a crash. 'repeat stop' ends the loop:

  mkdir -p "$HOME/.local/share/repeat"
  nohup sh -c 'until repeat daemon; do sleep 1; done' >> "$HOME/.local/share/repeat/repeat.log" 2>&1 &

The daemon holds a file lock on $DATA/repeat.lock. A second daemon on
the same data dir exits with code 4. The daemon sleeps until the
next deadline in the database. A SIGHUP from the CLI wakes the
daemon after every write. The daemon exits 0 on SIGTERM and the
supervisor leaves it stopped. The daemon exits 130 on SIGINT and
the supervisor restarts it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := dataDir()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return fmt.Errorf("mkdir data dir: %w", err)
			}

			// SIGHUP means Wake. The CLI sends it after every mutation.
			// The handler must exist before AcquireRunLock publishes
			// our pid: the default disposition is termination, so a
			// signal racing startup would kill the daemon. Early
			// signals wait in the buffer and cost one extra wake.
			hup := make(chan os.Signal, 4)
			signal.Notify(hup, syscall.SIGHUP)
			defer signal.Stop(hup)

			release, err := daemon.AcquireRunLock(dir)
			if err != nil {
				return err
			}
			if release == nil {
				pid, _, running, _ := daemon.ProbeRunLock(dir)
				if running && pid > 0 {
					return fmt.Errorf("%w: pid %d", repeat.ErrDaemonUp, pid)
				}
				return fmt.Errorf("%w: another process holds the startup lock", repeat.ErrDaemonUp)
			}
			defer release()

			st, err := store.Open(cmd.Context(), dir)
			if err != nil {
				return err
			}
			defer st.Close()

			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			s := scheduler.New(st, scheduler.Config{Logger: logger})

			go func() {
				for {
					select {
					case <-cmd.Context().Done():
						return
					case sig := <-hup:
						logger.Info("repeatd signal received", "signal", sig.String())
						s.Wake()
					}
				}
			}()

			logger.Info("repeatd started", "data_dir", dir, "pid", os.Getpid())
			s.Start(cmd.Context())
			logger.Info("repeatd stopped")
			return nil
		},
	}
}
