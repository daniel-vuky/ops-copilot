package sqlite

import (
	"cmp"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ErrSchemaTooNew is wrapped by the error Open returns when the database's
// schema version is higher than the newest migration this binary knows
// (ADR 0001 §3).
var ErrSchemaTooNew = errors.New("database schema is newer than this binary")

// schemaTooNewError is the refusal to open a database whose schema is newer
// than this binary. It matches ErrSchemaTooNew with errors.Is.
type schemaTooNewError struct {
	have, known int
}

func (e *schemaTooNewError) Error() string {
	return fmt.Sprintf("database schema version %d is newer than this binary knows (%d); "+
		"run a newer image or restore a pre-migration snapshot", e.have, e.known)
}

func (e *schemaTooNewError) Unwrap() error {
	return ErrSchemaTooNew
}

// migration is one NNNN_slug.sql file of the migration set.
type migration struct {
	Version int    // NNNN, from 1
	Name    string // the file name
	SQL     string // the file content
}

var (
	// migrationName is the file name rule; the first group is the version.
	migrationName = regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+\.sql$`)

	// txControl matches a statement that starts or ends a transaction.
	// Each migration runs in a transaction of its own, so a file must not
	// control one. A bare END; is not matched: it also ends a trigger body.
	txControl = regexp.MustCompile(`(?im)^\s*(BEGIN(\s+(DEFERRED|IMMEDIATE|EXCLUSIVE))?(\s+TRANSACTION)?|(COMMIT|END)\s+TRANSACTION|COMMIT|ROLLBACK(\s+TRANSACTION)?(\s+TO(\s+SAVEPOINT)?\s+\w+)?|SAVEPOINT\s+\w+|RELEASE(\s+SAVEPOINT)?\s+\w+)\s*;`)

	// timeDefault matches SQL that takes "now" from SQLite, which stores
	// text (ADR 0005 §1). 'subsec' and 'subsecond' are matched too: they
	// only make sense with "now". The check is textual: it also matches
	// comments and string literals.
	timeDefault = regexp.MustCompile(`(?i)\bCURRENT_(TIMESTAMP|DATE|TIME)\b|'now'|"now"|'subsec(ond)?'|\b(unixepoch|julianday|datetime|date|time)\s*\(\s*\)|\bstrftime\s*\(\s*'[^']*'\s*\)`)
)

// loadMigrations reads the migration set at the root of fsys, validates
// it and returns it ordered by version. Files not ending in .sql are
// ignored. Versions start at 1 and are contiguous.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}
	set := make([]migration, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		// Any case of .sql goes on to the name check, so 0003_x.SQL is
		// refused instead of silently skipped.
		if e.IsDir() || !strings.EqualFold(path.Ext(name), ".sql") {
			continue
		}
		m, err := readMigration(fsys, name)
		if err != nil {
			return nil, err
		}
		set = append(set, m)
	}
	slices.SortFunc(set, func(a, b migration) int {
		return cmp.Or(cmp.Compare(a.Version, b.Version), strings.Compare(a.Name, b.Name))
	})
	for i, m := range set {
		if i > 0 && m.Version == set[i-1].Version {
			return nil, fmt.Errorf("store: migrations %s and %s have the same version %d",
				set[i-1].Name, m.Name, m.Version)
		}
		if m.Version != i+1 {
			return nil, fmt.Errorf("store: migration version %d is missing; versions start at 1 and are contiguous", i+1)
		}
	}
	return set, nil
}

// readMigration reads and checks the migration file name.
func readMigration(fsys fs.FS, name string) (migration, error) {
	match := migrationName.FindStringSubmatch(name)
	if match == nil {
		return migration{}, fmt.Errorf("store: migration file %q: name must match NNNN_slug.sql", name)
	}
	version, err := strconv.Atoi(match[1])
	if err != nil {
		return migration{}, fmt.Errorf("store: migration file %q: %w", name, err)
	}
	if version == 0 {
		return migration{}, fmt.Errorf("store: migration file %q: versions start at 0001", name)
	}
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return migration{}, fmt.Errorf("store: read migration %s: %w", name, err)
	}
	text := string(data)
	if bad := txControl.FindString(text); bad != "" {
		return migration{}, fmt.Errorf("store: migration %s: transaction control %q is not allowed",
			name, strings.TrimSpace(bad))
	}
	if bad := timeDefault.FindString(text); bad != "" {
		return migration{}, fmt.Errorf("store: migration %s: time default %q is not allowed; "+
			"bind the time from the clock instead (ADR 0005 §1)", name, bad)
	}
	return migration{Version: version, Name: name, SQL: text}, nil
}

// migrate applies the pending migrations of fsys (ADR 0001 §3, ADR 0005
// §3). It validates the whole set first, refuses a schema newer than the
// set, and writes a pre-migration snapshot before the first pending
// migration. Each migration runs in its own BEGIN IMMEDIATE transaction
// on one writer connection, with foreign keys off so a table rebuild
// cannot cascade, and is checked with foreign_key_check before commit.
func (db *DB) migrate(ctx context.Context, fsys fs.FS) error {
	set, err := loadMigrations(fsys)
	if err != nil {
		return err
	}
	known := len(set) // versions are 1..len(set)

	var current int
	if err := db.writer.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	switch {
	case current < 0:
		return fmt.Errorf("store: schema version %d is not valid", current)
	case current > known:
		return &schemaTooNewError{have: current, known: known}
	case current == known:
		db.logger.Info("schema up to date", "version", current)
		return nil
	}

	snapshot, err := db.snapshot(ctx, current, known)
	if err != nil {
		return err
	}
	return db.applyPending(ctx, set[current:], snapshot)
}

