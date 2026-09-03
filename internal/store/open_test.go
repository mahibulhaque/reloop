package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mahibulhaque/reloop/internal/reloop"
)

func TestOpenInMemory(t *testing.T) {
	st, err := Open(t.Context(), "")
	if err != nil {
		t.Fatalf("Open in-memory: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if got := st.DBPath(); got != ":memory:" {
		t.Errorf("DBPath = %q, want %q", got, ":memory:")
	}
	if _, err := st.AddJob(t.Context(), reloop.JobSpec{
		Name: "mem", Command: []string{"true"}, Cron: "@hourly",
	}, time.Now()); err != nil {
		t.Errorf("AddJob on in-memory store: %v", err)
	}
}

func TestOpenMkdirFails(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if _, err := Open(t.Context(), file); err == nil {
		t.Errorf("Open with a file as data dir: want error, got nil")
	}
}

// realBusyError races two connections on one file with a 1ms busy
// timeout and returns the resulting SQLITE_BUSY error.
func realBusyError(t *testing.T) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "busy.db")
	dsn := "file:" + url.PathEscape(path) + "?_txlock=immediate&_pragma=busy_timeout(1)"

	db1, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open db1: %v", err)
	}
	t.Cleanup(func() { _ = db1.Close() })
	db1.SetMaxOpenConns(1)
	db2, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open db2: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	db2.SetMaxOpenConns(1)

	tx, err := db1.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("hold write lock: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	_, err = db2.ExecContext(t.Context(), `CREATE TABLE t (x INTEGER)`)
	if !sqliteCodeIs(err, sqliteBusy) {
		t.Fatalf("want SQLITE_BUSY, got %v", err)
	}
	return err
}

func TestRetryOnBusyRetriesThenSucceeds(t *testing.T) {
	busy := realBusyError(t)

	calls := 0
	err := retryOnBusy(t.Context(), func() error {
		calls++
		if calls < 3 {
			return busy
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryOnBusy: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestRetryOnBusyStopsOnContextCancel(t *testing.T) {
	busy := realBusyError(t)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err := retryOnBusy(ctx, func() error { return busy })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("retryOnBusy = %v, want context deadline", err)
	}
}
