package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	msqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/daniel-vuky/ops-copilot/internal/platform/store"
)

var errBoom = errors.New("boom")

// openKV opens a database in a temp dir and creates the table kv through
// a Write.
func openKV(t *testing.T) *DB {
	t.Helper()
	db := openTest(t, t.TempDir())
	err := db.Write(t.Context(), func(ctx context.Context) error {
		q, err := db.WriteTx(ctx)
		if err != nil {
			return err
		}
		_, err = q.ExecContext(ctx, "CREATE TABLE kv(k TEXT PRIMARY KEY, v TEXT) STRICT")
		return err
	})
	if err != nil {
		t.Fatalf("create kv: %v", err)
	}
	return db
}

// insert adds key k to kv through the write transaction ctx carries. It
// returns an error instead of failing the test, so goroutines can use it.
func insert(ctx context.Context, db *DB, k string) error {
	q, err := db.WriteTx(ctx)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, "INSERT INTO kv(k, v) VALUES(?, ?)", k, "v"); err != nil {
		return fmt.Errorf("insert %s: %w", k, err)
	}
	return nil
}

// countWith returns the number of rows in kv read through q.
func countWith(ctx context.Context, q Querier) (int, error) {
	var n int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM kv").Scan(&n); err != nil {
		return 0, fmt.Errorf("count: %w", err)
	}
	return n, nil
}

// count returns the number of committed rows in kv, read on a reader.
func count(t *testing.T, db *DB) int {
	t.Helper()
	n, err := countWith(t.Context(), db.Reader())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// writeOne inserts k in its own Write and fails the test on error. The
// Write is bounded by a second so a writer that was never released fails
// the test instead of hanging it.
func writeOne(t *testing.T, db *DB, k string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := db.Write(ctx, func(ctx context.Context) error {
		return insert(ctx, db, k)
	}); err != nil {
		t.Fatalf("later Write: %v", err)
	}
}

// runGuarded runs f in a goroutine and returns its error, or fails the
// test if f does not return within limit.
func runGuarded(t *testing.T, limit time.Duration, f func() error) error {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- f() }()
	select {
	case err := <-errc:
		return err
	case <-time.After(limit):
		t.Fatalf("did not return within %s", limit)
		return nil
	}
}

