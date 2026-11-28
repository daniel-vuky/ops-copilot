package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
// retry never change after Register; the dispatcher state the later tasks
// add (running count, last ensured period) is guarded by Scheduler.mu.
type kindState struct {
	kind  Kind            // normalised by Register
	retry []time.Duration // resolved delays, own copy; empty means one attempt
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

// maxUniqueKey is the longest unique key, in bytes (the jobs table's
// CHECK).
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
