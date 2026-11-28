package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/daniel-vuky/ops-copilot/internal/platform/jobs"
)

var _ jobs.Store = (*JobStore)(nil)

// jobColumns is the column list every job read selects, in scanRow order.
const jobColumns = `id, kind, payload, state, attempts, run_after_at, period_at, unique_key,
	last_error, created_at, updated_at, started_at, finished_at`

// JobStore implements jobs.Store on the jobs table (migration 0001). Writes
// and the claim reads use the write transaction of the caller's Write;
// DueKinds, RunningIDs and QueuedByKind use the reader pool.
type JobStore struct {
	db *DB
}

// NewJobStore returns a job store on db.
func NewJobStore(db *DB) *JobStore {
	return &JobStore{db: db}
}

// tx returns the write transaction ctx carries, wrapping the error with op.
func (s *JobStore) tx(ctx context.Context, op string) (Querier, error) {
	q, err := s.db.WriteTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("jobs store: %s: %w", op, err)
	}
	return q, nil
}

// Insert implements jobs.Store.
func (s *JobStore) Insert(ctx context.Context, j jobs.NewJob) (int64, bool, error) {
	q, err := s.tx(ctx, "insert")
	if err != nil {
		return 0, false, err
	}
	payload := j.Payload
	if payload == nil {
		payload = []byte{}
	}
	res, err := q.ExecContext(ctx, `INSERT INTO jobs
		(kind, payload, state, attempts, run_after_at, period_at, unique_key, created_at, updated_at)
		VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`,
		j.Kind, payload, string(jobs.StateQueued), j.RunAfter, nullTime(j.PeriodAt),
		nullString(j.UniqueKey), j.Now, j.Now)
	if err != nil {
		return 0, false, fmt.Errorf("jobs store: insert %s: %w", j.Kind, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("jobs store: insert %s: %w", j.Kind, err)
	}
	if n != 1 {
		return 0, false, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("jobs store: insert %s: %w", j.Kind, err)
	}
	return id, true, nil
}

// CountQueued implements jobs.Store.
func (s *JobStore) CountQueued(ctx context.Context, kind string) (int, error) {
	q, err := s.tx(ctx, "count queued")
	if err != nil {
		return 0, err
	}
	var n int
	err = q.QueryRowContext(ctx,
		"SELECT count(*) FROM jobs WHERE state = ? AND kind = ?", string(jobs.StateQueued), kind).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("jobs store: count queued %s: %w", kind, err)
	}
	return n, nil
}

// NextDue implements jobs.Store.
func (s *JobStore) NextDue(ctx context.Context, kind string, now time.Time, skip []int64) (jobs.Row, bool, error) {
	q, err := s.tx(ctx, "next due")
	if err != nil {
		return jobs.Row{}, false, err
	}
	notIn, idArgs := notInIDs(skip)
	args := append([]any{string(jobs.StateQueued), kind, now}, idArgs...)
	row := q.QueryRowContext(ctx, "SELECT "+jobColumns+` FROM jobs
		WHERE state = ? AND kind = ? AND run_after_at <= ?`+notIn+`
		ORDER BY run_after_at, id LIMIT 1`, args...)
	r, err := scanRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Row{}, false, nil
	}
	if err != nil {
		return jobs.Row{}, false, fmt.Errorf("jobs store: next due %s: %w", kind, err)
	}
	return r, true, nil
}

// MarkRunning implements jobs.Store.
func (s *JobStore) MarkRunning(ctx context.Context, id int64, now time.Time) (bool, error) {
	return s.update(ctx, "mark running", id, `UPDATE jobs
		SET state = ?, attempts = attempts + 1, started_at = ?, updated_at = ?
		WHERE id = ? AND state = ?`,
		string(jobs.StateRunning), now, now, id, string(jobs.StateQueued))
}

// MarkSucceeded implements jobs.Store.
func (s *JobStore) MarkSucceeded(ctx context.Context, id int64, now time.Time) (bool, error) {
	return s.update(ctx, "mark succeeded", id, `UPDATE jobs
		SET state = ?, last_error = NULL, finished_at = ?, updated_at = ?
		WHERE id = ? AND state = ?`,
		string(jobs.StateSucceeded), now, now, id, string(jobs.StateRunning))
}

// MarkRetry implements jobs.Store.
func (s *JobStore) MarkRetry(ctx context.Context, id int64, runAfter time.Time, lastErr string, now time.Time) (bool, error) {
	return s.update(ctx, "mark retry", id, `UPDATE jobs
		SET state = ?, run_after_at = ?, last_error = ?, updated_at = ?
		WHERE id = ? AND state = ?`,
		string(jobs.StateQueued), runAfter, lastErr, now, id, string(jobs.StateRunning))
}

// MarkFinal implements jobs.Store.
func (s *JobStore) MarkFinal(ctx context.Context, id int64, from, to jobs.State, lastErr *string, now time.Time) (bool, error) {
	if from != jobs.StateQueued && from != jobs.StateRunning {
		return false, fmt.Errorf("jobs store: mark final job %d: from state %q is not queued or running", id, from)
	}
	if to != jobs.StateFailed && to != jobs.StateCancelled {
		return false, fmt.Errorf("jobs store: mark final job %d: state %q is not failed or cancelled", id, to)
	}
	if lastErr == nil {
		return s.update(ctx, "mark final", id, `UPDATE jobs
			SET state = ?, finished_at = ?, updated_at = ?
			WHERE id = ? AND state = ?`,
			string(to), now, now, id, string(from))
	}
	return s.update(ctx, "mark final", id, `UPDATE jobs
		SET state = ?, last_error = ?, finished_at = ?, updated_at = ?
		WHERE id = ? AND state = ?`,
		string(to), *lastErr, now, now, id, string(from))
}

