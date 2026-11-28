package sqlite

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/daniel-vuky/ops-copilot/internal/platform/jobs"
	"github.com/daniel-vuky/ops-copilot/internal/platform/store"
)

// jobNow is the "now" of the job store tests, on a whole millisecond so it
// survives the round trip through Unix ms.
var jobNow = time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

// openJobs opens a database with the embedded migrations in a temp dir and
// returns it with a job store on it.
func openJobs(t *testing.T) (*DB, *JobStore) {
	t.Helper()
	db, err := Open(t.Context(), Options{
		Dir:        t.TempDir(),
		Migrations: EmbeddedMigrations(),
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db, NewJobStore(db)
}

// write runs fn in a Write on db and fails the test on error.
func write(t *testing.T, db *DB, fn func(ctx context.Context) error) {
	t.Helper()
	if err := db.Write(t.Context(), fn); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

// newJob returns a NewJob of kind due at runAfter, created at jobNow.
func newJob(kind string, runAfter time.Time) jobs.NewJob {
	return jobs.NewJob{Kind: kind, Payload: []byte(`{"id":1}`), RunAfter: runAfter, Now: jobNow}
}

// mustInsert inserts j in its own Write and returns its id, failing the
// test unless exactly one row was inserted.
func mustInsert(t *testing.T, db *DB, s *JobStore, j jobs.NewJob) int64 {
	t.Helper()
	var id int64
	write(t, db, func(ctx context.Context) error {
		var ok bool
		var err error
		id, ok, err = s.Insert(ctx, j)
		if err == nil && !ok {
			t.Errorf("Insert(%s) inserted = false, want true", j.Kind)
		}
		return err
	})
	return id
}

// mark runs one Mark* call in its own Write and returns its bool.
func mark(t *testing.T, db *DB, f func(ctx context.Context) (bool, error)) bool {
	t.Helper()
	var ok bool
	write(t, db, func(ctx context.Context) error {
		var err error
		ok, err = f(ctx)
		return err
	})
	return ok
}

// mustGet reads job id through the reader pool and fails the test when it
// is missing.
func mustGet(t *testing.T, s *JobStore, id int64) jobs.Row {
	t.Helper()
	r, ok, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get(%d): %v", id, err)
	}
	if !ok {
		t.Fatalf("Get(%d): not found", id)
	}
	return r
}

// jobCount returns the number of rows in jobs.
func jobCount(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRowContext(t.Context(), "SELECT count(*) FROM jobs").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestJobsTableIsStrict(t *testing.T) {
	t.Parallel()
	db, _ := openJobs(t)
	ctx := t.Context()

	var strict int
	err := db.Reader().QueryRowContext(ctx,
		"SELECT strict FROM pragma_table_list WHERE name = 'jobs'").Scan(&strict)
	if err != nil {
		t.Fatal(err)
	}
	if strict != 1 {
		t.Errorf("jobs strict = %d, want 1", strict)
	}

	err = db.Write(ctx, func(ctx context.Context) error {
		q, err := db.WriteTx(ctx)
		if err != nil {
			return err
		}
		_, err = q.ExecContext(ctx, `INSERT INTO jobs
			(kind, payload, state, run_after_at, created_at, updated_at)
			VALUES ('k', x'', 'queued', '2026-11-28', 1, 1)`)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "cannot store TEXT") {
		t.Errorf("inserting text into run_after_at = %v, want a STRICT %q error", err, "cannot store TEXT")
	}
}

func TestInsertRejectsZeroTimes(t *testing.T) {
	t.Parallel()
	db, s := openJobs(t)

	noNow := newJob("k", jobNow)
	noNow.Now = time.Time{}
	noRunAfter := newJob("k", time.Time{})
	for name, j := range map[string]jobs.NewJob{"zero Now": noNow, "zero RunAfter": noRunAfter} {
		err := db.Write(t.Context(), func(ctx context.Context) error {
			_, _, err := s.Insert(ctx, j)
			return err
		})
		if err == nil {
			t.Errorf("Insert with %s succeeded, want an error", name)
		}
	}
	if n := jobCount(t, db); n != 0 {
		t.Errorf("jobs rows = %d, want 0", n)
	}
}

func TestEmptyLastErrorIsNull(t *testing.T) {
	t.Parallel()
	db, s := openJobs(t)

	retried := mustInsert(t, db, s, newJob("k", jobNow))
	requeued := mustInsert(t, db, s, newJob("k", jobNow))
	for _, id := range []int64{retried, requeued} {
		mark(t, db, func(ctx context.Context) (bool, error) { return s.MarkRunning(ctx, id, jobNow) })
	}
	mark(t, db, func(ctx context.Context) (bool, error) { return s.MarkRetry(ctx, retried, jobNow, "", jobNow) })
	write(t, db, func(ctx context.Context) error {
		_, err := s.RequeueRunning(ctx, nil, "", jobNow)
		return err
	})
	for _, id := range []int64{retried, requeued} {
		var typ string
		err := db.Reader().QueryRowContext(t.Context(),
			"SELECT typeof(last_error) FROM jobs WHERE id = ?", id).Scan(&typ)
		if err != nil {
			t.Fatal(err)
		}
		if typ != "null" {
			t.Errorf("job %d last_error stored as %q, want null", id, typ)
		}
	}
}

func TestJobsPayloadLimit(t *testing.T) {
	t.Parallel()
	db, s := openJobs(t)

	j := newJob("k", jobNow)
	j.Payload = make([]byte, 65536)
	mustInsert(t, db, s, j)

	j.Payload = make([]byte, 65537)
	err := db.Write(t.Context(), func(ctx context.Context) error {
		_, _, err := s.Insert(ctx, j)
		return err
	})
	if err == nil {
		t.Error("Insert with a 65537-byte payload succeeded, want a CHECK error")
	}
	if n := jobCount(t, db); n != 1 {
		t.Errorf("jobs rows = %d, want 1", n)
	}
}

func TestInsertNilPayload(t *testing.T) {
	t.Parallel()
	db, s := openJobs(t)

	for _, p := range [][]byte{nil, {}} {
		j := newJob("k", jobNow)
		j.Payload = p
		id := mustInsert(t, db, s, j)

		var typ string
		var n int
		err := db.Reader().QueryRowContext(t.Context(),
			"SELECT typeof(payload), length(payload) FROM jobs WHERE id = ?", id).Scan(&typ, &n)
		if err != nil {
			t.Fatal(err)
		}
		if typ != "blob" || n != 0 {
			t.Errorf("payload %#v stored as typeof %q length %d, want blob length 0", p, typ, n)
		}
		if got := mustGet(t, s, id).Payload; !bytes.Equal(got, nil) {
			t.Errorf("payload read back = %#v, want empty", got)
		}
	}
}

func TestInsertOutsideWrite(t *testing.T) {
	t.Parallel()
	db, s := openJobs(t)
	ctx := t.Context()

	if _, _, err := s.Insert(ctx, newJob("k", jobNow)); !errors.Is(err, store.ErrNoWriteTx) {
		t.Errorf("Insert outside a Write: %v, want ErrNoWriteTx", err)
	}
	id := mustInsert(t, db, s, newJob("k", jobNow))

	calls := map[string]func() error{
		"CountQueued": func() error { _, err := s.CountQueued(ctx, "k"); return err },
		"NextDue":     func() error { _, _, err := s.NextDue(ctx, "k", jobNow, nil); return err },
		"MarkRunning": func() error { _, err := s.MarkRunning(ctx, id, jobNow); return err },
		"MarkSucceeded": func() error {
			_, err := s.MarkSucceeded(ctx, id, jobNow)
			return err
		},
		"MarkRetry": func() error { _, err := s.MarkRetry(ctx, id, jobNow, "e", jobNow); return err },
		"MarkFinal": func() error {
			_, err := s.MarkFinal(ctx, id, jobs.StateQueued, jobs.StateCancelled, nil, jobNow)
			return err
		},
		"RequeueRunning": func() error { _, err := s.RequeueRunning(ctx, nil, "e", jobNow); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, store.ErrNoWriteTx) {
			t.Errorf("%s outside a Write: %v, want ErrNoWriteTx", name, err)
		}
	}
	if r := mustGet(t, s, id); r.State != jobs.StateQueued {
		t.Errorf("state = %q after calls outside a Write, want queued", r.State)
	}
}

func TestInsertPeriodOnce(t *testing.T) {
	t.Parallel()
	db, s := openJobs(t)

	period := jobNow.Add(-time.Hour)
	j := newJob("scan", jobNow)
	j.PeriodAt = &period
	id := mustInsert(t, db, s, j)

	write(t, db, func(ctx context.Context) error {
		got, ok, err := s.Insert(ctx, j)
		if ok || got != 0 {
			t.Errorf("second Insert of the period = (%d, %v), want (0, false)", got, ok)
		}
		return err
	})
	if n := jobCount(t, db); n != 1 {
		t.Errorf("jobs rows = %d, want 1", n)
	}
	r := mustGet(t, s, id)
	if r.PeriodAt == nil || !r.PeriodAt.Equal(period) {
		t.Errorf("PeriodAt = %v, want %v", r.PeriodAt, period)
	}

	// Another kind with the same period is a different job.
	other := j
	other.Kind = "other"
	mustInsert(t, db, s, other)
}

func TestInsertUniqueKeyWhileActive(t *testing.T) {
	t.Parallel()
	db, s := openJobs(t)

	j := newJob("extract", jobNow)
	j.UniqueKey = "doc-1"
	id := mustInsert(t, db, s, j)

	tryInsert := func() bool {
		var ok bool
		write(t, db, func(ctx context.Context) error {
			var err error
			_, ok, err = s.Insert(ctx, j)
			return err
		})
		return ok
	}
	if tryInsert() {
		t.Fatal("Insert while the first is queued inserted, want not")
	}
	if !mark(t, db, func(ctx context.Context) (bool, error) { return s.MarkRunning(ctx, id, jobNow) }) {
		t.Fatal("MarkRunning = false, want true")
	}
	if tryInsert() {
		t.Fatal("Insert while the first is running inserted, want not")
	}
	if !mark(t, db, func(ctx context.Context) (bool, error) { return s.MarkSucceeded(ctx, id, jobNow) }) {
		t.Fatal("MarkSucceeded = false, want true")
	}
	if !tryInsert() {
		t.Fatal("Insert after the first succeeded did not insert, want inserted")
	}
	if got := mustGet(t, s, id).UniqueKey; got != "doc-1" {
		t.Errorf("UniqueKey = %q, want doc-1", got)
	}
}

func TestInsertEmptyUniqueKeyIsNull(t *testing.T) {
	t.Parallel()
	db, s := openJobs(t)

	id := mustInsert(t, db, s, newJob("k", jobNow))
	mustInsert(t, db, s, newJob("k", jobNow)) // two NULL keys never conflict

	var period, key, lastErr, started, finished string
	err := db.Reader().QueryRowContext(t.Context(), `SELECT typeof(period_at), typeof(unique_key),
		typeof(last_error), typeof(started_at), typeof(finished_at) FROM jobs WHERE id = ?`, id).
		Scan(&period, &key, &lastErr, &started, &finished)
	if err != nil {
		t.Fatal(err)
	}
	for name, typ := range map[string]string{
		"period_at": period, "unique_key": key, "last_error": lastErr,
		"started_at": started, "finished_at": finished,
	} {
		if typ != "null" {
			t.Errorf("typeof(%s) = %q, want null", name, typ)
		}
	}
}

func TestNextDueOrder(t *testing.T) {
	t.Parallel()
	db, s := openJobs(t)

	at := func(m int) time.Time { return jobNow.Add(time.Duration(m-10) * time.Minute) }
	id3 := mustInsert(t, db, s, newJob("k", at(3)))
	id1 := mustInsert(t, db, s, newJob("k", at(1)))
	id2 := mustInsert(t, db, s, newJob("k", at(2)))
	id2b := mustInsert(t, db, s, newJob("k", at(2))) // ties with id2, later id
	mustInsert(t, db, s, newJob("k", jobNow.Add(time.Minute)))
	mustInsert(t, db, s, newJob("other", at(0)))

	next := func(skip []int64) (int64, bool) {
		var r jobs.Row
		var ok bool
		write(t, db, func(ctx context.Context) error {
			var err error
			r, ok, err = s.NextDue(ctx, "k", jobNow, skip)
			return err
		})
		return r.ID, ok
	}

	steps := []struct {
		skip []int64
		want int64
	}{
		{nil, id1},
		{[]int64{id1}, id2},
		{[]int64{id1, id2}, id2b},
		{[]int64{id1, id2, id2b}, id3},
	}
	for _, st := range steps {
		got, ok := next(st.skip)
		if !ok || got != st.want {
			t.Errorf("NextDue(skip %v) = (%d, %v), want (%d, true)", st.skip, got, ok, st.want)
		}
	}
	if got, ok := next([]int64{id1, id2, id2b, id3}); ok {
		t.Errorf("NextDue with every due row skipped = %d, want none (the future row is not due)", got)
	}

	var r jobs.Row
	write(t, db, func(ctx context.Context) error {
		var err error
		r, _, err = s.NextDue(ctx, "k", jobNow, nil)
		return err
	})
	if r.Kind != "k" || r.State != jobs.StateQueued || !r.RunAfter.Equal(at(1)) ||
		!bytes.Equal(r.Payload, []byte(`{"id":1}`)) || !r.CreatedAt.Equal(jobNow) ||
		!r.UpdatedAt.Equal(jobNow) || r.RunAfter.Location() != time.UTC {
		t.Errorf("NextDue row = %+v, want kind k queued at %v created %v (UTC)", r, at(1), jobNow)
	}

	// A running row is not due.
	if !mark(t, db, func(ctx context.Context) (bool, error) { return s.MarkRunning(ctx, id1, jobNow) }) {
		t.Fatal("MarkRunning = false")
	}
	if got, _ := next(nil); got != id2 {
		t.Errorf("NextDue after claiming %d = %d, want %d", id1, got, id2)
	}
}

func TestMarkTransitionsGuarded(t *testing.T) {
	t.Parallel()
	db, s := openJobs(t)
	t1 := jobNow.Add(time.Second)
	t2 := jobNow.Add(2 * time.Second)
	t3 := jobNow.Add(3 * time.Second)
	t4 := jobNow.Add(4 * time.Second)

	id := mustInsert(t, db, s, newJob("k", jobNow))

	if mark(t, db, func(ctx context.Context) (bool, error) { return s.MarkSucceeded(ctx, id, t1) }) {
		t.Error("MarkSucceeded on a queued row = true, want false")
	}
	if mark(t, db, func(ctx context.Context) (bool, error) { return s.MarkRetry(ctx, id, t1, "e", t1) }) {
		t.Error("MarkRetry on a queued row = true, want false")
	}

	// Claim: queued -> running.
	if !mark(t, db, func(ctx context.Context) (bool, error) { return s.MarkRunning(ctx, id, t1) }) {
		t.Fatal("MarkRunning on a queued row = false, want true")
	}
	r := mustGet(t, s, id)
	if r.State != jobs.StateRunning || r.Attempts != 1 || r.StartedAt == nil || !r.StartedAt.Equal(t1) ||
		!r.UpdatedAt.Equal(t1) || r.FinishedAt != nil {
		t.Errorf("after MarkRunning: %+v, want running, attempts 1, started and updated %v", r, t1)
	}
	if mark(t, db, func(ctx context.Context) (bool, error) { return s.MarkRunning(ctx, id, t1) }) {
		t.Error("MarkRunning on a running row = true, want false")
	}
	if got := mustGet(t, s, id).Attempts; got != 1 {
		t.Errorf("attempts after a refused MarkRunning = %d, want 1", got)
	}

	// Retry: running -> queued with the error and a new run_after.
	if !mark(t, db, func(ctx context.Context) (bool, error) { return s.MarkRetry(ctx, id, t3, "boom", t2) }) {
		t.Fatal("MarkRetry on a running row = false, want true")
	}
	r = mustGet(t, s, id)
	if r.State != jobs.StateQueued || r.LastError != "boom" || !r.RunAfter.Equal(t3) ||
		!r.UpdatedAt.Equal(t2) || r.Attempts != 1 || r.FinishedAt != nil {
		t.Errorf("after MarkRetry: %+v, want queued, last_error boom, run_after %v, updated %v", r, t3, t2)
	}

	// Second attempt, then success clears last_error.
	if !mark(t, db, func(ctx context.Context) (bool, error) { return s.MarkRunning(ctx, id, t3) }) {
		t.Fatal("second MarkRunning = false, want true")
	}
	r = mustGet(t, s, id)
	if r.Attempts != 2 || !r.StartedAt.Equal(t3) || r.LastError != "boom" {
		t.Errorf("after the second MarkRunning: %+v, want attempts 2, started %v, last_error kept", r, t3)
	}
	if !mark(t, db, func(ctx context.Context) (bool, error) { return s.MarkSucceeded(ctx, id, t4) }) {
		t.Fatal("MarkSucceeded on a running row = false, want true")
	}
	r = mustGet(t, s, id)
	if r.State != jobs.StateSucceeded || r.LastError != "" || r.FinishedAt == nil ||
		!r.FinishedAt.Equal(t4) || !r.UpdatedAt.Equal(t4) {
		t.Errorf("after MarkSucceeded: %+v, want succeeded, no last_error, finished %v", r, t4)
	}
	if mark(t, db, func(ctx context.Context) (bool, error) {
		return s.MarkFinal(ctx, id, jobs.StateRunning, jobs.StateFailed, nil, t4)
	}) {
		t.Error("MarkFinal from running on a succeeded row = true, want false")
	}
}

func TestMarkFinal(t *testing.T) {
	t.Parallel()
	db, s := openJobs(t)
	t1 := jobNow.Add(time.Second)
	t2 := jobNow.Add(2 * time.Second)

	// Running -> failed with nil keeps the stored error.
	id := mustInsert(t, db, s, newJob("k", jobNow))
	mark(t, db, func(ctx context.Context) (bool, error) { return s.MarkRunning(ctx, id, jobNow) })
	mark(t, db, func(ctx context.Context) (bool, error) { return s.MarkRetry(ctx, id, jobNow, "old", jobNow) })
	mark(t, db, func(ctx context.Context) (bool, error) { return s.MarkRunning(ctx, id, t1) })
	if mark(t, db, func(ctx context.Context) (bool, error) {
		return s.MarkFinal(ctx, id, jobs.StateQueued, jobs.StateFailed, nil, t2)
	}) {
		t.Error("MarkFinal from queued on a running row = true, want false")
	}
	if !mark(t, db, func(ctx context.Context) (bool, error) {
		return s.MarkFinal(ctx, id, jobs.StateRunning, jobs.StateFailed, nil, t2)
	}) {
		t.Fatal("MarkFinal running -> failed = false, want true")
	}
	r := mustGet(t, s, id)
	if r.State != jobs.StateFailed || r.LastError != "old" || r.FinishedAt == nil ||
		!r.FinishedAt.Equal(t2) || !r.UpdatedAt.Equal(t2) {
		t.Errorf("after MarkFinal(nil): %+v, want failed, last_error old, finished %v", r, t2)
	}

	// Queued -> cancelled with an error sets it.
	id2 := mustInsert(t, db, s, newJob("k", jobNow))
	msg := "cancelled by admin"
	if !mark(t, db, func(ctx context.Context) (bool, error) {
		return s.MarkFinal(ctx, id2, jobs.StateQueued, jobs.StateCancelled, &msg, t1)
	}) {
		t.Fatal("MarkFinal queued -> cancelled = false, want true")
	}
	r = mustGet(t, s, id2)
	if r.State != jobs.StateCancelled || r.LastError != msg || r.FinishedAt == nil || !r.FinishedAt.Equal(t1) {
		t.Errorf("after MarkFinal(&msg): %+v, want cancelled, last_error %q, finished %v", r, msg, t1)
	}

	// Only failed or cancelled are final targets, and only from queued or
	// running.
	id3 := mustInsert(t, db, s, newJob("k", jobNow))
	bad := []struct{ from, to jobs.State }{
		{jobs.StateQueued, jobs.StateSucceeded},
		{jobs.StateQueued, jobs.StateRunning},
		{jobs.StateQueued, jobs.StateQueued},
		{jobs.StateFailed, jobs.StateCancelled},
		{jobs.State("bogus"), jobs.StateFailed},
	}
	for _, b := range bad {
		err := db.Write(t.Context(), func(ctx context.Context) error {
			_, err := s.MarkFinal(ctx, id3, b.from, b.to, nil, t1)
			return err
		})
		if err == nil {
			t.Errorf("MarkFinal(%s -> %s) succeeded, want an error", b.from, b.to)
		}
	}
	if got := mustGet(t, s, id3).State; got != jobs.StateQueued {
		t.Errorf("state after refused MarkFinal calls = %q, want queued", got)
	}
}

func TestRequeueRunningExcept(t *testing.T) {
	t.Parallel()
	db, s := openJobs(t)
	t1 := jobNow.Add(time.Second)
	t2 := jobNow.Add(time.Minute)

	keep := mustInsert(t, db, s, newJob("k", jobNow))
	back := mustInsert(t, db, s, newJob("k", jobNow))
	queued := mustInsert(t, db, s, newJob("k", jobNow))
	for _, id := range []int64{keep, back} {
		if !mark(t, db, func(ctx context.Context) (bool, error) { return s.MarkRunning(ctx, id, t1) }) {
			t.Fatalf("MarkRunning(%d) = false", id)
		}
	}

	var n int64
	write(t, db, func(ctx context.Context) error {
		var err error
		n, err = s.RequeueRunning(ctx, []int64{keep}, "interrupted", t2)
		return err
	})
	if n != 1 {
		t.Errorf("RequeueRunning = %d, want 1", n)
	}
	if r := mustGet(t, s, keep); r.State != jobs.StateRunning || r.LastError != "" {
		t.Errorf("excepted row: %+v, want still running, no last_error", r)
	}
	r := mustGet(t, s, back)
	if r.State != jobs.StateQueued || r.Attempts != 1 || r.LastError != "interrupted" ||
		!r.RunAfter.Equal(t2) || !r.UpdatedAt.Equal(t2) {
		t.Errorf("requeued row: %+v, want queued, attempts 1, last_error interrupted, run_after %v", r, t2)
	}
	if r := mustGet(t, s, queued); !r.UpdatedAt.Equal(jobNow) || r.LastError != "" {
		t.Errorf("queued row changed by RequeueRunning: %+v", r)
	}

	// With no exceptions every running row goes back.
	write(t, db, func(ctx context.Context) error {
		var err error
		n, err = s.RequeueRunning(ctx, nil, "restart", t2)
		return err
	})
	if n != 1 {
		t.Errorf("RequeueRunning(nil) = %d, want 1", n)
	}
	if got := mustGet(t, s, keep).State; got != jobs.StateQueued {
		t.Errorf("state after RequeueRunning(nil) = %q, want queued", got)
	}
}

func TestTimesStoredAsUnixMilli(t *testing.T) {
	t.Parallel()
	db, s := openJobs(t)

	period := jobNow.Add(-time.Hour)
	j := newJob("k", jobNow.Add(time.Minute))
	j.PeriodAt = &period
	id := mustInsert(t, db, s, j)

	cols := map[string]int64{
		"created_at":   jobNow.UnixMilli(),
		"updated_at":   jobNow.UnixMilli(),
		"run_after_at": jobNow.Add(time.Minute).UnixMilli(),
		"period_at":    period.UnixMilli(),
	}
	for col, want := range cols {
		var typ string
		var v int64
		err := db.Reader().QueryRowContext(t.Context(),
			"SELECT typeof("+col+"), "+col+" FROM jobs WHERE id = ?", id).Scan(&typ, &v)
		if err != nil {
			t.Fatal(err)
		}
		if typ != "integer" || v != want {
			t.Errorf("%s = %s %d, want integer %d", col, typ, v, want)
		}
	}
}

func TestReaderQueries(t *testing.T) {
	t.Parallel()
	db, s := openJobs(t)
	ctx := t.Context()

	b1 := mustInsert(t, db, s, newJob("b", jobNow.Add(-time.Minute)))
	mustInsert(t, db, s, newJob("b", jobNow))
	a1 := mustInsert(t, db, s, newJob("a", jobNow.Add(-time.Second)))
	mustInsert(t, db, s, newJob("future", jobNow.Add(time.Second)))
	c1 := mustInsert(t, db, s, newJob("c", jobNow))
	c2 := mustInsert(t, db, s, newJob("c", jobNow))
	for _, id := range []int64{c1, c2} {
		mark(t, db, func(ctx context.Context) (bool, error) { return s.MarkRunning(ctx, id, jobNow) })
	}
	mark(t, db, func(ctx context.Context) (bool, error) { return s.MarkSucceeded(ctx, c1, jobNow) })

	kinds, err := s.DueKinds(ctx, jobNow, nil)
	if err != nil {
		t.Fatalf("DueKinds: %v", err)
	}
	if want := []string{"a", "b"}; !slices.Equal(kinds, want) {
		t.Errorf("DueKinds = %v, want %v", kinds, want)
	}

	// A kind whose only due rows are skipped is not due; one with another
	// due row still is.
	kinds, err = s.DueKinds(ctx, jobNow, []int64{b1, a1})
	if err != nil {
		t.Fatalf("DueKinds(skip): %v", err)
	}
	if want := []string{"b"}; !slices.Equal(kinds, want) {
		t.Errorf("DueKinds(skip a's job and b's first) = %v, want %v", kinds, want)
	}

	ids, err := s.RunningIDs(ctx)
	if err != nil {
		t.Fatalf("RunningIDs: %v", err)
	}
	if want := []int64{c2}; !slices.Equal(ids, want) {
		t.Errorf("RunningIDs = %v, want %v", ids, want)
	}

	counts, err := s.QueuedByKind(ctx)
	if err != nil {
		t.Fatalf("QueuedByKind: %v", err)
	}
	want := map[string]int{"a": 1, "b": 2, "future": 1}
	if len(counts) != len(want) {
		t.Errorf("QueuedByKind = %v, want %v", counts, want)
	}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("QueuedByKind[%s] = %d, want %d (all %v)", k, counts[k], v, counts)
		}
	}

	write(t, db, func(ctx context.Context) error {
		for kind, want := range map[string]int{"b": 2, "c": 0, "future": 1, "none": 0} {
			n, err := s.CountQueued(ctx, kind)
			if err != nil {
				return err
			}
			if n != want {
				t.Errorf("CountQueued(%s) = %d, want %d", kind, n, want)
			}
		}
		return nil
	})

	if _, ok, err := s.Get(ctx, b1+1000); err != nil || ok {
		t.Errorf("Get(missing) = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestGetSeesOwnWrite(t *testing.T) {
	t.Parallel()
	db, s := openJobs(t)

	write(t, db, func(ctx context.Context) error {
		id, _, err := s.Insert(ctx, newJob("k", jobNow))
		if err != nil {
			return err
		}
		r, ok, err := s.Get(ctx, id)
		if err != nil {
			return err
		}
		if !ok || r.ID != id {
			t.Errorf("Get(%d) inside the Write = (%+v, %v), want the uncommitted row", id, r, ok)
		}
		return nil
	})
}

func TestIDsNotReused(t *testing.T) {
	t.Parallel()
	db, s := openJobs(t)

	id := mustInsert(t, db, s, newJob("k", jobNow))
	write(t, db, func(ctx context.Context) error {
		q, err := db.WriteTx(ctx)
		if err != nil {
			return err
		}
		_, err = q.ExecContext(ctx, "DELETE FROM jobs")
		return err
	})
	if next := mustInsert(t, db, s, newJob("k", jobNow)); next <= id {
		t.Errorf("id after delete = %d, want > %d (AUTOINCREMENT)", next, id)
	}
}

func TestKindLengthChecked(t *testing.T) {
	t.Parallel()
	db, s := openJobs(t)

	for _, kind := range []string{"", strings.Repeat("k", 65)} {
		err := db.Write(t.Context(), func(ctx context.Context) error {
			_, _, err := s.Insert(ctx, newJob(kind, jobNow))
			return err
		})
		if err == nil {
			t.Errorf("Insert with kind of length %d succeeded, want a CHECK error", len(kind))
		}
	}
}
