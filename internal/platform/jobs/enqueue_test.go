package jobs_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daniel-vuky/ops-copilot/internal/platform/jobs"
	"github.com/daniel-vuky/ops-copilot/internal/platform/store"
)

func TestEnqueueInsertsQueued(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	s := newScheduler(t, db, st, clk)
	register(t, s, jobs.Kind{Name: "doc.extract", Handler: noop})

	payload := []byte(`{"document_id":7}`)
	id := enqueue(t, db, s, "doc.extract", payload)
	if id <= 0 {
		t.Fatalf("Enqueue id = %d, want > 0", id)
	}
	r := jobRow(t, db, id)
	if r.State != jobs.StateQueued || r.Attempts != 0 {
		t.Errorf("row state %q attempts %d, want queued and 0", r.State, r.Attempts)
	}
	if !r.RunAfter.Equal(testNow) {
		t.Errorf("RunAfter = %v, want now %v", r.RunAfter, testNow)
	}
	if !bytes.Equal(r.Payload, payload) {
		t.Errorf("payload = %q, want %q", r.Payload, payload)
	}
	if r.PeriodAt != nil || r.UniqueKey != "" {
		t.Errorf("PeriodAt %v UniqueKey %q, want nil and \"\"", r.PeriodAt, r.UniqueKey)
	}
}

func TestEnqueueRunAfter(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{Name: "doc.extract", Handler: noop})

	at := testNow.Add(time.Hour)
	id := enqueue(t, db, s, "doc.extract", nil, jobs.RunAfter(at))
	if got := jobRow(t, db, id).RunAfter; !got.Equal(at) {
		t.Errorf("RunAfter = %v, want %v", got, at)
	}
}

func TestEnqueueOutsideWrite(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{Name: "doc.extract", Handler: noop})
	register(t, s, jobs.Kind{Name: "investigate", Handler: noop, MaxWaiting: 5})

	for _, kind := range []string{"doc.extract", "investigate"} {
		if _, err := s.Enqueue(t.Context(), kind, nil); !errors.Is(err, store.ErrNoWriteTx) {
			t.Errorf("Enqueue(%s) outside a Write = %v, want store.ErrNoWriteTx", kind, err)
		}
	}
	if n := count(t, db, "SELECT count(*) FROM jobs"); n != 0 {
		t.Errorf("jobs = %d, want 0", n)
	}
}

func TestEnqueueUnknownKind(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())

	err := db.Write(t.Context(), func(ctx context.Context) error {
		_, err := s.Enqueue(ctx, "nope", nil)
		return err
	})
	if !errors.Is(err, jobs.ErrUnknownKind) {
		t.Errorf("Enqueue(unknown) = %v, want ErrUnknownKind", err)
	}
}

func TestEnqueuePayloadTooLarge(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{Name: "doc.extract", Handler: noop})

	err := db.Write(t.Context(), func(ctx context.Context) error {
		_, err := s.Enqueue(ctx, "doc.extract", make([]byte, jobs.MaxPayload+1))
		return err
	})
	if !errors.Is(err, jobs.ErrPayloadTooLarge) {
		t.Errorf("Enqueue(64 KiB + 1) = %v, want ErrPayloadTooLarge", err)
	}
	enqueue(t, db, s, "doc.extract", make([]byte, jobs.MaxPayload)) // exactly the limit is fine
}

func TestEnqueueRejectsBadUniqueKey(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{Name: "doc.extract", Handler: noop})

	for _, key := range []string{"", strings.Repeat("k", 129)} {
		err := db.Write(t.Context(), func(ctx context.Context) error {
			_, err := s.Enqueue(ctx, "doc.extract", nil, jobs.UniqueKey(key))
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "unique key") {
			t.Errorf("Enqueue(UniqueKey of %d bytes) = %v, want a unique key error", len(key), err)
		}
	}
	enqueue(t, db, s, "doc.extract", nil, jobs.UniqueKey(strings.Repeat("k", 128)))
	if n := count(t, db, "SELECT count(*) FROM jobs"); n != 1 {
		t.Errorf("jobs = %d, want 1", n)
	}
}

