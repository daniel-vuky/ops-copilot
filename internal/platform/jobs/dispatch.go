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

// Dispatcher step names, for the per-step error backoff and the log.
const (
	stepDue   = "due"   // the due-kinds read
	stepSweep = "sweep" // the orphan sweep
)

// periodsStep is the step name of kind's period insert.
func periodsStep(kind string) string { return "periods:" + kind }

// claimStep is the step name of kind's claims.
func claimStep(kind string) string { return "claim:" + kind }

// stepLogEvery is how often a failing step is logged at most.
const stepLogEvery = time.Minute

// lastErrOrphan is the last error of a running job the orphan sweep
// requeued: its finish could not be recorded.
const lastErrOrphan = "finish not recorded"

// tick runs one dispatcher round: the recurring periods, then the claims
// of the due kinds, then the orphan sweep. It is safe for concurrent
// calls. A step in error backoff is skipped, even on a nudge. Once ctx is
// done no step starts, and the claim Write refuses to claim.
func (s *Scheduler) tick(ctx context.Context) {
	s.ensurePeriods(ctx)
	s.claimDue(ctx)
	s.sweep(ctx)
}

// ensurePeriods queues the current period's job of each recurring kind
// whose period this process has not ensured yet (ADR 0006 §5): one Write
// per kind, inserting with the period and the kind's PeriodKey. A period
// that already has its job, or whose key is held by an active job, inserts
// nothing; either way the period is remembered, so it is skipped and not
// retried. Any change of period counts, backwards too: when a clock that
// ran ahead is set back, the real current period is queued at once. A
// failed insert backs off that kind's step only.
func (s *Scheduler) ensurePeriods(ctx context.Context) {
	now := s.clock.Now()
	for _, k := range s.recurringKinds() {
		if ctx.Err() != nil {
			return
		}
		p := periodStart(now, k.kind.Every, k.kind.Offset)
		step := periodsStep(k.kind.Name)
		s.mu.Lock()
		due := !p.Equal(k.lastPeriod) && s.stepReady(step, now)
		s.mu.Unlock()
		if !due {
			continue
		}

		err := s.tx.Write(ctx, func(ctx context.Context) error {
			_, _, err := s.st.Insert(ctx, NewJob{
				Kind:      k.kind.Name,
				RunAfter:  p,
				PeriodAt:  &p,
				UniqueKey: k.kind.PeriodKey,
				Now:       now,
			})
			return err
		})
		if err != nil {
			s.stepFailed(ctx, step, fmt.Errorf("period %s of %s: %w", p.Format(time.RFC3339), k.kind.Name, err))
			continue
		}
		s.mu.Lock()
		k.lastPeriod = p
		s.stepSucceeded(step)
		s.mu.Unlock()
	}
}

// recurringKinds returns the recurring kinds, sorted by name.
func (s *Scheduler) recurringKinds() []*kindState {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*kindState
	for _, name := range slices.Sorted(maps.Keys(s.kinds)) {
		if k := s.kinds[name]; k.kind.Every > 0 {
			out = append(out, k)
		}
	}
	return out
}

// claimDue reads the kinds with a due job, outside any Write, and claims
// the jobs of each registered one whose claim step is not in backoff.
// Unregistered kinds are ignored and stay queued. Jobs in hook backoff do
// not make their kind due.
func (s *Scheduler) claimDue(ctx context.Context) {
	now := s.clock.Now()
	if ctx.Err() != nil || !s.stepReadyNow(stepDue, now) {
		return
	}
	kinds, err := s.st.DueKinds(ctx, now, s.hookSkip(now))
	if err != nil {
		s.stepFailed(ctx, stepDue, err)
		return
	}
	s.stepOK(stepDue)

	for _, name := range kinds {
		k := s.kind(name)
		step := claimStep(name)
		if k == nil || !s.stepReadyNow(step, now) {
			continue
		}
		if err := s.claimKind(ctx, k); err != nil {
			s.stepFailed(ctx, step, err)
			continue
		}
		s.stepOK(step)
	}
}

// sweep requeues running jobs this process does not hold: their finish
// could not be recorded (ADR 0006 §1). It runs at most once a sweep
// interval. It reads the running ids on a reader first and takes the
// writer only when one is not in the registry; inside its Write it reads
// the registry again under s.mu, so a job claimed or finished meanwhile is
// never requeued.
func (s *Scheduler) sweep(ctx context.Context) {
	now := s.clock.Now()
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	prev := s.lastSweep
	due := now.Sub(prev) >= s.t.sweepEvery && s.stepReady(stepSweep, now)
	if due {
		s.lastSweep = now // concurrent ticks: one sweeps
	}
	s.mu.Unlock()
	if !due {
		return
	}
	failed := func(err error) {
		s.mu.Lock()
		if s.lastSweep.Equal(now) {
			s.lastSweep = prev // the step backoff decides the retry
		}
		s.mu.Unlock()
		s.stepFailed(ctx, stepSweep, err)
	}

	ids, err := s.st.RunningIDs(ctx)
	if err != nil {
		failed(err)
		return
	}
	if !s.anyUnheld(ids) {
		s.stepOK(stepSweep)
		return
	}

	var requeued int64
	err = s.tx.Write(ctx, func(ctx context.Context) error {
		// Under the writer no claim or finish commits, so the registry
		// holds exactly this process's running jobs.
		s.mu.Lock()
		except := slices.Sorted(maps.Keys(s.jobs))
		s.mu.Unlock()
		var err error
		requeued, err = s.st.RequeueRunning(ctx, except, lastErrOrphan, now)
		return err
	})
	if err != nil {
		failed(err)
		return
	}
	s.stepOK(stepSweep)
	if requeued > 0 {
		s.log.Warn("orphaned jobs requeued", "count", requeued)
		s.wake()
	}
}

