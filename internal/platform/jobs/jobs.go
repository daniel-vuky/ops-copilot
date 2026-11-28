package jobs

import (
	"context"
	"errors"
	"time"
)

// Job is one attempt of a job, as a handler sees it.
type Job struct {
	// ID is the job row's id.
	ID int64
	// Kind is the registered kind name.
	Kind string
	// Payload holds references (ids), not content. It is never logged.
	Payload []byte
	// Attempt is the attempt a handler runs, 1 for the first. Attempts
	// are counted at claim, so one cut off by a restart, a shutdown or a
	// lost finish still counts (ADR 0006 §2). A FailedHook sees the
	// attempts started: 0 for a job cancelled before its first claim.
	Attempt int
	// MaxAttempts is the kind's number of attempts.
	MaxAttempts int
	// PeriodAt is the period start of a recurring job, else nil.
	PeriodAt *time.Time
	// UniqueKey is the job's unique key, "" when none.
	UniqueKey string
}

// Handler runs one attempt of a job.
//
// Delivery is at least once (ADR 0006 §1): a handler may run again for an
// attempt that already did its work (lost finish, restart), so it must be
// idempotent. Its ctx ends at the kind's Timeout, on shutdown after the
// drain window, or when the job is stopped (cause ErrStopped).
//
// The returned error's text, or a panic value, is scrubbed, cut to 1 KiB,
// stored as the job's last error and logged (ADR 0006 §7): it must hold no
// secret. Wrap it with Permanent to skip the remaining attempts.
type Handler func(ctx context.Context, job Job) error

// Reason says why a FailedHook runs.
type Reason int

// Reasons passed to a FailedHook.
const (
	// ReasonFailed: the job has no attempt left, or its error was
	// Permanent.
	ReasonFailed Reason = iota + 1
	// ReasonCancelled: the job was cancelled while queued, or stopped
	// while running.
	ReasonCancelled
)

// String returns the reason's name.
func (r Reason) String() string {
	switch r {
	case ReasonFailed:
		return "failed"
	case ReasonCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

// FailedHook tells a job's owning domain that the job reached a final
// failed or cancelled state (ADR 0006 §3).
//
// It runs inside the Write that marks the job failed or cancelled, with
// that Write's ctx, so the owner moves its own row to a final state in the
// same transaction. It does database work only (ADR 0005 §2).
//
// Because delivery is at least once, a job can be marked failed after its
// last attempt did its work but its finish was lost: the hook must move
// the owning row only when that row is not already final (a conditional
// UPDATE). States in between are the owning domain's to record.
//
// lastErr is the job's scrubbed last error. It may be a scheduler text
// such as "finish not recorded" or "interrupted by a restart" rather than
// a handler's error.
//
// An error rolls the whole Write back: the job stays as it was and the
// scheduler tries again later with its own backoff.
type FailedHook func(ctx context.Context, job Job, reason Reason, lastErr string) error

// Kind describes a job kind. Register it on the Scheduler before Recover.
type Kind struct {
	// Name matches ^[a-z][a-z0-9_.]{0,63}$ and is unique.
	Name string
	// Handler runs each attempt. Required.
	Handler Handler
	// OnFailed, when not nil, runs when a job of the kind fails for good
	// or is cancelled.
	OnFailed FailedHook
	// Concurrency is how many jobs of the kind run at once; 0 means 1.
	Concurrency int
	// MaxWaiting caps the kind's waiting jobs (queued, minus free running
	// slots, ADR 0006 §6); 0 means no limit. Not allowed on a recurring
	// kind.
	MaxWaiting int
	// Retry lists the delays before attempt 2, 3, ...; nil means the
	// default (30 s, then 2 min: 3 attempts).
	Retry []time.Duration
	// NoRetry gives the kind a single attempt. Retry must then be nil.
	NoRetry bool
	// Timeout is the deadline of one attempt; 0 means DefaultTimeout.
	Timeout time.Duration
	// Every, when > 0, makes the kind recurring with this period: at least
	// 1 minute, in whole seconds (ADR 0006 §5).
	Every time.Duration
	// Offset shifts period starts from the Unix epoch (UTC), in [0, Every)
	// and whole seconds. Only for a recurring kind.
	Offset time.Duration
	// PeriodKey is the unique key period jobs carry, so a manual job
	// enqueued with the same key never overlaps a period's job. Only for a
	// recurring kind; "" means "period".
	PeriodKey string
}

// DefaultTimeout is the deadline of one attempt when a Kind sets none
// (ADR 0006 §4).
const DefaultTimeout = 15 * time.Minute

// MaxPayload is the largest payload, in bytes (ADR 0006 §7).
const MaxPayload = 64 << 10

// defaultRetry is the delay before attempts 2 and 3 when a Kind sets no
// Retry (ADR 0001 §6, ADR 0006 §4). Never handed out without a copy.
var defaultRetry = []time.Duration{30 * time.Second, 2 * time.Minute}

// Errors returned by the Scheduler. Match them with errors.Is.
var (
	// ErrQueueFull is returned by Enqueue when the kind's MaxWaiting is
	// reached; nothing was inserted.
	ErrQueueFull = errors.New("jobs: queue full")
	// ErrAlreadyActive is returned by Enqueue when a queued or running job
	// of the kind has the same unique key; nothing was inserted.
	ErrAlreadyActive = errors.New("jobs: a job with this key is already active")
	// ErrUnknownKind is returned for a kind that is not registered.
	ErrUnknownKind = errors.New("jobs: unknown kind")
	// ErrPayloadTooLarge is returned by Enqueue for a payload over
	// MaxPayload bytes.
	ErrPayloadTooLarge = errors.New("jobs: payload too large")
	// ErrNotCancellable is returned by Cancel for a job that is missing,
	// of another kind, or not queued.
	ErrNotCancellable = errors.New("jobs: job is not cancellable")
	// ErrStopped is the cause of a handler context cancelled by Stop.
	ErrStopped = errors.New("jobs: job stopped")
	// ErrNotRecovered is returned by Run when Recover has not completed.
	ErrNotRecovered = errors.New("jobs: Recover has not run")
)

// Permanent marks err as permanent: a handler returning it (or an error
// wrapping it) skips the job's remaining attempts. errors.Is(Permanent(e),
// e) holds. Permanent(nil) is nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

// permanentError is the error Permanent returns.
type permanentError struct {
	err error
}

// Error returns the wrapped error's text unchanged.
func (e *permanentError) Error() string { return e.err.Error() }

// Unwrap returns the wrapped error.
func (e *permanentError) Unwrap() error { return e.err }

// isPermanent reports whether err is, or wraps, an error made by Permanent.
func isPermanent(err error) bool {
	var pe *permanentError
	return errors.As(err, &pe)
}
