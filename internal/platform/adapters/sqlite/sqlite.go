// Package sqlite is the SQLite adapter of the platform store (ADR 0001 §3,
// ADR 0005). It opens one database file in DATA_DIR with two pools: one
// writer connection in WAL mode whose transactions start with BEGIN
// IMMEDIATE, and four read-only reader connections. Bound time.Time values
// are stored as INTEGER Unix milliseconds (ADR 0005 §1).
//
// Both pools use synchronous=NORMAL, which is safe in WAL mode: a power
// failure can lose the most recent commits but never corrupts the file.
//
// The driver is modernc.org/sqlite (no CGO). Its modernc.org/libc dependency
// must stay at exactly the version modernc.org/sqlite's go.mod requires:
// upgrade the two together, never libc on its own.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	msqlite "modernc.org/sqlite"

	"github.com/daniel-vuky/ops-copilot/internal/platform/clock"
)

// Names inside DATA_DIR (ADR 0005, Layout).
const (
	// FileName is the database file name in DATA_DIR.
	FileName = "ops-copilot.db"
	// SnapshotDirName is the directory in DATA_DIR that holds the
	// pre-migration snapshots.
	SnapshotDirName = "snapshots"
)

// Pre-migration snapshot names (ADR 0005 §3): a snapshot is written to its
// final name plus ".tmp" and renamed when complete.
const (
	snapshotPrefix    = "pre-migration-"
	snapshotTmpSuffix = ".db.tmp"
)

// Modes of what the store creates (ADR 0005, Layout).
const (
	dirMode  fs.FileMode = 0o700
	fileMode fs.FileMode = 0o600
)

// Connection settings (ADR 0001 §3).
const (
	driverName     = "sqlite"
	readerConns    = 4
	busyTimeoutMS  = 5000
	journalSizeMax = 64 << 20 // bytes the WAL is truncated to after a checkpoint
)

// Write transaction bounds (ADR 0005 §2). Tests in this package may lower
// the DB fields set from them.
const (
	defaultWriterWait = 30 * time.Second
	defaultSlowWrite  = time.Second
)

// The driver runs each _pragma DSN value verbatim. With strict pragmas on,
// it refuses a value holding more than one statement. It is a process-wide
// switch, so it is set before any connection opens.
func init() {
	msqlite.StrictPragmas(true)
}

// Options configures Open.
type Options struct {
	// Dir is DATA_DIR: the directory that holds the database and its
	// snapshots. It is created with mode 0700 when missing.
	Dir string
	// Migrations holds NNNN_slug.sql files at its root. Nil means
	// EmbeddedMigrations().
	Migrations fs.FS
	// Clock gives the time for snapshot names. Nil means clock.System{}.
	Clock clock.Clock
	// Logger receives the store's log lines. Nil means slog.Default().
	Logger *slog.Logger
}

// DB is an open store: one writer connection and a pool of reader
// connections on the same database file. It is safe for concurrent use.
type DB struct {
	writer  *sql.DB
	readers *sql.DB
	path    string
	dir     string
	clock   clock.Clock
	logger  *slog.Logger

	// writerWait bounds the wait for the writer connection in Write, and
	// slowWrite is the Write duration above which it logs a warning.
	writerWait time.Duration
	slowWrite  time.Duration

	closeOnce sync.Once
	closeErr  error
}

