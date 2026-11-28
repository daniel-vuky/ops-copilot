package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	msqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/daniel-vuky/ops-copilot/internal/platform/store"
)

var (
	_ store.Transactor = (*DB)(nil)
	_ store.TxState    = (*txState)(nil)
	_ Querier          = txQuerier{}
	_ Querier          = (*sql.DB)(nil)
)

// errTxLost reports that the write transaction was lost before Write ended
// it: SQLite rolled it back on its own (for example after an interrupted
// write statement), or the store rolled it back because a statement failed
// under an ended context.
var errTxLost = errors.New("store: write transaction was lost and rolled back")

// Querier runs statements. The reader pool (*sql.DB) and the handle WriteTx
// returns both implement it, so a repository reads the same way inside and
// outside a Write.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// txState is the store.TxState this adapter keeps in the context of a
// Write. tx is set before the state is shared and never changes. mu guards
// the fields below it; stmtMu serialises the statements of the
// transaction, so none can start between the loss of the transaction and
// its detection. The hook flags are atomic because SQLite calls the hooks
// while a statement runs, with stmtMu held.
type txState struct {
	owner *DB
	tx    *sql.Tx

	stmtMu sync.Mutex

	mu     sync.Mutex
	done   bool  // no statement may run: committing, ended or lost
	ended  bool  // Write has returned: a Write with this ctx starts anew
	failed bool  // a nested Write failed (set only by join): roll back
	lost   bool  // SQLite ended the transaction before Write did
	cause  error // the statement error that showed the loss, if any

	rolledBack atomic.Bool // set by SQLite's rollback hook
	committing atomic.Bool // Write is running COMMIT; other commits are vetoed
}

// Done reports whether the transaction has committed, rolled back or been
// lost. It turns true just before the commit starts.
func (s *txState) Done() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

func (s *txState) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = true
}

func (s *txState) markEnded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = true
	s.ended = true
}

func (s *txState) isEnded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended
}

func (s *txState) fail() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = true
}

func (s *txState) hasFailed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

// lose marks the transaction lost and rolls back the *sql.Tx, so
// database/sql stops running statements on it in autocommit. The caller
// holds stmtMu.
func (s *txState) lose(cause error) {
	s.mu.Lock()
	if !s.lost {
		s.lost = true
		s.cause = cause
	}
	s.done = true
	s.mu.Unlock()
	// SQLite may already have rolled back, so the ROLLBACK can fail; the
	// transaction is gone either way.
	_ = s.tx.Rollback()
}

// lostErr returns the error Write reports when the transaction was lost,
// or nil.
func (s *txState) lostErr() error {
	s.mu.Lock()
	lost, cause := s.lost, s.cause
	s.mu.Unlock()
	switch {
	case lost && cause != nil:
		// %w, not %v: the caller can match the statement's cause, such as
		// the DeadlineExceeded of a context fn derived for one statement.
		return fmt.Errorf("%w: %w", errTxLost, cause)
	case lost || s.rolledBack.Load():
		return errTxLost
	default:
		return nil
	}
}

// usable returns nil when a statement may run, and store.ErrWriteTxDone
// otherwise. The caller holds stmtMu.
func (s *txState) usable() error {
	if s.rolledBack.Load() && !s.Done() {
		s.lose(nil)
	}
	if s.Done() {
		return store.ErrWriteTxDone
	}
	return nil
}

// check runs after every statement: if SQLite rolled the transaction back
// (the hook fired, e.g. on an executed ROLLBACK) or the statement failed in
// a way that can end it, the transaction is marked lost. The caller holds
// stmtMu.
func (s *txState) check(ctx context.Context, err error) {
	if s.rolledBack.Load() || (err != nil && endsTx(ctx, err)) {
		s.lose(err)
	}
}

// endsTx reports whether a statement error is treated as the end of the
// transaction: any statement, read or write, that failed while its context
// had ended (SQLite rolls back on an interrupted write), and any statement
// failing with SQLITE_INTERRUPT, SQLITE_FULL, SQLITE_IOERR or SQLITE_NOMEM.
// The rollback hook is the primary detection; this is the fallback.
func endsTx(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	var se *msqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code() & 0xff {
	case sqlite3.SQLITE_INTERRUPT, sqlite3.SQLITE_FULL, sqlite3.SQLITE_IOERR, sqlite3.SQLITE_NOMEM:
		return true
	}
	return false
}

// commitHook vetoes every commit on the writer connection during a Write
// except Write's own COMMIT. A statement that database/sql runs after
// SQLite lost the transaction would otherwise commit on its own in
// autocommit mode.
func (s *txState) commitHook() int32 {
	if s.committing.Load() {
		return 0
	}
	return 1
}

func (s *txState) rollbackHook() {
	s.rolledBack.Store(true)
}