func TestEnqueueQueueFullKeepsCallerWrites(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{Name: "investigate", Handler: noop, MaxWaiting: 2, Concurrency: 1})

	var errs [4]error
	write(t, db, func(ctx context.Context) error {
		q, err := db.WriteTx(ctx)
		if err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS marker(x INTEGER) STRICT"); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, "INSERT INTO marker (x) VALUES (1)"); err != nil {
			return err
		}
		for i := range errs {
			_, errs[i] = s.Enqueue(ctx, "investigate", nil)
		}
		return nil
	})

	// One free slot: 3 queued - 1 = 2 waiting, which reaches MaxWaiting 2
	// and refuses only the 4th.
	for i, err := range errs[:3] {
		if err != nil {
			t.Errorf("enqueue %d = %v, want nil", i+1, err)
		}
	}
	if !errors.Is(errs[3], jobs.ErrQueueFull) {
		t.Errorf("enqueue 4 = %v, want ErrQueueFull", errs[3])
	}
	if n := count(t, db, "SELECT count(*) FROM marker"); n != 1 {
		t.Errorf("marker rows = %d, want 1 (the caller's write committed)", n)
	}
	if n := count(t, db, "SELECT count(*) FROM jobs"); n != 3 {
		t.Errorf("jobs = %d, want 3", n)
	}
}

// TestEnqueueCountsOnlyFreeSlots checks that a slot taken by a running job
// is not subtracted from the waiting count.
func TestEnqueueCountsOnlyFreeSlots(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{Name: "investigate", Handler: noop, MaxWaiting: 2, Concurrency: 2})
	s.SetRunningForTest("investigate", 1) // one slot free

	var errs [4]error
	write(t, db, func(ctx context.Context) error {
		for i := range errs {
			_, errs[i] = s.Enqueue(ctx, "investigate", nil)
		}
		return nil
	})
	for i, err := range errs[:3] {
		if err != nil {
			t.Errorf("enqueue %d = %v, want nil", i+1, err)
		}
	}
	if !errors.Is(errs[3], jobs.ErrQueueFull) {
		t.Errorf("enqueue 4 = %v, want ErrQueueFull", errs[3])
	}

	// A fresh kind with no slot free: every queued job is waiting.
	register(t, s, jobs.Kind{Name: "busy", Handler: noop, MaxWaiting: 2, Concurrency: 2})
	s.SetRunningForTest("busy", 2)
	var busy [3]error
	write(t, db, func(ctx context.Context) error {
		for i := range busy {
			_, busy[i] = s.Enqueue(ctx, "busy", nil)
		}
		return nil
	})
	for i, err := range busy[:2] {
		if err != nil {
			t.Errorf("busy enqueue %d = %v, want nil", i+1, err)
		}
	}
	if !errors.Is(busy[2], jobs.ErrQueueFull) {
		t.Errorf("busy enqueue 3 = %v, want ErrQueueFull", busy[2])
	}
}

func TestEnqueueRolledBackWithCaller(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{Name: "doc.extract", Handler: noop})

	boom := errors.New("boom")
	err := db.Write(t.Context(), func(ctx context.Context) error {
		if _, err := s.Enqueue(ctx, "doc.extract", nil); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Write = %v, want boom", err)
	}
	if n := count(t, db, "SELECT count(*) FROM jobs"); n != 0 {
		t.Errorf("jobs = %d, want 0", n)
	}
}

func TestEnqueueUniqueKey(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{Name: "cve.scan", Handler: noop, Every: 24 * time.Hour, PeriodKey: "all"})

	first := enqueue(t, db, s, "cve.scan", nil, jobs.UniqueKey("all"))
	r := jobRow(t, db, first)
	if r.PeriodAt != nil || r.UniqueKey != "all" {
		t.Errorf("manual job PeriodAt %v UniqueKey %q, want nil and \"all\"", r.PeriodAt, r.UniqueKey)
	}

	err := db.Write(t.Context(), func(ctx context.Context) error {
		_, err := s.Enqueue(ctx, "cve.scan", nil, jobs.UniqueKey("all"))
		return err
	})
	if !errors.Is(err, jobs.ErrAlreadyActive) {
		t.Errorf("second Enqueue with the same key = %v, want ErrAlreadyActive", err)
	}
	enqueue(t, db, s, "cve.scan", nil, jobs.UniqueKey("other"))

	// Once the first is no longer active, the key is free again.
	write(t, db, func(ctx context.Context) error { return s.Cancel(ctx, "cve.scan", first) })
	enqueue(t, db, s, "cve.scan", nil, jobs.UniqueKey("all"))
	if n := count(t, db, "SELECT count(*) FROM jobs"); n != 3 {
		t.Errorf("jobs = %d, want 3", n)
	}
}

