package daemon

import (
	"strings"
	"testing"
)

// Unit renderers must survive hostile paths: spaces break unquoted
// systemd ExecStart tokens, & breaks plist XML, and % is a systemd
// specifier.
func TestRenderLaunchdPlistEscapes(t *testing.T) {
	got := renderLaunchdPlist("/Users/a&b/my tools/reloop", "/data <dir>", []string{"--data-dir", "/data <dir>"})
	for _, want := range []string{
		"<string>/Users/a&amp;b/my tools/reloop</string>",
		"<string>--data-dir</string>",
		"<string>/data &lt;dir&gt;</string>",
		"<key>SuccessfulExit</key><false/>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "/Users/a&b") {
		t.Errorf("plist contains raw ampersand:\n%s", got)
	}
}

func TestRenderSystemdUnitQuotes(t *testing.T) {
	got := renderSystemdUnit("/home/a b/reloop 100%", "/data 100%", []string{"--data-dir", "/data 100%"})
	for _, want := range []string{
		`ExecStart="/home/a b/reloop 100%%" daemon "--data-dir" "/data 100%%"`,
		"StandardOutput=append:/data 100%%/reloop.log",
		"Restart=on-failure",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit missing %q:\n%s", want, got)
		}
	}
}

func TestRenderSystemdUnitEscapesDollar(t *testing.T) {
	// systemd substitutes $VAR even inside quoted ExecStart tokens. A
	// literal $ must render as $$ or the daemon serves the wrong path.
	got := renderSystemdUnit("/opt/reloop", "/data/x${HOME}y", []string{"--data-dir", "/data/x${HOME}y"})
	if !strings.Contains(got, `"/data/x$${HOME}y"`) {
		t.Errorf("unit leaves $ unescaped:\n%s", got)
	}
}

func TestValidUnitValuesRejectsControlChars(t *testing.T) {
	// A newline in a systemd value injects extra directives. There is
	// no in-value escape for it, so Install must reject it.
	for _, bad := range []string{"/tmp/d\nExecStartPre=/bin/evil", "/tmp/d\x08", "/tmp/d\x7f", "/tmp/d\u0085"} {
		if err := validUnitValues([]string{bad}); err == nil {
			t.Errorf("validUnitValues(%q) = nil, want error", bad)
		}
	}
	if err := validUnitValues([]string{"/data 100%", "/Users/a&b/emoji \u2764"}); err != nil {
		t.Errorf("validUnitValues(safe) = %v, want nil", err)
	}
}
