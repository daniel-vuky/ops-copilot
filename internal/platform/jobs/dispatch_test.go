package jobs_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daniel-vuky/ops-copilot/internal/platform/adapters/sqlite"
	"github.com/daniel-vuky/ops-copilot/internal/platform/jobs"
	"github.com/daniel-vuky/ops-copilot/internal/platform/store"
)

// mustRecover runs s.Recover and fails the test on error.
func mustRecover(t *testing.T, s *jobs.Scheduler) {
	t.Helper()
	if err := s.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
}

// tickAndWait runs one dispatcher tick and waits for the jobs it started.
func tickAndWait(t *testing.T, s *jobs.Scheduler) {
	t.Helper()
	s.TickForTest(t.Context())
	s.WaitJobsForTest()
}

// within runs fn and fails the test if it does not return within 5 s.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return within 5 s", what)
	}
}

// waitUntil polls cond until it holds and fails the test after 5 s. It is
// a failure guard, not an ordering tool.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after 5 s waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// recv receives one value from ch and fails the test after 5 s.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out after 5 s waiting for %s", what)
		var zero T
		return zero
	}
}

// startTickers runs n goroutines that tick s until the returned stop is
// called; stop joins them. It is also called at cleanup.
func startTickers(t *testing.T, s *jobs.Scheduler, n int) (stop func()) {
	t.Helper()
	var halt atomic.Bool
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !halt.Load() {
				s.TickForTest(context.Background())
			}
		}()
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			halt.Store(true)
			wg.Wait()
		})
	}
	t.Cleanup(stop)
	return stop
}

// hookRecorder records FailedHook calls. It is safe for concurrent use.
type hookRecorder struct {
	mu    sync.Mutex
	calls []hookCall
	fail  func(jobs.Job) error // optional: the hook's result
}

// hook is the FailedHook.
func (h *hookRecorder) hook(ctx context.Context, job jobs.Job, reason jobs.Reason, lastErr string) error {
	h.mu.Lock()
	h.calls = append(h.calls, hookCall{job: job, reason: reason, lastErr: lastErr, inWrite: store.InWrite(ctx)})
	fail := h.fail
	h.mu.Unlock()
	if fail != nil {
		return fail(job)
	}
	return nil
}

// list returns a copy of the calls so far.
func (h *hookRecorder) list() []hookCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hookCall(nil), h.calls...)
}

// callsFor returns how many calls were made for job id.
func (h *hookRecorder) callsFor(id int64) int {
	n := 0
	for _, c := range h.list() {
		if c.job.ID == id {
			n++
		}
	}
	return n
}

func TestHandlerSeesJob(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s, lb := newLoggedScheduler(t, db, st, newFakeClock())
	got := make(chan jobs.Job, 1)
	register(t, s, jobs.Kind{Name: "work", Handler: func(_ context.Context, job jobs.Job) error {
		got <- job
		return nil
	}})
	mustRecover(t, s)

	id := enqueue(t, db, s, "work", []byte("p"), jobs.UniqueKey("k"))
	tickAndWait(t, s)

	job := recv(t, got, "the handler")
	if job.ID != id || job.Kind != "work" || !bytes.Equal(job.Payload, []byte("p")) ||
		job.Attempt != 1 || job.MaxAttempts != 3 || job.UniqueKey != "k" || job.PeriodAt != nil {
		t.Errorf("handler job = %+v, want id %d of work, payload p, attempt 1 of 3, key k", job, id)
	}
	r := jobRow(t, db, id)
	if r.State != jobs.StateSucceeded || r.Attempts != 1 || r.FinishedAt == nil || !r.FinishedAt.Equal(testNow) {
		t.Errorf("row = %q attempts %d finished %v, want succeeded, 1, at %v", r.State, r.Attempts, r.FinishedAt, testNow)
	}
	claimed := lb.lines(t, "job claimed")
	if len(claimed) != 1 || claimed[0]["level"] != "DEBUG" || claimed[0]["id"] != float64(id) ||
		claimed[0]["kind"] != "work" || claimed[0]["attempt"] != float64(1) {
		t.Errorf("job claimed lines = %v, want one DEBUG line for job %d, attempt 1", claimed, id)
	}
	ok := lb.lines(t, "job succeeded")
	if len(ok) != 1 || ok[0]["level"] != "INFO" || ok[0]["id"] != float64(id) || ok[0]["duration_ms"] == nil {
		t.Errorf("job succeeded lines = %v, want one INFO line for job %d with duration_ms", ok, id)
	}
}

func TestClaimRunsEachJobOnce(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	var mu sync.Mutex
	runs := map[int64]int{}
	register(t, s, jobs.Kind{Name: "work", Concurrency: 3, Handler: func(_ context.Context, job jobs.Job) error {
		mu.Lock()
		defer mu.Unlock()
		runs[job.ID]++
		return nil
	}})
	mustRecover(t, s)

	ids := make([]int64, 30)
	for i := range ids {
		ids[i] = enqueue(t, db, s, "work", nil)
	}
	stop := startTickers(t, s, 4)
	waitUntil(t, "30 succeeded jobs", func() bool {
		return count(t, db, "SELECT count(*) FROM jobs WHERE state = 'succeeded'") == 30
	})
	stop()
	s.WaitJobsForTest()

	mu.Lock()
	defer mu.Unlock()
	for _, id := range ids {
		if runs[id] != 1 {
			t.Errorf("job %d ran %d times, want 1", id, runs[id])
		}
	}
}

func TestClaimOldestFirst(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	var order []int64
	register(t, s, jobs.Kind{Name: "work", Handler: func(_ context.Context, job jobs.Job) error {
		order = append(order, job.ID) // Concurrency 1 and one tick at a time: no race
		return nil
	}})
	mustRecover(t, s)

	a := enqueue(t, db, s, "work", nil, jobs.RunAfter(testNow.Add(-time.Minute)))
	b := enqueue(t, db, s, "work", nil, jobs.RunAfter(testNow.Add(-3*time.Minute)))
	c := enqueue(t, db, s, "work", nil, jobs.RunAfter(testNow.Add(-2*time.Minute)))
	for range 3 {
		tickAndWait(t, s)
	}
	if want := []int64{b, c, a}; fmt.Sprint(order) != fmt.Sprint(want) {
		t.Errorf("run order = %v, want %v", order, want)
	}
}

