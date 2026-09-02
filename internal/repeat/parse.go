package repeat

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/robfig/cron/v3"
)

// maxSchedulable is the latest instant repeat accepts for a fire time.
// RFC3339 cannot express a five-digit year, and the CLI renders times
// in the local zone, where the largest legal offset is +14:00. Stopping
// 14 hours before the end of year 9999 UTC keeps every stored time
// displayable in any timezone.
var maxSchedulable = time.Date(9999, 12, 31, 9, 59, 59, 0, time.UTC)

// cronParser is the standard 5-field crontab with the @descriptor and
// @every shortcuts. Kept package-private so callers can't accidentally
// drift the accepted syntax across the codebase.
var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow |
		cron.Descriptor,
)

// cronCache memoises ParseCron results.
//
// cron.Schedule is immutable.
// The scheduler can hit this once per second for @every 1s jobs.
// The cache is bounded by distinct cron expressions in the database.
var cronCache sync.Map // map[string]cron.Schedule

// ParseCron validates a cron expression.
//
// Important behavior:
//   - It returns a schedule that computes next-fire times.
//   - Parser errors wrap ErrInvalidCron.
//   - @every durations must be positive.
//
// robfig/cron silently rounds sub-second @every values up to one
// second. Pre-validation rejects them instead.
func ParseCron(expr string) (cron.Schedule, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("%w: empty expression", ErrInvalidCron)
	}
	if len(expr) > maxCronBytes {
		return nil, fmt.Errorf("%w: expression longer than %d bytes", ErrInvalidCron, maxCronBytes)
	}
	// robfig splits fields on any whitespace, so an embedded newline or
	// tab would parse yet corrupt table output later. Reject instead.
	if strings.ContainsFunc(expr, unicode.IsControl) {
		return nil, fmt.Errorf("%w: control character in expression", ErrInvalidCron)
	}
	if v, ok := cronCache.Load(expr); ok {
		return v.(cron.Schedule), nil
	}
	if rest, ok := strings.CutPrefix(expr, "@every "); ok {
		d, err := time.ParseDuration(strings.TrimSpace(rest))
		if err != nil {
			return nil, fmt.Errorf("%w: @every duration: %w", ErrInvalidCron, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("%w: @every duration must be positive: got %v",
				ErrInvalidCron, d)
		}
		if d < time.Second {
			return nil, fmt.Errorf("%w: @every duration must be at least 1s: got %v",
				ErrInvalidCron, d)
		}
	}
	sched, err := cronParser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidCron, err)
	}
	cronCache.Store(expr, sched)
	return sched, nil
}

// ParseAt resolves a one-shot time specifier relative to now. It
// accepts:
//
//   - RFC3339:        "2026-05-12T15:30:00-07:00"
//   - relative offset: "+30m", "+2h", "+45s", "+3d"
//   - today HH:MM:     "today 17:00", "today 5:00pm"
//   - tomorrow HH:MM:  "tomorrow 9am", "tomorrow 09:30"
//
// All "today"/"tomorrow" anchors use now's [time.Location]. Returns
// [ErrInvalidTime] on parse failure or when the resolved instant is
// in the past (we do not schedule retroactively).
func ParseAt(spec string, now time.Time) (time.Time, error) {
	raw := strings.TrimSpace(spec)
	if raw == "" {
		return time.Time{}, fmt.Errorf("%w: empty", ErrInvalidTime)
	}

	got, err := parseAtRaw(raw, now)
	if err != nil {
		return time.Time{}, err
	}
	if !got.After(now) {
		return time.Time{}, fmt.Errorf("%w: %q resolves to %s, not in the future",
			ErrInvalidTime, spec, got.Format(time.RFC3339))
	}
	if got.After(maxSchedulable) {
		return time.Time{}, fmt.Errorf("%w: %q is past the maximum schedulable time %s",
			ErrInvalidTime, spec, maxSchedulable.Format(time.DateOnly))
	}
	return got, nil
}

