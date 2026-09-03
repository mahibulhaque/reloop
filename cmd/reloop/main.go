package main

import (
	"context"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/charmbracelet/fang"
)

// version and commit are overridden at release build time via -ldflags.
// When version is empty, Fang falls back to Go build-info metadata, which
// keeps `go install ...@version` builds self-describing.
var (
	version string
	commit  string
)

func main() { os.Exit(run()) }

// run is main's body, split out so the script tests can register the
// real entrypoint.
//
// SIGINT surfaces as the conventional exit 130, which NotifyContext
// cannot report. SIGTERM keeps the plain error path: the supervised
// daemon must exit 0 after draining, or Restart=on-failure would
// respawn it after every `reloop stop`.
func run() int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var interrupted atomic.Bool
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		for sig := range sigCh {
			if sig == syscall.SIGINT {
				interrupted.Store(true)
			}
			cancel()
		}
	}()

	// AnsiColorScheme uses the user's terminal palette.
	//
	// That keeps help and error text legible on dark and light themes.
	// Fang's default colors can look dim on dark mode.

	err := fang.Execute(ctx, newRoot(), fangOptions()...)
	if interrupted.Load() {
		return 130
	}
	if err != nil {
		return exitCode(err)
	}
	return 0
}

func fangOptions() []fang.Option {
	opts := []fang.Option{fang.WithColorSchemeFunc(fang.AnsiColorScheme)}
	if version != "" {
		opts = append(opts, fang.WithVersion(version))
	}
	if commit != "" {
		opts = append(opts, fang.WithCommit(commit))
	}
	return opts
}
