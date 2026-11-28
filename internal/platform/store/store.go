// Package store holds the persistence port domain code writes through
// (ADR 0001 §3, ADR 0005 §2). It knows no SQL and no driver: adapters
// implement Transactor and keep their transaction in the context with
// WithTx, so repositories of several domains can share one transaction
// without importing each other.
package store

import (
	"context"
	"errors"
)

// Transactor runs write transactions on the one writer connection.
type Transactor interface {
	// Write runs fn in one write transaction that starts with BEGIN
	// IMMEDIATE, and passes fn a context that carries it. Repositories take
	// the transaction from that context, so every repository called with
	// fn's ctx writes, and reads its own uncommitted rows, in this one
	// transaction.
	//
	// Write commits when fn returns nil. It rolls back when fn returns an
	// error, panics (the panic is not swallowed) or exits its goroutine
	// with runtime.Goexit.
	//
	// A Write called with a context that already carries a live transaction
	// joins it instead of starting another. If the nested fn fails, the
	// transaction stays usable until the outer fn returns and then rolls
	// back: the outer Write returns ErrRolledBack even when its own fn
	// returns nil.
	//
	// Inside fn, always pass fn's ctx on, never context.Background() or a
	// context derived from something else. With a context that carries no
	// transaction:
	//   - a repository write fails with ErrNoWriteTx (ErrWriteTxDone if
	//     the transaction already ended) and writes nothing;
	//   - a nested Write waits for the writer held by the outer Write and
	//     fails with ErrWriterBusy;
	//   - a read goes to a reader connection and does not see the
	//     transaction's own uncommitted rows.
	// fn's ctx ends when Write returns, so a context kept from fn cannot
	// start a later Write.
	//
	// Waiting for the writer connection is bounded by 30 s, or by ctx's
	// deadline if that is sooner. If the bound or the caller's deadline
	// expires first, Write fails with an error matching ErrWriterBusy (and
	// also context.DeadlineExceeded when it was the caller's deadline). If
	// the caller cancels ctx, Write fails with an error matching
	// context.Canceled, not ErrWriterBusy.
	//
	// fn does database work only: no network calls, no document
	// extraction, no waiting on other jobs, since every other writer waits
	// while it runs.
	Write(ctx context.Context, fn func(ctx context.Context) error) error
}

// Errors returned by Transactor implementations and the repositories built
// on them. Match them with errors.Is.
var (
	// ErrNoWriteTx is returned by a repository write whose context carries
	// no write transaction, or one of another store.
	ErrNoWriteTx = errors.New("store: no write transaction in context")

	// ErrWriteTxDone is returned by a repository write whose context
	// carries a transaction that already committed or rolled back.
	ErrWriteTxDone = errors.New("store: write transaction already ended")

	// ErrWriterBusy is returned by Write when the writer connection could
	// not be had within the wait bound.
	ErrWriterBusy = errors.New("store: writer connection not available")

	// ErrRolledBack is returned by an outer Write whose fn returned nil
	// after a nested Write failed; nothing was committed.
	ErrRolledBack = errors.New("store: transaction rolled back after a nested write failed")
)

// TxState is what an adapter stores in the context for a write transaction.
// Implementations must be safe for concurrent use: fn may fan out
// goroutines that share its context.
type TxState interface {
	// Done reports whether the transaction has committed or rolled back.
	Done() bool
}

// txKey is the context key for the TxState. It is unexported so only this
// package can set or read the value.
type txKey struct{}

// WithTx returns a copy of ctx that carries s. Adapters call it when they
// start a write transaction.
func WithTx(ctx context.Context, s TxState) context.Context {
	return context.WithValue(ctx, txKey{}, s)
}

// TxFrom returns the TxState ctx carries, and whether it carries one.
func TxFrom(ctx context.Context) (TxState, bool) {
	s, ok := ctx.Value(txKey{}).(TxState)
	return s, ok
}

// InWrite reports whether ctx carries a write transaction that has not
// ended.
func InWrite(ctx context.Context) bool {
	s, ok := TxFrom(ctx)
	return ok && !s.Done()
}
