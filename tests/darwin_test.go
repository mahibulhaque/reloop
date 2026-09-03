//go:build darwin

package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDarwinInstallWritesPlist checks launchd discovery without
// bootstrapping launchctl. A real bootstrap would require a user GUI
// session and would pollute the host, so the test writes the expected
// LaunchAgent path under a sandboxed HOME.
func TestDarwinInstallWritesPlist(t *testing.T) {
	requireGOOS(t, "darwin")

	bin := buildBinary(t)
	fakeHome := t.TempDir()
	dataDir := t.TempDir()

	// Sanity: status should still work under a fake HOME.
	t.Setenv("HOME", fakeHome)
	mustRun(t, bin, dataDir, "status")

	// The Install bootstrap step needs a real user session, so verify the
	// standard launchd LaunchAgents path directly.
	plistDir := filepath.Join(fakeHome, "Library", "LaunchAgents")
	if err := os.MkdirAll(plistDir, 0o755); err != nil {
		t.Fatalf("mkdir LaunchAgents: %v", err)
	}
	// IsSupervised looks for the data dir's log path inside the unit
	// file, so the marker must contain it.
	plistPath := filepath.Join(plistDir, "dev.reloop.reloopd.plist")
	marker := "<plist><string>" + filepath.Join(dataDir, "reloop.log") + "</string></plist>\n"
	if err := os.WriteFile(plistPath, []byte(marker), 0o644); err != nil {
		t.Fatalf("write plist: %v", err)
	}
	out := mustRun(t, bin, dataDir, "status")
	if !strings.Contains(out, "supervised=yes") {
		t.Errorf("status output = %q, want substring %q", out, "supervised=yes")
	}

	// A unit for a different data dir must not count as supervision.
	out = mustRun(t, bin, t.TempDir(), "status")
	if !strings.Contains(out, "supervised=no") {
		t.Errorf("status for other data dir = %q, want substring %q", out, "supervised=no")
	}
}
