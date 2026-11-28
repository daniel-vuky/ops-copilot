package jobs_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daniel-vuky/ops-copilot/internal/platform/jobs"
)

// startRun runs s.Run in a goroutine with a ctx of its own. The returned
// stop cancels that ctx and returns Run's error; it fails the test if Run
// does not return within 5 s. Cleanup calls stop too.
func startRun(t *testing.T, s *jobs.Scheduler) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx) }()
	var (
		once sync.Once
		err  error
	)
	stop = func() error {
		once.Do(func() {
			cancel()
			err = recv(t, errc, "Run to return")
		})
		return err
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

func TestRunRequiresRecover(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{Name: "work", Handler: noop})
	if err := s.Run(t.Context()); !errors.Is(err, jobs.ErrNotRecovered) {
		t.Errorf("Run before Recover = %v, want ErrNotRecovered", err)
	}
}

func TestRunOnlyOnce(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	mustRecover(t, s)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatalf("first Run = %v, want nil", err)
	}
	if err := s.Run(ctx); err == nil {
		t.Error("second Run = nil, want an error")
	}
}

func TestRunPicksUpEnqueued(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := jobs.New(st, db, jobs.Options{Logger: discard()}) // real clock
	s.SetTimingsForTest(jobs.TestTimings{Poll: 10 * time.Millisecond})
	ran := make(chan struct{}, 1)
	register(t, s, jobs.Kind{Name: "work", Handler: func(context.Context, jobs.Job) error {
		ran <- struct{}{}
		return nil
	}})
	mustRecover(t, s)
	stop := startRun(t, s)

	id := enqueue(t, db, s, "work", nil)
	recv(t, ran, "the handler")
	waitUntil(t, "the job to succeed", func() bool { return jobRow(t, db, id).State == jobs.StateSucceeded })
	if err := stop(); err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
}

func TestShutdownDrainsFinishingJob(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	s.SetTimingsForTest(jobs.TestTimings{Poll: 10 * time.Millisecond})
	started := make(chan struct{}, 1)
	shutdown := make(chan struct{})
	register(t, s, jobs.Kind{Name: "work", Handler: func(context.Context, jobs.Job) error {
		started <- struct{}{}
		<-shutdown
		time.Sleep(50 * time.Millisecond) // the work left after shutdown begins
		return nil
	}})
	mustRecover(t, s)
	stop := startRun(t, s)

	id := enqueue(t, db, s, "work", nil)
	recv(t, started, "the handler")
	close(shutdown)
	if err := stop(); err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
	if r := jobRow(t, db, id); r.State != jobs.StateSucceeded {
		t.Errorf("job = %q, want succeeded within the drain", r.State)
	}
}

func TestShutdownCancelsSlowJob(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	s.SetTimingsForTest(jobs.TestTimings{Poll: 10 * time.Millisecond, Drain: 50 * time.Millisecond})
	started := make(chan struct{}, 1)
	register(t, s, jobs.Kind{Name: "work", Handler: func(ctx context.Context, _ jobs.Job) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}})
	mustRecover(t, s)
	stop := startRun(t, s)

	id := enqueue(t, db, s, "work", nil)
	recv(t, started, "the handler")
	if err := stop(); err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
	r := jobRow(t, db, id)
	if r.State != jobs.StateQueued || r.LastError != "interrupted by shutdown" || r.Attempts != 1 {
		t.Errorf("job = %q last_error %q attempts %d, want queued, interrupted by shutdown, 1",
			r.State, r.LastError, r.Attempts)
	}
}

