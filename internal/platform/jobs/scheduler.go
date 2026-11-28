package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/daniel-vuky/ops-copilot/internal/platform/clock"
	"github.com/daniel-vuky/ops-copilot/internal/platform/store"
)

// Options configures a Scheduler. Zero fields take their defaults.
type Options struct {
	// Clock gives "now" for every stored time; nil means clock.System.
	Clock clock.Clock
	// Logger receives the scheduler's log lines; nil means slog.Default().
	Logger *slog.Logger
	// Scrub rewrites every error text (handler, hook, step) before it is
	// cut, stored or logged (ADR 0006 §7); nil means ScrubURLs.
	Scrub func(string) string
}

// Scheduler runs jobs of registered kinds from the jobs table (ADR 0001
// §6, ADR 0006).
//
// Register every kind, then call Recover once at start, then Run. All of
// its methods are safe for concurrent use.
type Scheduler struct {
	st    Store
	tx    store.Transactor
	clock clock.Clock
	log   *slog.Logger
	scrub func(string) string
	t     timings

	// jobCtx is the parent of every handler context. It is detached from
	// Run's context and cancelled, with cause errDrain, only when the
	// drain timeout passes.
	jobCtx     context.Context
	cancelJobs context.CancelCauseFunc

	// nudge wakes the dispatcher at once; sends never block (cap 1).
	nudge chan struct{}

	// mu guards everything below. It is never held while a Write, a store
	// method, a handler or a hook runs; a Write fn may take it briefly.
	mu sync.Mutex
	// kinds holds the registered kinds by name, with their dispatcher
	// state.
	kinds map[string]*kindState
	// jobs is the registry of jobs this process has claimed and not yet
	// finished (or given up finishing), by id.
	jobs map[int64]*jobEntry
	// steps holds the error backoff of each dispatcher step (due-kinds
	// read, period insert per kind, claim per kind, sweep), by step name.
	steps map[string]*stepState
	// hookBackoff holds, by job id, the jobs whose FailedHook failed on
	// the claim-time path and when they may be tried again.
	hookBackoff map[int64]*hookRetry
	// recovered is set once Recover has completed; Register is refused
	// after it and Run requires it.
	recovered bool
}

// timings are the scheduler's intervals. Tests lower them through
// export_test.go.
type timings struct {
	poll           time.Duration   // dispatcher tick without a nudge
	drain          time.Duration   // wait for running handlers on shutdown (ADR 0006 §8)
	cancelWait     time.Duration   // wait after cancelling them
	finishTimeout  time.Duration   // bound of one finish Write
	finishRetries  []time.Duration // delay before each finish try
	sweepEvery     time.Duration   // orphan sweep interval
	stepBackoffMax time.Duration   // longest backoff of a failing step
	hookBackoff    time.Duration   // first backoff of a failing claim-time hook
	hookBackoffMax time.Duration   // its longest backoff (doubling)
}

// defaultTimings returns the production timings.
func defaultTimings() timings {
	return timings{
		poll:           time.Second,
		drain:          6 * time.Second,
		cancelWait:     2 * time.Second,
		finishTimeout:  30 * time.Second,
		finishRetries:  []time.Duration{0, time.Second, 5 * time.Second},
		sweepEvery:     time.Minute,
		stepBackoffMax: 30 * time.Second,
		hookBackoff:    time.Minute,
		hookBackoffMax: 30 * time.Minute,
	}
}

// kindState is a registered kind with its dispatcher state. Its kind and
// retry never change after Register; the dispatcher state (running count,
// and the last ensured period the later tasks add) is guarded by
// Scheduler.mu.
type kindState struct {
	kind  Kind            // normalised by Register
	retry []time.Duration // resolved delays, own copy; empty means one attempt

	// running counts the kind's slots taken by this process: claimed jobs
	// whose finish has not been recorded or given up. Guarded by
	// Scheduler.mu.
	running int
}