func TestFutureJobWaits(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	s := newScheduler(t, db, st, clk)
	register(t, s, jobs.Kind{Name: "work", Handler: noop})
	mustRecover(t, s)

	id := enqueue(t, db, s, "work", nil, jobs.RunAfter(testNow.Add(time.Minute)))
	tickAndWait(t, s)
	if r := jobRow(t, db, id); r.State != jobs.StateQueued || r.Attempts != 0 {
		t.Fatalf("before run_after: %q attempts %d, want queued and 0", r.State, r.Attempts)
	}
	clk.Advance(time.Minute)
	tickAndWait(t, s)
	if r := jobRow(t, db, id); r.State != jobs.StateSucceeded {
		t.Errorf("at run_after: %q, want succeeded", r.State)
	}
}

func TestConcurrencyCaps(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	started := make(chan int64, 4)
	release := make(chan struct{})
	register(t, s, jobs.Kind{Name: "work", Concurrency: 2, Handler: func(_ context.Context, job jobs.Job) error {
		started <- job.ID
		<-release
		return nil
	}})
	mustRecover(t, s)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		s.WaitJobsForTest()
	})

	for range 4 {
		enqueue(t, db, s, "work", nil)
	}
	s.TickForTest(t.Context())
	recv(t, started, "the first handler")
	recv(t, started, "the second handler")
	s.TickForTest(t.Context()) // both slots taken: claims nothing
	if n := count(t, db, "SELECT count(*) FROM jobs WHERE state = 'running'"); n != 2 {
		t.Errorf("running = %d, want 2", n)
	}
	if n := len(started); n != 0 {
		t.Errorf("%d more handlers started, want none", n)
	}

	close(release)
	s.WaitJobsForTest()
	tickAndWait(t, s)
	if n := count(t, db, "SELECT count(*) FROM jobs WHERE state = 'succeeded'"); n != 4 {
		t.Errorf("succeeded = %d, want 4", n)
	}
}

func TestUnregisteredKindStaysQueued(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{Name: "work", Handler: noop})
	mustRecover(t, s)

	var id int64
	write(t, db, func(ctx context.Context) error {
		var err error
		id, _, err = st.Insert(ctx, jobs.NewJob{Kind: "ghost", RunAfter: testNow, Now: testNow})
		return err
	})
	tickAndWait(t, s)
	if r := jobRow(t, db, id); r.State != jobs.StateQueued || r.Attempts != 0 {
		t.Errorf("ghost job = %q attempts %d, want queued and 0", r.State, r.Attempts)
	}
}

func TestHandlerOutsideWrite(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	exec(t, db, "CREATE TABLE marker (x INTEGER) STRICT")
	register(t, s, jobs.Kind{Name: "work", Handler: func(ctx context.Context, _ jobs.Job) error {
		if store.InWrite(ctx) {
			return errors.New("handler ctx carries a write transaction")
		}
		return db.Write(ctx, func(ctx context.Context) error {
			q, err := db.WriteTx(ctx)
			if err != nil {
				return err
			}
			_, err = q.ExecContext(ctx, "INSERT INTO marker (x) VALUES (1)")
			return err
		})
	}})
	mustRecover(t, s)

	id := enqueue(t, db, s, "work", nil)
	tickAndWait(t, s)
	if r := jobRow(t, db, id); r.State != jobs.StateSucceeded {
		t.Errorf("job = %q last_error %q, want succeeded", r.State, r.LastError)
	}
	if n := count(t, db, "SELECT count(*) FROM marker"); n != 1 {
		t.Errorf("marker rows = %d, want 1", n)
	}
}

func TestDefaultRetryThenFailed(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	s, lb := newLoggedScheduler(t, db, st, clk)
	rec := &hookRecorder{}
	register(t, s, jobs.Kind{
		Name:     "work",
		Handler:  func(context.Context, jobs.Job) error { return errors.New("boom") },
		OnFailed: rec.hook,
	})
	mustRecover(t, s)
	id := enqueue(t, db, s, "work", nil)

	for i, delay := range []time.Duration{30 * time.Second, 2 * time.Minute} {
		tickAndWait(t, s)
		r := jobRow(t, db, id)
		if r.State != jobs.StateQueued || r.Attempts != i+1 || r.LastError != "boom" ||
			!r.RunAfter.Equal(clk.Now().Add(delay)) || r.FinishedAt != nil {
			t.Fatalf("after attempt %d: %q attempts %d last_error %q run_after %v, want queued, %d, boom, %v",
				i+1, r.State, r.Attempts, r.LastError, r.RunAfter, i+1, clk.Now().Add(delay))
		}
		clk.Advance(delay)
	}
	tickAndWait(t, s)

	r := jobRow(t, db, id)
	if r.State != jobs.StateFailed || r.Attempts != 3 || r.LastError != "boom" ||
		r.FinishedAt == nil || !r.FinishedAt.Equal(clk.Now()) {
		t.Errorf("after attempt 3: %q attempts %d last_error %q finished %v, want failed, 3, boom, %v",
			r.State, r.Attempts, r.LastError, r.FinishedAt, clk.Now())
	}
	calls := rec.list()
	if len(calls) != 1 {
		t.Fatalf("OnFailed calls = %d, want 1", len(calls))
	}
	if c := calls[0]; c.reason != jobs.ReasonFailed || !c.inWrite || c.lastErr != "boom" || c.job.Attempt != 3 {
		t.Errorf("OnFailed call = %+v, want ReasonFailed in the Write, lastErr boom, attempt 3", c)
	}

	retries := lb.lines(t, "job retry scheduled")
	if len(retries) != 2 || retries[0]["level"] != "WARN" ||
		retries[0]["retry_in_ms"] != float64(30000) || retries[1]["retry_in_ms"] != float64(120000) ||
		retries[0]["error"] != "boom" {
		t.Errorf("job retry scheduled lines = %v, want two WARN lines, 30000 and 120000 ms, error boom", retries)
	}
	failed := lb.lines(t, "job failed")
	if len(failed) != 1 || failed[0]["level"] != "ERROR" || failed[0]["error"] != "boom" || failed[0]["attempt"] != float64(3) {
		t.Errorf("job failed lines = %v, want one ERROR line, attempt 3, error boom", failed)
	}
}

