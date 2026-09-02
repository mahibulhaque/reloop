package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestMain re-execs the test binary as a fake daemon when the env
// var is set. The fake takes the run lock and parks, so stop and
// signal tests act on a real lock holder.
func TestMain(m *testing.M) {
	dir := os.Getenv("REPEAT_TEST_HOLD_LOCK")
	if dir == "" {
		os.Exit(m.Run())
	}
	holdLock(dir, os.Getenv("REPEAT_TEST_IGNORE_TERM") == "1")
	os.Exit(0)
}

// holdLock takes the run lock and waits for SIGTERM. With ignoreTerm
// it drops every SIGTERM so the caller has to escalate to SIGKILL.
func holdLock(dir string, ignoreTerm bool) {
	release, err := AcquireRunLock(dir)
	if err != nil || release == nil {
		os.Exit(1)
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	if ignoreTerm {
		for range ch {
		}
	}
	<-ch
	release()
}

// spawnLockHolder starts the fake daemon and waits until it holds
// the lock. Cleanup kills it if the test fails before stopping it.
func spawnLockHolder(t *testing.T, dir string, ignoreTerm bool) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "REPEAT_TEST_HOLD_LOCK="+dir)
	if ignoreTerm {
		cmd.Env = append(cmd.Env, "REPEAT_TEST_IGNORE_TERM=1")
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	if !waitLockState(dir, true) {
		t.Fatalf("lock holder never took the lock")
	}
}

// waitLockState polls until the run lock is held or free.
func waitLockState(dir string, held bool) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, _, running, _ := ProbeRunLock(dir)
		if running == held {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// lockBody formats pid and start time the way AcquireRunLock writes
// them.
func lockBody(pid int) string {
	return fmt.Sprintf("%07d\n%019d\n", pid, time.Now().UnixNano())
}

// holdLockInTest takes the exclusive flock in the test process and
// writes body into the lock file.
func holdLockInTest(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "repeat.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock: %v", err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatalf("write lock: %v", err)
	}
}

func TestStopDaemonNoDaemon(t *testing.T) {
	running, graceful, err := StopDaemon(t.TempDir(), time.Second)
	if err != nil {
		t.Fatalf("StopDaemon: %v", err)
	}
	if running || graceful {
		t.Errorf("StopDaemon = (%v, %v), want (false, false)", running, graceful)
	}
}

func TestStopDaemonGraceful(t *testing.T) {
	dir := t.TempDir()
	spawnLockHolder(t, dir, false)

	running, graceful, err := StopDaemon(dir, 10*time.Second)
	if err != nil {
		t.Fatalf("StopDaemon: %v", err)
	}
	if !running || !graceful {
		t.Errorf("StopDaemon = (%v, %v), want (true, true)", running, graceful)
	}
}

func TestStopDaemonEscalatesToKill(t *testing.T) {
	dir := t.TempDir()
	spawnLockHolder(t, dir, true)

	running, graceful, err := StopDaemon(dir, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("StopDaemon: %v", err)
	}
	if !running || graceful {
		t.Errorf("StopDaemon = (%v, %v), want (true, false)", running, graceful)
	}
	if !waitLockState(dir, false) {
		t.Errorf("holder survived SIGKILL")
	}
}

func TestStopDaemonNoPidRecordedYet(t *testing.T) {
	dir := t.TempDir()
	holdLockInTest(t, dir, "")

	running, _, err := StopDaemon(dir, time.Second)
	if !running {
		t.Errorf("StopDaemon running = false, want true")
	}
	if err == nil {
		t.Fatalf("StopDaemon with no pid: want error, got nil")
	}
}

func TestStopDaemonUnreadableLock(t *testing.T) {
	dir := t.TempDir()
	holdLockInTest(t, dir, "not-a-pid\n")

	running, _, err := StopDaemon(dir, time.Second)
	if !running {
		t.Errorf("StopDaemon running = false, want true")
	}
	if err == nil {
		t.Fatalf("StopDaemon with garbage lock: want error, got nil")
	}
}