// Open prepares Dir and the database file, opens the writer and reader
// pools, checks their settings and applies pending migrations. On any
// error nothing stays open.
func Open(ctx context.Context, opts Options) (*DB, error) {
	if opts.Dir == "" {
		return nil, errors.New("store: data dir is required")
	}
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("store: resolve data dir: %w", err)
	}
	if opts.Migrations == nil {
		opts.Migrations = EmbeddedMigrations()
	}
	if opts.Clock == nil {
		opts.Clock = clock.System{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	db := &DB{
		path:       filepath.Join(dir, FileName),
		dir:        dir,
		clock:      opts.Clock,
		logger:     opts.Logger,
		writerWait: defaultWriterWait,
		slowWrite:  defaultSlowWrite,
	}
	if err := db.prepareFiles(); err != nil {
		return nil, err
	}
	if err := db.openPools(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	if err := db.migrate(ctx, opts.Migrations); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

// Path returns the absolute path of the database file.
func (db *DB) Path() string {
	return db.path
}

// Close closes the reader pool, then the writer. Calls after the first
// return the first call's result.
func (db *DB) Close() error {
	db.closeOnce.Do(func() {
		var errs []error
		if db.readers != nil {
			if err := db.readers.Close(); err != nil {
				errs = append(errs, fmt.Errorf("store: close readers: %w", err))
			}
		}
		if db.writer != nil {
			if err := db.writer.Close(); err != nil {
				errs = append(errs, fmt.Errorf("store: close writer: %w", err))
			}
		}
		db.closeErr = errors.Join(errs...)
	})
	return db.closeErr
}

// prepareFiles creates Dir and the database file with their modes,
// tightens the modes of existing files and removes snapshot temp files
// left by a crash. It runs before any connection opens, so it never
// touches a file SQLite holds open.
//
// It assumes one process per DATA_DIR (ADR 0001 §6). The gap between each
// Lstat and the Chmod that follows is accepted: only a principal that can
// already write in DATA_DIR could swap the file in between, and the chmod
// only tightens a mode.
func (db *DB) prepareFiles() error {
	if err := os.MkdirAll(db.dir, dirMode); err != nil {
		return fmt.Errorf("store: create data dir: %w", err)
	}
	fi, err := os.Stat(db.dir)
	if err != nil {
		return fmt.Errorf("store: stat data dir: %w", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		db.logger.Warn("data dir is accessible by group or others",
			"path", db.dir, "mode", fmt.Sprintf("%04o", perm))
	}

	if err := createOrTighten(db.path); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := tightenIfExists(db.path + suffix); err != nil {
			return err
		}
	}
	return db.removeStaleSnapshotTmp()
}

// createOrTighten creates the database file empty with mode 0600, or sets
// an existing one to 0600. It refuses anything but a regular file, so a
// symlink cannot redirect the chmod or the database.
func createOrTighten(path string) error {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode) //nolint:gosec // G304: path is DATA_DIR plus a constant name
		if err != nil {
			return fmt.Errorf("store: create database file: %w", err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("store: create database file: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("store: stat database file: %w", err)
	case !fi.Mode().IsRegular():
		return fmt.Errorf("store: %s is not a regular file", path)
	}
	if err := os.Chmod(path, fileMode); err != nil {
		return fmt.Errorf("store: set database file mode: %w", err)
	}
	return nil
}

// tightenIfExists sets path to mode 0600 when it exists. It refuses
// anything but a regular file.
func tightenIfExists(path string) error {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("store: stat %s: %w", filepath.Base(path), err)
	case !fi.Mode().IsRegular():
		return fmt.Errorf("store: %s is not a regular file", path)
	}
	if err := os.Chmod(path, fileMode); err != nil {
		return fmt.Errorf("store: set %s mode: %w", filepath.Base(path), err)
	}
	return nil
}

// removeStaleSnapshotTmp sets the snapshot dir, when it exists, to mode
// 0700 and removes the snapshot temp files a crash left in it (ADR 0005
// §3). Only regular files named pre-migration-*.db.tmp, or that plus
// -journal (VACUUM INTO's rollback journal), are removed.
func (db *DB) removeStaleSnapshotTmp() error {
	snaps := filepath.Join(db.dir, SnapshotDirName)
	fi, err := os.Lstat(snaps)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("store: stat snapshot dir: %w", err)
	case !fi.IsDir():
		return fmt.Errorf("store: %s is not a directory", snaps)
	}
	if err := os.Chmod(snaps, dirMode); err != nil {
		return fmt.Errorf("store: set snapshot dir mode: %w", err)
	}

	entries, err := os.ReadDir(snaps)
	if err != nil {
		return fmt.Errorf("store: read snapshot dir: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		stale := strings.HasSuffix(name, snapshotTmpSuffix) || strings.HasSuffix(name, snapshotTmpSuffix+snapshotJournalSuffix)
		if !e.Type().IsRegular() || !strings.HasPrefix(name, snapshotPrefix) || !stale {
			continue
		}
		path := filepath.Join(snaps, name)
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("store: remove stale snapshot temp file: %w", err)
		}
		db.logger.Info("stale snapshot temp file removed", "path", path)
	}
	return nil
}

// openPools opens and checks the writer, then the readers. The writer
// goes first because it sets the persistent WAL journal mode.
func (db *DB) openPools(ctx context.Context) error {
	writer, err := sql.Open(driverName, dsn(db.path, true))
	if err != nil {
		return fmt.Errorf("store: open writer: %w", err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	writer.SetConnMaxLifetime(0)
	writer.SetConnMaxIdleTime(0)
	db.writer = writer
	if err := writer.PingContext(ctx); err != nil {
		return fmt.Errorf("store: open writer: %w", err)
	}
	if err := checkWriter(ctx, writer); err != nil {
		return err
	}

	readers, err := sql.Open(driverName, dsn(db.path, false))
	if err != nil {
		return fmt.Errorf("store: open readers: %w", err)
	}
	readers.SetMaxOpenConns(readerConns)
	readers.SetMaxIdleConns(readerConns)
	readers.SetConnMaxLifetime(0)
	readers.SetConnMaxIdleTime(0)
	db.readers = readers
	if err := readers.PingContext(ctx); err != nil {
		return fmt.Errorf("store: open readers: %w", err)
	}

	if err := checkTimeFormat(ctx, writer); err != nil {
		return fmt.Errorf("store: writer: %w", err)
	}
	if err := checkTimeFormat(ctx, readers); err != nil {
		return fmt.Errorf("store: reader: %w", err)
	}
	return nil
}

// dsn returns the driver name for path. The path is escaped as a file URI,
// so characters such as '?', '#', '%' or '&' in DATA_DIR stay part of the
// path and cannot add DSN options. Only the driver's validated shorthand
// keys are used, plus one constant _pragma that has no shorthand.
func dsn(path string, writer bool) string {
	q := url.Values{}
	q.Set("_busy_timeout", strconv.Itoa(busyTimeoutMS))
	q.Set("_foreign_keys", "1")
	q.Set("_synchronous", "NORMAL")
	q.Set("_time_integer_format", "unix_milli")
	q.Set("_pragma", "journal_size_limit("+strconv.Itoa(journalSizeMax)+")")
	if writer {
		q.Set("_journal_mode", "WAL")
		q.Set("_txlock", "immediate")
	} else {
		q.Set("_query_only", "1")
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	return u.String()
}

// checkWriter fails unless the writer runs in WAL mode with the 5 s busy
// timeout.
func checkWriter(ctx context.Context, writer *sql.DB) error {
	var mode string
	if err := writer.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("store: read journal_mode: %w", err)
	}
	if mode != "wal" {
		return fmt.Errorf("store: journal_mode is %q, want wal", mode)
	}
	var ms int
	if err := writer.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&ms); err != nil {
		return fmt.Errorf("store: read busy_timeout: %w", err)
	}
	if ms != busyTimeoutMS {
		return fmt.Errorf("store: busy_timeout is %d, want %d", ms, busyTimeoutMS)
	}
	return nil
}

// checkTimeFormat fails unless a bound time.Time reaches SQLite as an
// integer (ADR 0005 §1).
func checkTimeFormat(ctx context.Context, pool *sql.DB) error {
	var typ string
	if err := pool.QueryRowContext(ctx, "SELECT typeof(?)", time.Now()).Scan(&typ); err != nil {
		return fmt.Errorf("check time format: %w", err)
	}
	if typ != "integer" {
		return fmt.Errorf("a bound time is stored as %s, want integer", typ)
	}
	return nil
}