func TestRetrySchedules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		kind    jobs.Kind
		err     error
		delays  []time.Duration // expected delay after each failed attempt but the last
		reasons int             // OnFailed calls at the end
	}{
		{
			name:   "custom",
			kind:   jobs.Kind{Retry: []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}},
			err:    errors.New("boom"),
			delays: []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute},
		},
		{name: "no retry", kind: jobs.Kind{NoRetry: true}, err: errors.New("boom")},
		{name: "permanent", kind: jobs.Kind{}, err: jobs.Permanent(errors.New("boom"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db, st := openStore(t)
			clk := newFakeClock()
			s := newScheduler(t, db, st, clk)
			rec := &hookRecorder{}
			k := tt.kind
			k.Name = "work"
			k.Handler = func(context.Context, jobs.Job) error { return tt.err }
			k.OnFailed = rec.hook
			register(t, s, k)
			mustRecover(t, s)
			id := enqueue(t, db, s, "work", nil)

			for i, delay := range tt.delays {
				tickAndWait(t, s)
				r := jobRow(t, db, id)
				if r.State != jobs.StateQueued || r.Attempts != i+1 || !r.RunAfter.Equal(clk.Now().Add(delay)) {
					t.Fatalf("after attempt %d: %q attempts %d run_after %v, want queued, %d, %v",
						i+1, r.State, r.Attempts, r.RunAfter, i+1, clk.Now().Add(delay))
				}
				clk.Advance(delay - time.Second)
				tickAndWait(t, s) // not due yet
				if got := jobRow(t, db, id).Attempts; got != i+1 {
					t.Fatalf("a second before the retry: attempts %d, want %d", got, i+1)
				}
				clk.Advance(time.Second)
			}
			tickAndWait(t, s)

			r := jobRow(t, db, id)
			if want := len(tt.delays) + 1; r.State != jobs.StateFailed || r.Attempts != want || r.LastError != "boom" {
				t.Errorf("end: %q attempts %d last_error %q, want failed, %d, boom", r.State, r.Attempts, r.LastError, want)
			}
			if calls := rec.list(); len(calls) != 1 || calls[0].reason != jobs.ReasonFailed {
				t.Errorf("OnFailed calls = %+v, want one with ReasonFailed", calls)
			}
		})
	}
}

func TestPanicFailsAttempt(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s, lb := newLoggedScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{Name: "boom", Handler: func(context.Context, jobs.Job) error { panic("kaboom") }})
	register(t, s, jobs.Kind{Name: "fine", Handler: noop})
	mustRecover(t, s)

	bad := enqueue(t, db, s, "boom", nil)
	good := enqueue(t, db, s, "fine", nil)
	tickAndWait(t, s)

	r := jobRow(t, db, bad)
	if r.State != jobs.StateQueued || r.Attempts != 1 || r.LastError != "panic: kaboom" {
		t.Errorf("panicking job = %q attempts %d last_error %q, want queued, 1, %q",
			r.State, r.Attempts, r.LastError, "panic: kaboom")
	}
	retries := lb.lines(t, "job retry scheduled")
	if len(retries) != 1 || retries[0]["error"] != "panic: kaboom" {
		t.Fatalf("job retry scheduled lines = %v, want one with error %q", retries, "panic: kaboom")
	}
	if stack, _ := retries[0]["stack"].(string); !strings.Contains(stack, "goroutine") {
		t.Errorf("stack = %q, want a goroutine stack", stack)
	}
	if got := jobRow(t, db, good).State; got != jobs.StateSucceeded {
		t.Errorf("other job = %q, want succeeded", got)
	}
}

func TestPanicOnLastAttemptLogsStack(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s, lb := newLoggedScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{Name: "boom", NoRetry: true, Handler: func(context.Context, jobs.Job) error { panic("kaboom") }})
	mustRecover(t, s)

	id := enqueue(t, db, s, "boom", nil)
	tickAndWait(t, s)
	if r := jobRow(t, db, id); r.State != jobs.StateFailed || r.LastError != "panic: kaboom" {
		t.Errorf("job = %q last_error %q, want failed, %q", r.State, r.LastError, "panic: kaboom")
	}
	failed := lb.lines(t, "job failed")
	if len(failed) != 1 {
		t.Fatalf("job failed lines = %v, want one", failed)
	}
	if stack, _ := failed[0]["stack"].(string); !strings.Contains(stack, "goroutine") {
		t.Errorf("stack = %q, want a goroutine stack", stack)
	}
}

