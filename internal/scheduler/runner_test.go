// Tests for the exec runner in runner.go: PATH resolution, signal
// grace, exit codes, and start-failure capture.

package scheduler

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mahibulhaque/repeat/internal/repeat"
	"github.com/mahibulhaque/repeat/internal/store"
)

func TestExecRunnerResolvesBinaryViaSnapshotPATH(t *testing.T) {
	// When a job carries a snapshotted PATH that points somewhere the
	// daemon's own PATH does not, the runner must resolve the binary
	// against the snapshot, not the daemon's. Construct a temp dir
	// with one fake "echo" symlinked from /bin/echo, set PATH to that
	// dir in the job env, and give the process a PATH that lacks it.
	dir := t.TempDir()
	if err := os.Symlink("/bin/echo", filepath.Join(dir, "myecho")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	t.Setenv("PATH", "/usr/bin:/bin") // a daemon-like PATH that lacks dir

	sb := newCappedBuf(store.MaxOutputBytes)
	code, err := execRunner{}.Run(t.Context(), repeat.Job{
		Command: []string{"myecho", "from snapshot"},
		Env:     []string{"PATH=" + dir},
	}, sb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0; stdout=%q", code, string(sb.Bytes()))
	}
	if got := string(sb.Bytes()); got != "from snapshot\n" {
		t.Errorf("Run() stdout = %q, want %q", got, "from snapshot\n")
	}
}

func TestExecRunnerHonoursSIGTERMGrace(t *testing.T) {
	// Child traps SIGTERM and exits 7. With a 2s grace period the
	// runtime sends SIGTERM (not SIGKILL) on ctx cancel, so the trap
	// fires and the exit code is 7. With no grace,
	// exec.CommandContext would SIGKILL immediately and the trap
	// would never run.
	sb := newCappedBuf(store.MaxOutputBytes)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)

	runner := execRunner{gracePeriod: 2 * time.Second}
	go func() {
		sleep(150 * time.Millisecond)
		cancel(errTestCtxEnded)
	}()

	code, err := runner.Run(ctx, repeat.Job{
		// `sleep` is a child of sh.
		// It does not receive signals delivered to sh.
		// `& wait $!` makes sh's wait interruptible.
		// That lets the trap fire.
		Command: []string{"/bin/sh", "-c", "trap 'exit 7' TERM; sleep 30 & wait $!"},
	}, sb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7 (SIGTERM trap)", code)
	}
}

func TestExecRunnerCapturesNonZeroExit(t *testing.T) {
	sb := newCappedBuf(store.MaxOutputBytes)
	code, err := execRunner{}.Run(t.Context(), repeat.Job{Command: []string{"sh", "-c", "echo hi; exit 7"}}, sb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	if string(sb.Bytes()) != "hi\n" {
		t.Errorf("Run() stdout = %q, want %q", string(sb.Bytes()), "hi\n")
	}
}

func TestExecRunnerReportsStartError(t *testing.T) {
	_, err := execRunner{}.Run(t.Context(), repeat.Job{Command: []string{"/no/such/binary"}}, io.Discard)
	if err == nil {
		t.Errorf("Run(missing binary) = nil, want error")
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("Run(missing binary) = %v, want non-context error", err)
	}
}

// TestExecRunnerCapturesStartErrorInOutput checks that a job whose
// argv[0] does not exist gets the start error written to its run
// log. Without that, `repeat logs JOB` shows nothing and the user has
// no way to diagnose the failure.
func TestExecRunnerCapturesStartErrorInOutput(t *testing.T) {
	sb := newCappedBuf(store.MaxOutputBytes)
	_, err := execRunner{}.Run(t.Context(), repeat.Job{Command: []string{"echo hello"}}, sb)
	if err == nil {
		t.Errorf("Run(command with space in argv[0]) = nil, want error")
	}
	if !strings.Contains(string(sb.Bytes()), "failed to start") {
		t.Errorf("Run() stdout = %q, want substring %q", string(sb.Bytes()), "failed to start")
	}
}

func TestExecRunnerDoesNotFallBackToDaemonPATHWhenSnapshotPATHMisses(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")

	sb := newCappedBuf(store.MaxOutputBytes)
	code, err := execRunner{}.Run(t.Context(), repeat.Job{
		Command: []string{"sh", "-c", "echo should-not-run"},
		Env:     []string{"PATH=/definitely/not/a/real/repeat/path"},
	}, sb)
	if err == nil {
		t.Errorf("Run(snapshot PATH miss) = nil, want error; exit code=%d stdout=%q", code, string(sb.Bytes()))
	}
	if code != -1 {
		t.Errorf("exit code = %d, want -1 for start failure", code)
	}
	if !strings.Contains(string(sb.Bytes()), "failed to start") {
		t.Errorf("Run() stdout = %q, want substring %q", string(sb.Bytes()), "failed to start")
	}
}

func TestEnvPathAbsent(t *testing.T) {
	if got := envPath([]string{"HOME=/x", "TERM=dumb"}); got != "" {
		t.Errorf("envPath = %q, want empty when PATH is absent", got)
	}
}

func TestLookPathInEmptyPath(t *testing.T) {
	if _, err := lookPathIn("sh", ""); err == nil {
		t.Errorf("lookPathIn with an empty path: want error, got nil")
	}
}

func TestLookPathInEmptyEntryMeansDot(t *testing.T) {
	// A lone ":" is two empty entries, each read as the current dir.
	if _, err := lookPathIn("no-such-binary-zz", ":"); err == nil {
		t.Errorf("lookPathIn miss: want error, got nil")
	}
}
