package jobs

import (
	"context"
	"slices"
	"time"
)

// SetRunningForTest sets the running count of kind, as if that many of its
// jobs had been claimed, so tests can check what Enqueue counts as free
// slots without running the dispatcher.
func (s *Scheduler) SetRunningForTest(kind string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kinds[kind].running = n
}

// TickForTest runs one dispatcher tick.
func (s *Scheduler) TickForTest(ctx context.Context) {
	s.tick(ctx)
}

// WaitJobsForTest waits until every job goroutine started so far has
// returned. Join every goroutine that ticks before calling it.
func (s *Scheduler) WaitJobsForTest() {
	s.jobWG.Wait()
}

// TestTimings lowers the scheduler's timings in tests; zero fields keep
// the current value.
type TestTimings struct {
	FinishRetries  []time.Duration // delay before each finish try
	FinishTimeout  time.Duration
	HookBackoff    time.Duration
	HookBackoffMax time.Duration
}

// SetTimingsForTest applies t. Call it before the first tick.
func (s *Scheduler) SetTimingsForTest(t TestTimings) {
	if t.FinishRetries != nil {
		s.t.finishRetries = slices.Clone(t.FinishRetries)
	}
	if t.FinishTimeout > 0 {
		s.t.finishTimeout = t.FinishTimeout
	}
	if t.HookBackoff > 0 {
		s.t.hookBackoff = t.HookBackoff
	}
	if t.HookBackoffMax > 0 {
		s.t.hookBackoffMax = t.HookBackoffMax
	}
}

// OnBeforeHandlerForTest makes f run in each job goroutine after its claim
// commits and before the stopped check and the handler. Call it before
// the first tick.
func (s *Scheduler) OnBeforeHandlerForTest(f func(Job)) {
	s.seams.beforeHandler = f
}

// OnBeforeFinishForTest makes f run after each handler returns, before the
// finish. Call it before the first tick.
func (s *Scheduler) OnBeforeFinishForTest(f func(Job)) {
	s.seams.beforeFinish = f
}

// OnFinishWriteForTest makes f run inside each finish Write, once the
// entry is marked finishing. Call it before the first tick.
func (s *Scheduler) OnFinishWriteForTest(f func(Job)) {
	s.seams.finishWrite = f
}

// DrainForTest cancels the job context with the drain cause, as Run does
// when the drain timeout passes.
func (s *Scheduler) DrainForTest() {
	s.cancelJobs(errDrain)
}