func TestTimeoutDeadline(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	deadline := make(chan time.Time, 1)
	register(t, s, jobs.Kind{Name: "default", Handler: func(ctx context.Context, _ jobs.Job) error {
		d, _ := ctx.Deadline()
		deadline <- d
		return nil
	}})
	register(t, s, jobs.Kind{Name: "short", Timeout: 50 * time.Millisecond, Handler: func(ctx context.Context, _ jobs.Job) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	mustRecover(t, s)

	enqueue(t, db, s, "default", nil)
	short := enqueue(t, db, s, "short", nil)
	within(t, "the tick", func() { tickAndWait(t, s) })

	d := recv(t, deadline, "the default handler")
	if want := time.Now().Add(jobs.DefaultTimeout); d.Before(want.Add(-time.Minute)) || d.After(want.Add(time.Minute)) {
		t.Errorf("default deadline = %v, want about %v", d, want)
	}
	r := jobRow(t, db, short)
	if r.State != jobs.StateQueued || r.Attempts != 1 || r.LastError != context.DeadlineExceeded.Error() {
		t.Errorf("timed-out job = %q attempts %d last_error %q, want queued for retry, 1, %q",
			r.State, r.Attempts, r.LastError, context.DeadlineExceeded.Error())
	}
}

func TestErrorScrubbedAndCut(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s, lb := newLoggedScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{Name: "work", Concurrency: 2, Handler: func(_ context.Context, job jobs.Job) error {
		if string(job.Payload) == "long" {
			return errors.New(strings.Repeat("x", 5000))
		}
		return &url.Error{ //nolint:gosec // a fake credential the scrubber must remove
			Op:  "Post",
			URL: "https://user:pw@chat.example.test/v1/spaces/x/messages?key=k&token=t",
			Err: errors.New("timeout"),
		}
	}})
	mustRecover(t, s)

	webhook := enqueue(t, db, s, "work", []byte("url"))
	long := enqueue(t, db, s, "work", []byte("long"))
	tickAndWait(t, s)

	logged := map[float64]string{}
	for _, line := range lb.lines(t, "job retry scheduled") {
		id, _ := line["id"].(float64)
		logged[id], _ = line["error"].(string)
	}
	stored := jobRow(t, db, webhook).LastError
	for name, text := range map[string]string{"last_error": stored, "log error": logged[float64(webhook)]} {
		if !strings.Contains(text, "https://chat.example.test") {
			t.Errorf("%s = %q, want it to contain https://chat.example.test", name, text)
		}
		for _, leak := range []string{"pw", "key=", "token="} {
			if strings.Contains(text, leak) {
				t.Errorf("%s = %q, still contains %q", name, text, leak)
			}
		}
	}
	if stored != logged[float64(webhook)] {
		t.Errorf("log error %q differs from last_error %q", logged[float64(webhook)], stored)
	}
	if n := len(jobRow(t, db, long).LastError); n == 0 || n > 1024 {
		t.Errorf("long last_error is %d bytes, want 1 to 1024", n)
	}
	if n := len(logged[float64(long)]); n == 0 || n > 1024 {
		t.Errorf("long log error is %d bytes, want 1 to 1024", n)
	}
}

func TestStopRunning(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s, lb := newLoggedScheduler(t, db, st, newFakeClock())
	rec := &hookRecorder{}
	started := make(chan int64, 1)
	cause := make(chan error, 1)
	register(t, s, jobs.Kind{
		Name: "work",
		Handler: func(ctx context.Context, job jobs.Job) error {
			started <- job.ID
			<-ctx.Done()
			cause <- context.Cause(ctx)
			return ctx.Err()
		},
		OnFailed: rec.hook,
	})
	mustRecover(t, s)

	id := enqueue(t, db, s, "work", nil)
	s.TickForTest(t.Context())
	recv(t, started, "the handler")
	if s.Stop("other", id) {
		t.Error("Stop(another kind, id) = true, want false")
	}
	if s.Stop("work", id+1000) {
		t.Error("Stop(unknown id) = true, want false")
	}
	if !s.Stop("work", id) {
		t.Fatal("Stop(running job) = false, want true")
	}
	within(t, "the stopped job", s.WaitJobsForTest)

	if c := recv(t, cause, "the handler's cause"); !errors.Is(c, jobs.ErrStopped) {
		t.Errorf("handler ctx cause = %v, want ErrStopped", c)
	}
	r := jobRow(t, db, id)
	if r.State != jobs.StateCancelled || r.Attempts != 1 || r.FinishedAt == nil {
		t.Errorf("stopped job = %q attempts %d finished %v, want cancelled, 1, set", r.State, r.Attempts, r.FinishedAt)
	}
	if calls := rec.list(); len(calls) != 1 || calls[0].reason != jobs.ReasonCancelled || !calls[0].inWrite {
		t.Errorf("OnFailed calls = %+v, want one with ReasonCancelled in the Write", calls)
	}
	if lines := lb.lines(t, "job cancelled"); len(lines) != 1 || lines[0]["level"] != "INFO" {
		t.Errorf("job cancelled lines = %v, want one INFO line", lines)
	}
	if s.Stop("work", id) {
		t.Error("Stop(finished job) = true, want false")
	}
}

func TestStopBeforeHandlerStarts(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	rec := &hookRecorder{}
	var called atomic.Bool
	register(t, s, jobs.Kind{
		Name:     "work",
		Handler:  func(context.Context, jobs.Job) error { called.Store(true); return nil },
		OnFailed: rec.hook,
	})
	s.OnBeforeHandlerForTest(func(job jobs.Job) {
		if !s.Stop("work", job.ID) {
			t.Error("Stop between claim and handler = false, want true")
		}
	})
	mustRecover(t, s)

	id := enqueue(t, db, s, "work", nil)
	tickAndWait(t, s)
	if called.Load() {
		t.Error("handler ran, want it skipped")
	}
	if r := jobRow(t, db, id); r.State != jobs.StateCancelled || r.Attempts != 1 {
		t.Errorf("job = %q attempts %d, want cancelled, 1", r.State, r.Attempts)
	}
	if calls := rec.list(); len(calls) != 1 || calls[0].reason != jobs.ReasonCancelled {
		t.Errorf("OnFailed calls = %+v, want one with ReasonCancelled", calls)
	}
}

func TestStopAfterHandlerError(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	rec := &hookRecorder{}
	register(t, s, jobs.Kind{
		Name:     "work",
		Handler:  func(context.Context, jobs.Job) error { return errors.New("boom") },
		OnFailed: rec.hook,
	})
	s.OnBeforeFinishForTest(func(job jobs.Job) {
		if !s.Stop("work", job.ID) {
			t.Error("Stop before finish = false, want true")
		}
	})
	mustRecover(t, s)

	id := enqueue(t, db, s, "work", nil)
	tickAndWait(t, s)
	if r := jobRow(t, db, id); r.State != jobs.StateCancelled || r.Attempts != 1 || r.LastError != "boom" {
		t.Errorf("job = %q attempts %d last_error %q, want cancelled, 1, boom", r.State, r.Attempts, r.LastError)
	}
	if calls := rec.list(); len(calls) != 1 || calls[0].reason != jobs.ReasonCancelled {
		t.Errorf("OnFailed calls = %+v, want one with ReasonCancelled", calls)
	}
}

func TestStopDuringFinishReturnsFalse(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	s := newScheduler(t, db, st, clk)
	rec := &hookRecorder{}
	register(t, s, jobs.Kind{
		Name:     "work",
		Handler:  func(context.Context, jobs.Job) error { return errors.New("boom") },
		OnFailed: rec.hook,
	})
	var stopped atomic.Bool
	s.OnFinishWriteForTest(func(job jobs.Job) { stopped.Store(s.Stop("work", job.ID)) })
	mustRecover(t, s)

	id := enqueue(t, db, s, "work", nil)
	tickAndWait(t, s)
	if stopped.Load() {
		t.Error("Stop inside the finish Write = true, want false")
	}
	if r := jobRow(t, db, id); r.State != jobs.StateQueued || !r.RunAfter.Equal(clk.Now().Add(30*time.Second)) {
		t.Errorf("job = %q run_after %v, want queued for retry at %v", r.State, r.RunAfter, clk.Now().Add(30*time.Second))
	}
	if calls := rec.list(); len(calls) != 0 {
		t.Errorf("OnFailed calls = %+v, want none", calls)
	}
}

func TestStopWithTimeout(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	rec := &hookRecorder{}
	timedOut := make(chan int64, 1)
	proceed := make(chan struct{})
	register(t, s, jobs.Kind{
		Name:    "work",
		Timeout: 50 * time.Millisecond,
		Handler: func(ctx context.Context, job jobs.Job) error {
			<-ctx.Done()
			timedOut <- job.ID
			<-proceed
			return ctx.Err()
		},
		OnFailed: rec.hook,
	})
	mustRecover(t, s)

	enqueue(t, db, s, "work", nil)
	s.TickForTest(t.Context())
	id := recv(t, timedOut, "the timeout")
	if !s.Stop("work", id) {
		t.Error("Stop after the timeout = false, want true")
	}
	close(proceed)
	within(t, "the job", s.WaitJobsForTest)

	if r := jobRow(t, db, id); r.State != jobs.StateCancelled || r.LastError != context.DeadlineExceeded.Error() {
		t.Errorf("job = %q last_error %q, want cancelled, %q", r.State, r.LastError, context.DeadlineExceeded.Error())
	}
	if calls := rec.list(); len(calls) != 1 || calls[0].reason != jobs.ReasonCancelled {
		t.Errorf("OnFailed calls = %+v, want one with ReasonCancelled", calls)
	}
}

func TestStopSucceededStaysSucceeded(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	rec := &hookRecorder{}
	started := make(chan int64, 1)
	proceed := make(chan struct{})
	register(t, s, jobs.Kind{
		Name: "work",
		Handler: func(_ context.Context, job jobs.Job) error {
			started <- job.ID
			<-proceed
			return nil
		},
		OnFailed: rec.hook,
	})
	mustRecover(t, s)

	enqueue(t, db, s, "work", nil)
	s.TickForTest(t.Context())
	id := recv(t, started, "the handler")
	if !s.Stop("work", id) {
		t.Error("Stop(running job) = false, want true")
	}
	close(proceed)
	within(t, "the job", s.WaitJobsForTest)

	if r := jobRow(t, db, id); r.State != jobs.StateSucceeded || r.LastError != "" {
		t.Errorf("job = %q last_error %q, want succeeded with no error", r.State, r.LastError)
	}
	if calls := rec.list(); len(calls) != 0 {
		t.Errorf("OnFailed calls = %+v, want none", calls)
	}
}

func TestDrainInterruptsAttempt(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	s, lb := newLoggedScheduler(t, db, st, clk)
	rec := &hookRecorder{}
	started := make(chan int64, 1)
	register(t, s, jobs.Kind{
		Name: "work",
		Handler: func(ctx context.Context, job jobs.Job) error {
			started <- job.ID
			<-ctx.Done()
			return ctx.Err()
		},
		OnFailed: rec.hook,
	})
	mustRecover(t, s)

	enqueue(t, db, s, "work", nil)
	s.TickForTest(t.Context())
	id := recv(t, started, "the handler")
	clk.Advance(time.Second)
	s.DrainForTest()
	within(t, "the job", s.WaitJobsForTest)

	r := jobRow(t, db, id)
	if r.State != jobs.StateQueued || r.Attempts != 1 || r.LastError != "interrupted by shutdown" ||
		!r.RunAfter.Equal(clk.Now()) {
		t.Errorf("job = %q attempts %d last_error %q run_after %v, want queued, 1, %q, %v",
			r.State, r.Attempts, r.LastError, r.RunAfter, "interrupted by shutdown", clk.Now())
	}
	if lines := lb.lines(t, "job interrupted by shutdown"); len(lines) != 1 || lines[0]["level"] != "WARN" {
		t.Errorf("job interrupted by shutdown lines = %v, want one WARN line", lines)
	}
	if calls := rec.list(); len(calls) != 0 {
		t.Errorf("OnFailed calls = %+v, want none", calls)
	}
}

func TestMaxWaitingWithConcurrentClaim(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	release := make(chan struct{})
	done := make(chan struct{})
	register(t, s, jobs.Kind{
		Name: "investigate", Concurrency: 2, MaxWaiting: 3,
		Handler: func(ctx context.Context, _ jobs.Job) error {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		},
	})
	mustRecover(t, s)

	var releasers sync.WaitGroup
	releasers.Add(1)
	go func() {
		defer releasers.Done()
		for {
			select {
			case release <- struct{}{}:
			case <-done:
				return
			}
		}
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			close(done)
		}
		releasers.Wait()
	})
	stop := startTickers(t, s, 4)

	var accepted, violations atomic.Int64
	var enqueuers sync.WaitGroup
	for range 30 {
		enqueuers.Add(1)
		go func() {
			defer enqueuers.Done()
			err := db.Write(context.Background(), func(ctx context.Context) error {
				if _, err := s.Enqueue(ctx, "investigate", nil); err != nil {
					if errors.Is(err, jobs.ErrQueueFull) {
						return nil
					}
					return err
				}
				q, err := db.WriteTx(ctx)
				if err != nil {
					return err
				}
				var queued, running int
				err = q.QueryRowContext(ctx, `SELECT
					(SELECT count(*) FROM jobs WHERE kind = 'investigate' AND state = 'queued'),
					(SELECT count(*) FROM jobs WHERE kind = 'investigate' AND state = 'running')`).Scan(&queued, &running)
				if err != nil {
					return err
				}
				if queued-max(0, 2-running) > 3 {
					violations.Add(1)
				}
				accepted.Add(1)
				return nil
			})
			if err != nil {
				t.Errorf("enqueue Write: %v", err)
			}
		}()
	}
	enqueuers.Wait()
	waitUntil(t, "no queued or running job", func() bool {
		return count(t, db, "SELECT count(*) FROM jobs WHERE state IN ('queued', 'running')") == 0
	})
	stop()
	s.WaitJobsForTest()

	if n := violations.Load(); n != 0 {
		t.Errorf("%d enqueues saw more than 3 waiting", n)
	}
	if n := accepted.Load(); n < 5 {
		t.Errorf("accepted = %d, want at least 5 (2 free slots + 3 waiting)", n)
	}
	if got, want := count(t, db, "SELECT count(*) FROM jobs WHERE state = 'succeeded'"), int(accepted.Load()); got != want {
		t.Errorf("succeeded = %d, want accepted %d", got, want)
	}
}

