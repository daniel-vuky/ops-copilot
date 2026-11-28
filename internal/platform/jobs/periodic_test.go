package jobs_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daniel-vuky/ops-copilot/internal/platform/adapters/sqlite"
	"github.com/daniel-vuky/ops-copilot/internal/platform/jobs"
)

// periodRow is a recurring job's row, as the period tests check it.
type periodRow struct {
	periodAt  time.Time
	uniqueKey string
	state     string
}

// periodRows returns the jobs of kind that have a period, oldest first.
func periodRows(t *testing.T, db *sqlite.DB, kind string) []periodRow {
	t.Helper()
	rows, err := db.Reader().QueryContext(t.Context(),
		"SELECT period_at, unique_key, state FROM jobs WHERE kind = ? AND period_at IS NOT NULL ORDER BY id", kind)
	if err != nil {
		t.Fatalf("query period rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []periodRow
	for rows.Next() {
		var (
			at int64
			r  periodRow
		)
		if err := rows.Scan(&at, &r.uniqueKey, &r.state); err != nil {
			t.Fatalf("scan period row: %v", err)
		}
		r.periodAt = time.UnixMilli(at).UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("period rows: %v", err)
	}
	return out
}

// blocker is a handler that signals started and blocks until release is
// closed. release is closed at cleanup too, so a failing test never leaves
// a handler blocked.
type blocker struct {
	started chan jobs.Job
	release chan struct{}
	once    sync.Once
}

// newBlocker returns a blocker whose release is closed at cleanup.
func newBlocker(t *testing.T) *blocker {
	t.Helper()
	b := &blocker{started: make(chan jobs.Job, 16), release: make(chan struct{})}
	t.Cleanup(b.unblock)
	return b
}

// handler is the blocking Handler.
func (b *blocker) handler(_ context.Context, job jobs.Job) error {
	b.started <- job
	<-b.release
	return nil
}

// unblock releases every handler, once.
func (b *blocker) unblock() {
	b.once.Do(func() { close(b.release) })
}

func TestPeriodInsertedOncePerPeriod(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	clk.Advance(10 * time.Minute) // 12:10, inside the 12:00 period
	s := newScheduler(t, db, st, clk)
	register(t, s, jobs.Kind{Name: "hourly", Every: time.Hour, Handler: noop})
	mustRecover(t, s)

	for range 3 {
		tickAndWait(t, s)
		clk.Advance(time.Minute)
	}
	rows := periodRows(t, db, "hourly")
	if len(rows) != 1 {
		t.Fatalf("period rows after three ticks = %+v, want one", rows)
	}
	if want := testNow; !rows[0].periodAt.Equal(want) || rows[0].uniqueKey != "period" || rows[0].state != "succeeded" {
		t.Errorf("period row = %+v, want period_at %v, key period, succeeded", rows[0], want)
	}

	clk.Advance(time.Hour)
	tickAndWait(t, s)
	rows = periodRows(t, db, "hourly")
	if len(rows) != 2 {
		t.Fatalf("period rows in the next period = %+v, want two", rows)
	}
	if want := testNow.Add(time.Hour); !rows[1].periodAt.Equal(want) {
		t.Errorf("second period_at = %v, want %v", rows[1].periodAt, want)
	}
}

func TestPeriodSurvivesRestart(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	kind := jobs.Kind{Name: "hourly", Every: time.Hour, Handler: noop}

	s1 := newScheduler(t, db, st, clk)
	register(t, s1, kind)
	mustRecover(t, s1)
	tickAndWait(t, s1)
	if rows := periodRows(t, db, "hourly"); len(rows) != 1 || rows[0].state != "succeeded" {
		t.Fatalf("period rows on the first scheduler = %+v, want one succeeded", rows)
	}

	clk.Advance(5 * time.Minute)
	s2 := newScheduler(t, db, st, clk)
	register(t, s2, kind)
	mustRecover(t, s2)
	tickAndWait(t, s2)
	if rows := periodRows(t, db, "hourly"); len(rows) != 1 {
		t.Errorf("period rows after a restart in the same period = %+v, want still one", rows)
	}
}

func TestPeriodSkippedWhileActive(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	s := newScheduler(t, db, st, clk)
	b := newBlocker(t)
	register(t, s, jobs.Kind{Name: "hourly", Every: time.Hour, Handler: b.handler})
	mustRecover(t, s)

	s.TickForTest(t.Context())
	recv(t, b.started, "the first period's handler")
	clk.Advance(time.Hour)
	s.TickForTest(t.Context())
	if rows := periodRows(t, db, "hourly"); len(rows) != 1 {
		t.Fatalf("period rows while the first runs = %+v, want one (next period skipped)", rows)
	}

	b.unblock()
	s.WaitJobsForTest()
	clk.Advance(time.Minute)
	tickAndWait(t, s)
	if rows := periodRows(t, db, "hourly"); len(rows) != 1 {
		t.Fatalf("period rows later in the skipped period = %+v, want still one", rows)
	}

	clk.Advance(time.Hour)
	tickAndWait(t, s)
	rows := periodRows(t, db, "hourly")
	if len(rows) != 2 || !rows[1].periodAt.Equal(testNow.Add(2*time.Hour)) {
		t.Errorf("period rows in the next period = %+v, want a second one at %v", rows, testNow.Add(2*time.Hour))
	}
}

func TestPeriodOffset(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	clk.Advance(9 * time.Hour) // 2026-11-28T21:00:00Z
	s := newScheduler(t, db, st, clk)
	register(t, s, jobs.Kind{Name: "daily", Every: 24 * time.Hour, Offset: 20 * time.Hour, Handler: noop})
	mustRecover(t, s)

	tickAndWait(t, s)
	rows := periodRows(t, db, "daily")
	want := time.Date(2026, 11, 28, 20, 0, 0, 0, time.UTC)
	if len(rows) != 1 || !rows[0].periodAt.Equal(want) {
		t.Errorf("period rows = %+v, want one at %v", rows, want)
	}
}

func TestManualAndPeriodShareKey(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	b := newBlocker(t)
	register(t, s, jobs.Kind{Name: "scan", Every: 24 * time.Hour, PeriodKey: "all", Handler: b.handler})
	mustRecover(t, s)

	manual := enqueue(t, db, s, "scan", nil, jobs.UniqueKey("all"))
	s.TickForTest(t.Context())
	if job := recv(t, b.started, "the manual handler"); job.ID != manual {
		t.Fatalf("started job %d, want the manual job %d", job.ID, manual)
	}
	if rows := periodRows(t, db, "scan"); len(rows) != 0 {
		t.Errorf("period rows while a manual run is active = %+v, want none", rows)
	}

	err := db.Write(t.Context(), func(ctx context.Context) error {
		_, err := s.Enqueue(ctx, "scan", nil, jobs.UniqueKey("all"))
		return err
	})
	if !errors.Is(err, jobs.ErrAlreadyActive) {
		t.Errorf("second manual Enqueue = %v, want ErrAlreadyActive", err)
	}
	b.unblock()
	s.WaitJobsForTest()
}

func TestPeriodInsertFailureBacksOffPerKind(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	tx := &failingWriteTx{Transactor: db}
	s, lb := newLoggedSchedulerTx(t, st, tx, clk)
	register(t, s, jobs.Kind{Name: "hourly", Every: time.Hour, Handler: noop})
	mustRecover(t, s)
	tx.calls.Store(0)

	tx.fail.Store(true)
	s.TickForTest(t.Context())
	s.TickForTest(t.Context())
	lines := lb.lines(t, "job scheduler step failed")
	if len(lines) != 1 || lines[0]["step"] != "periods:hourly" {
		t.Fatalf("step failed lines = %v, want one for periods:hourly", lines)
	}
	if n := tx.calls.Load(); n != 1 {
		t.Errorf("Writes while the period step backs off = %d, want 1", n)
	}

	tx.fail.Store(false)
	clk.Advance(time.Second)
	tickAndWait(t, s)
	if rows := periodRows(t, db, "hourly"); len(rows) != 1 || rows[0].state != "succeeded" {
		t.Errorf("period rows after the backoff = %+v, want one succeeded", rows)
	}
}

func TestSweepRequeuesOrphan(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	s, fs, lb := newFlakyScheduler(t, db, st, clk)
	s.SetTimingsForTest(jobs.TestTimings{FinishRetries: []time.Duration{0, 0, 0}})
	b := newBlocker(t)
	var orphan int64
	register(t, s, jobs.Kind{Name: "work", Handler: func(ctx context.Context, job jobs.Job) error {
		if job.ID == orphan {
			return nil
		}
		return b.handler(ctx, job)
	}})
	mustRecover(t, s)

	orphan = enqueue(t, db, s, "work", nil)
	fs.failSucceed.Store(orphan)
	tickAndWait(t, s)
	if r := jobRow(t, db, orphan); r.State != jobs.StateRunning || r.Attempts != 1 {
		t.Fatalf("job after a lost finish = %q attempts %d, want running, 1", r.State, r.Attempts)
	}

	// The slot was released: a second job of the Concurrency 1 kind starts,
	// and it is still running when the sweep comes.
	held := enqueue(t, db, s, "work", nil)
	s.TickForTest(t.Context())
	recv(t, b.started, "the second job's handler")

	clk.Advance(time.Minute)
	s.TickForTest(t.Context())
	r := jobRow(t, db, orphan)
	if r.State != jobs.StateQueued || r.LastError != "finish not recorded" || r.Attempts != 1 {
		t.Errorf("orphan after the sweep = %q last_error %q attempts %d, want queued, %q, 1",
			r.State, r.LastError, r.Attempts, "finish not recorded")
	}
	if r := jobRow(t, db, held); r.State != jobs.StateRunning {
		t.Errorf("job held by this process after the sweep = %q, want still running", r.State)
	}
	lines := lb.lines(t, "orphaned jobs requeued")
	if len(lines) != 1 || lines[0]["level"] != "WARN" || lines[0]["count"] != float64(1) {
		t.Errorf("orphaned jobs requeued lines = %v, want one WARN line with count 1", lines)
	}

	b.unblock()
	s.WaitJobsForTest()
}

func TestSweepIdleUsesNoWriter(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	tx := &countingTx{Transactor: db}
	s := jobs.New(st, tx, jobs.Options{Clock: clk, Logger: discard()})
	b := newBlocker(t)
	register(t, s, jobs.Kind{Name: "work", Handler: b.handler})
	mustRecover(t, s)

	tx.writes.Store(0)
	clk.Advance(2 * time.Minute)
	s.TickForTest(t.Context())
	if n := tx.writes.Load(); n != 0 {
		t.Errorf("Writes of an idle sweep = %d, want 0", n)
	}

	// A running job held by this process does not make the sweep write.
	enqueue(t, db, s, "work", nil)
	s.TickForTest(t.Context())
	recv(t, b.started, "the handler")
	tx.writes.Store(0)
	clk.Advance(2 * time.Minute)
	s.TickForTest(t.Context())
	if n := tx.writes.Load(); n != 0 {
		t.Errorf("Writes of a sweep with only held jobs = %d, want 0", n)
	}
	b.unblock()
	s.WaitJobsForTest()
}

func TestStepBackoff(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	s, fs, lb := newFlakyScheduler(t, db, st, clk)
	register(t, s, jobs.Kind{Name: "work", Handler: noop})
	mustRecover(t, s)

	fs.failDue.Store(true)
	for range 5 {
		s.TickForTest(t.Context())
	}
	lines := lb.lines(t, "job scheduler step failed")
	if len(lines) != 1 {
		t.Fatalf("step failed lines after 5 ticks = %v, want one", lines)
	}
	if l := lines[0]; l["level"] != "ERROR" || l["step"] != "due" || !strings.Contains(l["error"].(string), errInjected.Error()) {
		t.Errorf("step failed line = %v, want ERROR, step due, the store error", l)
	}
	if n := fs.dueCalls.Load(); n != 1 {
		t.Errorf("DueKinds calls in backoff = %d, want 1 (later ticks skip the step)", n)
	}

	// The second failure backs off 2 s: not run after 1 s, run after 2 s.
	clk.Advance(time.Second)
	s.TickForTest(t.Context())
	clk.Advance(time.Second)
	s.TickForTest(t.Context())
	if n := fs.dueCalls.Load(); n != 2 {
		t.Errorf("DueKinds calls = %d, want 2 (1 s backoff over, 2 s backoff running)", n)
	}
	clk.Advance(time.Second)
	s.TickForTest(t.Context())
	if n := fs.dueCalls.Load(); n != 3 {
		t.Errorf("DueKinds calls = %d, want 3 (2 s backoff over)", n)
	}
	if lines := lb.lines(t, "job scheduler step failed"); len(lines) != 1 {
		t.Errorf("step failed lines within a minute = %d, want 1", len(lines))
	}

	// After a minute the step runs again and, once it works, claims.
	fs.failDue.Store(false)
	id := enqueue(t, db, s, "work", nil)
	clk.Advance(time.Minute)
	tickAndWait(t, s)
	if r := jobRow(t, db, id); r.State != jobs.StateSucceeded {
		t.Errorf("job after the step recovered = %q, want succeeded", r.State)
	}
}

func TestStepBackoffCapAndLogRate(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	s, fs, lb := newFlakyScheduler(t, db, st, clk)
	register(t, s, jobs.Kind{Name: "work", Handler: noop})
	mustRecover(t, s)

	// Backoffs 1, 2, 4, 8, 16, 30, 30, 30 s: ticking 31 s apart runs the
	// step every time; it logs at 0, 62, 124 and 186 s only.
	fs.failDue.Store(true)
	for range 8 {
		s.TickForTest(t.Context())
		clk.Advance(31 * time.Second)
	}
	if n := fs.dueCalls.Load(); n != 8 {
		t.Errorf("DueKinds calls 31 s apart = %d, want 8 (backoff capped at 30 s)", n)
	}
	if lines := lb.lines(t, "job scheduler step failed"); len(lines) != 4 {
		t.Errorf("step failed lines over 217 s = %d, want 4 (at most one a minute)", len(lines))
	}
}

func TestNoStepFailureWhileShuttingDown(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	s, fs, lb := newFlakyScheduler(t, db, st, clk)
	register(t, s, jobs.Kind{Name: "work", Handler: noop})
	mustRecover(t, s)
	id := enqueue(t, db, s, "work", nil)

	// Run's ctx ends while the due-kinds step runs, and the step fails.
	ctx, cancel := context.WithCancel(t.Context())
	fs.failDue.Store(true)
	fs.onDue = cancel
	s.TickForTest(ctx)
	if out := lb.String(); strings.Contains(out, "step failed") {
		t.Errorf("log after a step failed while shutting down:\n%s", out)
	}
	if r := jobRow(t, db, id); r.State != jobs.StateQueued {
		t.Fatalf("job after the cancelled tick = %q, want queued", r.State)
	}

	// No backoff either: the next tick runs the step at once.
	fs.failDue.Store(false)
	fs.onDue = nil
	tickAndWait(t, s)
	if r := jobRow(t, db, id); r.State != jobs.StateSucceeded {
		t.Errorf("job on the next tick = %q, want succeeded (no backoff)", r.State)
	}
}

func TestFinishHookFailureThenSweep(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	s, lb := newLoggedScheduler(t, db, st, clk)
	s.SetTimingsForTest(jobs.TestTimings{FinishRetries: []time.Duration{0, 0, 0}})
	var (
		calls   atomic.Int32
		lastErr atomic.Value
	)
	register(t, s, jobs.Kind{
		Name: "work", NoRetry: true,
		Handler: func(context.Context, jobs.Job) error { return errors.New("boom") },
		OnFailed: func(_ context.Context, _ jobs.Job, _ jobs.Reason, last string) error {
			lastErr.Store(last)
			if calls.Add(1) <= 3 {
				return errors.New(`owner update failed: Post "https://u:pw@owner.example.test/x?token=t"`)
			}
			return nil
		},
	})
	mustRecover(t, s)

	id := enqueue(t, db, s, "work", nil)
	tickAndWait(t, s)
	if r := jobRow(t, db, id); r.State != jobs.StateRunning {
		t.Fatalf("job after three failed finish tries = %q, want running", r.State)
	}
	tries := lb.lines(t, "job finish try failed")
	if len(tries) != 2 {
		t.Fatalf("job finish try failed lines = %v, want two (tries 1 and 2)", tries)
	}
	for i, l := range tries {
		text, _ := l["error"].(string)
		if l["level"] != "WARN" || l["try"] != float64(i+1) || l["id"] != float64(id) ||
			!strings.Contains(text, "https://owner.example.test") || strings.Contains(text, "token=") {
			t.Errorf("finish try line %d = %v, want WARN, try %d, the scrubbed hook error", i, l, i+1)
		}
	}
	if lines := lb.lines(t, "job finish not recorded"); len(lines) != 1 {
		t.Errorf("job finish not recorded lines = %v, want one", lines)
	}

	clk.Advance(time.Minute)
	tickAndWait(t, s) // the sweep, last in the tick
	if r := jobRow(t, db, id); r.State != jobs.StateQueued || r.LastError != "finish not recorded" || r.Attempts != 1 {
		t.Fatalf("job after the sweep = %q last_error %q attempts %d, want queued, finish not recorded, 1",
			r.State, r.LastError, r.Attempts)
	}

	tickAndWait(t, s) // the claim-time path at max attempts
	if r := jobRow(t, db, id); r.State != jobs.StateFailed || r.LastError != "finish not recorded" {
		t.Errorf("job after the claim-time path = %q last_error %q, want failed, finish not recorded", r.State, r.LastError)
	}
	if n := calls.Load(); n != 4 {
		t.Errorf("hook calls = %d, want 4", n)
	}
	if last, _ := lastErr.Load().(string); last != "finish not recorded" {
		t.Errorf("hook's last error on the claim-time path = %q, want finish not recorded", last)
	}
}
