package jobs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daniel-vuky/ops-copilot/internal/platform/adapters/sqlite"
	"github.com/daniel-vuky/ops-copilot/internal/platform/jobs"
)

// testNow is the fake clock's start, on a whole millisecond so it survives
// the round trip through Unix ms.
var testNow = time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

// fakeClock is a clock.Clock that moves only when the test advances it.
// It is safe for concurrent use.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// newFakeClock returns a fakeClock at testNow.
func newFakeClock() *fakeClock {
	return &fakeClock{now: testNow}
}

// Now returns the clock's current time.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// openStore opens a database with the embedded migrations in a temp dir and
// returns it with a job store on it.
func openStore(t *testing.T) (*sqlite.DB, *sqlite.JobStore) {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Options{
		Dir:        t.TempDir(),
		Migrations: sqlite.EmbeddedMigrations(),
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db, sqlite.NewJobStore(db)
}

// newScheduler returns a Scheduler on st and db with clk and a discarded
// log.
func newScheduler(t *testing.T, db *sqlite.DB, st *sqlite.JobStore, clk *fakeClock) *jobs.Scheduler {
	t.Helper()
	return jobs.New(st, db, jobs.Options{Clock: clk, Logger: slog.New(slog.DiscardHandler)})
}

// newLoggedScheduler is newScheduler with its log captured in a logBuffer.
func newLoggedScheduler(t *testing.T, db *sqlite.DB, st *sqlite.JobStore, clk *fakeClock) (*jobs.Scheduler, *logBuffer) {
	t.Helper()
	lb := &logBuffer{}
	return jobs.New(st, db, jobs.Options{Clock: clk, Logger: lb.logger()}), lb
}

// register registers k on s and fails the test on error.
func register(t *testing.T, s *jobs.Scheduler, k jobs.Kind) {
	t.Helper()
	if err := s.Register(k); err != nil {
		t.Fatalf("Register(%q): %v", k.Name, err)
	}
}

// noop is a handler that does nothing.
func noop(context.Context, jobs.Job) error { return nil }

// write runs fn in a Write on db and fails the test on error.
func write(t *testing.T, db *sqlite.DB, fn func(ctx context.Context) error) {
	t.Helper()
	if err := db.Write(t.Context(), fn); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

// exec runs one statement in its own Write on db.
func exec(t *testing.T, db *sqlite.DB, query string, args ...any) {
	t.Helper()
	write(t, db, func(ctx context.Context) error {
		q, err := db.WriteTx(ctx)
		if err != nil {
			return err
		}
		_, err = q.ExecContext(ctx, query, args...)
		return err
	})
}

// enqueue enqueues a job of kind in its own Write and returns its id.
func enqueue(t *testing.T, db *sqlite.DB, s *jobs.Scheduler, kind string, payload []byte, opts ...jobs.EnqueueOption) int64 {
	t.Helper()
	var id int64
	write(t, db, func(ctx context.Context) error {
		var err error
		id, err = s.Enqueue(ctx, kind, payload, opts...)
		return err
	})
	return id
}

// jobRow reads job id through the reader pool and fails the test when it
// is missing.
func jobRow(t *testing.T, db *sqlite.DB, id int64) jobs.Row {
	t.Helper()
	r, ok, err := sqlite.NewJobStore(db).Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get(%d): %v", id, err)
	}
	if !ok {
		t.Fatalf("Get(%d): not found", id)
	}
	return r
}

// count returns the result of a count query on the reader pool.
func count(t *testing.T, db *sqlite.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// logBuffer captures JSON log lines. It is safe for concurrent use.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p to the buffer.
func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns everything logged so far.
func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// logger returns a JSON logger at debug level writing to b.
func (b *logBuffer) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// lines returns every logged line with message msg, decoded.
func (b *logBuffer) lines(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}