func TestHookFailureDoesNotBlockKind(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	s, lb := newLoggedScheduler(t, db, st, clk)
	var first int64
	rec := &hookRecorder{fail: func(job jobs.Job) error {
		if job.ID == first {
			return errors.New(`owner update failed: Post "https://u:pw@owner.example.test/x?token=t"`)
		}
		return nil
	}}
	var runs atomic.Int64
	register(t, s, jobs.Kind{
		Name:     "work",
		Handler:  func(context.Context, jobs.Job) error { runs.Add(1); return nil },
		OnFailed: rec.hook,
	})
	mustRecover(t, s)

	first = enqueue(t, db, s, "work", nil)
	second := enqueue(t, db, s, "work", nil)
	exec(t, db, "UPDATE jobs SET attempts = 3, last_error = 'boom' WHERE id IN (?, ?)", first, second)
	tickAndWait(t, s)

	if r := jobRow(t, db, first); r.State != jobs.StateQueued {
		t.Errorf("first = %q, want still queued (its hook failed)", r.State)
	}
	if r := jobRow(t, db, second); r.State != jobs.StateFailed || r.LastError != "boom" {
		t.Errorf("second = %q last_error %q, want failed, boom", r.State, r.LastError)
	}
	lines := lb.lines(t, "job failure hook failed")
	if len(lines) != 1 || lines[0]["level"] != "ERROR" || lines[0]["id"] != float64(first) {
		t.Fatalf("job failure hook failed lines = %v, want one ERROR line for job %d", lines, first)
	}
	if text, _ := lines[0]["error"].(string); !strings.Contains(text, "https://owner.example.test") || strings.Contains(text, "token=") {
		t.Errorf("hook error logged as %q, want it scrubbed", text)
	}
	exhausted := lb.lines(t, "job failed: attempts exhausted after restart")
	if len(exhausted) != 1 || exhausted[0]["id"] != float64(second) {
		t.Errorf("attempts exhausted lines = %v, want one for job %d", exhausted, second)
	}

	third := enqueue(t, db, s, "work", nil)
	tickAndWait(t, s)
	if r := jobRow(t, db, third); r.State != jobs.StateSucceeded {
		t.Errorf("third = %q, want succeeded", r.State)
	}
	if n := runs.Load(); n != 1 {
		t.Errorf("handler runs = %d, want 1 (only the third job)", n)
	}
	if n := rec.callsFor(first); n != 1 {
		t.Errorf("hook calls for the first job = %d, want 1 (in backoff)", n)
	}

	// Backoff: 1 min, then 2 min.
	clk.Advance(time.Minute)
	tickAndWait(t, s)
	if n := rec.callsFor(first); n != 2 {
		t.Errorf("hook calls after 1 min = %d, want 2", n)
	}
	clk.Advance(time.Minute)
	tickAndWait(t, s)
	if n := rec.callsFor(first); n != 2 {
		t.Errorf("hook calls 1 min into a 2 min backoff = %d, want still 2", n)
	}
	clk.Advance(time.Minute)
	tickAndWait(t, s)
	if n := rec.callsFor(first); n != 3 {
		t.Errorf("hook calls after the 2 min backoff = %d, want 3", n)
	}

	// Once the hook works, the job fails for good.
	rec.mu.Lock()
	rec.fail = nil
	rec.mu.Unlock()
	clk.Advance(4 * time.Minute)
	tickAndWait(t, s)
	if r := jobRow(t, db, first); r.State != jobs.StateFailed {
		t.Errorf("first after the hook recovered = %q, want failed", r.State)
	}
}