// anyUnheld reports whether an id in ids is not in the registry.
func (s *Scheduler) anyUnheld(ids []int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if _, held := s.jobs[id]; !held {
			return true
		}
	}
	return false
}

// claimKind claims due jobs of k, one Write each, while k has a free slot
// and a job was claimed. A claim-time hook failure puts that job in hook
// backoff and claims again at once, so it never blocks the kind; it is not
// a step failure. It returns the error of a failed claim, or ctx's once
// ctx is done.
func (s *Scheduler) claimKind(ctx context.Context, k *kindState) error {
	// hookFailed holds the jobs whose hook failed in this round; they are
	// skipped even if their backoff has already run out.
	var hookFailed []int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.mu.Lock()
		free := s.freeSlots(k)
		s.mu.Unlock()
		if free == 0 {
			return nil
		}

		out, err := s.claim(ctx, k, hookFailed)
		if err != nil {
			return err
		}
		switch out.result {
		case claimNone:
			return nil
		case claimStarted:
			continue
		case claimHookFailed:
			if err := ctx.Err(); err != nil {
				return err // shutting down: not a hook failure
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
// handler always starts. The Write refuses to claim once ctx is done, so a
// job queued after shutdown began is never claimed.
func (s *Scheduler) claim(ctx context.Context, k *kindState, skip []int64) (claimOutcome, error) {
	var (
		out       claimOutcome
		entry     *jobEntry
		exhausted []Row
	)
	skip = append(s.hookSkip(s.clock.Now()), skip...)
	err := s.tx.Write(ctx, func(ctx context.Context) error {
		out, entry, exhausted = claimOutcome{}, nil, nil
		if err := ctx.Err(); err != nil {
			return err
		}
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
		s.mu.Lock()
		s.handlers++
		s.mu.Unlock()
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
//
// It also prunes entries that ran out more than twice the longest hook
// backoff ago. Such an entry mostly belongs to a job that left the queue
// another way: when Cancel commits between a claim Write whose hook failed
// and its backOffHook call, Cancel deletes the entry and backOffHook then
// creates it again for a job that is no longer queued. A job still queued
// is retried at the first tick after its entry runs out where its kind
// has a free slot; if the kind has had none for that long, pruning only
// restarts the job's backoff at its first step, which is harmless.
func (s *Scheduler) hookSkip(now time.Time) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []int64
	for _, id := range slices.Sorted(maps.Keys(s.hookBackoff)) {
		r := s.hookBackoff[id]
		switch {
		case r.until.After(now):
			ids = append(ids, id)
		case now.Sub(r.until) > 2*s.t.hookBackoffMax:
			delete(s.hookBackoff, id)
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

// stepReadyNow reports whether step may run at now: it is not in error
// backoff.
func (s *Scheduler) stepReadyNow(step string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stepReady(step, now)
}

// stepReady is stepReadyNow for a caller that holds s.mu.
func (s *Scheduler) stepReady(step string, now time.Time) bool {
	return !now.Before(s.steps[step].until)
}

// stepOK resets step's backoff after it succeeded.
func (s *Scheduler) stepOK(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stepSucceeded(step)
}

// stepSucceeded is stepOK for a caller that holds s.mu. The last log time
// is kept, so a flapping step still logs at most once a minute.
func (s *Scheduler) stepSucceeded(step string) {
	st := s.steps[step]
	st.failures = 0
	st.until = time.Time{}
}

// stepFailed records a failed dispatcher step: the step is skipped for
// min(2^(failures-1) s, 30 s) and the failure is logged at most once a
// minute per step, with its error scrubbed and cut. While ctx (Run's) is
// done the scheduler is shutting down, so the failure is not one: it is
// neither logged nor backed off.
func (s *Scheduler) stepFailed(ctx context.Context, step string, err error) {
	if ctx.Err() != nil {
		return
	}
	now := s.clock.Now()
	s.mu.Lock()
	st := s.steps[step]
	st.failures++
	st.until = now.Add(stepBackoff(st.failures, s.t.stepBackoffMax))
	logIt := st.lastLog.IsZero() || now.Sub(st.lastLog) >= stepLogEvery
	if logIt {
		st.lastLog = now
	}
	s.mu.Unlock()
	if logIt {
		s.log.Error("job scheduler step failed", "step", step, "error", s.errorText(err.Error()))
	}
}

// stepBackoff returns the backoff after a step's failures-th consecutive
// failure: 1 s, doubling, at most limit.
func stepBackoff(failures int, limit time.Duration) time.Duration {
	d := time.Second
	for i := 1; i < failures && d < limit; i++ {
		d *= 2
	}
	return min(d, limit)
}
