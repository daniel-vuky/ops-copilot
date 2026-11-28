package jobs

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"
)

// claimResult says what one claim did.
type claimResult int

const (
	// claimNone: no due job, or no free slot; nothing changed (jobs
	// failed at claim time for want of attempts may have been recorded).
	claimNone claimResult = iota
	// claimStarted: a job was claimed and its handler started.
	claimStarted
	// claimHookFailed: a claim-time FailedHook failed and the Write rolled
	// back. This is not a step failure: the job goes into hook backoff.
	claimHookFailed
)

// claimOutcome is the result of one claim.
type claimOutcome struct {
	result  claimResult
	hookID  int64  // claimHookFailed: the job whose hook failed
	hookErr string // claimHookFailed: its error, scrubbed and cut
}

// tick runs one dispatcher round: it reads the due kinds and claims their
// jobs while slots are free. It is safe for concurrent calls.
//
// Run (Task 5) adds the recurring periods before the claims and the orphan
// sweep after them.
func (s *Scheduler) tick(ctx context.Context) {
	s.claimDue(ctx, s.clock.Now())
}

// claimDue reads the kinds with a due job, outside any Write, and claims
// the jobs of each registered one. Unregistered kinds are ignored and stay
// queued. Jobs in hook backoff do not make their kind due.
func (s *Scheduler) claimDue(ctx context.Context, now time.Time) {
	kinds, err := s.st.DueKinds(ctx, now, s.hookSkip(now))
	if err != nil {
		s.stepFailed(ctx, "due kinds", "", err)
		return
	}
	for _, name := range kinds {
		if k := s.kind(name); k != nil {
			s.claimKind(ctx, k)
		}
	}
}

// claimKind claims due jobs of k, one Write each, while k has a free slot
// and a job was claimed. A claim-time hook failure puts that job in hook
// backoff and claims again at once, so it never blocks the kind.
func (s *Scheduler) claimKind(ctx context.Context, k *kindState) {
	// hookFailed holds the jobs whose hook failed in this round; they are
	// skipped even if their backoff has already run out.
	var hookFailed []int64
	for {
		s.mu.Lock()
		free := s.freeSlots(k)
		s.mu.Unlock()
		if free == 0 {
			return
		}

		out, err := s.claim(ctx, k, hookFailed)
		if err != nil {
			s.stepFailed(ctx, "claim", k.kind.Name, err)
			return
		}
		switch out.result {
		case claimNone:
			return
		case claimStarted:
			continue
		case claimHookFailed:
			if ctx.Err() != nil {
				return // shutting down: not a hook failure
			}
			hookFailed = append(hookFailed, out.hookID)
			s.backOffHook(out.hookID)
			s.log.Error("job failure hook failed",
				"kind", k.kind.Name, "id", out.hookID, "error", s.errorText(out.hookErr))
		}
	}
}

