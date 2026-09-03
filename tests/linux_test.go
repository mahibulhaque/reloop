//go:build linux

package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLinuxInstallWritesUnit verifies the systemd --user unit discovery path.
// We don't invoke systemctl --user daemon-reload because CI may not have a
// user dbus session, so the test asserts the file path IsSupervised relies on.
func TestLinuxInstallWritesUnit(t *testing.T) {
	requireGOOS(t, "linux")

	bin := buildBinary(t)
	fakeXDG := t.TempDir()
	dataDir := t.TempDir()

	t.Setenv("XDG_CONFIG_HOME", fakeXDG)
	mustRun(t, bin, dataDir, "status")

	unitDir := filepath.Join(fakeXDG, "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatalf("mkdir systemd dir: %v", err)
	}
	// IsSupervised looks for the data dir's log path inside the unit
	// file, so the marker must contain it.
	unitPath := filepath.Join(unitDir, "reloopd.service")
	marker := "[Unit]\nDescription=test\n[Service]\nStandardOutput=append:" +
		filepath.Join(dataDir, "reloop.log") + "\n"
	if err := os.WriteFile(unitPath, []byte(marker), 0o644); err != nil {
		t.Fatalf("write unit: %v", err)
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
