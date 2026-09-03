package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubSupervisorTools puts fake launchctl and systemctl on PATH so
// install code runs without touching the real user session.
func stubSupervisorTools(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"launchctl", "systemctl"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	t.Setenv("PATH", dir)
}

// fakeUnitHome points HOME and XDG_CONFIG_HOME at a temp dir so
// unitPath never resolves to the real unit location.
func fakeUnitHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg-config"))
	return home
}

const stubOK = "#!/bin/sh\nexit 0\n"

func TestInstallWritesUnitAndReportsSupervised(t *testing.T) {
	fakeUnitHome(t)
	stubSupervisorTools(t, stubOK)
	dataDir := t.TempDir()

	installed, err := Install("/bin/reloop", dataDir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !installed {
		t.Errorf("Install installed = false, want true")
	}
	p, err := unitPath()
	if err != nil {
		t.Fatalf("unitPath: %v", err)
	}
	unit, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if !strings.Contains(string(unit), logPath(dataDir)) {
		t.Errorf("unit is missing the log path %q:\n%s", logPath(dataDir), unit)
	}
	if !IsSupervised(dataDir) {
		t.Errorf("IsSupervised = false after install")
	}
}

func TestInstallNoOpWhileDaemonRunsWithCurrentUnit(t *testing.T) {
	fakeUnitHome(t)
	stubSupervisorTools(t, stubOK)
	dataDir := t.TempDir()

	if _, err := Install("/bin/reloop", dataDir); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	// Hold the run lock so the probe sees a live daemon.
	release, err := AcquireRunLock(dataDir)
	if err != nil || release == nil {
		t.Fatalf("AcquireRunLock: got no lock, err=%v", err)
	}
	t.Cleanup(release)

	installed, err := Install("/bin/reloop", dataDir)
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if installed {
		t.Errorf("Install installed = true, want no-op while the daemon runs")
	}
}

func TestInstallReappliesWhenDaemonDown(t *testing.T) {
	fakeUnitHome(t)
	stubSupervisorTools(t, stubOK)
	dataDir := t.TempDir()

	if _, err := Install("/bin/reloop", dataDir); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	installed, err := Install("/bin/reloop", dataDir)
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if !installed {
		t.Errorf("Install installed = false, want re-apply while the daemon is down")
	}
}

func TestInstallRejectsControlCharacters(t *testing.T) {
	if _, err := Install("/bin/reloop\n", t.TempDir()); err == nil {
		t.Errorf("Install with a newline in the binary path: want error, got nil")
	}
}

func TestInstallMkdirDataDirFails(t *testing.T) {
	fakeUnitHome(t)
	stubSupervisorTools(t, stubOK)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if _, err := Install("/bin/reloop", file); err == nil {
		t.Errorf("Install with a file as data dir: want error, got nil")
	}
}

func TestUninstallRemovesUnit(t *testing.T) {
	fakeUnitHome(t)
	stubSupervisorTools(t, stubOK)

	if _, err := Install("/bin/reloop", t.TempDir()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	removed, err := Uninstall()
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !removed {
		t.Errorf("Uninstall removed = false, want true")
	}
	removed, err = Uninstall()
	if err != nil {
		t.Fatalf("second Uninstall: %v", err)
	}
	if removed {
		t.Errorf("second Uninstall removed = true, want false")
	}
}

func TestUninstallRemoveFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	fakeUnitHome(t)
	stubSupervisorTools(t, stubOK)
	if _, err := Install("/bin/reloop", t.TempDir()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	p, err := unitPath()
	if err != nil {
		t.Fatalf("unitPath: %v", err)
	}
	if err := os.Chmod(filepath.Dir(p), 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(p), 0o700) })

	if _, err := Uninstall(); err == nil {
		t.Errorf("Uninstall with a read-only unit dir: want error, got nil")
	}
}

func TestIsSupervisedWithoutUnit(t *testing.T) {
	fakeUnitHome(t)
	if IsSupervised(t.TempDir()) {
		t.Errorf("IsSupervised = true with no unit file")
	}
}

func TestRunUnitFailureCarriesOutput(t *testing.T) {
	stubSupervisorTools(t, "#!/bin/sh\necho broken >&2\nexit 1\n")

	err := runUnit("systemctl daemon-reload", "systemctl", "--user", "daemon-reload")
	if err == nil {
		t.Fatalf("runUnit with a failing tool: want error, got nil")
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("runUnit error %q is missing the tool output", err)
	}
}

func TestInstallLaunchdReplacesExistingUnit(t *testing.T) {
	stubSupervisorTools(t, stubOK)
	path := filepath.Join(t.TempDir(), "dev.reloop.reloopd.plist")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("write old plist: %v", err)
	}

	if err := installLaunchd(path, "new-plist"); err != nil {
		t.Fatalf("installLaunchd: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	if string(got) != "new-plist" {
		t.Errorf("plist = %q, want %q", got, "new-plist")
	}
}

func TestInstallSystemdWritesAndStarts(t *testing.T) {
	stubSupervisorTools(t, stubOK)
	path := filepath.Join(t.TempDir(), "reloopd.service")

	if err := installSystemd(path, "unit-content"); err != nil {
		t.Fatalf("installSystemd: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if string(got) != "unit-content" {
		t.Errorf("unit = %q, want %q", got, "unit-content")
	}
}

func TestInstallSystemdReloadFailure(t *testing.T) {
	stubSupervisorTools(t, "#!/bin/sh\nexit 1\n")
	path := filepath.Join(t.TempDir(), "reloopd.service")

	if err := installSystemd(path, "unit"); err == nil {
		t.Errorf("installSystemd with failing systemctl: want error, got nil")
	}
}

func TestInstallSystemdEnableFailure(t *testing.T) {
	// daemon-reload succeeds, enable fails.
	stubSupervisorTools(t, "#!/bin/sh\ncase \"$2\" in enable) exit 1 ;; esac\nexit 0\n")
	path := filepath.Join(t.TempDir(), "reloopd.service")

	if err := installSystemd(path, "unit"); err == nil {
		t.Errorf("installSystemd with failing enable: want error, got nil")
	}
}

func TestInstallersFailOnUnwritableUnitDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	stubSupervisorTools(t, stubOK)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := installLaunchd(filepath.Join(dir, "p.plist"), "x"); err == nil {
		t.Errorf("installLaunchd into a read-only dir: want error, got nil")
	}
	if err := installSystemd(filepath.Join(dir, "u.service"), "x"); err == nil {
		t.Errorf("installSystemd into a read-only dir: want error, got nil")
	}
}

func TestWriteUnitFileMkdirFails(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if err := writeUnitFile(filepath.Join(file, "unit"), "x"); err == nil {
		t.Errorf("writeUnitFile under a file: want error, got nil")
	}
}

func TestUnitPathLinuxFallsBackToHomeConfig(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only fallback")
	}
	t.Setenv("HOME", "/fake/home")
	t.Setenv("XDG_CONFIG_HOME", "")

	p, err := unitPath()
	if err != nil {
		t.Fatalf("unitPath: %v", err)
	}
	want := filepath.Join("/fake/home", ".config", "systemd", "user", unitName)
	if p != want {
		t.Errorf("unitPath = %q, want %q", p, want)
	}
}
