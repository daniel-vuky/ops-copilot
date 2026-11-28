package store

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a settable clock.Clock safe for concurrent use.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// steppingClock is a clock.Clock that advances 1 ns on every Now call.
type steppingClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *steppingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Nanosecond)
	return c.now
}

var errBoom = errors.New("boom")

// recorder is a write func that counts its calls and records the last time.
type recorder struct {
	mu    sync.Mutex
	calls int
	at    time.Time
	err   error
}

func (r *recorder) write(_ context.Context, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.at = at
	return r.err
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

const every = time.Minute

func TestTouchFirstCallWrites(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	l := NewLastUsed(clk, every)
	var r recorder

	if err := l.Touch(context.Background(), "k", r.write); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if r.calls != 1 {
		t.Fatalf("writes = %d, want 1", r.calls)
	}
	if !r.at.Equal(clk.Now()) {
		t.Errorf("at = %v, want %v", r.at, clk.Now())
	}
}

func TestTouchWithinWindowSkips(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	l := NewLastUsed(clk, every)
	var r recorder
	ctx := context.Background()

	if err := l.Touch(ctx, "k", r.write); err != nil {
		t.Fatalf("Touch 1: %v", err)
	}
	clk.Advance(59 * time.Second)
	if err := l.Touch(ctx, "k", r.write); err != nil {
		t.Fatalf("Touch 2: %v", err)
	}
	if r.count() != 1 {
		t.Fatalf("writes after 59s = %d, want 1", r.count())
	}

	clk.Advance(time.Second)
	if err := l.Touch(ctx, "k", r.write); err != nil {
		t.Fatalf("Touch 3: %v", err)
	}
	if r.count() != 2 {
		t.Fatalf("writes after 60s = %d, want 2", r.count())
	}
	if !r.at.Equal(clk.Now()) {
		t.Errorf("at = %v, want %v", r.at, clk.Now())
	}
}

func TestTouchClockSteppedBackWrites(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	l := NewLastUsed(clk, every)
	var r recorder
	ctx := context.Background()

	if err := l.Touch(ctx, "k", r.write); err != nil {
		t.Fatalf("Touch 1: %v", err)
	}
	clk.Set(clk.Now().Add(-time.Hour))
	if err := l.Touch(ctx, "k", r.write); err != nil {
		t.Fatalf("Touch 2: %v", err)
	}
	if r.count() != 2 {
		t.Fatalf("writes = %d, want 2", r.count())
	}
	if !r.at.Equal(clk.Now()) {
		t.Errorf("at = %v, want %v", r.at, clk.Now())
	}
}

func TestTouchFailedWriteRetried(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	l := NewLastUsed(clk, every)
	ctx := context.Background()

	failing := recorder{err: fmt.Errorf("update: %w", errBoom)}
	if err := l.Touch(ctx, "k", failing.write); !errors.Is(err, errBoom) {
		t.Fatalf("Touch err = %v, want errBoom", err)
	}

	var ok recorder
	if err := l.Touch(ctx, "k", ok.write); err != nil {
		t.Fatalf("Touch retry: %v", err)
	}
	if ok.count() != 1 {
		t.Fatalf("retry writes = %d, want 1", ok.count())
	}

	// The successful retry holds the stamp again.
	if err := l.Touch(ctx, "k", ok.write); err != nil {
		t.Fatalf("Touch 3: %v", err)
	}
	if ok.count() != 1 {
		t.Errorf("writes after retry = %d, want 1", ok.count())
	}
}

func TestTouchFailedFirstWriteForgetsKey(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	l := NewLastUsed(clk, every)

	failing := recorder{err: errBoom}
	if err := l.Touch(context.Background(), "k", failing.write); !errors.Is(err, errBoom) {
		t.Fatalf("Touch err = %v, want errBoom", err)
	}

	l.mu.Lock()
	_, ok := l.stamps["k"]
	l.mu.Unlock()
	if ok {
		t.Error("stamp kept after the first write failed")
	}
}

func TestTouchSlowFailureKeepsNewerStamp(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	l := NewLastUsed(clk, every)
	ctx := context.Background()

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock) // never leave the goroutine blocked if the test fails early
	slowErr := make(chan error, 1)
	go func() {
		slowErr <- l.Touch(ctx, "k", func(context.Context, time.Time) error {
			close(started)
			<-release
			return errBoom
		})
	}()
	<-started

	clk.Advance(61 * time.Second)
	var r recorder
	if err := l.Touch(ctx, "k", r.write); err != nil {
		t.Fatalf("Touch B: %v", err)
	}
	if r.count() != 1 {
		t.Fatalf("B writes = %d, want 1", r.count())
	}

	unblock()
	if err := <-slowErr; !errors.Is(err, errBoom) {
		t.Fatalf("Touch A err = %v, want errBoom", err)
	}

	if err := l.Touch(ctx, "k", r.write); err != nil {
		t.Fatalf("Touch C: %v", err)
	}
	if r.count() != 1 {
		t.Errorf("writes after A failed = %d, want 1 (B's stamp must stay)", r.count())
	}
}