// hookCall is one call of a FailedHook.
type hookCall struct {
	job     jobs.Job
	reason  jobs.Reason
	lastErr string
	inWrite bool
}

func TestCancelQueued(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	s := newScheduler(t, db, st, clk)
	var calls []hookCall
	register(t, s, jobs.Kind{
		Name: "investigate", Handler: noop, MaxWaiting: 1,
		OnFailed: func(ctx context.Context, job jobs.Job, reason jobs.Reason, lastErr string) error {
			calls = append(calls, hookCall{job: job, reason: reason, lastErr: lastErr, inWrite: store.InWrite(ctx)})
			return nil
		},
	})

	id := enqueue(t, db, s, "investigate", []byte("p"))
	enqueue(t, db, s, "investigate", nil)
	full := db.Write(t.Context(), func(ctx context.Context) error {
		_, err := s.Enqueue(ctx, "investigate", nil)
		return err
	})
	if !errors.Is(full, jobs.ErrQueueFull) {
		t.Fatalf("third Enqueue = %v, want ErrQueueFull", full)
	}

	clk.Advance(time.Minute)
	write(t, db, func(ctx context.Context) error { return s.Cancel(ctx, "investigate", id) })

	if len(calls) != 1 {
		t.Fatalf("OnFailed calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.reason != jobs.ReasonCancelled || !c.inWrite {
		t.Errorf("OnFailed reason %v inWrite %v, want cancelled inside the Write", c.reason, c.inWrite)
	}
	if c.job.ID != id || c.job.Kind != "investigate" || !bytes.Equal(c.job.Payload, []byte("p")) ||
		c.job.MaxAttempts != 3 || c.job.Attempt != 0 {
		t.Errorf("OnFailed job = %+v, want id %d of investigate with its payload, 3 attempts, none started", c.job, id)
	}
	r := jobRow(t, db, id)
	if r.State != jobs.StateCancelled || r.FinishedAt == nil || !r.FinishedAt.Equal(clk.Now()) {
		t.Errorf("row state %q finished_at %v, want cancelled at %v", r.State, r.FinishedAt, clk.Now())
	}
	enqueue(t, db, s, "investigate", nil) // the cancelled job freed a waiting place
}

func TestCancelNotQueued(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{Name: "investigate", Handler: noop})
	register(t, s, jobs.Kind{Name: "doc.extract", Handler: noop})

	running := enqueue(t, db, s, "investigate", nil)
	write(t, db, func(ctx context.Context) error {
		ok, err := st.MarkRunning(ctx, running, testNow)
		if err == nil && !ok {
			t.Error("MarkRunning = false, want true")
		}
		return err
	})
	other := enqueue(t, db, s, "doc.extract", nil)

	cancel := func(kind string, id int64) error {
		return db.Write(t.Context(), func(ctx context.Context) error { return s.Cancel(ctx, kind, id) })
	}
	if err := cancel("investigate", running); !errors.Is(err, jobs.ErrNotCancellable) {
		t.Errorf("Cancel(running) = %v, want ErrNotCancellable", err)
	}
	if err := cancel("investigate", other); !errors.Is(err, jobs.ErrNotCancellable) {
		t.Errorf("Cancel(another kind's id) = %v, want ErrNotCancellable", err)
	}
	if err := cancel("investigate", 9999); !errors.Is(err, jobs.ErrNotCancellable) {
		t.Errorf("Cancel(missing id) = %v, want ErrNotCancellable", err)
	}
	if err := cancel("ghost", other); !errors.Is(err, jobs.ErrUnknownKind) {
		t.Errorf("Cancel(unregistered kind) = %v, want ErrUnknownKind", err)
	}
	if r := jobRow(t, db, running); r.State != jobs.StateRunning || r.FinishedAt != nil {
		t.Errorf("running row = %q finished %v, want untouched", r.State, r.FinishedAt)
	}
	if r := jobRow(t, db, other); r.State != jobs.StateQueued || r.FinishedAt != nil {
		t.Errorf("other kind's row = %q finished %v, want untouched", r.State, r.FinishedAt)
	}
}