// setHooks registers s's commit and rollback hooks on the driver
// connection of conn, or clears them when s is nil.
func setHooks(conn *sql.Conn, s *txState) error {
	return conn.Raw(func(dc any) error {
		hr, ok := dc.(msqlite.HookRegisterer)
		if !ok {
			return fmt.Errorf("driver connection %T has no hooks", dc)
		}
		if s == nil {
			hr.RegisterCommitHook(nil)
			hr.RegisterRollbackHook(nil)
			return nil
		}
		hr.RegisterCommitHook(s.commitHook)
		hr.RegisterRollbackHook(s.rollbackHook)
		return nil
	})
}

// txQuerier is the handle WriteTx returns. It runs statements on the write
// transaction one at a time, refuses them once the transaction is done or
// lost, and does not expose the *sql.Tx, so only Write commits or rolls
// back.
type txQuerier struct {
	s *txState
}

// ExecContext runs a statement in the transaction. It fails with
// store.ErrWriteTxDone once the transaction is done or lost.
func (q txQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	s := q.s
	s.stmtMu.Lock()
	defer s.stmtMu.Unlock()
	if err := s.usable(); err != nil {
		return nil, err
	}
	res, err := s.tx.ExecContext(ctx, query, args...)
	s.check(ctx, err)
	return res, err
}

// QueryContext runs a query in the transaction. It fails with
// store.ErrWriteTxDone once the transaction is done or lost. An error met
// while iterating the rows is not seen here; if it ended the transaction,
// the rollback hook marks it lost.
func (q txQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	s := q.s
	s.stmtMu.Lock()
	defer s.stmtMu.Unlock()
	if err := s.usable(); err != nil {
		return nil, err
	}
	rows, err := s.tx.QueryContext(ctx, query, args...)
	s.check(ctx, err)
	return rows, err
}

// QueryRowContext runs a one-row query in the transaction. A *sql.Row
// cannot carry another error, so once the transaction is lost or ended its
// Scan fails with sql.ErrTxDone rather than store.ErrWriteTxDone.
func (q txQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	s := q.s
	s.stmtMu.Lock()
	defer s.stmtMu.Unlock()
	_ = s.usable() // rolls the *sql.Tx back if SQLite lost the transaction
	row := s.tx.QueryRowContext(ctx, query, args...)
	s.check(ctx, row.Err())
	return row
}

// Write implements store.Transactor: it runs fn in one BEGIN IMMEDIATE
// transaction on the writer connection, or joins the transaction of this
// DB that ctx carries while its Write runs. See store.Transactor for the
// contract.
//
// fn must wait for every goroutine it starts before it returns. On the
// rollback path the context passed to fn is cancelled first, so a
// statement still running under it is interrupted.
//
// If SQLite ends the transaction on its own (an interrupted write
// statement, a full disk, an I/O error), or a statement fails while its
// context has ended (a read included), the transaction is treated as
// lost: every later statement in fn fails with store.ErrWriteTxDone, no
// commit can happen on the writer connection until Write returns, and
// Write fails with an error that wraps the statement error (and
// store.ErrRolledBack when the loss surfaced in a nested Write and fn
// returned nil).
func (db *DB) Write(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	if s, ok := db.stateOf(ctx); ok && !s.isEnded() {
		return join(ctx, s, fn)
	}

	conn, err := db.acquireWriter(ctx)
	if err != nil {
		return err
	}
	s := &txState{owner: db}
	if err := setHooks(conn, s); err != nil {
		_ = conn.Close()
		return fmt.Errorf("store: set transaction hooks: %w", err)
	}
	// ctx, not the wait bound: database/sql rolls the transaction back
	// when the context given to BeginTx ends. The driver's _txlock makes
	// this BEGIN IMMEDIATE.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		_ = setHooks(conn, nil)
		_ = conn.Close()
		return fmt.Errorf("store: begin write transaction: %w", err)
	}
	s.tx = tx
	start := time.Now()
	fnCtx, cancelFn := context.WithCancel(store.WithTx(ctx, s))
	defer cancelFn()

	// Runs on return, panic and runtime.Goexit alike.
	committed := false
	defer func() {
		if !committed {
			cancelFn() // interrupt statements fn left running
		}
		s.finish()
		if !committed {
			// After SQLite rolled back on its own, the ROLLBACK fails; that
			// is expected and the loss is already reported.
			if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) && !s.rolledBack.Load() {
				err = errors.Join(err, fmt.Errorf("store: roll back write transaction: %w", rbErr))
			}
		}
		var cleanup []error
		if hErr := setHooks(conn, nil); hErr != nil && !errors.Is(hErr, sql.ErrConnDone) {
			cleanup = append(cleanup, fmt.Errorf("store: clear transaction hooks: %w", hErr))
		}
		s.markEnded()
		if cErr := conn.Close(); cErr != nil && !errors.Is(cErr, sql.ErrConnDone) {
			cleanup = append(cleanup, fmt.Errorf("store: release the writer: %w", cErr))
		}
		if cErr := errors.Join(cleanup...); cErr != nil {
			if committed {
				// The data is committed; failing the Write would mislead.
				db.logger.Error("write transaction cleanup failed", "error", cErr)
			} else {
				err = errors.Join(err, cErr)
			}
		}
		if d := time.Since(start); d > db.slowWrite {
			db.logger.Warn("slow write transaction", "duration_ms", d.Milliseconds())
		}
	}()

	fnErr := fn(fnCtx)
	// Read the loss before ctx: a cancel landing in between then still
	// takes the cancellation branch, whose error matches ctx's.
	lostErr := s.lostErr()
	if ctxErr := ctx.Err(); ctxErr != nil {
		// The cancellation explains any rollback; keep it matchable.
		switch {
		case fnErr == nil:
			return fmt.Errorf("store: write cancelled before commit: %w", ctxErr)
		case errors.Is(fnErr, ctxErr):
			return fnErr
		default:
			return fmt.Errorf("store: write cancelled: %w: %w", ctxErr, fnErr)
		}
	}
	if lostErr != nil {
		if fnErr == nil && s.hasFailed() {
			return errors.Join(store.ErrRolledBack, lostErr)
		}
		return errors.Join(lostErr, fnErr)
	}
	if fnErr != nil {
		return fnErr
	}
	if s.hasFailed() {
		return store.ErrRolledBack
	}

	s.finish()
	s.committing.Store(true)
	if cErr := tx.Commit(); cErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(cErr, sql.ErrTxDone) {
			return fmt.Errorf("store: write cancelled before commit: %w", ctxErr)
		}
		return fmt.Errorf("store: commit write transaction: %w", cErr)
	}
	committed = true
	return nil
}