func TestTouchConcurrentSameKeyWritesOnce(t *testing.T) {
	t.Parallel()

	// Every Now call returns a later time, so concurrent calls stamp
	// distinct times and the ordering of the stamps matters.
	clk := &steppingClock{now: newFakeClock().Now()}
	l := NewLastUsed(clk, every)
	ctx := context.Background()

	const n = 50
	var writes atomic.Int64
	write := func(context.Context, time.Time) error {
		writes.Add(1)
		return nil
	}

	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- l.Touch(ctx, "k", write)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("Touch: %v", err)
		}
	}
	if got := writes.Load(); got != 1 {
		t.Errorf("writes = %d, want 1", got)
	}
}

func TestTouchSweepsOldKeys(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	l := NewLastUsed(clk, every)
	ctx := context.Background()
	var r recorder

	for i := range 100 {
		if err := l.Touch(ctx, fmt.Sprintf("key-%d", i), r.write); err != nil {
			t.Fatalf("Touch %d: %v", i, err)
		}
	}
	clk.Advance(2 * time.Minute)
	if err := l.Touch(ctx, "key-0", r.write); err != nil {
		t.Fatalf("Touch after 2m: %v", err)
	}

	l.mu.Lock()
	n := len(l.stamps)
	l.mu.Unlock()
	if n > 1 {
		t.Errorf("stamps = %d, want at most 1", n)
	}
}

func TestTouchFailureAfterSuccessWritesAgain(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	l := NewLastUsed(clk, every)
	ctx := context.Background()

	var ok recorder
	if err := l.Touch(ctx, "k", ok.write); err != nil {
		t.Fatalf("Touch 1: %v", err)
	}
	clk.Advance(every)

	failing := recorder{err: errBoom}
	if err := l.Touch(ctx, "k", failing.write); !errors.Is(err, errBoom) {
		t.Fatalf("Touch 2 err = %v, want errBoom", err)
	}
	if failing.count() != 1 {
		t.Fatalf("failing writes = %d, want 1", failing.count())
	}

	if err := l.Touch(ctx, "k", ok.write); err != nil {
		t.Fatalf("Touch 3: %v", err)
	}
	if ok.count() != 2 {
		t.Errorf("writes = %d, want 2 (the key must be due after the failure)", ok.count())
	}
}

func TestTouchZeroPeriodAlwaysWrites(t *testing.T) {
	t.Parallel()

	for _, period := range []time.Duration{0, -time.Second} {
		t.Run(period.String(), func(t *testing.T) {
			t.Parallel()

			l := NewLastUsed(newFakeClock(), period)
			var r recorder
			for i := range 3 {
				if err := l.Touch(context.Background(), "k", r.write); err != nil {
					t.Fatalf("Touch %d: %v", i, err)
				}
			}
			if r.count() != 3 {
				t.Errorf("writes = %d, want 3", r.count())
			}
		})
	}
}

func TestTouchPanicForgetsStamp(t *testing.T) {
	t.Parallel()

	l := NewLastUsed(newFakeClock(), every)
	ctx := context.Background()

	func() {
		defer func() {
			if v := recover(); v != errBoom { //nolint:errorlint // comparing the recovered panic value, not a returned error
				t.Errorf("recovered %v, want the write's panic", v)
			}
		}()
		_ = l.Touch(ctx, "k", func(context.Context, time.Time) error { panic(errBoom) })
		t.Error("Touch returned, want the panic to propagate")
	}()

	var r recorder
	if err := l.Touch(ctx, "k", r.write); err != nil {
		t.Fatalf("Touch after panic: %v", err)
	}
	if r.count() != 1 {
		t.Errorf("writes after panic = %d, want 1", r.count())
	}
}

func TestTouchGoexitForgetsStamp(t *testing.T) {
	t.Parallel()

	l := NewLastUsed(newFakeClock(), every)
	ctx := context.Background()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = l.Touch(ctx, "k", func(context.Context, time.Time) error {
			runtime.Goexit()
			return nil
		})
	}()
	<-done

	var r recorder
	if err := l.Touch(ctx, "k", r.write); err != nil {
		t.Fatalf("Touch after Goexit: %v", err)
	}
	if r.count() != 1 {
		t.Errorf("writes after Goexit = %d, want 1", r.count())
	}
}