// claim claims the oldest due job of k in one Write and starts its
// handler, skipping the jobs in hook backoff and those in skip.
//
// Inside the Write, a due job that already used all its attempts (it was
// requeued after its last one) is marked failed, its hook runs, and the
// next due job is read. For a job with attempts left, the free slot is
// checked again under s.mu, reserved and the job registered in the same
// critical section, then the job is marked running. If the Write fails,
// the reservation and registration are undone. Once it commits, the
// handler always starts.
func (s *Scheduler) claim(ctx context.Context, k *kindState, skip []int64) (claimOutcome, error) {
	var (
		out       claimOutcome
		entry     *jobEntry
		exhausted []Row
	)
	skip = append(s.hookSkip(s.clock.Now()), skip...)
	err := s.tx.Write(ctx, func(ctx context.Context) error {
		out, entry, exhausted = claimOutcome{}, nil, nil
		now := s.clock.Now()
		for {
			row, ok, err := s.st.NextDue(ctx, k.kind.Name, now, skip)
			if err != nil || !ok {
				return err
			}

			if row.Attempts >= k.maxAttempts() {
				changed, err := s.st.MarkFinal(ctx, row.ID, StateQueued, StateFailed, nil, now)
				if err != nil {
					return err
				}
				if !changed {
					return fmt.Errorf("job %d is no longer queued", row.ID)
				}
				if herr := s.runHook(ctx, k, jobOf(k, row), ReasonFailed, row.LastError); herr != nil {
					out = claimOutcome{result: claimHookFailed, hookID: row.ID, hookErr: herr.Error()}
					return herr
				}
				exhausted = append(exhausted, row)
				continue
			}

			if entry = s.reserve(k, row, now); entry == nil {
				return nil // another claim took the last slot
			}
			changed, err := s.st.MarkRunning(ctx, row.ID, now)
			if err != nil {
				return err
			}
			if !changed {
				return fmt.Errorf("job %d is no longer queued", row.ID)
			}
			out.result = claimStarted
			return nil
		}
	})
	if err != nil {
		if entry != nil {
			s.release(entry)
		}
		if out.result == claimHookFailed {
			return out, nil
		}
		return claimOutcome{}, fmt.Errorf("claim %s: %w", k.kind.Name, err)
	}

	for _, row := range exhausted {
		s.mu.Lock()
		delete(s.hookBackoff, row.ID)
		s.mu.Unlock()
		s.log.Error("job failed: attempts exhausted after restart",
			"kind", row.Kind, "id", row.ID, "attempt", row.Attempts, "error", s.errorText(row.LastError))
	}
	if entry != nil {
		s.log.Debug("job claimed", "kind", entry.job.Kind, "id", entry.job.ID, "attempt", entry.job.Attempt)
		s.jobWG.Add(1)
		go s.runJob(entry)
	}
	return out, nil
}

// reserve takes a slot of k for row and registers its job entry, in one
// critical section, if k still has a free slot; else it returns nil. It
// runs inside the claim Write (lock order: writer, then s.mu).
func (s *Scheduler) reserve(k *kindState, row Row, now time.Time) *jobEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.freeSlots(k) == 0 {
		return nil
	}
	job := jobOf(k, row)
	job.Attempt = row.Attempts + 1
	stopCtx, cancel := context.WithCancelCause(s.jobCtx)
	ctx, stopTimer := context.WithTimeout(stopCtx, k.kind.Timeout)
	e := &jobEntry{job: job, kind: k, ctx: ctx, cancel: cancel, stopTimer: stopTimer, start: now}
	k.running++
	s.jobs[row.ID] = e
	return e
}

// release frees e's slot, removes it from the registry and ends its
// context. It undoes a reservation whose claim rolled back, and ends a job
// whose finish was recorded or given up.
func (s *Scheduler) release(e *jobEntry) {
	s.mu.Lock()
	e.kind.running--
	if s.jobs[e.job.ID] == e {
		delete(s.jobs, e.job.ID)
	}
	s.mu.Unlock()
	e.stopTimer()
	e.cancel(nil)
}

// hookSkip returns the ids of the jobs whose claim-time hook backoff has
// not run out at now.
func (s *Scheduler) hookSkip(now time.Time) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []int64
	for _, id := range slices.Sorted(maps.Keys(s.hookBackoff)) {
		if s.hookBackoff[id].until.After(now) {
			ids = append(ids, id)
		}
	}
	return ids
}

// backOffHook records a claim-time hook failure of job id: the job is
// skipped for the hook backoff (1 min at first), which doubles with each
// failure up to its maximum (30 min).
func (s *Scheduler) backOffHook(id int64) {
	now := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.hookBackoff[id]
	if r == nil {
		r = &hookRetry{next: s.t.hookBackoff}
		s.hookBackoff[id] = r
	}
	r.until = now.Add(r.next)
	r.next = min(2*r.next, s.t.hookBackoffMax)
}

// stepFailed logs a failed dispatcher step (for a kind, when kind is not
// ""), with its error scrubbed and cut. While ctx is done the scheduler is
// shutting down, so the failure is not one and is not logged.
func (s *Scheduler) stepFailed(ctx context.Context, step, kind string, err error) {
	if ctx.Err() != nil {
		return
	}
	attrs := []any{"step", step}
	if kind != "" {
		attrs = append(attrs, "kind", kind)
	}
	attrs = append(attrs, "error", s.errorText(err.Error()))
	s.log.Error("job step failed", attrs...)
}
