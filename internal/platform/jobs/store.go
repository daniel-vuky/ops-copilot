package jobs

import (
	"context"
	"time"
)

// State is the state of a job row.
type State string

// Job states. Queued and running are active; succeeded, failed and
// cancelled are final.
const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

// NewJob is a job to insert. It is inserted queued with no attempts.
type NewJob struct {
	// Kind names the registered job kind, 1 to 64 characters.
	Kind string
	// Payload holds references (ids), not content, at most 64 KiB. Nil is
	// stored as an empty blob.
	Payload []byte
	// RunAfter is the earliest time the job may be claimed.
	RunAfter time.Time
	// PeriodAt is the period of a periodic job; at most one job of a kind
	// exists per period. Nil means not periodic.
	PeriodAt *time.Time
	// UniqueKey, when not "", allows at most one active (queued or
	// running) job of the kind with this key, 1 to 128 characters. "" is
	// stored as NULL.
	UniqueKey string
	// Now is the creation time, from the clock.
	Now time.Time
}

// Row is a stored job. Times are UTC.
type Row struct {
	ID       int64
	Kind     string
	Payload  []byte
	State    State
	Attempts int // attempts started, incremented at claim
	RunAfter time.Time
	PeriodAt *time.Time // nil when not periodic
	// UniqueKey is "" when the job has none.
	UniqueKey string
	// LastError is the latest scrubbed error text, "" when none.
	LastError string
	CreatedAt time.Time
	UpdatedAt time.Time
	// StartedAt is the start of the latest attempt, nil before the first.
	StartedAt *time.Time
	// FinishedAt is set when the job reaches a final state.
	FinishedAt *time.Time
}

// Store persists jobs. Implementations know the SQL; this package does
// not.
//
// The write methods and the reads that decide a claim run inside a
// store.Transactor Write and use its transaction: called with a context
// that carries none, they fail with an error matching store.ErrNoWriteTx.
// Every bool result of a Mark method reports whether exactly one row
// changed; each update is guarded by the state it moves from, so false
// means the row is missing or not in that state.
type Store interface {
	// Insert adds j as a queued job. When j conflicts with an existing
	// job of its kind (same PeriodAt, or same UniqueKey while that job is
	// active), nothing is inserted and Insert returns (0, false, nil).
	Insert(ctx context.Context, j NewJob) (id int64, inserted bool, err error)
	// CountQueued returns the number of queued jobs of kind, due or not.
	CountQueued(ctx context.Context, kind string) (int, error)
	// NextDue returns the queued job of kind with the oldest RunAfter at
	// or before now (ties by id), skipping the ids in skip. ok is false
	// when there is none.
	NextDue(ctx context.Context, kind string, now time.Time, skip []int64) (r Row, ok bool, err error)
	// MarkRunning claims a queued job: it becomes running, its attempts
	// grow by one and StartedAt is now.
	MarkRunning(ctx context.Context, id int64, now time.Time) (bool, error)
	// MarkSucceeded finishes a running job: it becomes succeeded,
	// FinishedAt is now and LastError is cleared.
	MarkSucceeded(ctx context.Context, id int64, now time.Time) (bool, error)
	// MarkRetry puts a running job back in the queue, due at runAfter,
	// with lastErr as its LastError.
	MarkRetry(ctx context.Context, id int64, runAfter time.Time, lastErr string, now time.Time) (bool, error)
	// MarkFinal moves a job from state from (queued or running) to state
	// to (failed or cancelled) and sets FinishedAt. A nil lastErr keeps
	// the stored LastError. Any other from or to is an error.
	MarkFinal(ctx context.Context, id int64, from, to State, lastErr *string, now time.Time) (bool, error)
	// RequeueRunning puts every running job whose id is not in except
	// back in the queue, due now, with lastErr as its LastError and its
	// attempts unchanged. It returns the number of jobs requeued.
	RequeueRunning(ctx context.Context, except []int64, lastErr string, now time.Time) (int64, error)

	// Get returns job id, read in the Write that ctx carries if any (so it
	// sees that Write's own rows), else from the readers. ok is false when
	// there is no such job.
	Get(ctx context.Context, id int64) (r Row, ok bool, err error)

	// DueKinds returns, in order, the kinds with a queued job due at or
	// before now. It reads from the readers, outside any Write.
	DueKinds(ctx context.Context, now time.Time) ([]string, error)
	// RunningIDs returns the ids of the running jobs in order. It reads
	// from the readers, outside any Write.
	RunningIDs(ctx context.Context) ([]int64, error)
	// QueuedByKind returns the number of queued jobs of each kind that has
	// any. It reads from the readers, outside any Write.
	QueuedByKind(ctx context.Context) (map[string]int, error)
}