// join runs fn in the outer Write's transaction. If fn returns an error,
// panics or exits its goroutine, the transaction is marked to roll back
// when the outer fn returns.
func join(ctx context.Context, s *txState, fn func(ctx context.Context) error) error {
	ok := false
	defer func() {
		if !ok {
			s.fail()
		}
	}()
	if err := fn(ctx); err != nil {
		return err
	}
	ok = true
	return nil
}

// acquireWriter waits for the writer connection, at most db.writerWait or
// until ctx ends (ADR 0005 §2).
func (db *DB) acquireWriter(ctx context.Context) (*sql.Conn, error) {
	waitCtx, cancel := context.WithTimeout(ctx, db.writerWait)
	defer cancel()
	conn, err := db.writer.Conn(waitCtx)
	if err == nil {
		return conn, nil
	}
	ctxErr := ctx.Err()
	switch {
	case errors.Is(ctxErr, context.Canceled):
		return nil, fmt.Errorf("store: waiting for the writer: %w", ctxErr)
	case errors.Is(ctxErr, context.DeadlineExceeded):
		return nil, fmt.Errorf("store: waiting for the writer: %w: %w", store.ErrWriterBusy, ctxErr)
	case waitCtx.Err() != nil:
		return nil, fmt.Errorf("store: waiting for the writer: %w after %s", store.ErrWriterBusy, db.writerWait)
	default:
		return nil, fmt.Errorf("store: get the writer: %w", err)
	}
}

// stateOf returns the txState of this DB that ctx carries, ended or not.
// The context has one transaction slot: a Write on another DB inside fn
// replaces it, which is fine with one DB per process.
func (db *DB) stateOf(ctx context.Context) (*txState, bool) {
	ts, ok := store.TxFrom(ctx)
	if !ok {
		return nil, false
	}
	s, ok := ts.(*txState)
	if !ok || s.owner != db {
		return nil, false
	}
	return s, true
}

// WriteTx returns a handle on the write transaction ctx carries, for
// repository writes. It fails with store.ErrNoWriteTx when ctx carries
// none of this DB, and with store.ErrWriteTxDone when it is done. The
// handle refuses statements once the transaction is done, and only Write
// commits or rolls back.
func (db *DB) WriteTx(ctx context.Context) (Querier, error) {
	s, ok := db.stateOf(ctx)
	if !ok {
		return nil, store.ErrNoWriteTx
	}
	if s.Done() {
		return nil, store.ErrWriteTxDone
	}
	return txQuerier{s: s}, nil
}

// Querier returns where a repository reads: the write transaction of this
// DB that ctx carries while its Write runs, so a read sees its uncommitted
// rows (and fails once the transaction is done), or else the reader pool.
func (db *DB) Querier(ctx context.Context) Querier {
	if s, ok := db.stateOf(ctx); ok && !s.isEnded() {
		return txQuerier{s: s}
	}
	return db.readers
}

// Reader returns the read-only reader pool.
func (db *DB) Reader() *sql.DB {
	return db.readers
}