func parseAtRaw(raw string, now time.Time) (time.Time, error) {
	// RFC3339 and the looser variants Go accepts.
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}

	// Relative offset: +30m, +2h, +45s, +3d.
	if rest, ok := strings.CutPrefix(raw, "+"); ok {
		return parseOffset(rest, now)
	}

	lower := strings.ToLower(raw)
	if rest, ok := strings.CutPrefix(lower, "today "); ok {
		return parseClock(rest, now, 0)
	}
	if rest, ok := strings.CutPrefix(lower, "tomorrow "); ok {
		return parseClock(rest, now, 1)
	}

	return time.Time{}, fmt.Errorf("%w: %q", ErrInvalidTime, raw)
}

// maxOffsetDays bounds "+Nd" offsets at roughly 8,000 years. Larger
// counts must not reach AddDate: its normalization wraps modulo 2^64
// seconds and can land an absurd offset back inside the valid window.
const maxOffsetDays = 3_000_000

// Input bounds enforced by Validate and ParseCron. Oversized values
// are junk: they blow argv limits at exec time or make output
// unusable, so they are rejected at add time with a named reason.
const (
	// MaxNameRunes bounds a job name.
	MaxNameRunes = 255

	// maxCronBytes bounds a cron expression.
	maxCronBytes = 256

	// maxCommandBytes bounds the command argv.
	maxCommandBytes = 128 << 10

	// maxEnvBytes bounds the captured environment snapshot.
	maxEnvBytes = 1 << 20
)

// parseOffset handles "+30m", "+2h", "+3d", and "+1h30m".
//
// time.ParseDuration does not handle "d".
// Whole days advance the calendar via AddDate.
// That avoids int64 duration overflow on absurd counts. The
// maxSchedulable check in ParseAt then rejects them.
func parseOffset(s string, now time.Time) (time.Time, error) {
	if rest, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(rest)
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: bad offset %q", ErrInvalidTime, s)
		}
		if n <= 0 {
			return time.Time{}, fmt.Errorf("%w: offset must be positive", ErrInvalidTime)
		}
		if n > maxOffsetDays {
			return time.Time{}, fmt.Errorf("%w: %q is past the maximum schedulable time %s",
				ErrInvalidTime, s, maxSchedulable.Format(time.DateOnly))
		}
		return now.AddDate(0, 0, n), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: bad offset %q", ErrInvalidTime, s)
	}
	if d <= 0 {
		return time.Time{}, fmt.Errorf("%w: offset must be positive", ErrInvalidTime)
	}
	// A sub-second offset is stale before the store's own clock reads
	// it and would fail there with a confusing "not in the future".
	if d < time.Second {
		return time.Time{}, fmt.Errorf("%w: offset must be at least 1s: got %v", ErrInvalidTime, d)
	}
	return now.Add(d), nil
}

// parseClock accepts "17:00", "5:30pm", "9am" and applies them to the
// day anchored on (now + dayOffset), preserving now's location.
func parseClock(s string, now time.Time, dayOffsetDays int) (time.Time, error) {
	s = strings.TrimSpace(s)
	hour, min, err := parseHourMinute(s)
	if err != nil {
		return time.Time{}, err
	}
	anchor := now.AddDate(0, 0, dayOffsetDays)
	y, m, d := anchor.Date()
	return time.Date(y, m, d, hour, min, 0, 0, now.Location()), nil
}

func parseHourMinute(s string) (hour, minute int, err error) {
	low := strings.ToLower(s)
	ampm := ""
	if rest, ok := strings.CutSuffix(low, "am"); ok {
		ampm, low = "am", strings.TrimSpace(rest)
	} else if rest, ok := strings.CutSuffix(low, "pm"); ok {
		ampm, low = "pm", strings.TrimSpace(rest)
	}

	if h, m, ok := strings.Cut(low, ":"); ok {
		hh, herr := strconv.Atoi(strings.TrimSpace(h))
		mm, merr := strconv.Atoi(strings.TrimSpace(m))
		if herr != nil || merr != nil {
			return 0, 0, fmt.Errorf("%w: bad clock %q", ErrInvalidTime, s)
		}
		hour, minute = hh, mm
	} else {
		hh, err := strconv.Atoi(strings.TrimSpace(low))
		if err != nil {
			return 0, 0, fmt.Errorf("%w: bad clock %q", ErrInvalidTime, s)
		}
		hour = hh
	}

	switch ampm {
	case "am":
		if hour == 12 {
			hour = 0
		}
	case "pm":
		if hour != 12 {
			hour += 12
		}
	}

	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("%w: out-of-range clock %q", ErrInvalidTime, s)
	}
	return hour, minute, nil
}

