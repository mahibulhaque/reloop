// Package daemon contains daemon-lifecycle helpers.
//
// It provides:
//   - Per-user DataDir resolution.
//   - The flock-based single-instance lock.
//   - Supervisor unit install helpers.
package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DataDir returns the per-user directory for reloop state.
//
//   - macOS: ~/Library/Application Support/reloop
//   - Linux/other: $XDG_DATA_HOME/reloop, falling back to ~/.local/share/reloop
//
// It does not create the directory.
func DataDir() (string, error) {
	if runtime.GOOS == "darwin" {
		home, err := homeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "reloop"), nil
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "reloop"), nil
	}
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "reloop"), nil
}

// homeDir wraps os.UserHomeDir with the package's error text.
func homeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return home, nil
}

func lockPath(dataDir string) string { return filepath.Join(dataDir, "reloop.lock") }
func logPath(dataDir string) string  { return filepath.Join(dataDir, "reloop.log") }

// AcquireRunLock takes the daemon-lifetime exclusive lock.
//
// Behavior:
//   - It creates $dataDir/reloop.lock if needed.
//   - It writes pid and start time into the file.
//   - The release closure unlocks and closes the file.
//   - The OS releases the flock if the process exits.
//
// It returns (nil, nil) if another live daemon holds the lock.
// Callers should then use ProbeRunLock and exit with a conflict.
func AcquireRunLock(dataDir string) (release func(), err error) {
	// 0700 matches the store: this directory holds env snapshots.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir data dir: %w", err)
	}
	f, err := os.OpenFile(lockPath(dataDir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := flockExclusiveRetry(f); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, fmt.Errorf("flock: %w", err)
	}
	unlock := func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	// Fixed-width fields keep the file the same length across
	// restarts, so one write replaces the old content exactly and a
	// racing reader can never see a stale tail.
	body := fmt.Sprintf("%07d\n%019d\n", os.Getpid(), time.Now().UnixNano())
	if _, err := f.WriteAt([]byte(body), 0); err != nil {
		unlock()
		return nil, fmt.Errorf("write lock: %w", err)
	}
	return unlock, nil
}

// flockExclusiveRetry takes the exclusive lock, retrying for about a
// second. CLI probes hold a shared lock for a moment, and startup
// must not mistake overlapping probes for a running daemon.
func flockExclusiveRetry(f *os.File) error {
	var err error
	for range 25 {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		time.Sleep(40 * time.Millisecond)
	}
	return err
}

// ProbeRunLock reports whether a daemon holds the lock.
//
// If a daemon is running, it returns the recorded pid and start time.
// The probe takes a shared lock, which the daemon's exclusive lock
// blocks. Shared locks do not conflict with each other, so concurrent
// probes cannot mistake one another for a running daemon.
func ProbeRunLock(dataDir string) (pid int, startedAt time.Time, running bool, err error) {
	f, err := os.Open(lockPath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return 0, time.Time{}, false, nil
	}
	if err != nil {
		return 0, time.Time{}, false, fmt.Errorf("open lock: %w", err)
	}
	defer f.Close()

	if lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); lockErr == nil {
		// No exclusive holder, so no daemon. Release.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return 0, time.Time{}, false, nil
	}

	pid, startedAt, err = readLockContents(f)
	if err != nil {
		return 0, time.Time{}, true, err
	}
	return pid, startedAt, true, nil
}

func readLockContents(f *os.File) (int, time.Time, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, time.Time{}, err
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return 0, time.Time{}, err
	}
	pidText, startedText, _ := strings.Cut(strings.TrimSpace(string(buf)), "\n")
	if pidText == "" {
		return 0, time.Time{}, nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(pidText))
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("parse pid: %w", err)
	}
	var startedAt time.Time
	startedText = strings.TrimSpace(startedText)
	if startedText != "" {
		ns, err := strconv.ParseInt(startedText, 10, 64)
		if err != nil {
			return 0, time.Time{}, fmt.Errorf("parse start time: %w", err)
		}
		if ns > 0 {
			startedAt = time.Unix(0, ns).UTC()
		}
	}
	return pid, startedAt, nil
}

// SignalDaemon sends sig to the running daemon, if any. Returns
// (false, nil) when no daemon is running, including when it exits
// between the probe and the kill.
func SignalDaemon(dataDir string, sig syscall.Signal) (sent bool, err error) {
	pid, _, running, err := ProbeRunLock(dataDir)
	if err != nil {
		return false, err
	}
	if !running || pid <= 0 {
		return false, nil
	}
	if err := syscall.Kill(pid, sig); err != nil {
		if isProcessGone(err) {
			return false, nil
		}
		return false, fmt.Errorf("signal %d to %d: %w", sig, pid, err)
	}
	return true, nil
}

// isProcessGone reports that a signal target no longer exists.
//
// Raw syscall.Kill surfaces ESRCH and os.ErrProcessDone never
// matches it (Errno.Is only maps permission/exist/unsupported
// errors). Both checks are needed or a daemon exiting mid-stop
// looks like a failure.
func isProcessGone(err error) bool {
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone)
}

// StopDaemon asks the daemon at dataDir to exit.
//
// Behavior:
//   - It returns (false, nil) when no daemon is running.
//   - It sends SIGTERM first.
//   - It polls the lock until timeout.
//   - It escalates to SIGKILL if the same daemon still holds the lock.
//
// The graceful result is true when SIGTERM was enough.
func StopDaemon(dataDir string, timeout time.Duration) (running, graceful bool, err error) {
	pid, _, running, err := ProbeRunLock(dataDir)
	if err != nil {
		// The probe can report running=true with an unreadable lock.
		// Pass both through.
		return running, false, err
	}
	if !running {
		return false, false, nil
	}
	if pid <= 0 {
		// The daemon holds the lock but has not written its pid yet.
		// Signaling pid 0 would hit our own process group.
		return true, false, fmt.Errorf("daemon is starting, no pid recorded yet")
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !isProcessGone(err) {
		return true, false, fmt.Errorf("signal SIGTERM to %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, _, alive, err := ProbeRunLock(dataDir)
		if err != nil {
			return true, false, fmt.Errorf("probe daemon: %w", err)
		}
		if !alive {
			return true, true, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Re-probe before escalating. A new daemon may have taken the lock
	// during the wait, and killing the old pid could hit a recycled
	// process.
	newPid, _, alive, err := ProbeRunLock(dataDir)
	if err != nil {
		return true, false, fmt.Errorf("probe daemon: %w", err)
	}
	if !alive {
		return true, true, nil
	}
	if newPid != pid {
		return true, false, fmt.Errorf("daemon pid %d exited but a new daemon (pid %d) started", pid, newPid)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !isProcessGone(err) {
		return true, false, fmt.Errorf("signal SIGKILL to %d: %w", pid, err)
	}
	return true, false, nil
}