func TestRequeuedAtLastAttemptFailsWithoutRunning(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s, lb := newLoggedScheduler(t, db, st, newFakeClock())
	rec := &hookRecorder{}
	var called atomic.Bool
	register(t, s, jobs.Kind{
		Name:     "work",
		Handler:  func(context.Context, jobs.Job) error { called.Store(true); return nil },
		OnFailed: rec.hook,
	})

	id := enqueue(t, db, s, "work", nil)
	exec(t, db, "UPDATE jobs SET state = 'running', attempts = 3 WHERE id = ?", id)
	mustRecover(t, s)
	tickAndWait(t, s)

	if called.Load() {
		t.Error("handler ran, want the job failed without running")
	}
	r := jobRow(t, db, id)
	if r.State != jobs.StateFailed || r.Attempts != 3 || r.LastError != "interrupted by a restart" || r.FinishedAt == nil {
		t.Errorf("job = %q attempts %d last_error %q finished %v, want failed, 3, %q, set",
			r.State, r.Attempts, r.LastError, r.FinishedAt, "interrupted by a restart")
	}
	calls := rec.list()
	if len(calls) != 1 {
		t.Fatalf("OnFailed calls = %d, want 1", len(calls))
	}
	if c := calls[0]; c.reason != jobs.ReasonFailed || !c.inWrite || c.lastErr != "interrupted by a restart" || c.job.Attempt != 3 {
		t.Errorf("OnFailed call = %+v, want ReasonFailed in the Write, the restart error, attempt 3", c)
	}
	if lines := lb.lines(t, "job failed: attempts exhausted after restart"); len(lines) != 1 || lines[0]["level"] != "ERROR" {
		t.Errorf("attempts exhausted lines = %v, want one ERROR line", lines)
	}
}

// failingSucceed is a job store whose MarkSucceeded fails while fails is
// above zero.
type failingSucceed struct {
	*sqlite.JobStore
	fails atomic.Int32
}

// MarkSucceeded fails while f.fails is above zero, then defers to the
// real store.
func (f *failingSucceed) MarkSucceeded(ctx context.Context, id int64, now time.Time) (bool, error) {
	if f.fails.Add(-1) >= 0 {
		return false, errors.New("disk on fire")
	}
	return f.JobStore.MarkSucceeded(ctx, id, now)
}