// RequeueRunning implements jobs.Store.
func (s *JobStore) RequeueRunning(ctx context.Context, except []int64, lastErr string, now time.Time) (int64, error) {
	q, err := s.tx(ctx, "requeue running")
	if err != nil {
		return 0, err
	}
	notIn, idArgs := notInIDs(except)
	args := append([]any{string(jobs.StateQueued), now, lastErr, now, string(jobs.StateRunning)}, idArgs...)
	res, err := q.ExecContext(ctx, `UPDATE jobs
		SET state = ?, run_after_at = ?, last_error = ?, updated_at = ?
		WHERE state = ?`+notIn, args...)
	if err != nil {
		return 0, fmt.Errorf("jobs store: requeue running: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("jobs store: requeue running: %w", err)
	}
	return n, nil
}

// update runs a guarded one-row UPDATE in the write transaction and
// reports whether exactly one row changed.
func (s *JobStore) update(ctx context.Context, op string, id int64, query string, args ...any) (bool, error) {
	q, err := s.tx(ctx, op)
	if err != nil {
		return false, err
	}
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("jobs store: %s job %d: %w", op, id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("jobs store: %s job %d: %w", op, id, err)
	}
	return n == 1, nil
}

// Get implements jobs.Store.
func (s *JobStore) Get(ctx context.Context, id int64) (jobs.Row, bool, error) {
	row := s.db.Querier(ctx).QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id = ?", id)
	r, err := scanRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Row{}, false, nil
	}
	if err != nil {
		return jobs.Row{}, false, fmt.Errorf("jobs store: get job %d: %w", id, err)
	}
	return r, true, nil
}

// DueKinds implements jobs.Store.
func (s *JobStore) DueKinds(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT DISTINCT kind FROM jobs
		WHERE state = ? AND run_after_at <= ? ORDER BY kind`, string(jobs.StateQueued), now)
	if err != nil {
		return nil, fmt.Errorf("jobs store: due kinds: %w", err)
	}
	kinds, err := collect(rows, func(rows *sql.Rows) (string, error) {
		var k string
		err := rows.Scan(&k)
		return k, err
	})
	if err != nil {
		return nil, fmt.Errorf("jobs store: due kinds: %w", err)
	}
	return kinds, nil
}

// RunningIDs implements jobs.Store.
func (s *JobStore) RunningIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.Reader().QueryContext(ctx,
		"SELECT id FROM jobs WHERE state = ? ORDER BY id", string(jobs.StateRunning))
	if err != nil {
		return nil, fmt.Errorf("jobs store: running ids: %w", err)
	}
	ids, err := collect(rows, func(rows *sql.Rows) (int64, error) {
		var id int64
		err := rows.Scan(&id)
		return id, err
	})
	if err != nil {
		return nil, fmt.Errorf("jobs store: running ids: %w", err)
	}
	return ids, nil
}

// QueuedByKind implements jobs.Store.
func (s *JobStore) QueuedByKind(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.Reader().QueryContext(ctx,
		"SELECT kind, count(*) FROM jobs WHERE state = ? GROUP BY kind", string(jobs.StateQueued))
	if err != nil {
		return nil, fmt.Errorf("jobs store: queued by kind: %w", err)
	}
	type kindCount struct {
		kind string
		n    int
	}
	list, err := collect(rows, func(rows *sql.Rows) (kindCount, error) {
		var kc kindCount
		err := rows.Scan(&kc.kind, &kc.n)
		return kc, err
	})
	if err != nil {
		return nil, fmt.Errorf("jobs store: queued by kind: %w", err)
	}
	counts := make(map[string]int, len(list))
	for _, kc := range list {
		counts[kc.kind] = kc.n
	}
	return counts, nil
}

// collect scans every row with scan and closes rows.
func collect[T any](rows *sql.Rows, scan func(*sql.Rows) (T, error)) (out []T, err error) {
	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// scanRow scans one row selected with jobColumns.
func scanRow(row *sql.Row) (jobs.Row, error) {
	var (
		r                          jobs.Row
		state                      string
		runAfter, created, updated int64
		period, started, finished  sql.NullInt64
		uniqueKey, lastErr         sql.NullString
	)
	err := row.Scan(&r.ID, &r.Kind, &r.Payload, &state, &r.Attempts, &runAfter, &period, &uniqueKey,
		&lastErr, &created, &updated, &started, &finished)
	if err != nil {
		return jobs.Row{}, err
	}
	r.State = jobs.State(state)
	r.RunAfter = fromMilli(runAfter)
	r.PeriodAt = fromNullMilli(period)
	r.UniqueKey = uniqueKey.String
	r.LastError = lastErr.String
	r.CreatedAt = fromMilli(created)
	r.UpdatedAt = fromMilli(updated)
	r.StartedAt = fromNullMilli(started)
	r.FinishedAt = fromNullMilli(finished)
	return r, nil
}

// notInIDs returns an " AND id NOT IN (?, ...)" clause with one placeholder
// per id, and the ids as arguments. No ids means no clause.
func notInIDs(ids []int64) (string, []any) {
	if len(ids) == 0 {
		return "", nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return " AND id NOT IN (?" + strings.Repeat(", ?", len(ids)-1) + ")", args
}

// nullTime binds t, or NULL when t is nil.
func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

// nullString binds s, or NULL when s is "".
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// fromMilli converts a stored Unix ms time to a UTC time.Time.
func fromMilli(v int64) time.Time {
	return time.UnixMilli(v).UTC()
}

// fromNullMilli converts a nullable stored Unix ms time; NULL is nil.
func fromNullMilli(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMilli(v.Int64)
	return &t
}
