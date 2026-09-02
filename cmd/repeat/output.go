package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/mahibulhaque/repeat/internal/repeat"
)

// writeJSON serialises v with a trailing newline. Errors go to
// stderr through a separate helper so scripts can rely on stdout
// being only data.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// nameColMax caps the NAME column in the ls table.
//
// Long default names would blow out alignment.
// Full names remain available through show and JSON output.
const nameColMax = 32

// writeJobsTable formats jobs as a tab-aligned table.
//
// Table choices:
//   - Times render in the local timezone.
//   - ID and Kind stay narrow.
//   - STATUS is enabled, disabled, or done.
//   - RESULT is ok, fail, or skipped_overlap.
func writeJobsTable(w io.Writer, jobs []repeat.Job) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tNAME\tSCHEDULE\tSTATUS\tLAST RUN\tRESULT")
	for _, j := range jobs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			j.ID, j.Kind, truncate(sanitize(j.Name), nameColMax), sanitize(scheduleSummary(j)),
			j.Status, fmtTimeOrDash(j.LastRunAt), orDash(string(j.LastStatus)),
		)
	}
	tw.Flush()
}

// sanitize replaces control characters with '?'. Names and commands
// are user input echoed back to the terminal, and a raw escape byte could
// retitle or recolor it. JSON output stays exact.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

// truncate clamps s to at most max runes, using a trailing ellipsis
// when it overflows. Operates on runes so multi-byte characters
// don't get sliced mid-character.
func truncate(s string, max int) string {
	if len(s) <= max {
		// Byte length bounds rune count, so most values skip the
		// rune conversion entirely.
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	// No room for an ellipsis below 4 runes, so cut hard.
	if max <= 3 {
		return string(r[:max])
	}
	return string(r[:max-3]) + "..."
}

func writeJobDetail(w io.Writer, j repeat.Job) {
	fmt.Fprintf(w, "id:           %s\n", j.ID)
	fmt.Fprintf(w, "kind:         %s\n", j.Kind)
	fmt.Fprintf(w, "name:         %s\n", sanitize(j.Name))
	fmt.Fprintf(w, "command:      %s\n", sanitize(strings.Join(j.Command, " ")))
	fmt.Fprintf(w, "schedule:     %s\n", sanitize(scheduleSummary(j)))
	fmt.Fprintf(w, "status:       %s\n", j.Status)
	fmt.Fprintf(w, "next fire:    %s\n", fmtTimeOrDash(j.NextFireAt))
	fmt.Fprintf(w, "last run:     %s\n", fmtTimeOrDash(j.LastRunAt))
	fmt.Fprintf(w, "last result:  %s\n", orDash(string(j.LastStatus)))
	fmt.Fprintf(w, "created:      %s\n", fmtLocal(j.CreatedAt))
	fmt.Fprintf(w, "updated:      %s\n", fmtLocal(j.UpdatedAt))
}

func writeStatus(w io.Writer, s repeat.Status) {
	supervised := "no"
	if s.Daemon.Supervised {
		supervised = "yes"
	}
	if s.Daemon.Running {
		fmt.Fprintf(w, "daemon:     running  pid=%d started=%s supervised=%s\n",
			s.Daemon.PID, fmtLocal(s.Daemon.StartedAt), supervised)
	} else {
		fmt.Fprintf(w, "daemon:     stopped  supervised=%s\n", supervised)
	}
	fmt.Fprintf(w, "data dir:   %s\n", s.DataDir)
	fmt.Fprintf(w, "database:   %s\n", s.DBPath)
	// Disabled counts appear only when nonzero, so the buckets always
	// sum to the totals without padding the common all-enabled case.
	fmt.Fprintf(w, "jobs:       total=%d  cron=%d", s.Jobs.Total, s.Jobs.Cron)
	if s.Jobs.CronDisabled > 0 {
		fmt.Fprintf(w, " (%d disabled)", s.Jobs.CronDisabled)
	}
	fmt.Fprintf(w, "  oneshot pending=%d  oneshot done=%d", s.Jobs.OneshotPending, s.Jobs.OneshotDone)
	if s.Jobs.OneshotDisabled > 0 {
		fmt.Fprintf(w, "  oneshot disabled=%d", s.Jobs.OneshotDisabled)
	}
	fmt.Fprintln(w)
	// Claimed-but-unfinished one-shots mean different things depending
	// on daemon state: executing right now, or stranded by a crash and
	// awaiting startup recovery. Zero is the normal case and prints
	// nothing.
	if s.Jobs.OneshotInFlight == 0 {
		return
	}
	if s.Daemon.Running {
		fmt.Fprintf(w, "in flight:  %d one-shot(s) running right now\n", s.Jobs.OneshotInFlight)
		return
	}
	fmt.Fprintf(w, "interrupted: %d one-shot(s) claimed but unfinished, the next daemon start marks them interrupted\n",
		s.Jobs.OneshotInFlight)
}

func scheduleSummary(j repeat.Job) string {
	switch j.Kind {
	case repeat.KindCron:
		return j.Cron
	case repeat.KindOneshot:
		return "at " + fmtLocal(j.FireAt)
	}
	return ""
}

// fmtLocal is the CLI's one display format for times.
func fmtLocal(t time.Time) string { return t.Local().Format(time.RFC3339) }

func fmtTimeOrDash(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return fmtLocal(t)
}

func orDash(s string) string { return cmp.Or(s, "-") }