func TestLostFinishAfterSuccessKeepsOwnerFinal(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	exec(t, db, "CREATE TABLE owner (id INTEGER PRIMARY KEY, state TEXT NOT NULL) STRICT")
	exec(t, db, "INSERT INTO owner (id, state) VALUES (1, 'pending')")
	handler := func(ctx context.Context, _ jobs.Job) error {
		return db.Write(ctx, func(ctx context.Context) error {
			q, err := db.WriteTx(ctx)
			if err != nil {
				return err
			}
			_, err = q.ExecContext(ctx, "UPDATE owner SET state = 'done' WHERE id = 1")
			return err
		})
	}

	// First process: the attempt does its work, but its finish is lost.
	failing := &failingSucceed{JobStore: st}
	failing.fails.Store(3)
	lb := &logBuffer{}
	s1 := jobs.New(failing, db, jobs.Options{Clock: clk, Logger: lb.logger()})
	s1.SetTimingsForTest(jobs.TestTimings{FinishRetries: []time.Duration{0, 0, 0}})
	register(t, s1, jobs.Kind{Name: "work", NoRetry: true, Handler: handler})
	mustRecover(t, s1)
	payload := []byte("owner:1")
	id := enqueue(t, db, s1, "work", payload)
	tickAndWait(t, s1)

	if r := jobRow(t, db, id); r.State != jobs.StateRunning {
		t.Fatalf("job after a lost finish = %q, want running", r.State)
	}
	lost := lb.lines(t, "job finish not recorded")
	if len(lost) != 1 || lost[0]["level"] != "ERROR" {
		t.Fatalf("job finish not recorded lines = %v, want one ERROR line", lost)
	}
	if text, _ := lost[0]["error"].(string); !strings.Contains(text, "disk on fire") {
		t.Errorf("finish error logged as %q, want it to contain %q", text, "disk on fire")
	}
	if _, ok := lost[0]["handler_error"]; ok {
		t.Errorf("handler_error = %v, want none for a nil handler error", lost[0]["handler_error"])
	}
	// The slot was released: s1 runs another job of the kind.
	next := enqueue(t, db, s1, "work", nil)
	tickAndWait(t, s1)
	if r := jobRow(t, db, next); r.State != jobs.StateSucceeded {
		t.Errorf("next job on s1 = %q, want succeeded (slot released)", r.State)
	}

	// Restart: the job is requeued at its last attempt and failed at claim;
	// the hook's conditional update keeps the owner final.
	s2 := newScheduler(t, db, st, clk)
	var hookJob jobs.Job
	register(t, s2, jobs.Kind{
		Name: "work", NoRetry: true, Handler: handler,
		OnFailed: func(ctx context.Context, job jobs.Job, _ jobs.Reason, _ string) error {
			hookJob = job
			q, err := db.WriteTx(ctx)
			if err != nil {
				return err
			}
			_, err = q.ExecContext(ctx,
				"UPDATE owner SET state = 'failed' WHERE id = 1 AND state NOT IN ('done', 'failed')")
			return err
		},
	})
	mustRecover(t, s2)
	tickAndWait(t, s2)

	if r := jobRow(t, db, id); r.State != jobs.StateFailed {
		t.Errorf("job after restart = %q, want failed", r.State)
	}
	if hookJob.ID != id || !bytes.Equal(hookJob.Payload, payload) {
		t.Errorf("OnFailed job = %+v, want job %d with payload %q", hookJob, id, payload)
	}
	var state string
	if err := db.Reader().QueryRowContext(t.Context(), "SELECT state FROM owner WHERE id = 1").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "done" {
		t.Errorf("owner state = %q, want done", state)
	}
}

func TestFinishNotRecordedLogsHandlerError(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	lb := &logBuffer{}
	s := jobs.New(st, db, jobs.Options{Clock: newFakeClock(), Logger: lb.logger()})
	s.SetTimingsForTest(jobs.TestTimings{FinishRetries: []time.Duration{0, 0, 0}})
	register(t, s, jobs.Kind{
		Name: "work", NoRetry: true,
		Handler: func(context.Context, jobs.Job) error {
			return errors.New(`send failed: Post "https://u:pw@chat.example.test/x?key=k"`)
		},
		OnFailed: func(context.Context, jobs.Job, jobs.Reason, string) error { return errors.New("owner gone") },
	})
	mustRecover(t, s)

	id := enqueue(t, db, s, "work", nil)
	tickAndWait(t, s)
	if r := jobRow(t, db, id); r.State != jobs.StateRunning {
		t.Errorf("job = %q, want still running (finish rolled back three times)", r.State)
	}
	lost := lb.lines(t, "job finish not recorded")
	if len(lost) != 1 {
		t.Fatalf("job finish not recorded lines = %v, want one", lost)
	}
	if text, _ := lost[0]["error"].(string); !strings.Contains(text, "owner gone") {
		t.Errorf("error = %q, want the hook's error", text)
	}
	if text, _ := lost[0]["handler_error"].(string); text != `send failed: Post "https://chat.example.test"` {
		t.Errorf("handler_error = %q, want the scrubbed handler error", text)
	}
}

func TestHookCanEnqueueOnClaimAndFinish(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{Name: "notify.chat", Handler: noop, MaxWaiting: 5})
	register(t, s, jobs.Kind{
		Name: "investigate", NoRetry: true,
		Handler: func(context.Context, jobs.Job) error { return errors.New("boom") },
		OnFailed: func(ctx context.Context, _ jobs.Job, _ jobs.Reason, _ string) error {
			_, err := s.Enqueue(ctx, "notify.chat", []byte("n"))
			return err
		},
	})
	mustRecover(t, s)

	// Finish path: the attempt fails, the finish Write runs the hook.
	failed := enqueue(t, db, s, "investigate", nil)
	within(t, "the tick (finish path)", func() { tickAndWait(t, s) })
	if r := jobRow(t, db, failed); r.State != jobs.StateFailed {
		t.Errorf("finish-path job = %q, want failed", r.State)
	}

	// Claim-time path: a job requeued at its last attempt.
	exhausted := enqueue(t, db, s, "investigate", nil)
	exec(t, db, "UPDATE jobs SET attempts = 1 WHERE id = ?", exhausted)
	within(t, "the tick (claim path)", func() { tickAndWait(t, s) })
	if r := jobRow(t, db, exhausted); r.State != jobs.StateFailed {
		t.Errorf("claim-path job = %q, want failed", r.State)
	}
	if n := count(t, db, "SELECT count(*) FROM jobs WHERE kind = 'notify.chat'"); n != 2 {
		t.Errorf("notify.chat jobs = %d, want 2 (one per hook call)", n)
	}
}

