package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode"

	"github.com/mahibulhaque/reloop/internal/reloop"
)

const (
	launchLabel = "dev.reloop.reloopd" // launchd plist Label (macOS)
	unitName    = "reloopd.service"    // systemd --user unit filename (linux)
)

// Install writes the platform supervisor unit and starts it.
//
// daemonArgs are extra CLI arguments baked into the unit's daemon
// invocation, e.g. --data-dir for a non-default location. Without
// them a custom data dir would only affect the CLI while the
// supervised daemon silently served the default one.
//
// Supported supervisors:
//   - macOS: launchd LaunchAgent.
//   - Linux: systemd --user unit.
//
// The daemon starts at login and restarts after a crash. The daemon
// stays stopped after 'reloop stop' on both platforms.
//
// Install is idempotent and self-healing. When the on-disk unit
// already matches the desired content it is a no-op and installed is
// false. When it differs (binary moved by an upgrade, data dir
// changed, format updated) the unit is rewritten and reloaded.
func Install(binary, dataDir string, daemonArgs ...string) (installed bool, err error) {
	if err := validUnitValues(append([]string{binary, dataDir}, daemonArgs...)); err != nil {
		return false, err
	}
	p, err := unitPath()
	if err != nil {
		return false, err
	}
	var desired string
	var apply func(path, unit string) error
	switch runtime.GOOS {
	case "darwin":
		desired = renderLaunchdPlist(binary, dataDir, daemonArgs)
		apply = installLaunchd
	case "linux":
		desired = renderSystemdUnit(binary, dataDir, daemonArgs)
		apply = installSystemd
	default:
		return false, reloop.ErrUnsupportedOS
	}
	existing, readErr := os.ReadFile(p)
	if readErr == nil && string(existing) == desired {
		// The unit is current. Re-apply only when the daemon is down,
		// so install after stop starts it again and a healthy daemon
		// is left alone.
		_, _, running, _ := ProbeRunLock(dataDir)
		if running {
			return false, nil
		}
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return false, fmt.Errorf("mkdir data dir: %w", err)
	}
	return true, apply(p, desired)
}

// validUnitValues rejects control characters in values rendered into
// a unit file. A newline in a systemd value injects extra directives
// and control bytes make a plist invalid XML.
func validUnitValues(vals []string) error {
	for _, v := range vals {
		if strings.ContainsFunc(v, unicode.IsControl) {
			return fmt.Errorf("install: control character in %q", v)
		}
	}
	return nil
}

// Uninstall removes the platform supervisor unit. The removed result
// is true only when a unit was present. An OS without a supervisor
// has nothing to remove, so it reports (false, nil).
func Uninstall() (removed bool, err error) {
	p, err := unitPath()
	if err != nil {
		return false, nil
	}
	if _, err := os.Stat(p); err != nil {
		return false, nil
	}
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("launchctl", "bootout", launchctlTarget(), p).Run()
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return true, fmt.Errorf("remove plist: %w", err)
		}
	case "linux":
		_ = exec.Command("systemctl", "--user", "disable", "--now", unitName).Run()
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return true, fmt.Errorf("remove unit: %w", err)
		}
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	}
	return true, nil
}

// IsSupervised reports whether a supervisor unit is installed for
// this data dir. Every unit file contains the data dir's log path,
// so the check is to look for that path in the file. A unit that
// was installed for some other data dir does not count.
func IsSupervised(dataDir string) bool {
	p, err := unitPath()
	if err != nil {
		return false
	}
	unit, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	return strings.Contains(string(unit), logPath(dataDir))
}

// unitPath returns the on-disk location of the platform unit file.
// Returns ErrUnsupportedOS on platforms we don't handle.
func unitPath() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := homeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "LaunchAgents", launchLabel+".plist"), nil
	case "linux":
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			home, err := homeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(home, ".config")
		}
		return filepath.Join(base, "systemd", "user", unitName), nil
	default:
		return "", reloop.ErrUnsupportedOS
	}
}