func TestShutdownReportsStuckHandler(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	s.SetTimingsForTest(jobs.TestTimings{
		Poll: 10 * time.Millisecond, Drain: 50 * time.Millisecond, CancelWait: 50 * time.Millisecond,
	})
	b := newBlocker(t)
	register(t, s, jobs.Kind{Name: "work", Handler: b.handler})
	mustRecover(t, s)
	stop := startRun(t, s)

	id := enqueue(t, db, s, "work", nil)
	recv(t, b.started, "the handler")
	err := stop()
	if err == nil || !strings.Contains(err.Error(), "1 handlers still running") {
		t.Errorf("Run = %v, want an error naming 1 handler still running", err)
	}
	if r := jobRow(t, db, id); r.State != jobs.StateRunning {
		t.Errorf("job = %q, want still running", r.State)
	}
	b.unblock()
	s.WaitJobsForTest()
}

func TestNoClaimAfterShutdown(t *testing.T) {
	t.Parallel()

	t.Run("ctx done before Run", func(t *testing.T) {
		t.Parallel()
		db, st := openStore(t)
		s := newScheduler(t, db, st, newFakeClock())
		var ran atomic.Bool
		register(t, s, jobs.Kind{Name: "work", Handler: func(context.Context, jobs.Job) error {
			ran.Store(true)
			return nil
		}})
		mustRecover(t, s)
		id := enqueue(t, db, s, "work", nil)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := s.Run(ctx); err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
		if ran.Load() {
			t.Error("handler ran after shutdown")
		}
		if r := jobRow(t, db, id); r.State != jobs.StateQueued {
			t.Errorf("job = %q, want queued", r.State)
		}
	})

	t.Run("enqueued while draining", func(t *testing.T) {
		t.Parallel()
		db, st := openStore(t)
		s := newScheduler(t, db, st, newFakeClock())
		s.SetTimingsForTest(jobs.TestTimings{Poll: 10 * time.Millisecond})
		b := newBlocker(t)
		var ran atomic.Bool
		register(t, s, jobs.Kind{Name: "slow", Handler: b.handler})
		register(t, s, jobs.Kind{Name: "work", Handler: func(context.Context, jobs.Job) error {
			ran.Store(true)
			return nil
		}})
		mustRecover(t, s)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		errc := make(chan error, 1)
		go func() { errc <- s.Run(ctx) }()

		enqueue(t, db, s, "slow", nil)
		recv(t, b.started, "the slow handler")
		cancel() // Run now drains, waiting for the slow handler

		// A loop still ticking would claim this job within a poll; give it
		// ten polls before letting Run return.
		id := enqueue(t, db, s, "work", nil)
		time.Sleep(100 * time.Millisecond) // a negative check window, not ordering
		b.unblock()
		if err := recv(t, errc, "Run to return"); err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
		if ran.Load() {
			t.Error("a job enqueued after shutdown began was run")
		}
		if r := jobRow(t, db, id); r.State != jobs.StateQueued || r.Attempts != 0 {
			t.Errorf("job = %q attempts %d, want queued, 0", r.State, r.Attempts)
		}
	})
}

func TestFinishDelaysCutByDrain(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	s, fs, lb := newFlakyScheduler(t, db, st, clk)
	s.SetTimingsForTest(jobs.TestTimings{
		Poll: 10 * time.Millisecond, Drain: 50 * time.Millisecond,
		FinishRetries: []time.Duration{0, time.Hour, time.Hour},
	})
	register(t, s, jobs.Kind{Name: "work", Handler: noop})
	mustRecover(t, s)

	// The job's id is the first one: fail its MarkSucceeded from the start.
	fs.failSucceed.Store(1)
	stop := startRun(t, s)
	id := enqueue(t, db, s, "work", nil)
	if id != 1 {
		t.Fatalf("job id = %d, want 1", id)
	}
	recv(t, fs.succeedCalls, "the first finish try")

	// The finish now waits an hour before its second try; after the drain
	// the remaining tries run at once, and Run returns within its budget.
	if err := stop(); err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
	if lines := lb.lines(t, "job finish not recorded"); len(lines) != 1 {
		t.Errorf("job finish not recorded lines = %v, want one", lines)
	}
	if lines := lb.lines(t, "job finish try failed"); len(lines) != 2 {
		t.Errorf("job finish try failed lines = %v, want two", lines)
	}
}