func TestPayloadNeverLogged(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	s, lb := newLoggedScheduler(t, db, st, clk)
	register(t, s, jobs.Kind{Name: "work", Handler: func(_ context.Context, job jobs.Job) error {
		if job.Attempt == 1 {
			return errors.New("first attempt fails")
		}
		return nil
	}})
	mustRecover(t, s)

	id := enqueue(t, db, s, "work", []byte("PAYLOAD-MARKER-123"))
	tickAndWait(t, s)
	clk.Advance(30 * time.Second)
	tickAndWait(t, s)

	if r := jobRow(t, db, id); r.State != jobs.StateSucceeded {
		t.Errorf("job = %q, want succeeded", r.State)
	}
	if out := lb.String(); strings.Contains(out, "PAYLOAD-MARKER-123") {
		t.Errorf("log contains the payload:\n%s", out)
	}
}

// countingTx counts the Write calls made through it.
type countingTx struct {
	store.Transactor
	writes atomic.Int64
}

// Write counts the call and defers to the wrapped Transactor.
func (c *countingTx) Write(ctx context.Context, fn func(ctx context.Context) error) error {
	c.writes.Add(1)
	return c.Transactor.Write(ctx, fn)
}

func TestClaimAndFinishUseOneWriteEach(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	tx := &countingTx{Transactor: db}
	s := jobs.New(st, tx, jobs.Options{Clock: newFakeClock(), Logger: slog.New(slog.DiscardHandler)})
	s.SetTimingsForTest(jobs.TestTimings{SweepEvery: 24 * time.Hour})
	register(t, s, jobs.Kind{Name: "work", Concurrency: 1, Handler: noop})
	mustRecover(t, s)

	id := enqueue(t, db, s, "work", nil) // the test's own Write, on db
	tx.writes.Store(0)
	tickAndWait(t, s)

	if r := jobRow(t, db, id); r.State != jobs.StateSucceeded {
		t.Fatalf("job = %q, want succeeded", r.State)
	}
	if n := tx.writes.Load(); n != 2 {
		t.Errorf("scheduler Writes = %d, want 2 (claim and finish)", n)
	}
}

func TestClaimRollbackUndoesReservation(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	s, fs, lb := newFlakyScheduler(t, db, st, clk)
	register(t, s, jobs.Kind{Name: "work", Concurrency: 1, Handler: noop})
	mustRecover(t, s)

	id := enqueue(t, db, s, "work", nil)
	fs.failRunning.Store(1)
	tickAndWait(t, s)
	if r := jobRow(t, db, id); r.State != jobs.StateQueued || r.Attempts != 0 {
		t.Fatalf("job after a rolled-back claim = %q attempts %d, want queued, 0", r.State, r.Attempts)
	}
	if s.Stop("work", id) {
		t.Error("Stop after a rolled-back claim = true, want false (registration undone)")
	}
	lines := lb.lines(t, "job scheduler step failed")
	if len(lines) != 1 || lines[0]["step"] != "claim:work" {
		t.Errorf("step failed lines = %v, want one for claim:work", lines)
	}

	// The reservation was undone: past the step's backoff, the next tick
	// claims the job in the kind's only slot.
	clk.Advance(time.Second)
	tickAndWait(t, s)
	if r := jobRow(t, db, id); r.State != jobs.StateSucceeded || r.Attempts != 1 {
		t.Errorf("job on the next tick = %q attempts %d, want succeeded, 1", r.State, r.Attempts)
	}
}

func TestClaimHookFailsForSecondExhaustedJob(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	var first, second int64
	rec := &hookRecorder{fail: func(job jobs.Job) error {
		if job.ID == second {
			return errors.New("owner locked")
		}
		return nil
	}}
	register(t, s, jobs.Kind{Name: "work", Handler: noop, OnFailed: rec.hook})
	mustRecover(t, s)

	first = enqueue(t, db, s, "work", nil)
	second = enqueue(t, db, s, "work", nil)
	exec(t, db, "UPDATE jobs SET attempts = 3 WHERE id IN (?, ?)", first, second)
	tickAndWait(t, s)

	// One Write failed both and rolled back on the second's hook; the retry
	// without the second fails the first for good.
	if r := jobRow(t, db, first); r.State != jobs.StateFailed {
		t.Errorf("first = %q, want failed after the retry", r.State)
	}
	if r := jobRow(t, db, second); r.State != jobs.StateQueued {
		t.Errorf("second = %q, want queued (hook backoff)", r.State)
	}
	if n := rec.callsFor(first); n != 2 {
		t.Errorf("hook calls for the first = %d, want 2 (rolled back once)", n)
	}
}

func TestHookBackoffCap(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	s := newScheduler(t, db, st, clk)
	rec := &hookRecorder{fail: func(jobs.Job) error { return errors.New("owner locked") }}
	register(t, s, jobs.Kind{Name: "work", Handler: noop, OnFailed: rec.hook})
	mustRecover(t, s)

	id := enqueue(t, db, s, "work", nil)
	exec(t, db, "UPDATE jobs SET attempts = 3 WHERE id = ?", id)
	tickAndWait(t, s)

	// Backoffs 1, 2, 4, 8, 16 min, then capped at 30 (not 32).
	calls := 1
	for _, d := range []time.Duration{1, 2, 4, 8, 16} {
		clk.Advance(d * time.Minute)
		tickAndWait(t, s)
		calls++
		if n := rec.callsFor(id); n != calls {
			t.Fatalf("hook calls after a %d min backoff = %d, want %d", d, n, calls)
		}
	}
	clk.Advance(29 * time.Minute)
	tickAndWait(t, s)
	if n := rec.callsFor(id); n != calls {
		t.Fatalf("hook calls 29 min into the capped backoff = %d, want %d", n, calls)
	}
	clk.Advance(time.Minute)
	tickAndWait(t, s)
	if n := rec.callsFor(id); n != calls+1 {
		t.Errorf("hook calls after the 30 min cap = %d, want %d", n, calls+1)
	}
}
