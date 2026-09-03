package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/fang"
	"github.com/google/go-cmp/cmp"
	"github.com/mahibulhaque/repeat/internal/repeat"
)

// runCmd executes the root command with the given argv. It returns
// stdout, stderr, and the error so callers can map it through
// exitCode for the contract a real shell would observe. data-dir is
// injected as the first flag so every invocation is hermetic.
func runCmd(t *testing.T, dir string, argv ...string) (stdoutS, stderrS string, err error) {
	t.Helper()

	// Reset persistent flag state between invocations.
	// Cobra parses once, but globalFlags persists in-process.
	prev := globalFlags
	t.Cleanup(func() { globalFlags = prev })

	root := newRoot()
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(append([]string{"--data-dir", dir, "--quiet"}, argv...))

	prevStderr := stderr
	stderr = &errBuf
	t.Cleanup(func() { stderr = prevStderr })

	err = root.ExecuteContext(t.Context())
	return outBuf.String(), errBuf.String(), err
}

func TestCLIAddListShowRm(t *testing.T) {
	dir := t.TempDir()

	out, _, err := runCmd(t, dir, "add", "--cron", "@hourly", "--name", "backup", "--", "echo", "hi")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(out, "added job ") {
		t.Errorf("add stdout = %q, want substring %q", out, "added job ")
	}

	out, _, err = runCmd(t, dir, "ls", "--json")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	var jobs []repeat.Job
	if err := json.Unmarshal([]byte(out), &jobs); err != nil {
		t.Fatalf("ls --json: %v\n%s", err, out)
	}
	wantRows := []cliJobRow{{
		Name:    "backup",
		Kind:    repeat.KindCron,
		Command: []string{"echo", "hi"},
		Cron:    "@hourly",
		Status:  repeat.StatusEnabled,
	}}
	if diff := cmp.Diff(wantRows, cliJobRows(jobs)); diff != "" {
		t.Errorf("ls --json rows mismatch (-want +got):\n%s", diff)
	}
	if len(jobs) == 0 {
		t.Fatalf("ls --json returned no jobs")
	}
	id := jobs[0].ID
	if len(id) != 5 {
		t.Fatalf("len(job ID) = %d, want 5; id=%q", len(id), id)
	}

	// show resolves by ID and by name.
	for _, ref := range []string{string(id), "backup"} {
		out, _, err = runCmd(t, dir, "show", ref, "--json")
		if err != nil {
			t.Fatalf("show %q: %v", ref, err)
		}
		var got repeat.Job
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("show %q --json: %v\n%s", ref, err, out)
		}
		if got.ID != id {
			t.Errorf("show %q id = %q, want %q", ref, got.ID, id)
		}
	}

	out, _, err = runCmd(t, dir, "rm", string(id))
	if err != nil {
		t.Fatalf("rm: %v", err)
	}
	if !strings.Contains(out, "deleted job "+string(id)) {
		t.Errorf("rm stdout = %q, want substring %q", out, "deleted job "+string(id))
	}
}

type cliJobRow struct {
	Name    string
	Kind    repeat.JobKind
	Command []string
	Cron    string
	Status  repeat.JobStatus
}

func cliJobRows(jobs []repeat.Job) []cliJobRow {
	rows := make([]cliJobRow, 0, len(jobs))
	for _, job := range jobs {
		rows = append(rows, cliJobRow{
			Name:    job.Name,
			Kind:    job.Kind,
			Command: job.Command,
			Cron:    job.Cron,
			Status:  job.Status,
		})
	}
	return rows
}

func TestInjectedVersionWinsOverFangBuildInfo(t *testing.T) {
	prevVersion, prevCommit := version, commit
	version = "1.2.3"
	commit = ""
	t.Cleanup(func() {
		version = prevVersion
		commit = prevCommit
	})

	root := newRoot()
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"--version"})

	if err := fang.Execute(t.Context(), root, fangOptions()...); err != nil {
		t.Fatalf("version: %v\nstderr: %s", err, errBuf.String())
	}
	if got := outBuf.String(); !strings.Contains(got, "repeat version 1.2.3") {
		t.Fatalf("--version output = %q", got)
	}
}