func TestHandlerGoexitFinishes(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	s := newScheduler(t, db, st, clk)
	register(t, s, jobs.Kind{Name: "work", Handler: func(_ context.Context, job jobs.Job) error {
		if job.Attempt == 1 {
			runtime.Goexit()
		}
		return nil
	}})
	mustRecover(t, s)

	id := enqueue(t, db, s, "work", nil)
	tickAndWait(t, s)
	r := jobRow(t, db, id)
	if r.State != jobs.StateQueued || r.Attempts != 1 || !strings.Contains(r.LastError, "runtime.Goexit") {
		t.Fatalf("job after Goexit = %q attempts %d last_error %q, want queued for retry, 1, naming runtime.Goexit",
			r.State, r.Attempts, r.LastError)
	}

	// The slot was released: the Concurrency 1 kind runs the retry.
	clk.Advance(30 * time.Second)
	tickAndWait(t, s)
	if r := jobRow(t, db, id); r.State != jobs.StateSucceeded {
		t.Errorf("job after the retry = %q, want succeeded", r.State)
	}
}

// nilPtrError is an error whose Error dereferences its receiver.
type nilPtrError struct{ msg string }

// Error returns the message; it panics on a nil receiver.
func (e *nilPtrError) Error() string { return e.msg }

// panicError is an error whose Error panics.
type panicError struct{}

// Error panics.
func (panicError) Error() string { panic("error text exploded") }

func TestHandlerErrorTextPanics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  func() error
		want string
	}{
		{"typed nil", func() error { var e *nilPtrError; return e }, "panic: runtime error"},
		{"panicking Error", func() error { return panicError{} }, "panic: error text exploded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db, st := openStore(t)
			s := newScheduler(t, db, st, newFakeClock())
			register(t, s, jobs.Kind{Name: "work", NoRetry: true, Handler: func(context.Context, jobs.Job) error {
				return tt.err()
			}})
			mustRecover(t, s)

			id := enqueue(t, db, s, "work", nil)
			tickAndWait(t, s)
			if r := jobRow(t, db, id); r.State != jobs.StateFailed || !strings.HasPrefix(r.LastError, tt.want) {
				t.Errorf("job = %q last_error %q, want failed with last_error starting %q", r.State, r.LastError, tt.want)
			}
		})
	}
}

func TestFinishFindsRowInAnotherState(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	tx := &countingTx{Transactor: db}
	s, lb := newLoggedSchedulerTx(t, st, tx, newFakeClock())
	b := newBlocker(t)
	register(t, s, jobs.Kind{Name: "work", Handler: func(ctx context.Context, job jobs.Job) error {
		if job.ID == 1 {
			return b.handler(ctx, job)
		}
		return nil
	}})
	mustRecover(t, s)

	id := enqueue(t, db, s, "work", nil)
	tx.writes.Store(0)
	s.TickForTest(t.Context())
	recv(t, b.started, "the handler")
	exec(t, db, "UPDATE jobs SET state = 'cancelled' WHERE id = ?", id)
	b.unblock()
	s.WaitJobsForTest()

	lines := lb.lines(t, "job finish found the row in another state")
	if len(lines) != 1 || lines[0]["level"] != "ERROR" || lines[0]["id"] != float64(id) {
		t.Errorf("finish found another state lines = %v, want one ERROR line for job %d", lines, id)
	}
	if n := tx.writes.Load(); n != 2 {
		t.Errorf("scheduler Writes = %d, want 2 (claim and one finish, not retried)", n)
	}
	if r := jobRow(t, db, id); r.State != jobs.StateCancelled {
		t.Errorf("job = %q, want left cancelled", r.State)
	}

	// The slot was released.
	next := enqueue(t, db, s, "work", nil)
	tickAndWait(t, s)
	if r := jobRow(t, db, next); r.State != jobs.StateSucceeded {
		t.Errorf("next job = %q, want succeeded", r.State)
	}
}