func TestProbeRunLockBadStartTime(t *testing.T) {
	dir := t.TempDir()
	holdLockInTest(t, dir, fmt.Sprintf("%07d\nnot-nanos\n", os.Getpid()))

	_, _, running, err := ProbeRunLock(dir)
	if !running {
		t.Errorf("ProbeRunLock running = false, want true")
	}
	if err == nil {
		t.Fatalf("ProbeRunLock with bad start time: want error, got nil")
	}
}

func TestSignalDaemonSends(t *testing.T) {
	dir := t.TempDir()
	spawnLockHolder(t, dir, false)

	sent, err := SignalDaemon(dir, syscall.SIGTERM)
	if err != nil {
		t.Fatalf("SignalDaemon: %v", err)
	}
	if !sent {
		t.Errorf("SignalDaemon sent = false, want true")
	}
}

func TestSignalDaemonDeadPid(t *testing.T) {
	// A pid that just exited: the recorded daemon died between the
	// probe and the kill, which must read as "no daemon", not an error.
	child := exec.Command("true")
	if err := child.Run(); err != nil {
		t.Fatalf("run child: %v", err)
	}
	dir := t.TempDir()
	holdLockInTest(t, dir, lockBody(child.Process.Pid))

	sent, err := SignalDaemon(dir, syscall.SIGTERM)
	if err != nil {
		t.Fatalf("SignalDaemon: %v", err)
	}
	if sent {
		t.Errorf("SignalDaemon sent = true, want false for a dead pid")
	}
}

func TestDataDirDefault(t *testing.T) {
	t.Setenv("HOME", "/fake/home")
	t.Setenv("XDG_DATA_HOME", "")

	dir, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	want := filepath.Join("/fake/home", ".local", "share", "repeat")
	if runtime.GOOS == "darwin" {
		want = filepath.Join("/fake/home", "Library", "Application Support", "repeat")
	}
	if dir != want {
		t.Errorf("DataDir = %q, want %q", dir, want)
	}
}

func TestDataDirXDG(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("darwin ignores XDG_DATA_HOME")
	}
	t.Setenv("XDG_DATA_HOME", "/fake/xdg")

	dir, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if want := filepath.Join("/fake/xdg", "repeat"); dir != want {
		t.Errorf("DataDir = %q, want %q", dir, want)
	}
}

func TestDataDirNoHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_DATA_HOME", "")

	if _, err := DataDir(); err == nil {
		t.Errorf("DataDir with no HOME: want error, got nil")
	}
}

func TestAcquireRunLockMkdirFails(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if _, err := AcquireRunLock(file); err == nil {
		t.Errorf("AcquireRunLock over a file: want error, got nil")
	}
}

func TestProbeRunLockUnreadableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "repeat.lock"), nil, 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if _, _, _, err := ProbeRunLock(dir); err == nil {
		t.Errorf("ProbeRunLock in an unreadable dir: want error, got nil")
	}
}

func TestSignalDaemonUnreadableLock(t *testing.T) {
	dir := t.TempDir()
	holdLockInTest(t, dir, "not-a-pid\n")

	if _, err := SignalDaemon(dir, syscall.SIGHUP); err == nil {
		t.Errorf("SignalDaemon with a garbage lock: want error, got nil")
	}
}

func TestSignalDaemonPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may signal pid 1")
	}
	// pid 1 exists but a non-root user cannot signal it, so the kill
	// error path runs without any real signal landing.
	dir := t.TempDir()
	holdLockInTest(t, dir, lockBody(1))

	if _, err := SignalDaemon(dir, syscall.SIGTERM); err == nil {
		t.Errorf("SignalDaemon to pid 1: want permission error, got nil")
	}
}

func TestStopDaemonPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may signal pid 1")
	}
	dir := t.TempDir()
	holdLockInTest(t, dir, lockBody(1))

	running, _, err := StopDaemon(dir, time.Second)
	if !running {
		t.Errorf("StopDaemon running = false, want true")
	}
	if err == nil {
		t.Errorf("StopDaemon to pid 1: want permission error, got nil")
	}
}

func TestAcquireRunLockOpenFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if _, err := AcquireRunLock(dir); err == nil {
		t.Errorf("AcquireRunLock in a read-only dir: want error, got nil")
	}
}
