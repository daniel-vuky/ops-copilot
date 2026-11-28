package jobs

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"time"
)

// finishOutcome is how a finish records an attempt.
type finishOutcome int

const (
	finishSucceeded   finishOutcome = iota + 1 // nil handler error
	finishCancelled                            // stopped, with an error
	finishInterrupted                          // the drain deadline cut it off
	finishRetry                                // attempts left, not permanent
	finishFailed                               // no attempt left, or permanent
	finishOtherState                           // the row was no longer running
)

// finishResult is what one recorded finish decided.
type finishResult struct {
	outcome finishOutcome
	retryIn time.Duration // finishRetry: delay before the next attempt
	now     time.Time     // when the finish was recorded
}

// runJob is the goroutine of one claimed job: it runs the handler, unless
// the job was stopped before it started, then records the finish.
func (s *Scheduler) runJob(e *jobEntry) {
	defer s.jobWG.Done()
	if f := s.seams.beforeHandler; f != nil {
		f(e.job)
	}

	s.mu.Lock()
	stopped := e.stopped
	s.mu.Unlock()

	var (
		herr  error
		stack []byte
	)
	if stopped {
		herr = ErrStopped
	} else {
		stack, herr = s.callHandler(e)
	}

	if f := s.seams.beforeFinish; f != nil {
		f(e.job)
	}
	s.finish(e, herr, stack)
}

// callHandler runs e's handler with e's context. A panic is recovered as
// the error "panic: <value>", returned with its stack.
func (s *Scheduler) callHandler(e *jobEntry) (stack []byte, err error) {
	defer func() {
		if v := recover(); v != nil {
			stack = debug.Stack()
			err = fmt.Errorf("panic: %v", v)
		}
	}()
	return nil, e.kind.kind.Handler(e.ctx, e.job)
}

// finish records the end of e's attempt with handler error herr (and the
// panic stack, if any), then frees e's slot and nudges the dispatcher.
//
// Each try is one Write with a context detached from shutdown, bounded by
// the finish timeout; there are three tries (at once, +1 s, +5 s). A row
// found in another state is logged and not tried again. If every try
// fails, the slot is released anyway and the row stays running until the
// orphan sweep requeues it.
func (s *Scheduler) finish(e *jobEntry, herr error, stack []byte) {
	defer func() {
		s.release(e)
		s.wake()
	}()

	var text string
	if herr != nil {
		text = s.errorText(herr.Error())
	}
	var lastErr error
	for _, delay := range s.t.finishRetries {
		if delay > 0 {
			time.Sleep(delay)
		}
		res, err := s.recordFinish(e, herr, text)
		if err == nil {
			s.logFinish(e, res, text, stack)
			return
		}
		lastErr = err
	}

	attrs := []any{"kind", e.job.Kind, "id", e.job.ID, "attempt", e.job.Attempt}
	if lastErr != nil {
		attrs = append(attrs, "error", s.errorText(lastErr.Error()))
	}
	if herr != nil {
		attrs = append(attrs, "handler_error", text)
	}
	s.log.Error("job finish not recorded", attrs...)
}

// recordFinish runs one finish Write for e. Under s.mu it reads e's
// stopped flag and sets finishing, so a later Stop returns false; finishing
// is cleared again if the Write fails.
func (s *Scheduler) recordFinish(e *jobEntry, herr error, text string) (finishResult, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(e.ctx), s.t.finishTimeout)
	defer cancel()

	var res finishResult
	err := s.tx.Write(ctx, func(ctx context.Context) error {
		s.mu.Lock()
		stopped := e.stopped
		e.finishing = true
		s.mu.Unlock()
		if f := s.seams.finishWrite; f != nil {
			f(e.job)
		}

		res = s.decide(e, herr, stopped, s.clock.Now())
		changed, err := s.markFinish(ctx, e, res, text)
		if err != nil {
			return err
		}
		if !changed {
			res.outcome = finishOtherState
			return nil
		}
		switch res.outcome {
		case finishCancelled:
			return s.runHook(ctx, e.kind, e.job, ReasonCancelled, text)
		case finishFailed:
			return s.runHook(ctx, e.kind, e.job, ReasonFailed, text)
		}
		return nil
	})
	if err != nil {
		s.mu.Lock()
		e.finishing = false
		s.mu.Unlock()
		return finishResult{}, err
	}
	return res, nil
}