func TestCancelOutsideWrite(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{Name: "investigate", Handler: noop})

	id := enqueue(t, db, s, "investigate", nil)
	if err := s.Cancel(t.Context(), "investigate", id); !errors.Is(err, store.ErrNoWriteTx) {
		t.Errorf("Cancel outside a Write = %v, want store.ErrNoWriteTx", err)
	}
	if r := jobRow(t, db, id); r.State != jobs.StateQueued {
		t.Errorf("row = %q, want still queued", r.State)
	}
}

func TestEnqueueThenCancelInOneWrite(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{Name: "investigate", Handler: noop})

	var id int64
	write(t, db, func(ctx context.Context) error {
		var err error
		if id, err = s.Enqueue(ctx, "investigate", nil); err != nil {
			return err
		}
		return s.Cancel(ctx, "investigate", id)
	})
	if r := jobRow(t, db, id); r.State != jobs.StateCancelled {
		t.Errorf("row = %q, want cancelled", r.State)
	}
}

func TestCancelHookErrorRollsBack(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{
		Name: "investigate", Handler: noop,
		OnFailed: func(context.Context, jobs.Job, jobs.Reason, string) error {
			return errors.New(`owner update failed: Post "https://u:pw@owner.example.test/x?token=t"`)
		},
	})

	id := enqueue(t, db, s, "investigate", nil)
	err := db.Write(t.Context(), func(ctx context.Context) error { return s.Cancel(ctx, "investigate", id) })
	if err == nil {
		t.Fatal("Write = nil, want the hook's error")
	}
	if want := `owner update failed: Post "https://owner.example.test"`; !strings.Contains(err.Error(), want) {
		t.Errorf("Write = %q, want it to contain the scrubbed hook text %q", err, want)
	}
	if strings.Contains(err.Error(), "token=") || strings.Contains(err.Error(), "pw@") {
		t.Errorf("Write = %q, still carries the URL's secrets", err)
	}
	if r := jobRow(t, db, id); r.State != jobs.StateQueued || r.FinishedAt != nil {
		t.Errorf("row = %q finished %v, want still queued", r.State, r.FinishedAt)
	}
}

func TestHookPanicIsAnError(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s, lb := newLoggedScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{
		Name: "investigate", Handler: noop,
		OnFailed: func(context.Context, jobs.Job, jobs.Reason, string) error { panic("owner row gone") },
	})

	id := enqueue(t, db, s, "investigate", nil)
	err := db.Write(t.Context(), func(ctx context.Context) error { return s.Cancel(ctx, "investigate", id) })
	if err == nil || !strings.Contains(err.Error(), "hook panic: owner row gone") {
		t.Errorf("Write = %v, want an error with %q", err, "hook panic: owner row gone")
	}
	if r := jobRow(t, db, id); r.State != jobs.StateQueued {
		t.Errorf("row = %q, want still queued", r.State)
	}
	lines := lb.lines(t, "job failure hook panicked")
	if len(lines) != 1 || lines[0]["level"] != "ERROR" || lines[0]["id"] != float64(id) || lines[0]["kind"] != "investigate" {
		t.Fatalf("job failure hook panicked lines = %v, want one ERROR line for job %d", lines, id)
	}
	if stack, _ := lines[0]["stack"].(string); !strings.Contains(stack, "goroutine") {
		t.Errorf("stack = %q, want a goroutine stack", stack)
	}
}

