package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mahibulhaque/reloop/internal/reloop"
)

// TestWriteStatusInFlightLine: claimed-but-unfinished one-shots render
// as "in flight" while the daemon runs and as "interrupted" when it is
// down. A zero count prints neither line.
func TestWriteStatusInFlightLine(t *testing.T) {
	cases := map[string]struct {
		inFlight int
		running  bool
		want     string
	}{
		"none":               {inFlight: 0, running: true, want: ""},
		"running mid-job":    {inFlight: 2, running: true, want: "in flight:"},
		"daemon down midway": {inFlight: 1, running: false, want: "interrupted:"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			writeStatus(&buf, reloop.Status{
				Daemon: reloop.DaemonStatus{Running: tc.running},
				Jobs:   reloop.JobCounts{Total: 3, OneshotInFlight: tc.inFlight},
			})
			out := buf.String()
			if tc.want == "" {
				if strings.Contains(out, "in flight:") || strings.Contains(out, "interrupted:") {
					t.Errorf("zero count printed a claim line:\n%s", out)
				}
				return
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output missing %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestWriteStatusDisabledCounts: disabled buckets render only when
// nonzero, so the jobs line stays short in the all-enabled case.
func TestWriteStatusDisabledCounts(t *testing.T) {
	var buf bytes.Buffer
	writeStatus(&buf, reloop.Status{
		Jobs: reloop.JobCounts{Total: 5, Cron: 3, CronDisabled: 1, OneshotPending: 1, OneshotDisabled: 1},
	})
	out := buf.String()
	for _, want := range []string{"cron=3 (1 disabled)", "oneshot disabled=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	buf.Reset()
	writeStatus(&buf, reloop.Status{Jobs: reloop.JobCounts{Total: 2, Cron: 1, OneshotPending: 1}})
	if strings.Contains(buf.String(), "disabled") {
		t.Errorf("zero disabled counts printed a disabled segment:\n%s", buf.String())
	}
}

func TestSanitizeReplacesControlCharacters(t *testing.T) {
	if got := sanitize("a\x1b[31mb"); got != "a?[31mb" {
		t.Errorf("sanitize = %q, want %q", got, "a?[31mb")
	}
	if got := sanitize("plain"); got != "plain" {
		t.Errorf("sanitize = %q, want unchanged", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate short = %q, want unchanged", got)
	}
	if got := truncate("aaaaaaaaaa", 5); got != "aa..." {
		t.Errorf("truncate overflow = %q, want %q", got, "aa...")
	}
	// No room for an ellipsis: cut hard instead of panicking.
	if got := truncate("abcd", 2); got != "ab" {
		t.Errorf("truncate tiny max = %q, want %q", got, "ab")
	}
	// Three runes in six bytes: the byte length exceeds max but the
	// rune count does not, so the value stays whole.
	if got := truncate("ééé", 3); got != "ééé" {
		t.Errorf("truncate multibyte = %q, want unchanged", got)
	}
}

func TestScheduleSummaryUnknownKind(t *testing.T) {
	if got := scheduleSummary(reloop.Job{Kind: "weird"}); got != "" {
		t.Errorf("scheduleSummary = %q, want empty for an unknown kind", got)
	}
}