// applyPending applies pending, in order, on one dedicated writer
// connection. snapshot is named in the error of a failed migration.
func (db *DB) applyPending(ctx context.Context, pending []migration, snapshot string) (err error) {
	conn, err := db.writer.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: get the writer for migrations: %w", err)
	}
	defer func() {
		// After a discard the conn is already closed: ErrConnDone is expected.
		if cerr := conn.Close(); cerr != nil && !errors.Is(cerr, sql.ErrConnDone) {
			err = errors.Join(err, fmt.Errorf("store: release the writer after migrations: %w", cerr))
		}
	}()

	// foreign_keys has no effect inside a transaction, so it is switched
	// off here, around the migrations, and switched back on after them.
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return fmt.Errorf("store: turn foreign keys off for migrations: %w", err)
	}
	defer func() {
		if ferr := restoreForeignKeys(ctx, conn); ferr != nil {
			err = errors.Join(err, ferr)
		}
	}()

	for _, m := range pending {
		start := time.Now()
		if err := applyOne(ctx, conn, m); err != nil {
			return fmt.Errorf("store: migration %d (%s) failed: %w; snapshot kept at %s", m.Version, m.Name, err, snapshot)
		}
		db.logger.Info("migration applied",
			"version", m.Version, "name", m.Name, "duration_ms", time.Since(start).Milliseconds())
	}
	return nil
}

// restoreForeignKeys turns foreign keys back on for conn and reads the
// setting back. When ctx was cancelled mid-migration, database/sql rolls
// the transaction back asynchronously, so the PRAGMA may run while SQLite
// still holds the transaction open, where it is silently ignored. If the
// setting is not 1 afterwards, conn is discarded: the pool opens a new
// connection from the DSN, which turns foreign keys on.
func restoreForeignKeys(ctx context.Context, conn *sql.Conn) error {
	ctx = context.WithoutCancel(ctx)
	var errs []error
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		errs = append(errs, fmt.Errorf("store: turn foreign keys back on after migrations: %w", err))
	} else {
		var on int
		if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&on); err != nil {
			errs = append(errs, fmt.Errorf("store: read foreign keys after migrations: %w", err))
		} else if on != 1 {
			errs = append(errs, errors.New("store: foreign keys still off after migrations"))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	if err := conn.Raw(func(any) error { return driver.ErrBadConn }); err != nil && !errors.Is(err, driver.ErrBadConn) {
		errs = append(errs, fmt.Errorf("store: discard the writer: %w", err))
	}
	return errors.Join(errs...)
}

// migrationSavepoint wraps the script of a migration. If the script ends
// the transaction itself (a COMMIT the textual check missed), releasing
// it fails, so user_version is never set outside the transaction.
const migrationSavepoint = "oc_migration"

// applyOne runs m and sets user_version to its version in one transaction
// (BEGIN IMMEDIATE, from the writer's _txlock). It rolls back unless every
// check passes.
func applyOne(ctx context.Context, conn *sql.Conn, m migration) (err error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rerr := tx.Rollback(); rerr != nil && !errors.Is(rerr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback: %w", rerr))
		}
	}()

	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+migrationSavepoint); err != nil {
		return fmt.Errorf("savepoint: %w", err)
	}
	// The driver runs a script of several statements one by one.
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return err
	}
	// Inside the BEGIN IMMEDIATE transaction, releasing the savepoint
	// does not commit; it fails if the script ended the transaction.
	if _, err := tx.ExecContext(ctx, "RELEASE "+migrationSavepoint); err != nil {
		return fmt.Errorf("the script must not end its transaction: %w", err)
	}
	if err := checkForeignKeys(ctx, tx); err != nil {
		return err
	}
	if err := checkStrict(ctx, tx); err != nil {
		return err
	}
	// PRAGMA takes no bound parameter; the version is an int parsed from
	// a validated file name.
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(m.Version)); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// checkForeignKeys fails if any row breaks a foreign key, which a
// migration run with foreign keys off could leave behind.
func checkForeignKeys(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		var (
			table, parent string
			rowid         sql.NullInt64
			fkid          int
		)
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return fmt.Errorf("foreign key check: %w", err)
		}
		if rowid.Valid {
			return fmt.Errorf("foreign key check: row %d of table %s has no matching row in %s", rowid.Int64, table, parent)
		}
		return fmt.Errorf("foreign key check: a row of table %s has no matching row in %s", table, parent)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	return nil
}

// checkStrict fails if an ordinary table is not STRICT (ADR 0005 §1).
// Virtual tables (FTS5) and their shadow tables have other types in
// pragma_table_list, and SQLite's internal tables are skipped by name.
func checkStrict(ctx context.Context, tx *sql.Tx) error {
	var name string
	err := tx.QueryRowContext(ctx, `SELECT name FROM pragma_table_list
		WHERE schema = 'main' AND type = 'table' AND strict = 0 AND name NOT LIKE 'sqlite_%'
		ORDER BY name LIMIT 1`).Scan(&name)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("STRICT check: %w", err)
	}
	return fmt.Errorf("table %s is not STRICT (ADR 0005 §1)", name)
}
