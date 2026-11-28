package jobs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daniel-vuky/ops-copilot/internal/platform/adapters/sqlite"
	"github.com/daniel-vuky/ops-copilot/internal/platform/jobs"
	"github.com/daniel-vuky/ops-copilot/internal/platform/store"
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

// errInjected is the error a flakyStore returns.
var errInjected = errors.New("injected store failure")

// flakyStore is a job store whose methods fail on demand. It is safe for
// concurrent use.
type flakyStore struct {
	*sqlite.JobStore
	failDue     atomic.Bool  // DueKinds fails while set
	dueCalls    atomic.Int64 // DueKinds calls so far
	failRunning atomic.Int32 // MarkRunning fails while above zero
	failSucceed atomic.Int64 // MarkSucceeded fails for this job id; 0 means none
	// succeedCalls, when not nil, gets a value (without blocking) on each
	// MarkSucceeded call.
	succeedCalls chan struct{}
	// onDue and onRunning, when not nil, run in each DueKinds and
	// MarkRunning call. Set them before the tick that uses them, on the
	// goroutine that ticks.
	onDue     func()
	onRunning func()
	// failRunningIDs makes RunningIDs fail while set; runningIDCalls
	// counts its calls.
	failRunningIDs atomic.Bool
	runningIDCalls atomic.Int64
}

// newFlakyStore returns a flakyStore on st that fails nothing yet.
func newFlakyStore(st *sqlite.JobStore) *flakyStore {
	return &flakyStore{JobStore: st, succeedCalls: make(chan struct{}, 16)}
}

// DueKinds counts the call and fails while failDue is set.
func (f *flakyStore) DueKinds(ctx context.Context, now time.Time, skip []int64) ([]string, error) {
	f.dueCalls.Add(1)
	if f.onDue != nil {
		f.onDue()
	}
	if f.failDue.Load() {
		return nil, errInjected
	}
	return f.JobStore.DueKinds(ctx, now, skip)
}

// MarkRunning fails while failRunning is above zero.
func (f *flakyStore) MarkRunning(ctx context.Context, id int64, now time.Time) (bool, error) {
	if f.onRunning != nil {
		f.onRunning()
	}
	if f.failRunning.Add(-1) >= 0 {
		return false, errInjected
	}
	return f.JobStore.MarkRunning(ctx, id, now)
}

// RunningIDs counts the call and fails while failRunningIDs is set.
func (f *flakyStore) RunningIDs(ctx context.Context) ([]int64, error) {
	f.runningIDCalls.Add(1)
	if f.failRunningIDs.Load() {
		return nil, errInjected
	}
	return f.JobStore.RunningIDs(ctx)
}

// MarkSucceeded signals the call and fails for the job failSucceed names.
func (f *flakyStore) MarkSucceeded(ctx context.Context, id int64, now time.Time) (bool, error) {
	select {
	case f.succeedCalls <- struct{}{}:
	default:
	}
	if id == f.failSucceed.Load() {
		return false, errInjected
	}
	return f.JobStore.MarkSucceeded(ctx, id, now)
}

// newFlakyScheduler returns a Scheduler on a flakyStore over st, with clk
// and its log captured.
func newFlakyScheduler(t *testing.T, db *sqlite.DB, st *sqlite.JobStore, clk *fakeClock) (*jobs.Scheduler, *flakyStore, *logBuffer) {
	t.Helper()
	fs := newFlakyStore(st)
	lb := &logBuffer{}
	return jobs.New(fs, db, jobs.Options{Clock: clk, Logger: lb.logger()}), fs, lb
}

// failingWriteTx is a store.Transactor that counts its Write calls and
// fails them while fail is set.
type failingWriteTx struct {
	store.Transactor
	fail  atomic.Bool
	calls atomic.Int64
}

// Write counts the call, then fails or defers to the wrapped Transactor.
func (f *failingWriteTx) Write(ctx context.Context, fn func(ctx context.Context) error) error {
	f.calls.Add(1)
	if f.fail.Load() {
		return errInjected
	}
	return f.Transactor.Write(ctx, fn)
}

// newLoggedSchedulerTx returns a Scheduler on st writing through tx, with
// clk and its log captured.
func newLoggedSchedulerTx(t *testing.T, st *sqlite.JobStore, tx store.Transactor, clk *fakeClock) (*jobs.Scheduler, *logBuffer) {
	t.Helper()
	lb := &logBuffer{}
	return jobs.New(st, tx, jobs.Options{Clock: clk, Logger: lb.logger()}), lb
}

// discard returns a logger that drops everything.
func discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