// decide returns how e's attempt ends, at now: a nil error succeeds even
// if a stop came late; a stopped job is cancelled; an error after the
// drain deadline is an interruption (the attempt stays counted); else the
// job is retried while it has attempts left and the error is not
// Permanent, and fails otherwise. The kind's own Timeout is an ordinary
// error.
func (s *Scheduler) decide(e *jobEntry, herr error, stopped bool, now time.Time) finishResult {
	res := finishResult{now: now}
	switch {
	case herr == nil:
		res.outcome = finishSucceeded
	case stopped:
		res.outcome = finishCancelled
	case errors.Is(context.Cause(s.jobCtx), errDrain):
		res.outcome = finishInterrupted
	case !isPermanent(herr) && e.job.Attempt < e.kind.maxAttempts():
		res.outcome = finishRetry
		res.retryIn = e.kind.delay(e.job.Attempt)
	default:
		res.outcome = finishFailed
	}
	return res
}

// markFinish applies res to e's running row in the finish Write and
// reports whether the row changed.
func (s *Scheduler) markFinish(ctx context.Context, e *jobEntry, res finishResult, text string) (bool, error) {
	id := e.job.ID
	switch res.outcome {
	case finishSucceeded:
		return s.st.MarkSucceeded(ctx, id, res.now)
	case finishCancelled:
		return s.st.MarkFinal(ctx, id, StateRunning, StateCancelled, &text, res.now)
	case finishInterrupted:
		return s.st.MarkRetry(ctx, id, res.now, lastErrShutdown, res.now)
	case finishRetry:
		return s.st.MarkRetry(ctx, id, res.now.Add(res.retryIn), text, res.now)
	default:
		return s.st.MarkFinal(ctx, id, StateRunning, StateFailed, &text, res.now)
	}
}

// logFinish logs a recorded finish. The error is the scrubbed text stored
// as last_error; the payload is never logged.
func (s *Scheduler) logFinish(e *jobEntry, res finishResult, text string, stack []byte) {
	attrs := []any{"kind", e.job.Kind, "id", e.job.ID, "attempt", e.job.Attempt}
	withErr := func(extra ...any) []any {
		out := slices.Concat(attrs, extra, []any{"error", text})
		if stack != nil {
			out = append(out, "stack", string(stack))
		}
		return out
	}
	switch res.outcome {
	case finishSucceeded:
		s.log.Info("job succeeded", append(attrs, "duration_ms", res.now.Sub(e.start).Milliseconds())...)
	case finishCancelled:
		s.log.Info("job cancelled", withErr()...)
	case finishInterrupted:
		s.log.Warn("job interrupted by shutdown", withErr()...)
	case finishRetry:
		s.log.Warn("job retry scheduled", withErr("retry_in_ms", res.retryIn.Milliseconds())...)
	case finishFailed:
		s.log.Error("job failed", withErr()...)
	case finishOtherState:
		s.log.Error("job finish found the row in another state", attrs...)
	}
}

// Stop asks the running job id of kind to stop: it sets the job's sticky
// stopped flag and cancels its handler's context with cause ErrStopped.
// The job then ends cancelled (no retry, OnFailed with ReasonCancelled),
// unless its handler returns nil, which is recorded as succeeded. A job
// stopped before its handler starts never runs it.
//
// It returns true when a stop was requested, not that it will be recorded:
// the claim may still roll back, or the finish may be lost. It returns
// false when this process holds no such job of that kind, or the job's
// finish is already being written; if the row is still queued or running
// then, re-read it and call Cancel again. Owners that must not run again
// record "cancel requested" in their own row, and their handler checks it
// (ADR 0006 §3).
//
// Protocol (ADR 0006 §3): call Cancel inside the domain's Write; on
// ErrNotCancellable, call Stop after that Write commits.
func (s *Scheduler) Stop(kind string, id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.jobs[id]
	if e == nil || e.job.Kind != kind || e.finishing {
		return false
	}
	e.stopped = true
	e.cancel(ErrStopped)
	return true
}