// TestHookCanEnqueue checks the ADR 0004 §5 pattern: a FailedHook queues
// another kind's job in the same Write, without a deadlock.
func TestHookCanEnqueue(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	var notified int64
	register(t, s, jobs.Kind{Name: "notify.chat", Handler: noop, MaxWaiting: 5})
	register(t, s, jobs.Kind{
		Name: "investigate", Handler: noop,
		OnFailed: func(ctx context.Context, _ jobs.Job, _ jobs.Reason, _ string) error {
			id, err := s.Enqueue(ctx, "notify.chat", []byte("n"))
			notified = id
			return err
		},
	})

	id := enqueue(t, db, s, "investigate", nil)
	done := make(chan error, 1)
	go func() {
		done <- db.Write(t.Context(), func(ctx context.Context) error { return s.Cancel(ctx, "investigate", id) })
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Write = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel with a hook that enqueues did not return within 5 s")
	}
	if r := jobRow(t, db, notified); r.Kind != "notify.chat" || r.State != jobs.StateQueued {
		t.Errorf("hook's job = %q %q, want a queued notify.chat job", r.Kind, r.State)
	}
}

func TestRecover(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	clk := newFakeClock()
	s, lb := newLoggedScheduler(t, db, st, clk)
	register(t, s, jobs.Kind{Name: "investigate", Handler: noop})

	a := enqueue(t, db, s, "investigate", nil)
	b := enqueue(t, db, s, "investigate", nil)
	exec(t, db, "UPDATE jobs SET state = 'running', attempts = 1 WHERE id = ?", a)
	exec(t, db, "UPDATE jobs SET state = 'running', attempts = 3 WHERE id = ?", b)
	write(t, db, func(ctx context.Context) error {
		_, _, err := st.Insert(ctx, jobs.NewJob{Kind: "ghost", RunAfter: testNow, Now: testNow})
		return err
	})

	clk.Advance(time.Minute)
	if err := s.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	for id, attempts := range map[int64]int{a: 1, b: 3} {
		r := jobRow(t, db, id)
		if r.State != jobs.StateQueued || r.Attempts != attempts || r.LastError != "interrupted by a restart" {
			t.Errorf("job %d = %q attempts %d last_error %q, want queued, %d, %q",
				id, r.State, r.Attempts, r.LastError, attempts, "interrupted by a restart")
		}
		if !r.RunAfter.Equal(clk.Now()) {
			t.Errorf("job %d RunAfter = %v, want %v", id, r.RunAfter, clk.Now())
		}
	}

	requeued := lb.lines(t, "jobs requeued")
	if len(requeued) != 1 || requeued[0]["level"] != "INFO" || requeued[0]["count"] != float64(2) {
		t.Errorf("jobs requeued lines = %v, want one INFO line with count 2", requeued)
	}
	ghost := lb.lines(t, "queued jobs of unregistered kinds")
	if len(ghost) != 1 || ghost[0]["level"] != "WARN" {
		t.Fatalf("unregistered kinds lines = %v, want one WARN line", ghost)
	}
	if kinds, _ := ghost[0]["kinds"].(map[string]any); kinds["ghost"] != float64(1) || len(kinds) != 1 {
		t.Errorf("unregistered kinds = %v, want only ghost: 1", ghost[0]["kinds"])
	}

	if err := s.Register(jobs.Kind{Name: "late", Handler: noop}); err == nil {
		t.Error("Register after Recover = nil, want an error")
	}
	if err := s.Recover(t.Context()); err == nil {
		t.Error("second Recover = nil, want an error")
	}
}

func TestRecoverQuietWhenNothingToDo(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s, lb := newLoggedScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{Name: "investigate", Handler: noop})
	enqueue(t, db, s, "investigate", nil)

	if err := s.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if out := lb.String(); out != "" {
		t.Errorf("log = %q, want nothing", out)
	}
}

func TestRecoverRunsOnce(t *testing.T) {
	t.Parallel()
	db, st := openStore(t)
	s := newScheduler(t, db, st, newFakeClock())
	register(t, s, jobs.Kind{Name: "investigate", Handler: noop})

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.Recover(t.Context())
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Errorf("concurrent Recover calls: %d succeeded (%v), want exactly 1", ok, errs)
	}
}