func TestWriteBeginsImmediate(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	u := url.URL{Scheme: "file", Path: db.Path(), RawQuery: "_busy_timeout=0"}
	raw, err := sql.Open(driverName, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	raw.SetMaxOpenConns(1)

	err = db.Write(t.Context(), func(ctx context.Context) error {
		_, err := raw.ExecContext(ctx, "BEGIN IMMEDIATE")
		if err == nil {
			_, _ = raw.ExecContext(ctx, "ROLLBACK")
			return errors.New("second BEGIN IMMEDIATE succeeded while a Write was open")
		}
		var se *msqlite.Error
		if !errors.As(err, &se) || se.Code()&0xff != sqlite3.SQLITE_BUSY {
			return fmt.Errorf("second BEGIN IMMEDIATE: got %w, want SQLITE_BUSY", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestReadersDuringWrite(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	err := db.Write(t.Context(), func(ctx context.Context) error {
		if err := insert(ctx, db, "a"); err != nil {
			return err
		}

		// A failure bound only: the readers must not wait for the writer.
		rctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()

		var held sync.WaitGroup // barrier: all four reader conns held at once
		held.Add(readerConns)
		results := make(chan error, readerConns)
		for range readerConns {
			go func() {
				conn, err := db.Reader().Conn(rctx)
				held.Done()
				if err != nil {
					results <- fmt.Errorf("reader conn: %w", err)
					return
				}
				defer func() { _ = conn.Close() }()
				held.Wait()
				n, err := countWith(rctx, conn)
				if err == nil && n != 0 {
					err = fmt.Errorf("reader saw %d rows during the write, want 0", n)
				}
				results <- err
			}()
		}
		var errs []error
		for range readerConns {
			errs = append(errs, <-results)
		}
		return errors.Join(errs...)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := count(t, db); got != 1 {
		t.Errorf("count after commit = %d, want 1", got)
	}
}

func TestWriteCommits(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	if store.InWrite(t.Context()) {
		t.Error("InWrite outside Write = true")
	}
	err := db.Write(t.Context(), func(ctx context.Context) error {
		if !store.InWrite(ctx) {
			return errors.New("InWrite inside fn = false")
		}
		return insert(ctx, db, "a")
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := count(t, db); got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
}

func TestWriteRollsBackOnError(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	err := db.Write(t.Context(), func(ctx context.Context) error {
		if err := insert(ctx, db, "a"); err != nil {
			return err
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("Write = %v, want errBoom", err)
	}
	if got := count(t, db); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
	writeOne(t, db, "b")
}

func TestWriteRollsBackOnPanic(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	var insertErr error
	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Errorf("recovered %v, want the panic from fn", r)
			}
		}()
		_ = db.Write(t.Context(), func(ctx context.Context) error {
			insertErr = insert(ctx, db, "a")
			panic("boom")
		})
		t.Error("Write returned instead of panicking")
	}()
	if insertErr != nil {
		t.Fatal(insertErr)
	}
	if got := count(t, db); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
	writeOne(t, db, "b")
}

func TestWriteRollsBackOnGoexit(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	insertErr := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = db.Write(t.Context(), func(ctx context.Context) error {
			insertErr <- insert(ctx, db, "a")
			runtime.Goexit()
			return nil
		})
	}()
	<-exited
	if err := <-insertErr; err != nil {
		t.Fatal(err)
	}
	if got := count(t, db); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
	writeOne(t, db, "b")
}

func TestNestedWriteJoins(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	err := db.Write(t.Context(), func(ctx context.Context) error {
		if err := insert(ctx, db, "a"); err != nil {
			return err
		}
		outer, err := db.WriteTx(ctx)
		if err != nil {
			return err
		}
		if err := db.Write(ctx, func(ctx context.Context) error {
			inner, err := db.WriteTx(ctx)
			if err != nil {
				return err
			}
			if inner != outer {
				return errors.New("nested Write started another transaction")
			}
			return insert(ctx, db, "b")
		}); err != nil {
			return fmt.Errorf("nested Write: %w", err)
		}
		n, err := countWith(ctx, db.Querier(ctx))
		if err != nil {
			return err
		}
		if n != 2 {
			return fmt.Errorf("outer Querier sees %d rows, want 2", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := count(t, db); got != 2 {
		t.Errorf("count = %d, want 2", got)
	}
}

func TestNestedFailureMakesRollbackOnly(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	err := db.Write(t.Context(), func(ctx context.Context) error {
		if err := insert(ctx, db, "a"); err != nil {
			return err
		}
		nested := db.Write(ctx, func(ctx context.Context) error {
			if err := insert(ctx, db, "b"); err != nil {
				return err
			}
			return errBoom
		})
		if !errors.Is(nested, errBoom) {
			return fmt.Errorf("nested Write = %w, want errBoom", nested)
		}
		// The transaction stays usable until the outer fn returns.
		return insert(ctx, db, "c")
	})
	if !errors.Is(err, store.ErrRolledBack) {
		t.Fatalf("Write = %v, want ErrRolledBack", err)
	}
	if got := count(t, db); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
	writeOne(t, db, "d")
}

func TestWriteFanOut(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	const workers = 8
	err := db.Write(t.Context(), func(ctx context.Context) error {
		errs := make([]error, workers)
		var wg sync.WaitGroup
		for i := range workers {
			wg.Go(func() {
				if !store.InWrite(ctx) {
					errs[i] = errors.New("InWrite in a goroutine = false")
					return
				}
				errs[i] = insert(ctx, db, fmt.Sprintf("k%d", i))
			})
		}
		wg.Wait()
		return errors.Join(errs...)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := count(t, db); got != workers {
		t.Errorf("count = %d, want %d", got, workers)
	}
}

func TestWriteTxOutsideWrite(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	if _, err := db.WriteTx(t.Context()); !errors.Is(err, store.ErrNoWriteTx) {
		t.Errorf("WriteTx = %v, want ErrNoWriteTx", err)
	}
	if q := db.Querier(t.Context()); q != Querier(db.Reader()) {
		t.Error("Querier outside Write is not the reader pool")
	}
}

func TestWriteTxAfterEnd(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		result error
	}{
		{name: "committed", result: nil},
		{name: "rolled back", result: errBoom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := openKV(t)

			var captured context.Context
			err := db.Write(t.Context(), func(ctx context.Context) error {
				captured = ctx
				return tt.result
			})
			if !errors.Is(err, tt.result) {
				t.Fatalf("Write = %v, want %v", err, tt.result)
			}
			if _, err := db.WriteTx(captured); !errors.Is(err, store.ErrWriteTxDone) {
				t.Errorf("WriteTx = %v, want ErrWriteTxDone", err)
			}
			if store.InWrite(captured) {
				t.Error("InWrite after the Write ended = true")
			}
			if q := db.Querier(captured); q != Querier(db.Reader()) {
				t.Error("Querier after the Write ended is not the reader pool")
			}
		})
	}
}

func TestWriteTxOtherDB(t *testing.T) {
	t.Parallel()
	a := openKV(t)
	b := openKV(t)

	err := a.Write(t.Context(), func(ctx context.Context) error {
		if _, err := b.WriteTx(ctx); !errors.Is(err, store.ErrNoWriteTx) {
			return fmt.Errorf("other DB's WriteTx = %w, want ErrNoWriteTx", err)
		}
		if q := b.Querier(ctx); q != Querier(b.Reader()) {
			return errors.New("other DB's Querier is not its reader pool")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestQuerierSeesOwnWrites(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	err := db.Write(t.Context(), func(ctx context.Context) error {
		if err := insert(ctx, db, "a"); err != nil {
			return err
		}
		own, err := countWith(ctx, db.Querier(ctx))
		if err != nil {
			return err
		}
		committed, err := countWith(ctx, db.Reader())
		if err != nil {
			return err
		}
		if own != 1 || committed != 0 {
			return fmt.Errorf("Querier sees %d, Reader sees %d; want 1 and 0", own, committed)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWriteWrongContextFailsBusy(t *testing.T) {
	t.Parallel()
	db := openKV(t)
	db.writerWait = 100 * time.Millisecond

	err := db.Write(t.Context(), func(ctx context.Context) error {
		if err := insert(ctx, db, "a"); err != nil {
			return err
		}
		err := runGuarded(t, 5*time.Second, func() error {
			return db.Write(context.Background(), func(ctx context.Context) error {
				return insert(ctx, db, "b")
			})
		})
		if !errors.Is(err, store.ErrWriterBusy) {
			return fmt.Errorf("Write with a context lacking the tx = %w, want ErrWriterBusy", err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("Write = %w; the bound is not the caller's deadline", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := count(t, db); got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
}

func TestWriteCallerDeadlineWhileWaiting(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	err := db.Write(t.Context(), func(context.Context) error {
		err := runGuarded(t, 5*time.Second, func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			return db.Write(ctx, func(context.Context) error { return nil })
		})
		if !errors.Is(err, store.ErrWriterBusy) || !errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("Write = %w, want ErrWriterBusy and DeadlineExceeded", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWriteCancelledContext(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := db.Write(ctx, func(ctx context.Context) error {
		if err := insert(ctx, db, "a"); err != nil {
			return err
		}
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Write = %v, want context.Canceled", err)
	}
	if errors.Is(err, sql.ErrTxDone) {
		t.Errorf("Write = %v, must not carry sql.ErrTxDone", err)
	}
	if got := count(t, db); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
	writeOne(t, db, "b")
}

func TestWriteCancelledBeforeCommit(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := db.Write(ctx, func(ctx context.Context) error {
		if err := insert(ctx, db, "a"); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Write = %v, want context.Canceled", err)
	}
	if errors.Is(err, sql.ErrTxDone) {
		t.Errorf("Write = %v, must not carry sql.ErrTxDone", err)
	}
	if got := count(t, db); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
	writeOne(t, db, "b")
}

func TestWriteAlreadyCancelled(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	err := db.Write(ctx, func(context.Context) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Write = %v, want context.Canceled", err)
	}
	if errors.Is(err, store.ErrWriterBusy) {
		t.Errorf("Write = %v, must not match ErrWriterBusy", err)
	}
	if called {
		t.Error("fn ran with an already cancelled context")
	}
}

func TestSlowWriteLogged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		slowWrite time.Duration
		sleep     time.Duration
		want      bool
	}{
		{name: "slow", slowWrite: 10 * time.Millisecond, sleep: 20 * time.Millisecond, want: true},
		{name: "fast", slowWrite: time.Hour, sleep: 0, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := openKV(t)
			var buf bytes.Buffer
			db.logger = slog.New(slog.NewTextHandler(&buf, nil))
			db.slowWrite = tt.slowWrite

			err := db.Write(t.Context(), func(context.Context) error {
				time.Sleep(tt.sleep)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			got := strings.Contains(out, "level=WARN") &&
				strings.Contains(out, `msg="slow write transaction"`) &&
				strings.Contains(out, "duration_ms=")
			if got != tt.want {
				t.Errorf("slow write line logged = %v, want %v; log:\n%s", got, tt.want, out)
			}
		})
	}
}

func TestNestedPanicMakesRollbackOnly(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	err := db.Write(t.Context(), func(ctx context.Context) error {
		if err := insert(ctx, db, "a"); err != nil {
			return err
		}
		func() {
			defer func() { _ = recover() }()
			_ = db.Write(ctx, func(context.Context) error { panic("boom") })
		}()
		return nil
	})
	if !errors.Is(err, store.ErrRolledBack) {
		t.Fatalf("Write = %v, want ErrRolledBack", err)
	}
	if got := count(t, db); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
}

func TestWriteCancelledKeepsFnError(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := db.Write(ctx, func(ctx context.Context) error {
		if err := insert(ctx, db, "a"); err != nil {
			return err
		}
		cancel()
		return errBoom
	})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, errBoom) {
		t.Fatalf("Write = %v, want context.Canceled and errBoom", err)
	}
	if got := count(t, db); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
	writeOne(t, db, "b")
}

// longInsert runs for many seconds unless it is interrupted.
const longInsert = "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 50000000) " +
	"INSERT INTO kv(k, v) SELECT 'big' || x, 'v' FROM c"

// interruptWrite runs longInsert through q under a context derived from
// ctx that ends after 50 ms, so SQLite interrupts it and rolls back the
// whole transaction. It returns the statement's error.
func interruptWrite(ctx context.Context, q Querier) error {
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err := q.ExecContext(short, longInsert)
	return err
}

// readerCount returns the committed row count of kv without failing the
// test, for use while a Write runs.
func readerCount(t *testing.T, db *DB) int {
	t.Helper()
	n, err := countWith(t.Context(), db.Reader())
	if err != nil {
		t.Errorf("reader count: %v", err)
	}
	return n
}

func TestWriteInterruptedStatementCommitsNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		swallow bool // fn ignores every error and returns nil
	}{
		{name: "fn returns the error", swallow: false},
		{name: "fn swallows the error", swallow: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := openKV(t)

			err := db.Write(t.Context(), func(ctx context.Context) error {
				if err := insert(ctx, db, "before"); err != nil {
					return err
				}
				q, err := db.WriteTx(ctx)
				if err != nil {
					return err
				}
				if err := interruptWrite(ctx, q); err == nil {
					t.Error("long insert was not interrupted")
				}
				afterErr := insert(ctx, db, "after")
				if !errors.Is(afterErr, store.ErrWriteTxDone) {
					t.Errorf("insert after the loss = %v, want ErrWriteTxDone", afterErr)
				}
				if n := readerCount(t, db); n != 0 {
					t.Errorf("readers see %d rows while fn runs, want 0", n)
				}
				if tt.swallow {
					return nil
				}
				return afterErr
			})
			if !errors.Is(err, errTxLost) {
				t.Fatalf("Write = %v, want errTxLost", err)
			}
			if got := count(t, db); got != 0 {
				t.Errorf("count = %d, want 0", got)
			}
			writeOne(t, db, "later")
		})
	}
}

func TestNestedInterruptedStatementCommitsNothing(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	err := db.Write(t.Context(), func(ctx context.Context) error {
		if err := insert(ctx, db, "before"); err != nil {
			return err
		}
		nested := db.Write(ctx, func(ctx context.Context) error {
			q, err := db.WriteTx(ctx)
			if err != nil {
				return err
			}
			return interruptWrite(ctx, q)
		})
		if nested == nil {
			t.Error("nested Write with an interrupted statement succeeded")
		}
		if err := insert(ctx, db, "after"); !errors.Is(err, store.ErrWriteTxDone) {
			t.Errorf("insert after the loss = %v, want ErrWriteTxDone", err)
		}
		if n := readerCount(t, db); n != 0 {
			t.Errorf("readers see %d rows while fn runs, want 0", n)
		}
		return nil // swallows both errors
	})
	if !errors.Is(err, errTxLost) || !errors.Is(err, store.ErrRolledBack) {
		t.Fatalf("Write = %v, want errTxLost and ErrRolledBack", err)
	}
	if got := count(t, db); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
	writeOne(t, db, "later")
}

// TestHooksOnInterrupt bypasses the handle WriteTx returns and uses the
// *sql.Tx directly, to check the driver hooks themselves: the rollback
// hook fires on SQLite's automatic rollback, and the commit hook vetoes
// the autocommit statement database/sql would then run.
func TestHooksOnInterrupt(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	err := db.Write(t.Context(), func(ctx context.Context) error {
		s, ok := db.stateOf(ctx)
		if !ok {
			return errors.New("no state in fn's ctx")
		}
		if _, err := s.tx.ExecContext(ctx, "INSERT INTO kv(k, v) VALUES('before', 'v')"); err != nil {
			return err
		}
		short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		if _, err := s.tx.ExecContext(short, longInsert); err == nil {
			t.Error("long insert was not interrupted")
		}
		if !s.rolledBack.Load() {
			t.Error("rollback hook did not fire on the automatic rollback after SQLITE_INTERRUPT")
		}
		// database/sql still treats the Tx as open, so this runs in
		// autocommit mode; the commit hook must veto it.
		if _, err := s.tx.ExecContext(ctx, "INSERT INTO kv(k, v) VALUES('after', 'v')"); err == nil {
			t.Error("autocommit insert after the loss succeeded")
		}
		if n := readerCount(t, db); n != 0 {
			t.Errorf("readers see %d rows while fn runs, want 0", n)
		}
		return nil
	})
	if err == nil {
		t.Fatal("Write succeeded after SQLite rolled back its transaction")
	}
	if got := count(t, db); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
	writeOne(t, db, "later")
}

func TestStatementErrorKeepsTransaction(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	err := db.Write(t.Context(), func(ctx context.Context) error {
		if err := insert(ctx, db, "a"); err != nil {
			return err
		}
		if err := insert(ctx, db, "a"); err == nil {
			return errors.New("duplicate insert succeeded")
		}
		return insert(ctx, db, "b")
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := count(t, db); got != 2 {
		t.Errorf("count = %d, want 2", got)
	}
}

func TestTxControlThroughHandleFails(t *testing.T) {
	t.Parallel()
	for _, stmt := range []string{"ROLLBACK", "COMMIT"} {
		t.Run(stmt, func(t *testing.T) {
			t.Parallel()
			db := openKV(t)

			err := db.Write(t.Context(), func(ctx context.Context) error {
				if err := insert(ctx, db, "a"); err != nil {
					return err
				}
				q, err := db.WriteTx(ctx)
				if err != nil {
					return err
				}
				_, _ = q.ExecContext(ctx, stmt)
				if err := insert(ctx, db, "b"); !errors.Is(err, store.ErrWriteTxDone) {
					t.Errorf("insert after %s = %v, want ErrWriteTxDone", stmt, err)
				}
				if n := readerCount(t, db); n != 0 {
					t.Errorf("readers see %d rows while fn runs, want 0", n)
				}
				return nil
			})
			if err == nil {
				t.Fatalf("Write succeeded after fn ran %s", stmt)
			}
			if got := count(t, db); got != 0 {
				t.Errorf("count = %d, want 0", got)
			}
			writeOne(t, db, "later")
		})
	}
}

func TestRetainedHandleAfterEnd(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	var q Querier
	err := db.Write(t.Context(), func(ctx context.Context) error {
		var err error
		if q, err = db.WriteTx(ctx); err != nil {
			return err
		}
		return insert(ctx, db, "a")
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := q.(*sql.Tx); ok {
		t.Error("WriteTx handle is a *sql.Tx")
	}
	if _, err := q.ExecContext(t.Context(), "INSERT INTO kv(k, v) VALUES('b', 'v')"); !errors.Is(err, store.ErrWriteTxDone) {
		t.Errorf("ExecContext after the end = %v, want ErrWriteTxDone", err)
	}
	if rows, err := q.QueryContext(t.Context(), "SELECT k FROM kv"); !errors.Is(err, store.ErrWriteTxDone) {
		if rows != nil {
			_ = rows.Close()
		}
		t.Errorf("QueryContext after the end = %v, want ErrWriteTxDone", err)
	}
	var n int
	if err := q.QueryRowContext(t.Context(), "SELECT count(*) FROM kv").Scan(&n); !errors.Is(err, sql.ErrTxDone) {
		t.Errorf("QueryRowContext after the end = %v, want sql.ErrTxDone", err)
	}
	if got := count(t, db); got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
}

func TestWriteCommitFailure(t *testing.T) {
	t.Parallel()
	db := openKV(t)
	err := db.Write(t.Context(), func(ctx context.Context) error {
		q, err := db.WriteTx(ctx)
		if err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, "CREATE TABLE p(id INTEGER PRIMARY KEY) STRICT"); err != nil {
			return err
		}
		_, err = q.ExecContext(ctx,
			"CREATE TABLE c(pid INTEGER REFERENCES p(id) DEFERRABLE INITIALLY DEFERRED) STRICT")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	err = db.Write(t.Context(), func(ctx context.Context) error {
		q, err := db.WriteTx(ctx)
		if err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, "INSERT INTO c(pid) VALUES(99)"); err != nil {
			return err
		}
		return insert(ctx, db, "a")
	})
	if err == nil {
		t.Fatal("Write with an orphan row under a deferred foreign key succeeded")
	}
	if got := count(t, db); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
	writeOne(t, db, "later")
}

func TestWriteCancelledWhileWaiting(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	err := db.Write(t.Context(), func(context.Context) error {
		before := db.writer.Stats().WaitCount
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		errc := make(chan error, 1)
		go func() {
			errc <- db.Write(ctx, func(context.Context) error { return nil })
		}()
		limit := time.Now().Add(5 * time.Second)
		for db.writer.Stats().WaitCount == before {
			if time.Now().After(limit) {
				return errors.New("second Write never waited for the writer")
			}
			runtime.Gosched()
		}
		cancel()
		select {
		case err := <-errc:
			if !errors.Is(err, context.Canceled) || errors.Is(err, store.ErrWriterBusy) {
				return fmt.Errorf("Write cancelled while waiting = %w, want context.Canceled only", err)
			}
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("Write cancelled while waiting did not return")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWriteClosedDB(t *testing.T) {
	t.Parallel()
	db := openKV(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	err := db.Write(t.Context(), func(context.Context) error { return nil })
	if err == nil {
		t.Fatal("Write on a closed DB succeeded")
	}
	if errors.Is(err, store.ErrWriterBusy) {
		t.Errorf("Write on a closed DB = %v, must not match ErrWriterBusy", err)
	}
}

func TestNestedFailureInGoroutine(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	err := db.Write(t.Context(), func(ctx context.Context) error {
		if err := insert(ctx, db, "a"); err != nil {
			return err
		}
		var nested error
		var wg sync.WaitGroup
		wg.Go(func() {
			nested = db.Write(ctx, func(ctx context.Context) error {
				if err := insert(ctx, db, "b"); err != nil {
					return err
				}
				return errBoom
			})
		})
		wg.Wait()
		if !errors.Is(nested, errBoom) {
			return fmt.Errorf("nested Write = %w, want errBoom", nested)
		}
		return nil
	})
	if !errors.Is(err, store.ErrRolledBack) {
		t.Fatalf("Write = %v, want ErrRolledBack", err)
	}
	if got := count(t, db); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
}

func TestWriteCapturedCtxStartsNewTx(t *testing.T) {
	t.Parallel()
	db := openKV(t)

	var captured context.Context
	if err := db.Write(t.Context(), func(ctx context.Context) error {
		captured = ctx
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// fn's ctx is cancelled when Write returns; drop the cancellation to
	// reuse its values.
	err := db.Write(context.WithoutCancel(captured), func(ctx context.Context) error {
		return insert(ctx, db, "a")
	})
	if err != nil {
		t.Fatalf("Write with an ended transaction's ctx = %v", err)
	}
	if got := count(t, db); got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
}
