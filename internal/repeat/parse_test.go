package repeat

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func mustTime(t *testing.T, layout, value string) time.Time {
	t.Helper()
	got, err := time.Parse(layout, value)
	if err != nil {
		t.Fatalf("setup: parse %q: %v", value, err)
	}
	return got
}

func TestParseAtAccepted(t *testing.T) {
	now := mustTime(t, time.RFC3339, "2026-05-13T10:00:00-07:00")
	cases := []struct {
		name string
		in   string
		want time.Time
	}{
		{name: "rfc3339_future", in: "2026-05-13T12:00:00-07:00", want: mustTime(t, time.RFC3339, "2026-05-13T12:00:00-07:00")},
		{name: "offset_minutes", in: "+30m", want: now.Add(30 * time.Minute)},
		{name: "offset_hours", in: "+2h", want: now.Add(2 * time.Hour)},
		{name: "offset_seconds", in: "+45s", want: now.Add(45 * time.Second)},
		{name: "offset_days", in: "+3d", want: now.Add(3 * 24 * time.Hour)},
		{name: "today_hhmm", in: "today 17:00", want: time.Date(2026, 5, 13, 17, 0, 0, 0, now.Location())},
		{name: "today_pm", in: "today 5:30pm", want: time.Date(2026, 5, 13, 17, 30, 0, 0, now.Location())},
		{name: "tomorrow_am", in: "tomorrow 9am", want: time.Date(2026, 5, 14, 9, 0, 0, 0, now.Location())},
		{name: "tomorrow_hhmm", in: "tomorrow 09:30", want: time.Date(2026, 5, 14, 9, 30, 0, 0, now.Location())},
		{name: "tomorrow_midnight", in: "tomorrow 12am", want: time.Date(2026, 5, 14, 0, 0, 0, 0, now.Location())},
		{name: "tomorrow_noon", in: "tomorrow 12pm", want: time.Date(2026, 5, 14, 12, 0, 0, 0, now.Location())},
		{name: "uppercase_shortcut", in: "TOMORROW 9am", want: time.Date(2026, 5, 14, 9, 0, 0, 0, now.Location())},
		// The schedulable ceiling: 14 hours before the end of year 9999,
		// so the time renders with a four-digit year in any timezone.
		{name: "rfc3339_max", in: "9999-12-31T09:59:59Z", want: mustTime(t, time.RFC3339, "9999-12-31T09:59:59Z")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAt(tc.in, now)
			if err != nil {
				t.Fatalf("ParseAt(%q): %v", tc.in, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("ParseAt(%q) = %s, want %s", tc.in, got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
		})
	}
}

func TestParseAtTomorrowUsesCalendarDayAcrossDSTFallback(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// "tomorrow" must advance the calendar date, not add a fixed 24h.
	now := time.Date(2026, 10, 25, 0, 30, 0, 0, loc)

	got, err := ParseAt("tomorrow 09:00", now)
	if err != nil {
		t.Fatalf("ParseAt: %v", err)
	}
	want := time.Date(2026, 10, 26, 9, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("ParseAt tomorrow across DST fallback = %s, want %s",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestParseAtDSTSpringForwardGap checks how a clock time that does not
// exist on the target day resolves. US spring forward 2026: on Mar 8
// the clock jumps from 02:00 EST to 03:00 EDT, so "2:30am" never
// occurs. time.Date resolves the gap to 01:30 EST, an hour before the
// requested wall time but still on the right calendar day.
//
// This documents current behavior. If a Go upgrade changes the
// normalization rule, this failure is the notice, not a user's
// mis-timed job.
func TestParseAtDSTSpringForwardGap(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	now := time.Date(2026, 3, 7, 22, 0, 0, 0, ny)

	got, err := ParseAt("tomorrow 2:30am", now)
	if err != nil {
		t.Fatalf("ParseAt: %v", err)
	}
	want := time.Date(2026, 3, 8, 1, 30, 0, 0, ny) // resolves to 01:30 EST
	if !got.Equal(want) {
		t.Fatalf("ParseAt across DST gap = %s, want %s",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if !got.After(now) {
		t.Fatalf("ParseAt across DST gap = %s, not after now %s",
			got.Format(time.RFC3339), now.Format(time.RFC3339))
	}
}

// TestParseAtDSTFallBackAmbiguity checks which of the two occurrences an
// ambiguous clock time resolves to. US fall back 2026: on Nov 1 the
// clock repeats 01:00-02:00, so "1:30am" happens twice. time.Date
// picks the first occurrence (EDT, -04:00).
func TestParseAtDSTFallBackAmbiguity(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	now := time.Date(2026, 10, 31, 22, 0, 0, 0, ny)

	got, err := ParseAt("tomorrow 1:30am", now)
	if err != nil {
		t.Fatalf("ParseAt: %v", err)
	}
	if _, offset := got.Zone(); offset != -4*60*60 {
		t.Fatalf("ParseAt ambiguous clock = %s (offset %d), want the first occurrence at -04:00",
			got.Format(time.RFC3339), offset)
	}
}

// TestNextFireCronAcrossDST checks robfig's DST handling, which repeat
// inherits through NextFire.
//
// Documented behavior:
//   - A fire scheduled inside the spring-forward gap is skipped for
//     that day entirely, not shifted to the next valid clock time.
//   - An hourly job fires the repeated fall-back hour twice, once per
//     UTC offset.
//
// Neither is repeat's choice to make. The tests exist so a robfig upgrade
// that changes the rules fails loudly here.
func TestNextFireCronAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}

	t.Run("spring_forward_skips_nonexistent_fire", func(t *testing.T) {
		job := Job{Kind: KindCron, Cron: "30 2 * * *", Status: StatusEnabled}
		got := NextFire(job, time.Date(2026, 3, 8, 1, 0, 0, 0, ny))
		want := time.Date(2026, 3, 9, 2, 30, 0, 0, ny) // Mar 8 has no 02:30
		if !got.Equal(want) {
			t.Errorf("NextFire into DST gap = %s, want %s",
				got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	})

	t.Run("fall_back_fires_repeated_hour_twice", func(t *testing.T) {
		job := Job{Kind: KindCron, Cron: "0 * * * *", Status: StatusEnabled}
		first := NextFire(job, time.Date(2026, 11, 1, 0, 30, 0, 0, ny))
		second := NextFire(job, first)
		_, firstOff := first.Zone()
		_, secondOff := second.Zone()
		if first.Hour() != 1 || second.Hour() != 1 || firstOff == secondOff {
			t.Errorf("fires across fall back = %s then %s, want 01:00 in both offsets",
				first.Format(time.RFC3339), second.Format(time.RFC3339))
		}
		if second.Sub(first) != time.Hour {
			t.Errorf("repeated hour gap = %s, want 1h of real time", second.Sub(first))
		}
	})
}

func TestParseAtRejected(t *testing.T) {
	now := mustTime(t, time.RFC3339, "2026-05-13T10:00:00-07:00")
	cases := []struct {
		name string
		in   string
	}{
		{name: "empty", in: ""},
		{name: "blank", in: "   "},
		{name: "gibberish", in: "next thursday-ish"},
		{name: "past_rfc3339", in: "2020-01-01T00:00:00Z"},
		{name: "past_offset_zero", in: "+0s"},
		{name: "zero_days", in: "+0d"},
		{name: "bad_days", in: "+xd"},
		{name: "negative_offset", in: "+-1h"},
		{name: "bad_offset_unit", in: "+5y"},
		{name: "today_past", in: "today 09:00"},
		{name: "tomorrow_bad_clock", in: "tomorrow 25:00"},
		{name: "tomorrow_bad_minute", in: "tomorrow 9:xx"},
		{name: "tomorrow_non_numeric", in: "tomorrow abc"},
		// Day offsets can land past year 9999, which RFC3339 cannot
		// express.
		{name: "huge_day_offset", in: "+999999999d"},
		// 2^57+1 days: AddDate normalization wraps modulo 2^64 seconds
		// and would land this back inside the valid window.
		{name: "wrapping_day_offset", in: "+144115188075855873d"},
		// Just past the schedulable ceiling.
		{name: "past_ceiling", in: "9999-12-31T10:00:00Z"},
		// Sub-second offsets are stale before the store re-reads the
		// clock and would fail there with "not in the future".
		{name: "subsecond_offset_ns", in: "+1ns"},
		{name: "subsecond_offset_ms", in: "+500ms"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAt(tc.in, now)
			if err == nil {
				t.Fatalf("ParseAt(%q) = no error, want one", tc.in)
			}
			if !errors.Is(err, ErrInvalidTime) {
				t.Errorf("ParseAt(%q) error %v: want errors.Is(ErrInvalidTime)", tc.in, err)
			}
		})
	}
}

func TestParseCronAccepted(t *testing.T) {
	cases := []struct {
		name string
		expr string
	}{
		{name: "every_minute", expr: "* * * * *"},
		{name: "step_minutes", expr: "*/5 * * * *"},
		{name: "weekday_morning", expr: "0 9 * * 1-5"},
		{name: "hourly_descriptor", expr: "@hourly"},
		{name: "daily_descriptor", expr: "@daily"},
		{name: "every_seconds", expr: "@every 30s"},
		{name: "every_minutes", expr: "@every 5m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseCron(tc.expr); err != nil {
				t.Fatalf("ParseCron(%q): %v", tc.expr, err)
			}
		})
	}
}

func TestParseCronCachesExpression(t *testing.T) {
	expr := "*/5 * * * *"
	if _, err := ParseCron(expr); err != nil {
		t.Fatalf("initial ParseCron(%q): %v", expr, err)
	}
	if _, err := ParseCron(expr); err != nil {
		t.Fatalf("cached ParseCron(%q): %v", expr, err)
	}
}

func TestParseCronRejected(t *testing.T) {
	cases := []struct {
		name string
		expr string
	}{
		{name: "empty", expr: ""},
		{name: "blank", expr: "   "},
		{name: "not_cron", expr: "not-a-cron"},
		{name: "too_few_fields", expr: "0 0 * *"},
		{name: "unknown_descriptor", expr: "@nope"},
		{name: "bad_every_duration", expr: "@every nope"},
		// robfig splits fields on any whitespace, so these would parse
		// but corrupt table output later.
		{name: "embedded_newline", expr: "0 0 * *\n*"},
		{name: "embedded_tab", expr: "0 0\t* * *"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCron(tc.expr)
			if err == nil {
				t.Fatalf("ParseCron(%q) = no error, want one", tc.expr)
			}
			if !errors.Is(err, ErrInvalidCron) {
				t.Errorf("ParseCron(%q) error %v: want errors.Is(ErrInvalidCron)", tc.expr, err)
			}
		})
	}
}

func TestNextFire(t *testing.T) {
	now := mustTime(t, time.RFC3339, "2026-05-13T10:00:00Z")

	t.Run("cron_next", func(t *testing.T) {
		j := Job{Kind: KindCron, Cron: "*/15 * * * *"}
		got := NextFire(j, now)
		want := time.Date(2026, 5, 13, 10, 15, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("NextFire cron = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	})

	t.Run("oneshot_future", func(t *testing.T) {
		at := now.Add(2 * time.Hour)
		j := Job{Kind: KindOneshot, FireAt: at}
		if got := NextFire(j, now); !got.Equal(at) {
			t.Errorf("NextFire oneshot = %s, want %s", got, at)
		}
	})

	t.Run("oneshot_past_enabled", func(t *testing.T) {
		j := Job{Kind: KindOneshot, FireAt: now.Add(-1 * time.Hour), Status: StatusEnabled}
		if got := NextFire(j, now); !got.Equal(now) {
			t.Errorf("NextFire missed oneshot = %s, want %s", got, now)
		}
	})

	t.Run("oneshot_done", func(t *testing.T) {
		j := Job{Kind: KindOneshot, FireAt: now.Add(-1 * time.Hour), Status: StatusDone}
		if got := NextFire(j, now); !got.IsZero() {
			t.Errorf("NextFire done oneshot = %s, want zero", got)
		}
	})

	t.Run("cron_invalid", func(t *testing.T) {
		j := Job{Kind: KindCron, Cron: "garbage"}
		if got := NextFire(j, now); !got.IsZero() {
			t.Errorf("NextFire invalid cron = %s, want zero", got)
		}
	})

	t.Run("unknown_kind", func(t *testing.T) {
		j := Job{Kind: JobKind("mystery")}
		if got := NextFire(j, now); !got.IsZero() {
			t.Errorf("NextFire unknown kind = %s, want zero", got)
		}
	})
}

func TestJobSpecValidate(t *testing.T) {
	now := mustTime(t, time.RFC3339, "2026-05-13T10:00:00Z")
	future := now.Add(time.Hour)

	cases := []struct {
		name    string
		spec    JobSpec
		wantErr error
	}{
		{name: "ok_cron", spec: JobSpec{Name: "x", Command: []string{"echo"}, Cron: "@hourly"}},
		{name: "ok_oneshot", spec: JobSpec{Name: "x", Command: []string{"echo"}, FireAt: future}},
		{name: "missing_name", spec: JobSpec{Command: []string{"echo"}, Cron: "@hourly"}, wantErr: ErrInvalidSpec},
		{name: "missing_command", spec: JobSpec{Name: "x", Cron: "@hourly"}, wantErr: ErrInvalidSpec},
		{name: "empty_argv0", spec: JobSpec{Name: "x", Command: []string{""}, Cron: "@hourly"}, wantErr: ErrInvalidSpec},
		{name: "both_cron_and_at", spec: JobSpec{Name: "x", Command: []string{"echo"}, Cron: "@hourly", FireAt: future}, wantErr: ErrInvalidSpec},
		{name: "neither_cron_nor_at", spec: JobSpec{Name: "x", Command: []string{"echo"}}, wantErr: ErrInvalidSpec},
		{name: "bad_cron", spec: JobSpec{Name: "x", Command: []string{"echo"}, Cron: "garbage"}, wantErr: ErrInvalidCron},
		{name: "past_oneshot", spec: JobSpec{Name: "x", Command: []string{"echo"}, FireAt: now.Add(-time.Minute)}, wantErr: ErrInvalidTime},
		// Zero or negative @every schedules would tight-loop the scheduler.
		{name: "every_zero", spec: JobSpec{Name: "x", Command: []string{"echo"}, Cron: "@every 0s"}, wantErr: ErrInvalidCron},
		{name: "every_negative", spec: JobSpec{Name: "x", Command: []string{"echo"}, Cron: "@every -1s"}, wantErr: ErrInvalidCron},
		// Feb 31 parses but never occurs. The job would never fire.
		{name: "cron_never_fires", spec: JobSpec{Name: "x", Command: []string{"echo"}, Cron: "0 0 31 2 *"}, wantErr: ErrInvalidCron},
		// Every input has an upper bound. Past it is an explicit error.
		{name: "whitespace_name", spec: JobSpec{Name: "   ", Command: []string{"echo"}, Cron: "@hourly"}, wantErr: ErrInvalidSpec},
		{name: "long_name", spec: JobSpec{Name: strings.Repeat("n", MaxNameRunes+1), Command: []string{"echo"}, Cron: "@hourly"}, wantErr: ErrInvalidSpec},
		{name: "long_cron", spec: JobSpec{Name: "x", Command: []string{"echo"}, Cron: "0 0 * * " + strings.Repeat("1,", maxCronBytes)}, wantErr: ErrInvalidCron},
		{name: "every_subsecond", spec: JobSpec{Name: "x", Command: []string{"echo"}, Cron: "@every 500ms"}, wantErr: ErrInvalidCron},
		{name: "huge_command", spec: JobSpec{Name: "x", Command: []string{"echo", strings.Repeat("a", maxCommandBytes)}, Cron: "@hourly"}, wantErr: ErrInvalidSpec},
		{name: "huge_env", spec: JobSpec{Name: "x", Command: []string{"echo"}, Env: []string{"K=" + strings.Repeat("v", maxEnvBytes)}, Cron: "@hourly"}, wantErr: ErrInvalidSpec},
		// Past the RFC3339 ceiling (year 9999).
		{name: "oneshot_past_max", spec: JobSpec{Name: "x", Command: []string{"echo"}, FireAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}, wantErr: ErrInvalidTime},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate(now)
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("JobSpec.Validate() = %v, want nil", err)
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("JobSpec.Validate() error = %v, want errors.Is(%v)", err, tc.wantErr)
			}
		})
	}
}

// TestValidateLeapDayCronNearCentury checks that a leap-day cron checked
// while the next Feb 29 is over five years out still validates: from
// 2096-03-01 the next one is 2104 because 2100 skips the leap year.
func TestValidateLeapDayCronNearCentury(t *testing.T) {
	now := time.Date(2096, 3, 1, 0, 0, 0, 0, time.UTC)
	spec := JobSpec{Name: "x", Command: []string{"echo"}, Cron: "0 0 29 2 *"}
	if err := spec.Validate(now); err != nil {
		t.Fatalf("Validate(leap-day cron at %s) = %v, want nil", now, err)
	}
}
