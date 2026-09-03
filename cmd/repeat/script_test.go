package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"
)

func TestMain(m *testing.M) {
	// Wire repeat as a testscript-callable command. The scripts drive it
	// like a real shell session.
	testscript.Main(m, map[string]func(){
		"repeat":  func() { os.Exit(run()) },
		"timeout": func() { os.Exit(runTimeoutMain()) },
		"waitfor": func() { os.Exit(runWaitForMain()) },
	})
}

// runWaitForMain polls a command until its combined output contains a
// substring. Fixed sleeps in the scripts made every run pay the
// worst-case wait. Usage: waitfor <timeout> <substring> <cmd> [args].
func runWaitForMain() int {
	if len(os.Args) < 4 {
		return 2
	}
	d, err := parseTimeoutDuration(os.Args[1])
	if err != nil {
		return 2
	}
	deadline := time.Now().Add(d)
	for {
		out, _ := exec.Command(os.Args[3], os.Args[4:]...).CombinedOutput()
		if strings.Contains(string(out), os.Args[2]) {
			return 0
		}
		if time.Now().After(deadline) {
			_, _ = os.Stderr.Write(out)
			return 124
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func runTimeoutMain() int {
	if len(os.Args) < 3 {
		return 2
	}
	d, err := parseTimeoutDuration(os.Args[1])
	if err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[2], os.Args[3:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return 124
	}
	if err == nil {
		return 0
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode()
	}
	return 1
}

func parseTimeoutDuration(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(s)
}

func TestScripts(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "testdata/script",
		Setup: func(env *testscript.Env) error {
			// Each script gets its own HOME / XDG so the data directory lives
			// entirely inside $WORK and never touches the user's real repeat state.
			env.Setenv("HOME", env.WorkDir)
			env.Setenv("XDG_DATA_HOME", env.WorkDir+"/xdg")
			env.Setenv("XDG_CONFIG_HOME", env.WorkDir+"/xdg-config")
			// Fang detects non-TTY output, but pinning these keeps the golden
			// output byte-stable across terminal types.
			env.Setenv("CLICOLOR", "0")
			env.Setenv("NO_COLOR", "1")
			return nil
		},
	})
}