// launchctlTarget is the user domain selector launchctl wants for
// bootstrap/bootout operations.
func launchctlTarget() string { return "gui/" + strconv.Itoa(os.Getuid()) }

// xmlEscape makes a string safe inside a plist <string> element.
// Paths can legally contain & or <, which would break the XML.
var xmlEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace

// renderLaunchdPlist builds the LaunchAgent definition.
//
// KeepAlive with SuccessfulExit=false restarts the daemon only after
// a crash. The daemon exits 0 on 'reloop stop' and launchd leaves it
// stopped. systemd behaves the same with Restart=on-failure.
func renderLaunchdPlist(binary, dataDir string, daemonArgs []string) string {
	var args strings.Builder
	for _, a := range daemonArgs {
		fmt.Fprintf(&args, "      <string>%s</string>\n", xmlEscape(a))
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>Label</key><string>%s</string>
    <key>ProgramArguments</key>
    <array>
      <string>%s</string>
      <string>daemon</string>
%s    </array>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key>
    <dict>
      <key>SuccessfulExit</key><false/>
    </dict>
    <key>StandardOutPath</key><string>%s</string>
    <key>StandardErrorPath</key><string>%s</string>
  </dict>
</plist>
`, launchLabel, xmlEscape(binary), args.String(),
		xmlEscape(logPath(dataDir)), xmlEscape(logPath(dataDir)))
}

// systemdQuote wraps a command-line token for an ExecStart= value.
// systemd splits on unquoted whitespace, expands % specifiers, and
// substitutes $VAR even inside quotes, so all three need escaping.
func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, `%`, `%%`)
	s = strings.ReplaceAll(s, `$`, `$$`)
	return `"` + s + `"`
}

// systemdPath escapes a path used in a non-ExecStart directive value.
// Those values run to end of line (no word splitting), but % is still
// a specifier.
func systemdPath(s string) string { return strings.ReplaceAll(s, `%`, `%%`) }

func renderSystemdUnit(binary, dataDir string, daemonArgs []string) string {
	tokens := []string{systemdQuote(binary), "daemon"}
	for _, a := range daemonArgs {
		tokens = append(tokens, systemdQuote(a))
	}
	return fmt.Sprintf(`[Unit]
Description=reloop job scheduler daemon
After=default.target

[Service]
Type=simple
ExecStart=%s
Restart=on-failure
RestartSec=2
StandardOutput=append:%s
StandardError=append:%s

[Install]
WantedBy=default.target
`, strings.Join(tokens, " "), systemdPath(logPath(dataDir)), systemdPath(logPath(dataDir)))
}

// runUnit executes a supervisor command. A failure leaves the unit
// file in place. Install re-applies it while the daemon is down, so
// the next install heals a transient failure.
func runUnit(what string, argv ...string) error {
	if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", what, err, out)
	}
	return nil
}

// writeUnitFile creates the unit directory and writes the unit.
func writeUnitFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir unit dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	return nil
}

func installLaunchd(path, plist string) error {
	// Unload any previous version first. bootstrap refuses to load over
	// an already-bootstrapped label. Errors are expected when nothing
	// was loaded.
	if _, err := os.Stat(path); err == nil {
		_ = exec.Command("launchctl", "bootout", launchctlTarget(), path).Run()
	}
	if err := writeUnitFile(path, plist); err != nil {
		return err
	}
	return runUnit("launchctl bootstrap", "launchctl", "bootstrap", launchctlTarget(), path)
}

func installSystemd(path, unit string) error {
	if err := writeUnitFile(path, unit); err != nil {
		return err
	}
	if err := runUnit("systemctl daemon-reload", "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	// Enable without --now, then restart. restart also starts a
	// stopped unit, so the daemon starts exactly once even when a
	// rewritten unit needs the reload kick.
	if err := runUnit("systemctl enable", "systemctl", "--user", "enable", unitName); err != nil {
		return err
	}
	return runUnit("systemctl restart", "systemctl", "--user", "restart", unitName)
}