// NextFire returns the next time the given spec should fire after now.
//
// One-shot jobs:
//   - Status == StatusDone: zero time (already ran).
//   - FireAt > now: FireAt.
//   - FireAt <= now: now.
//
// Past-due one-shots fire on the next tick.
// That handles the daemon being down at the scheduled time.
//
// Cron jobs delegate to the parsed schedule's Next.
func NextFire(j Job, now time.Time) time.Time {
	switch j.Kind {
	case KindOneshot:
		if j.Status == StatusDone {
			return time.Time{}
		}
		if j.FireAt.After(now) {
			return j.FireAt
		}
		return now
	case KindCron:
		sched, err := ParseCron(j.Cron)
		if err != nil {
			return time.Time{}
		}
		return sched.Next(now)
	}
	return time.Time{}
}

// Validate checks a JobSpec before insert.
func (s JobSpec) Validate(now time.Time) error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("%w: name required", ErrInvalidSpec)
	}
	if utf8.RuneCountInString(s.Name) > MaxNameRunes {
		return fmt.Errorf("%w: name exceeds %d characters", ErrInvalidSpec, MaxNameRunes)
	}
	if len(s.Command) == 0 || s.Command[0] == "" {
		return fmt.Errorf("%w: command required", ErrInvalidSpec)
	}
	if n := byteLen(s.Command); n > maxCommandBytes {
		return fmt.Errorf("%w: command is %d bytes, limit %d", ErrInvalidSpec, n, maxCommandBytes)
	}
	if n := byteLen(s.Env); n > maxEnvBytes {
		return fmt.Errorf("%w: env snapshot is %d bytes, limit %d", ErrInvalidSpec, n, maxEnvBytes)
	}
	hasCron := s.Cron != ""
	hasAt := !s.FireAt.IsZero()
	if hasCron == hasAt {
		return fmt.Errorf("%w: exactly one of cron or fire-at must be set", ErrInvalidSpec)
	}
	if hasCron {
		sched, err := ParseCron(s.Cron)
		if err != nil {
			return err
		}
		// Syntactically valid expressions can still describe a day that
		// never occurs (e.g. "0 0 31 2 *"). robfig returns the zero time
		// after a five-year scan. One window is not enough: a leap-day
		// cron checked during 2096-2099 first fires in 2104.
		neverFires := true
		for probe := now; probe.Before(now.AddDate(15, 0, 0)); probe = probe.AddDate(5, 0, 0) {
			if !sched.Next(probe).IsZero() {
				neverFires = false
				break
			}
		}
		if neverFires {
			return fmt.Errorf("%w: %q matches no future time", ErrInvalidCron, s.Cron)
		}
	}
	if hasAt {
		if !s.FireAt.After(now) {
			return fmt.Errorf("%w: fire time %s not in the future",
				ErrInvalidTime, s.FireAt.Format(time.RFC3339))
		}
		if s.FireAt.After(maxSchedulable) {
			return fmt.Errorf("%w: fire time %s is past the maximum schedulable time %s",
				ErrInvalidTime, s.FireAt.Format(time.RFC3339), maxSchedulable.Format(time.DateOnly))
		}
	}
	return nil
}

// byteLen is the serialized size of a string slice, one separator per
// element.
func byteLen(ss []string) int {
	n := 0
	for _, s := range ss {
		n += len(s) + 1
	}
	return n
}
