// Package tests holds the per-OS supervisor-discovery coverage that
// needs a real binary and a fake HOME/XDG. The broader end-to-end
// surface lives in the script tests (cmd/reloop/testdata/script) and the
// e2e harness (tests/e2e/run.sh).
package tests

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// buildBinary cross-compiles reloop for the host OS into a temp file.
//
// Per-run builds keep the test hermetic.
// The Go build cache makes repeat builds fast.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "reloop")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/reloop")
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build reloop: %v\n%s", err, out)
	}
	return bin
}

// repoRoot walks up from the test file's cwd until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for d := wd; d != string(filepath.Separator); d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatalf("go.mod not found above %s", wd)
	return ""
}

// runCmd executes reloop with argv against the given data dir.
//
// It returns combined stdout and stderr plus exit code.
// Non-zero exit is not a test failure here.
// Callers decide what to assert.
func runCmd(t *testing.T, bin, dataDir string, argv ...string) (out string, code int) {
	t.Helper()
	out, code, err := runCmdRaw(bin, dataDir, argv...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return out, code
}

func runCmdRaw(bin, dataDir string, argv ...string) (out string, code int, err error) {
	args := append([]string{"--data-dir", dataDir, "--quiet"}, argv...)
	cmd := exec.Command(bin, args...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err = cmd.Run()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return buf.String(), exitErr.ExitCode(), nil
	}
	if err != nil {
		return buf.String(), 0, fmt.Errorf("exec reloop %v: %w", argv, err)
	}
	return buf.String(), 0, nil
}

// mustRun is runCmd with the "exit 0" assertion baked in.
func mustRun(t *testing.T, bin, dataDir string, argv ...string) string {
	t.Helper()
	out, code := runCmd(t, bin, dataDir, argv...)
	if code != 0 {
		t.Fatalf("reloop %v: exit %d\n%s", argv, code, out)
	}
	return out
}

// requireGOOS skips the test when run on the wrong platform. Belt and
// braces alongside the go:build tag protects callers who execute helpers
// outside a normal `go test` run.
func requireGOOS(t *testing.T, want string) {
	t.Helper()
	if runtime.GOOS != want {
		t.Skipf("test requires GOOS=%s, have %s", want, runtime.GOOS)
	}
}