func TestCLIExitCodesForBadInput(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		argv []string
		want int
	}{
		{name: "add_no_args", argv: []string{"add"}, want: 2},
		{name: "add_invalid_cron", argv: []string{"add", "--cron", "garbage", "--", "echo"}, want: 5},
		{name: "add_past_time", argv: []string{"add", "--at", "2020-01-01T00:00:00Z", "--", "echo"}, want: 5},
		{name: "add_both_schedules", argv: []string{"add", "--cron", "@hourly", "--at", "+1h", "echo"}, want: 2},
		{name: "add_no_command", argv: []string{"add", "--cron", "@hourly"}, want: 2},
		// "abcde" is syntactically a valid 5-char ID.
		// Lookup misses, so this should be not-found.
		// It should not be a usage error.
		// A non-5-char arg is treated as a name and also misses -> 3.
		{name: "show_unknown_id", argv: []string{"show", "abcde"}, want: 3},
		{name: "show_unknown_name", argv: []string{"show", "nope"}, want: 3},
		{name: "rm_unknown_id", argv: []string{"rm", "abcde"}, want: 3},
		{name: "enable_unknown", argv: []string{"enable", "abcde"}, want: 3},
		{name: "ls_bad_kind", argv: []string{"ls", "--kind", "weird"}, want: 2},

		// Cobra-level usage errors must all land on exit 2.
		// Without tagUsageErrors these fell through to 1.
		{name: "unknown_subcommand", argv: []string{"foo"}, want: 2},
		{name: "unknown_root_flag", argv: []string{"--badflag"}, want: 2},
		{name: "unknown_sub_flag", argv: []string{"ls", "--badflag"}, want: 2},
		{name: "show_no_arg", argv: []string{"show"}, want: 2},
		{name: "show_extra_arg", argv: []string{"show", "abcde", "extra"}, want: 2},
		{name: "show_shorthand_parse", argv: []string{"show", "-1"}, want: 2},
		{name: "rm_no_arg", argv: []string{"rm"}, want: 2},
		{name: "rm_extra_arg", argv: []string{"rm", "abcde", "extra"}, want: 2},
		{name: "enable_no_arg", argv: []string{"enable"}, want: 2},
		{name: "disable_no_arg", argv: []string{"disable"}, want: 2},
		{name: "logs_no_arg", argv: []string{"logs"}, want: 2},
		{name: "logs_bad_duration", argv: []string{"logs", "abcde", "--since", "garbage"}, want: 2},
		{name: "ls_extra_arg", argv: []string{"ls", "extra"}, want: 2},
		{name: "status_extra_arg", argv: []string{"status", "extra"}, want: 2},
		{name: "stop_extra_arg", argv: []string{"stop", "extra"}, want: 2},
		// edit command was removed.
		// Treat it as an unknown subcommand.
		{name: "edit_gone", argv: []string{"edit", "abcde", "foo"}, want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := runCmd(t, dir, tc.argv...)
			if got := exitCode(err); got != tc.want {
				t.Errorf("exit = %d, want %d (err=%v)", got, tc.want, err)
			}
		})
	}
}

// TestCLIUnwritableDataDirExitsOne maps a store-open failure (data dir
// without write permission) to the generic error exit, not a panic or
// a misleading usage code.
func TestCLIUnwritableDataDirExitsOne(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, _, err := runCmd(t, dir, "ls")
	if err == nil {
		t.Fatal("ls with an unwritable data dir = nil error, want failure")
	}
	if got := exitCode(err); got != 1 {
		t.Errorf("exit = %d, want 1 (err=%v)", got, err)
	}
}

// fakeHome points HOME and XDG_CONFIG_HOME at a temp dir so the unit
// path never resolves to the real unit location.
func fakeHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg-config"))
}

// With no launchctl or systemctl on PATH, the install error must
// tell the user to run the daemon directly. This is a Go test
// because the script tests resolve the repeat command through PATH.
func TestInstallWithoutSupervisorTools(t *testing.T) {
	fakeHome(t)
	t.Setenv("PATH", t.TempDir())

	_, _, err := runCmd(t, t.TempDir(), "install")
	if err == nil || !strings.Contains(err.Error(), "no launchd/systemd") {
		t.Errorf("install without supervisor tools = %v, want the direct-daemon hint", err)
	}
}

// Without sqlite3 on PATH, the error must say sqlite3 is missing.
func TestDebugDBWithoutSqlite(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	_, _, err := runCmd(t, t.TempDir(), "debug", "db")
	if err == nil || !strings.Contains(err.Error(), "sqlite3 not on PATH") {
		t.Errorf("debug db without sqlite3 = %v, want the missing-dependency error", err)
	}
}

// TestKillForcePurges runs the destructive path in a sandbox:
// fake HOME, stubbed launchctl/systemctl, and a hard link to the
// test binary so the binary-removal step can be undone.
func TestKillForcePurges(t *testing.T) {
	fakeHome(t)
	stub := t.TempDir()
	for _, name := range []string{"launchctl", "systemctl"} {
		if err := os.WriteFile(filepath.Join(stub, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write stub: %v", err)
		}
	}
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	if _, _, err := runCmd(t, dir, "add", "--cron", "@hourly", "--name", "doomed", "--", "echo", "hi"); err != nil {
		t.Fatalf("add: %v", err)
	}
	// Install through the real path so IsSupervised sees the unit and
	// the supervisor step runs too.
	if _, _, err := runCmd(t, dir, "install"); err != nil {
		t.Fatalf("install: %v", err)
	}

	// kill unlinks the test binary, which is safe for the running
	// process, but later tests re-exec this path. The hard link keeps
	// the inode reachable so cleanup can put the name back.
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("locate self: %v", err)
	}
	backup := bin + ".kill-backup"
	if err := os.Link(bin, backup); err != nil {
		t.Fatalf("link self: %v", err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(bin); errors.Is(err, fs.ErrNotExist) {
			if err := os.Rename(backup, bin); err != nil {
				t.Fatalf("restore test binary: %v", err)
			}
			return
		}
		_ = os.Remove(backup)
	})

	out, _, err := runCmd(t, dir, "kill", "--force")
	if err != nil {
		t.Fatalf("kill --force: %v\n%s", err, out)
	}
	for _, want := range []string{"removing supervisor unit", "removing data dir", "removing binary", "done"} {
		if !strings.Contains(out, want) {
			t.Errorf("kill output = %q, want substring %q", out, want)
		}
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("data dir still present after kill --force")
	}
}
