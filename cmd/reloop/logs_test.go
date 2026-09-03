package main

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mahibulhaque/reloop/internal/reloop"
	"github.com/mahibulhaque/reloop/internal/store"
)

// seedRun records one finished run through the real state machine:
// claim at started, finish at the same instant with output.
func seedRun(t *testing.T, st *store.Store, jobID reloop.JobID, started time.Time, output string) int64 {
	t.Helper()
	job, err := st.Job(t.Context(), jobID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	runID, ran, err := st.Claim(t.Context(), jobID, job.NextFireAt, job.NextFireAt.Add(time.Hour), started, 100)
	if err != nil || !ran {
		t.Fatalf("Claim = (run=%v, %v), want a claimed run", ran, err)
	}
	if err := st.Finish(t.Context(), runID, jobID, 0, reloop.RunOK, []byte(output), started); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return runID
}

func TestStreamLogsFollowEmitsEveryRunBetweenPolls(t *testing.T) {
	st := newStore(t)

	now := time.Now()
	job, err := st.AddJob(t.Context(), reloop.JobSpec{
		Name: "fast", Command: []string{"true"}, Cron: "@every 1s",
	}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	seedID := seedRun(t, st, job.ID, now.Add(-time.Second), "seed\n")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var out lockedBuffer
	done := make(chan error, 1)
	go func() {
		done <- streamLogs(ctx, st, job.ID, logOpts{Follow: true}, &out)
	}()

	if !waitForCmdTest(5*time.Second, func() bool {
		return strings.Contains(out.String(), "run #"+strconv.FormatInt(seedID, 10))
	}) {
		t.Errorf("follow output = %q, want initial run #%d", out.String(), seedID)
	}

	firstID := seedRun(t, st, job.ID, now.Add(time.Second), "first\n")
	secondID := seedRun(t, st, job.ID, now.Add(2*time.Second), "second\n")

	if !waitForCmdTest(5*time.Second, func() bool {
		got := out.String()
		return strings.Contains(got, "run #"+strconv.FormatInt(firstID, 10)) &&
			strings.Contains(got, "run #"+strconv.FormatInt(secondID, 10)) &&
			strings.Contains(got, "first\n") &&
			strings.Contains(got, "second\n")
	}) {
		t.Errorf("follow output = %q, want runs #%d and #%d", out.String(), firstID, secondID)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("streamLogs: %v", err)
	}
}

// TestStreamLogsFollowShowsHistoryWhileRunOpen checks the help matrix:
// --follow prints the most recent completed run even while a newer
// run is open, then streams the open run when it closes.
func TestStreamLogsFollowShowsHistoryWhileRunOpen(t *testing.T) {
	st := newStore(t)

	now := time.Now()
	job, err := st.AddJob(t.Context(), reloop.JobSpec{
		Name: "busy", Command: []string{"true"}, Cron: "@hourly",
	}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	doneID := seedRun(t, st, job.ID, now.Add(-time.Minute), "history\n")
	cur, err := st.Job(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	openID, ran, err := st.Claim(t.Context(), job.ID, cur.NextFireAt, cur.NextFireAt.Add(time.Hour), now, 100)
	if err != nil || !ran {
		t.Fatalf("Claim = (run=%v, %v), want a claimed run", ran, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var out lockedBuffer
	done := make(chan error, 1)
	go func() {
		done <- streamLogs(ctx, st, job.ID, logOpts{Follow: true}, &out)
	}()

	if !waitForCmdTest(5*time.Second, func() bool {
		got := out.String()
		return strings.Contains(got, "run #"+strconv.FormatInt(doneID, 10)) &&
			strings.Contains(got, "history\n")
	}) {
		t.Errorf("follow output = %q, want completed run #%d while a newer one is open", out.String(), doneID)
	}
	if strings.Contains(out.String(), "run #"+strconv.FormatInt(openID, 10)) {
		t.Errorf("follow output = %q, leaked open run #%d", out.String(), openID)
	}

	if err := st.Finish(t.Context(), openID, job.ID, 0, reloop.RunOK, []byte("fresh\n"), now.Add(time.Second)); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if !waitForCmdTest(5*time.Second, func() bool {
		return strings.Contains(out.String(), "fresh\n")
	}) {
		t.Errorf("follow output = %q, want the closed run streamed after finish", out.String())
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("streamLogs: %v", err)
	}
}

// TestStreamLogsFollowDoesNotLeapfrogOpenRun checks the follow cursor
// against the claim-time record ID: a long run holds an open record
// while a later overlap skip closes first. Emitting the skip must not
// move the cursor past the open record, or its output is lost when it
// finally closes.
func TestStreamLogsFollowDoesNotLeapfrogOpenRun(t *testing.T) {
	st := newStore(t)

	now := time.Now()
	job, err := st.AddJob(t.Context(), reloop.JobSpec{
		Name: "slow", Command: []string{"true"}, Cron: "@hourly",
	}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	next := now.Add(time.Hour)
	runID, run, err := st.Claim(t.Context(), job.ID, job.NextFireAt, next, now, 100)
	if err != nil || !run {
		t.Fatalf("Claim = (run=%v, %v), want a claimed run", run, err)
	}
	// The next fire closes as an overlap skip with a higher ID
	// while the first record is still open.
	if _, run, err := st.Claim(t.Context(), job.ID, next, next.Add(time.Hour), now.Add(time.Second), 100); err != nil || run {
		t.Fatalf("overlap Claim = (run=%v, %v), want (false, nil)", run, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var out lockedBuffer
	done := make(chan error, 1)
	go func() {
		done <- streamLogs(ctx, st, job.ID, logOpts{Follow: true}, &out)
	}()

	// Nothing may stream while the run is open.
	time.Sleep(3 * pollInterval)
	if got := out.String(); got != "" {
		t.Errorf("follow output while run open = %q, want empty", got)
	}

	if err := st.Finish(t.Context(), runID, job.ID, 0, reloop.RunOK, []byte("late\n"), now.Add(time.Minute)); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if !waitForCmdTest(5*time.Second, func() bool {
		got := out.String()
		return strings.Contains(got, "run #"+strconv.FormatInt(runID, 10)) &&
			strings.Contains(got, "late\n") &&
			strings.Contains(got, "skipped_overlap")
	}) {
		t.Errorf("follow output = %q, want run #%d with its output followed by the overlap skip", out.String(), runID)
	}
	// Strict ID order: the finished run streams before the skip that
	// closed earlier.
	got := out.String()
	if runPos, skipPos := strings.Index(got, "late\n"), strings.Index(got, "skipped_overlap"); runPos > skipPos {
		t.Errorf("follow output emitted the overlap skip before the run that held it:\n%s", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("streamLogs: %v", err)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func waitForCmdTest(d time.Duration, pred func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return pred()
}

// errAfterWriter accepts the first n writes and fails the rest.
type errAfterWriter struct {
	n   int
	err error
}

func (w *errAfterWriter) Write(p []byte) (int, error) {
	if w.n <= 0 {
		return 0, w.err
	}
	w.n--
	return len(p), nil
}

func TestTailLines(t *testing.T) {
	cases := []struct {
		name string
		buf  string
		n    int
		want string
	}{
		{name: "zero_n_returns_all", buf: "a\nb\n", n: 0, want: "a\nb\n"},
		{name: "empty_buf", buf: "", n: 3, want: ""},
		{name: "last_two", buf: "a\nb\nc\nd\n", n: 2, want: "c\nd\n"},
		{name: "no_trailing_newline", buf: "a\nb\nc", n: 1, want: "c"},
		{name: "fewer_lines_than_n", buf: "a\nb\n", n: 9, want: "a\nb\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(tailLines([]byte(tc.buf), tc.n)); got != tc.want {
				t.Errorf("tailLines(%q, %d) = %q, want %q", tc.buf, tc.n, got, tc.want)
			}
		})
	}
}

func TestEmitRunOutputMissingRun(t *testing.T) {
	st := newStore(t)
	if err := emitRunOutput(t.Context(), st, 9999, 0, &bytes.Buffer{}); err == nil {
		t.Errorf("emitRunOutput for a missing run: want error, got nil")
	}
}

// TestStreamLogsSinceParksBehindOpenRun: with --since and --follow, an
// open run and its overlap skip stay unstreamed and the cursor parks
// just before the open row.
func TestStreamLogsSinceParksBehindOpenRun(t *testing.T) {
	st := newStore(t)
	now := time.Now()
	job, err := st.AddJob(t.Context(), reloop.JobSpec{
		Name: "parked", Command: []string{"true"}, Cron: "@hourly",
	}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	doneID := seedRun(t, st, job.ID, now.Add(-time.Minute), "done\n")
	fresh, err := st.Job(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	next := fresh.NextFireAt.Add(time.Hour)
	openID, ran, err := st.Claim(t.Context(), job.ID, fresh.NextFireAt, next, now, 100)
	if err != nil || !ran {
		t.Fatalf("Claim = (run=%v, %v), want a claimed run", ran, err)
	}
	// The overlap skip closes with an ID above the open row.
	if _, ran, err := st.Claim(t.Context(), job.ID, next, next.Add(time.Hour), now.Add(time.Second), 100); err != nil || ran {
		t.Fatalf("overlap Claim = (run=%v, %v), want (false, nil)", ran, err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	if err := streamLogs(ctx, st, job.ID, logOpts{Since: time.Hour, Follow: true}, &out); err != nil {
		t.Fatalf("streamLogs: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "run #"+strconv.FormatInt(doneID, 10)) {
		t.Errorf("output = %q, want the completed run #%d", got, doneID)
	}
	if strings.Contains(got, "run #"+strconv.FormatInt(openID, 10)) {
		t.Errorf("output = %q, must not stream past the open run #%d", got, openID)
	}
	if strings.Contains(got, "skipped_overlap") {
		t.Errorf("output = %q, must not stream the skip past the open run", got)
	}
}

func TestStreamLogsSinceStoreError(t *testing.T) {
	st := newStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := streamLogs(ctx, st, "xxxxx", logOpts{Since: time.Hour}, &bytes.Buffer{}); err == nil {
		t.Errorf("streamLogs with a cancelled context: want error, got nil")
	}
}

// TestStreamLogsFollowStartsEmpty: following a job with no history
// parks at zero and streams nothing until a run lands.
func TestStreamLogsFollowStartsEmpty(t *testing.T) {
	st := newStore(t)
	job, err := st.AddJob(t.Context(), reloop.JobSpec{
		Name: "silent", Command: []string{"true"}, Cron: "@hourly",
	}, time.Now())
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	for _, opts := range []logOpts{{Follow: true}, {Since: time.Hour, Follow: true}} {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		var out bytes.Buffer
		if err := streamLogs(ctx, st, job.ID, opts, &out); err != nil {
			t.Errorf("streamLogs(%+v): %v", opts, err)
		}
		cancel()
		if out.Len() != 0 {
			t.Errorf("streamLogs(%+v) output = %q, want empty", opts, out.String())
		}
	}
}

// TestStreamLogsSurfacesWriteErrors: a failing writer aborts the
// stream instead of spinning, on both replay paths.
func TestStreamLogsSurfacesWriteErrors(t *testing.T) {
	st := newStore(t)
	now := time.Now()
	job, err := st.AddJob(t.Context(), reloop.JobSpec{
		Name: "brokenpipe", Command: []string{"true"}, Cron: "@hourly",
	}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	seedRun(t, st, job.ID, now.Add(-time.Minute), "old\n")

	// Default follow shape: the latest completed run replays first.
	boom := errors.New("boom")
	w := &errAfterWriter{err: boom}
	if err := streamLogs(t.Context(), st, job.ID, logOpts{Follow: true}, w); !errors.Is(err, boom) {
		t.Errorf("streamLogs(follow) = %v, want the write error", err)
	}

	// Open-run shape: the history replay before the park also fails.
	fresh, err := st.Job(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if _, ran, err := st.Claim(t.Context(), job.ID, fresh.NextFireAt, fresh.NextFireAt.Add(time.Hour), now, 100); err != nil || !ran {
		t.Fatalf("Claim = (run=%v, %v), want a claimed run", ran, err)
	}
	w = &errAfterWriter{err: boom}
	if err := streamLogs(t.Context(), st, job.ID, logOpts{Follow: true}, w); !errors.Is(err, boom) {
		t.Errorf("streamLogs(follow, open run) = %v, want the write error", err)
	}
}

// TestStreamLogsFollowLoopSurfacesWriteError: a write failure on a run
// that lands mid-follow also aborts the stream.
func TestStreamLogsFollowLoopSurfacesWriteError(t *testing.T) {
	st := newStore(t)
	now := time.Now()
	job, err := st.AddJob(t.Context(), reloop.JobSpec{
		Name: "midflight", Command: []string{"true"}, Cron: "@hourly",
	}, now)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	boom := errors.New("boom")
	done := make(chan error, 1)
	go func() {
		done <- streamLogs(t.Context(), st, job.ID, logOpts{Follow: true}, &errAfterWriter{err: boom})
	}()
	seedRun(t, st, job.ID, now, "late\n")

	select {
	case err := <-done:
		if !errors.Is(err, boom) {
			t.Errorf("streamLogs = %v, want the write error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("streamLogs never surfaced the write error")
	}
}