// maxAttempts returns the kind's number of attempts: 1 with NoRetry, 3
// with the default delays, else len(Retry)+1.
func (k *kindState) maxAttempts() int {
	return len(k.retry) + 1
}

// delay returns the delay before attempt+1, after attempt failed. It is 0
// when attempt has no attempt after it.
func (k *kindState) delay(attempt int) time.Duration {
	if attempt < 1 || attempt > len(k.retry) {
		return 0
	}
	return k.retry[attempt-1]
}

// jobEntry is a claimed job in the registry. The claim and finish add its
// fields (kind, handler cancel func, sticky stopped flag, finishing flag).
type jobEntry struct{}

// stepState is the error backoff of one dispatcher step. The dispatcher
// adds its fields (consecutive failures, skip-until time, last log time).
type stepState struct{}

// hookRetry is the backoff of a job whose claim-time hook failed. The
// claim adds its fields (skip-until time, next delay).
type hookRetry struct{}

// New returns a Scheduler on st, writing through tx. Register kinds on it,
// then call Recover and Run.
func New(st Store, tx store.Transactor, opts Options) *Scheduler {
	if opts.Clock == nil {
		opts.Clock = clock.System{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Scrub == nil {
		opts.Scrub = ScrubURLs
	}
	jobCtx, cancelJobs := context.WithCancelCause(context.Background())
	return &Scheduler{
		st:          st,
		tx:          tx,
		clock:       opts.Clock,
		log:         opts.Logger,
		scrub:       opts.Scrub,
		t:           defaultTimings(),
		jobCtx:      jobCtx,
		cancelJobs:  cancelJobs,
		nudge:       make(chan struct{}, 1),
		kinds:       make(map[string]*kindState),
		jobs:        make(map[int64]*jobEntry),
		steps:       make(map[string]*stepState),
		hookBackoff: make(map[int64]*hookRetry),
	}
}

// kindName is the pattern of a Kind's Name.
var kindName = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)

// maxUniqueKey is the longest unique key, in bytes; stricter than the
// table's character-count CHECK.
const maxUniqueKey = 128

// errRegisterAfterRecover is returned by Register once Recover has run.
var errRegisterAfterRecover = errors.New("jobs: Register called after Recover")

// Register adds a job kind. It must be called before Recover. It rejects a
// bad or duplicate name, a nil handler, negative numbers or delays,
// NoRetry with a non-nil Retry, an Every under 1 minute or not in whole
// seconds, an Offset outside [0, Every) or not in whole seconds, a
// PeriodKey on a non-recurring kind and MaxWaiting on a recurring one.
//
// It normalises Concurrency 0 to 1, Timeout 0 to DefaultTimeout and, for a
// recurring kind, PeriodKey "" to "period", and keeps its own copy of
// Retry.
func (s *Scheduler) Register(k Kind) error {
	if err := validateKind(k); err != nil {
		return fmt.Errorf("jobs: register %q: %w", k.Name, err)
	}

	ks := &kindState{kind: k}
	switch {
	case k.NoRetry:
		ks.retry = []time.Duration{}
	case k.Retry == nil:
		ks.retry = slices.Clone(defaultRetry)
	default:
		ks.retry = slices.Clone(k.Retry)
	}
	ks.kind.Retry = slices.Clone(k.Retry) // nil stays nil
	if ks.kind.Concurrency == 0 {
		ks.kind.Concurrency = 1
	}
	if ks.kind.Timeout == 0 {
		ks.kind.Timeout = DefaultTimeout
	}
	if ks.kind.Every > 0 && ks.kind.PeriodKey == "" {
		ks.kind.PeriodKey = "period"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recovered {
		return fmt.Errorf("jobs: register %q: %w", k.Name, errRegisterAfterRecover)
	}
	if _, dup := s.kinds[k.Name]; dup {
		return fmt.Errorf("jobs: register %q: kind already registered", k.Name)
	}
	s.kinds[k.Name] = ks
	return nil
}

// validateKind checks k's fields for Register.
func validateKind(k Kind) error {
	switch {
	case !kindName.MatchString(k.Name):
		return fmt.Errorf("name must match %s", kindName)
	case k.Handler == nil:
		return errors.New("handler is nil")
	case k.Concurrency < 0:
		return errors.New("concurrency is negative")
	case k.MaxWaiting < 0:
		return errors.New("max waiting is negative")
	case k.Timeout < 0:
		return errors.New("timeout is negative")
	case k.NoRetry && k.Retry != nil:
		return errors.New("NoRetry with a non-nil Retry")
	case k.Retry != nil && len(k.Retry) == 0:
		return errors.New("empty Retry: use NoRetry for a single attempt")
	case k.Every < 0:
		return errors.New("every is negative")
	case k.Offset < 0:
		return errors.New("offset is negative")
	}
	for i, d := range k.Retry {
		if d < 0 {
			return fmt.Errorf("retry delay %d is negative", i+1)
		}
	}

	if k.Every == 0 {
		switch {
		case k.Offset != 0:
			return errors.New("offset on a non-recurring kind")
		case k.PeriodKey != "":
			return errors.New("period key on a non-recurring kind")
		}
		return nil
	}
	switch {
	case k.Every < minEvery:
		return fmt.Errorf("every is under %v", minEvery)
	case k.Every%time.Second != 0:
		return errors.New("every is not whole seconds")
	case k.Offset >= k.Every:
		return errors.New("offset is not under every")
	case k.Offset%time.Second != 0:
		return errors.New("offset is not whole seconds")
	case len(k.PeriodKey) > maxUniqueKey:
		return fmt.Errorf("period key is over %d bytes", maxUniqueKey)
	case k.MaxWaiting != 0:
		return errors.New("max waiting on a recurring kind")
	}
	return nil
}

// lastErrRestart is the last error of a job Recover requeued.
const lastErrRestart = "interrupted by a restart"

// maxRawError is how much of an error text is scrubbed: the text is cut to
// it before scrubbing and to maxErrorBytes after.
const maxRawError = 16 << 10

// errRecoverTwice is returned by a second call of Recover.
var errRecoverTwice = errors.New("jobs: Recover already ran")

// errorText returns text scrubbed and cut, ready to store or log (ADR 0006
// §7).
func (s *Scheduler) errorText(text string) string {
	return cutError(s.scrub(cutBytes(text, maxRawError)))
}

// enqueueOpts are the options of one Enqueue.
type enqueueOpts struct {
	runAfter  *time.Time
	uniqueKey string
	err       error
}

// EnqueueOption sets an option of Enqueue.
type EnqueueOption func(*enqueueOpts)

// RunAfter makes the job due at t instead of now.
func RunAfter(t time.Time) EnqueueOption {
	return func(o *enqueueOpts) { o.runAfter = &t }
}

// UniqueKey gives the job key k, 1 to 128 bytes: while a queued or running
// job of the kind has the same key, Enqueue refuses with ErrAlreadyActive.
// A recurring kind's period jobs carry its PeriodKey, so a manual job with
// that key never overlaps a period's job (ADR 0006 §5). A key outside 1 to
// 128 bytes makes Enqueue fail.
func UniqueKey(k string) EnqueueOption {
	return func(o *enqueueOpts) {
		if len(k) < 1 || len(k) > maxUniqueKey {
			o.err = fmt.Errorf("unique key must be 1 to %d bytes, got %d", maxUniqueKey, len(k))
			return
		}
		o.uniqueKey = k
	}
}

// kind returns the registered kind called name, or nil.
func (s *Scheduler) kind(name string) *kindState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kinds[name]
}

// freeSlots returns how many more jobs of k this process may run now. The
// caller holds s.mu.
func (s *Scheduler) freeSlots(k *kindState) int {
	return max(0, k.kind.Concurrency-k.running)
}

// Enqueue queues a job of kind with payload, due now unless RunAfter says
// otherwise, and returns its id. A nil payload is stored empty. It also
// works on a recurring kind (a manual run, no period).
//
// Call it inside a store.Transactor Write, with that Write's ctx, so the
// job is queued in the same transaction as the caller's own rows (ADR 0004
// §5); outside one it fails with an error matching store.ErrNoWriteTx and
// queues nothing. It never starts a Write of its own. Its errors leave the
// caller's transaction usable: ErrUnknownKind, ErrPayloadTooLarge,
// ErrQueueFull (the kind's MaxWaiting is reached, ADR 0006 §6) and
// ErrAlreadyActive (UniqueKey in use) insert nothing, and the caller
// decides what to record.
func (s *Scheduler) Enqueue(ctx context.Context, kind string, payload []byte, opts ...EnqueueOption) (int64, error) {
	var o enqueueOpts
	for _, opt := range opts {
		opt(&o)
	}
	if o.err != nil {
		return 0, fmt.Errorf("jobs: enqueue %q: %w", kind, o.err)
	}
	k := s.kind(kind)
	if k == nil {
		return 0, fmt.Errorf("jobs: enqueue %q: %w", kind, ErrUnknownKind)
	}
	if len(payload) > MaxPayload {
		return 0, fmt.Errorf("jobs: enqueue %q: %d bytes: %w", kind, len(payload), ErrPayloadTooLarge)
	}

	if k.kind.MaxWaiting > 0 {
		// The caller's Write holds the writer, so no claim can change the
		// count until it commits; s.mu is taken only after the store call
		// (lock order: writer, then s.mu).
		queued, err := s.st.CountQueued(ctx, kind)
		if err != nil {
			return 0, fmt.Errorf("jobs: enqueue %q: %w", kind, err)
		}
		s.mu.Lock()
		free := s.freeSlots(k)
		s.mu.Unlock()
		if max(0, queued-free) >= k.kind.MaxWaiting {
			return 0, fmt.Errorf("jobs: enqueue %q: %d waiting: %w", kind, queued-free, ErrQueueFull)
		}
	}

	now := s.clock.Now()
	runAfter := now
	if o.runAfter != nil {
		runAfter = *o.runAfter
	}
	id, inserted, err := s.st.Insert(ctx, NewJob{
		Kind:      kind,
		Payload:   payload,
		RunAfter:  runAfter,
		UniqueKey: o.uniqueKey,
		Now:       now,
	})
	if err != nil {
		return 0, fmt.Errorf("jobs: enqueue %q: %w", kind, err)
	}
	if !inserted {
		return 0, fmt.Errorf("jobs: enqueue %q with key %q: %w", kind, o.uniqueKey, ErrAlreadyActive)
	}
	s.wake()
	return id, nil
}

// wake nudges the dispatcher without blocking. The nudge may arrive before
// the caller's Write commits; the next poll then picks the job up.
func (s *Scheduler) wake() {
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

// Cancel cancels job id of kind while it is queued: the job becomes
// cancelled and the kind's OnFailed runs with ReasonCancelled in the same
// transaction (ADR 0006 §3).
//
// Call it inside a store.Transactor Write, with that Write's ctx; it never
// starts a Write of its own. It returns ErrUnknownKind for an unregistered
// kind and an error matching ErrNotCancellable when the job is missing, of
// another kind, or not queued; neither writes anything, so the caller's
// transaction stays usable. A running job is stopped with Stop after the
// caller's Write commits. Any other error, including the hook's, must be
// returned from the caller's Write fn, so the transaction rolls back.
func (s *Scheduler) Cancel(ctx context.Context, kind string, id int64) error {
	k := s.kind(kind)
	if k == nil {
		return fmt.Errorf("jobs: cancel %q job %d: %w", kind, id, ErrUnknownKind)
	}
	row, found, err := s.st.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("jobs: cancel %q job %d: %w", kind, id, err)
	}
	if !found || row.Kind != kind {
		return fmt.Errorf("jobs: cancel %q job %d: no such job: %w", kind, id, ErrNotCancellable)
	}
	if row.State != StateQueued {
		return fmt.Errorf("jobs: cancel %q job %d: job is %s: %w", kind, id, row.State, ErrNotCancellable)
	}

	changed, err := s.st.MarkFinal(ctx, id, StateQueued, StateCancelled, nil, s.clock.Now())
	if err != nil {
		return fmt.Errorf("jobs: cancel %q job %d: %w", kind, id, err)
	}
	if !changed {
		return fmt.Errorf("jobs: cancel %q job %d: job is no longer queued: %w", kind, id, ErrNotCancellable)
	}
	if err := s.runHook(ctx, k, row, ReasonCancelled, row.LastError); err != nil {
		return fmt.Errorf("jobs: cancel %q job %d: failure hook: %w", kind, id, err)
	}

	s.mu.Lock()
	delete(s.hookBackoff, id)
	s.mu.Unlock()
	return nil
}

// jobOf returns the Job a handler or hook of k sees for row.
func jobOf(k *kindState, row Row) Job {
	return Job{
		ID:          row.ID,
		Kind:        row.Kind,
		Payload:     row.Payload,
		Attempt:     row.Attempts,
		MaxAttempts: k.maxAttempts(),
		PeriodAt:    row.PeriodAt,
		UniqueKey:   row.UniqueKey,
	}
}

// runHook runs k's OnFailed, if any, for row with ctx, the ctx of the
// Write that marks the job failed or cancelled. A panic is recovered as
// the error "hook panic: <value>". The returned error's text is scrubbed
// and cut, and it wraps nothing, so the hook's raw text never escapes.
func (s *Scheduler) runHook(ctx context.Context, k *kindState, row Row, reason Reason, lastErr string) (err error) {
	if k.kind.OnFailed == nil {
		return nil
	}
	defer func() {
		if v := recover(); v != nil {
			err = errors.New(s.errorText(fmt.Sprintf("hook panic: %v", v)))
		}
	}()
	if herr := k.kind.OnFailed(ctx, jobOf(k, row), reason, lastErr); herr != nil {
		return errors.New(s.errorText(herr.Error()))
	}
	return nil
}

// Recover is the startup requeue (ADR 0001 §6, ADR 0006 §2). Call it once
// at start, after every Register and before Run and the HTTP listener.
//
// In one Write it puts every running job back in the queue, due now, with
// the last error "interrupted by a restart" and its attempts kept. It logs
// "jobs requeued" when it requeued any, and warns once about queued jobs
// of kinds that are not registered, which would otherwise stay queued
// silently. After it, Register fails. An error should fail the start.
func (s *Scheduler) Recover(ctx context.Context) error {
	s.mu.Lock()
	done := s.recovered
	s.mu.Unlock()
	if done {
		return errRecoverTwice
	}

	now := s.clock.Now()
	var requeued int64
	err := s.tx.Write(ctx, func(ctx context.Context) error {
		var err error
		requeued, err = s.st.RequeueRunning(ctx, nil, lastErrRestart, now)
		return err
	})
	if err != nil {
		return fmt.Errorf("jobs: recover: %w", err)
	}
	if requeued > 0 {
		s.log.InfoContext(ctx, "jobs requeued", "count", requeued)
	}

	// Read after the Write committed: the readers see the requeued rows.
	queued, err := s.st.QueuedByKind(ctx)
	if err != nil {
		return fmt.Errorf("jobs: recover: %w", err)
	}

	s.mu.Lock()
	var unknown []any
	for _, kind := range slices.Sorted(maps.Keys(queued)) {
		if _, ok := s.kinds[kind]; !ok {
			unknown = append(unknown, slog.Int(kind, queued[kind]))
		}
	}
	s.recovered = true
	s.mu.Unlock()

	if len(unknown) > 0 {
		s.log.WarnContext(ctx, "queued jobs of unregistered kinds", slog.Group("kinds", unknown...))
	}
	return nil
}
