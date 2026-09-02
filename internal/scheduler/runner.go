package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mahibulhaque/repeat/internal/repeat"
)

// runner executes a single job. Decoupling exec from the scheduler
// lets tests substitute a fake that records invocations without
// forking processes.
type runner interface {
	Run(ctx context.Context, job repeat.Job, out io.Writer) (exitCode int, err error)
}

// execRunner is the production [runner].
//
// Behavior:
//   - It shells out via os/exec.
//   - It streams merged stdout and stderr into out.
//   - gracePeriod > 0 sends SIGTERM before Go escalates to SIGKILL.
//   - gracePeriod == 0 uses Go's default immediate SIGKILL.
type execRunner struct {
	gracePeriod time.Duration
}

// Run executes job and writes merged stdout and stderr to out.
func (e execRunner) Run(ctx context.Context, job repeat.Job, out io.Writer) (int, error) {
	if len(job.Command) == 0 {
		return -1, errors.New("scheduler: empty command")
	}
	cmd := buildCmd(ctx, job)
	cmd.Stdout = out
	cmd.Stderr = out
	// A grace period opts the job into its own process group so
	// cancellation reaches grandchildren too, not just the direct
	// child. A job like `sh -c 'worker &'` otherwise leaks the worker
	// past shutdown. The post-run sweep below relies on this:
	// kill(-pid) is only safe once Setpgid isolates the job.
	ownProcessGroup := e.gracePeriod > 0
	if ownProcessGroup {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		// Send SIGTERM to the group on cancellation so children can run
		// cleanup. The negative pid addresses the whole group.
		//
		// WaitDelay is still required.
		// A child can trap SIGTERM and never exit.
		// In that case WaitDelay lets os/exec force-kill it.
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
		cmd.WaitDelay = e.gracePeriod
	}
	err := cmd.Run()
	if ownProcessGroup && ctx.Err() != nil && cmd.Process != nil {
		// Cancellation path: the direct child is gone (os/exec saw to
		// that), but trapped or backgrounded grandchildren may linger.
		// Sweep the whole group. ESRCH just means everyone already left.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if cmd.ProcessState != nil {
		// The child actually ran.
		//
		// ProcessState has the useful exit code.
		// A handled SIGTERM can still surface as a context error.
		return cmd.ProcessState.ExitCode(), nil
	}
	// The process never started.
	//
	// Persist the reason so `repeat logs` can show it.
	if _, writeErr := fmt.Fprintf(out, "repeat: failed to start: %v\n", err); writeErr != nil {
		return -1, errors.Join(err, writeErr)
	}
	return -1, err
}

// buildCmd wires the process for a job. A job with an env snapshot
// resolves argv[0] against the snapshot PATH from `repeat add` time,
// never against the daemon's, so a snapshot miss fails instead of
// falling back.
func buildCmd(ctx context.Context, job repeat.Job) *exec.Cmd {
	name := job.Command[0]
	if len(job.Env) == 0 {
		return exec.CommandContext(ctx, name, job.Command[1:]...)
	}
	resolved, err := lookPathIn(name, envPath(job.Env))
	if err != nil {
		cmd := exec.CommandContext(ctx, name, job.Command[1:]...)
		if !strings.ContainsRune(name, os.PathSeparator) {
			// Undo the daemon-PATH resolution CommandContext just did.
			cmd.Path = name
			cmd.Err = err
		}
		cmd.Env = job.Env
		return cmd
	}
	// Hand exec the resolved path directly and keep the job's own
	// argv, so the child still sees the name it was added with. Path
	// and Err are forced because a relative resolved name would send
	// CommandContext back to the daemon's PATH.
	cmd := exec.CommandContext(ctx, resolved)
	cmd.Path = resolved
	cmd.Err = nil
	cmd.Args = job.Command
	cmd.Env = job.Env
	return cmd
}

// envPath returns PATH from a `KEY=VALUE` slice.
//
// Behavior:
//   - It returns "" when PATH is absent.
//   - The first match wins.
//
// The first-match rule matches environments passed to exec.
func envPath(env []string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			return v
		}
	}
	return ""
}

// lookPathIn is exec.LookPath with an explicit search path.
//
// Needed because:
//   - The daemon's PATH can come from launchd or systemd.
//   - Jobs should use the PATH snapshotted at `repeat add` time.
func lookPathIn(name, path string) (string, error) {
	if strings.ContainsRune(name, os.PathSeparator) {
		// Already a path.
		// Let the OS resolve it at exec time.
		return name, nil
	}
	if path == "" {
		return "", exec.ErrNotFound
	}
	for dir := range strings.SplitSeq(path, string(os.PathListSeparator)) {
		if dir == "" {
			dir = "."
		}
		full := filepath.Join(dir, name)
		info, err := os.Stat(full)
		if err != nil || info.IsDir() {
			continue
		}
		executable := info.Mode()&0o111 != 0
		if executable {
			return full, nil
		}
	}
	return "", exec.ErrNotFound
}
