package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	msqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// openTest opens a database in dir with an empty migration set and closes
// it when the test ends.
func openTest(t *testing.T, dir string) *DB {
	t.Helper()
	db, err := Open(t.Context(), Options{
		Dir:        dir,
		Migrations: fstest.MapFS{},
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
	return db
}

// permOf returns the permission bits of path, failing the test if it
// cannot be read.
func permOf(t *testing.T, path string) fs.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

func TestOpenCreatesFile0600(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "data")

	db := openTest(t, dir)

	want := filepath.Join(dir, FileName)
	if got := db.Path(); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
	if got := permOf(t, want); got != 0o600 {
		t.Errorf("database mode = %#o, want 0600", got)
	}
	if got := permOf(t, dir); got != 0o700 {
		t.Errorf("data dir mode = %#o, want 0700", got)
	}
}

func TestOpenTightensExistingFileMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	for _, p := range []string{path, path + "-wal"} {
		// A deliberately loose fixture: Open must tighten it.
		if err := os.WriteFile(p, nil, 0o644); err != nil { //nolint:gosec // G306: deliberate 0644 fixture
			t.Fatal(err)
		}
	}

	openTest(t, dir)

	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if got := permOf(t, p); got != 0o600 {
			t.Errorf("%s mode = %#o, want 0600", filepath.Base(p), got)
		}
	}
}

func TestOpenRefusesNonRegularFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, FileName), 0o700); err != nil {
		t.Fatal(err)
	}

	db, err := Open(t.Context(), Options{
		Dir:        dir,
		Migrations: fstest.MapFS{},
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err == nil {
		_ = db.Close()
		t.Fatal("Open succeeded on a directory named like the database, want an error")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error = %q, want it to say not a regular file", err)
	}
}

func TestOpenWarnsOnOpenDataDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // G302: deliberate loose fixture
		t.Fatal(err)
	}
	var buf bytes.Buffer
	db, err := Open(t.Context(), Options{
		Dir:        dir,
		Migrations: fstest.MapFS{},
		Logger:     slog.New(slog.NewTextHandler(&buf, nil)),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	out := buf.String()
	if !strings.Contains(out, "data dir is accessible by group or others") || !strings.Contains(out, "mode=0755") {
		t.Errorf("log = %q, want the data dir warning with mode=0755", out)
	}
}

func TestOpenWAL(t *testing.T) {
	t.Parallel()
	db := openTest(t, t.TempDir())

	for name, pool := range map[string]*sql.DB{"writer": db.writer, "reader": db.readers} {
		var mode string
		if err := pool.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatalf("%s journal_mode: %v", name, err)
		}
		if mode != "wal" {
			t.Errorf("%s journal_mode = %q, want wal", name, mode)
		}
	}
}

func TestOpenPoolSizes(t *testing.T) {
	t.Parallel()
	db := openTest(t, t.TempDir())

	if got := db.writer.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("writer MaxOpenConnections = %d, want 1", got)
	}
	if got := db.readers.Stats().MaxOpenConnections; got != 4 {
		t.Errorf("readers MaxOpenConnections = %d, want 4", got)
	}
}

func TestOpenBusyTimeoutEveryConnection(t *testing.T) {
	t.Parallel()
	db := openTest(t, t.TempDir())
	ctx := t.Context()

	var ms int
	if err := db.writer.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&ms); err != nil {
		t.Fatal(err)
	}
	if ms != 5000 {
		t.Errorf("writer busy_timeout = %d, want 5000", ms)
	}

	// Hold all four reader connections at once, so each one is checked.
	conns := make([]*sql.Conn, 0, 4)
	for range 4 {
		c, err := db.readers.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for i, c := range conns {
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&ms); err != nil {
			t.Fatal(err)
		}
		if ms != 5000 {
			t.Errorf("reader %d busy_timeout = %d, want 5000", i, ms)
		}
	}
	for _, c := range conns {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	}
}

func TestReaderCannotWrite(t *testing.T) {
	t.Parallel()
	db := openTest(t, t.TempDir())

	_, err := db.readers.ExecContext(t.Context(), "CREATE TABLE t(x INTEGER) STRICT")
	if err == nil {
		t.Fatal("CREATE TABLE on a reader succeeded, want an error")
	}
	var serr *msqlite.Error
	if !errors.As(err, &serr) || serr.Code()&0xff != sqlite3.SQLITE_READONLY {
		t.Errorf("error = %v, want SQLITE_READONLY", err)
	}
}

// openRaw opens a pool on path with only the DSN options in query, without
// the settings Open adds.
func openRaw(t *testing.T, path, query string) *sql.DB {
	t.Helper()
	pool, err := sql.Open(driverName, (&url.URL{Scheme: "file", Path: path, RawQuery: query}).String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

func TestCheckTimeFormatFailsWithoutOption(t *testing.T) {
	t.Parallel()
	pool := openRaw(t, filepath.Join(t.TempDir(), FileName), "_busy_timeout=5000")

	err := checkTimeFormat(t.Context(), pool)
	if err == nil || !strings.Contains(err.Error(), "want integer") {
		t.Errorf("checkTimeFormat = %v, want a bound time not stored as integer", err)
	}
}

func TestCheckWriterFailsWithoutWAL(t *testing.T) {
	t.Parallel()
	pool := openRaw(t, filepath.Join(t.TempDir(), FileName), "_busy_timeout=5000")

	err := checkWriter(t.Context(), pool)
	if err == nil || !strings.Contains(err.Error(), "want wal") {
		t.Errorf("checkWriter = %v, want a journal_mode error", err)
	}
}

func TestCheckWriterFailsOnBusyTimeout(t *testing.T) {
	t.Parallel()
	pool := openRaw(t, filepath.Join(t.TempDir(), FileName), "_journal_mode=WAL&_busy_timeout=1000")

	err := checkWriter(t.Context(), pool)
	if err == nil || !strings.Contains(err.Error(), "busy_timeout is 1000") {
		t.Errorf("checkWriter = %v, want a busy_timeout error", err)
	}
}

func TestOpenRefusesSymlinkedDatabase(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.db")
	// A deliberately loose target: Open must not chmod it through the link.
	if err := os.WriteFile(target, nil, 0o644); err != nil { //nolint:gosec // G306: deliberate 0644 fixture
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, FileName)); err != nil {
		t.Fatal(err)
	}

	db, err := Open(t.Context(), Options{
		Dir:        dir,
		Migrations: fstest.MapFS{},
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err == nil {
		_ = db.Close()
		t.Fatal("Open succeeded on a symlinked database, want an error")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error = %q, want it to say not a regular file", err)
	}
	if got := permOf(t, target); got != 0o644 {
		t.Errorf("symlink target mode = %#o, want 0644 unchanged", got)
	}
}

func TestTimeStoredAsUnixMilli(t *testing.T) {
	t.Parallel()
	db := openTest(t, t.TempDir())
	ctx := t.Context()

	if _, err := db.writer.ExecContext(ctx, "CREATE TABLE t(at INTEGER) STRICT"); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 28, 1, 2, 3, 4_000_000, time.UTC)
	if _, err := db.writer.ExecContext(ctx, "INSERT INTO t(at) VALUES (?)", at); err != nil {
		t.Fatal(err)
	}

	var typ string
	var v int64
	if err := db.readers.QueryRowContext(ctx, "SELECT typeof(at), at FROM t").Scan(&typ, &v); err != nil {
		t.Fatal(err)
	}
	if typ != "integer" {
		t.Errorf("typeof(at) = %q, want integer", typ)
	}
	if want := at.UnixMilli(); v != want {
		t.Errorf("at = %d, want %d", v, want)
	}

	if _, err := db.writer.ExecContext(ctx, "INSERT INTO t(at) VALUES ('2026-11-28')"); err == nil {
		t.Error("inserting text into a STRICT INTEGER column succeeded, want an error")
	}
}

func TestOpenEscapesPath(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	dir := filepath.Join(parent, "a b?c%d&e#f[g")
	stale := filepath.Join(dir, SnapshotDirName, "pre-migration-v0-to-v1-20261128T000000Z.db.tmp")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	openTest(t, dir)

	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Errorf("database file: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale snapshot tmp still there (stat err %v)", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(dir) {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("parent dir holds %q, want only %q", names, filepath.Base(dir))
	}
}

func TestOpenRemovesStaleSnapshotTmp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	snaps := filepath.Join(dir, SnapshotDirName)
	if err := os.Mkdir(snaps, 0o755); err != nil { //nolint:gosec // G301: deliberate loose fixture
		t.Fatal(err)
	}
	stale := []string{
		filepath.Join(snaps, "pre-migration-v0-to-v1-20261128T000000Z.db.tmp"),
		filepath.Join(snaps, "pre-migration-v0-to-v1-20261128T000000Z.db.tmp-journal"),
	}
	files := []string{
		filepath.Join(snaps, "keep.db"),
		filepath.Join(snaps, "other.tmp"),
		filepath.Join(snaps, "other.db.tmp-journal"),
	}
	for _, p := range append(stale, files...) {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Named like stale temp files, but not regular files: they stay.
	tmpDir := filepath.Join(snaps, "pre-migration-v0-to-v2-20261128T000000Z.db.tmp")
	if err := os.Mkdir(tmpDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tmpLink := filepath.Join(snaps, "pre-migration-v0-to-v3-20261128T000000Z.db.tmp")
	if err := os.Symlink(files[0], tmpLink); err != nil {
		t.Fatal(err)
	}
	kept := append(files, tmpDir, tmpLink)

	openTest(t, dir)

	for _, p := range stale {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("stale %s still there (stat err %v)", filepath.Base(p), err)
		}
	}
	for _, p := range kept {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was removed: %v", filepath.Base(p), err)
		}
	}
	if got := permOf(t, snaps); got != 0o700 {
		t.Errorf("snapshots dir mode = %#o, want 0700", got)
	}
}

func TestCloseIdempotent(t *testing.T) {
	t.Parallel()
	db, err := Open(context.Background(), Options{
		Dir:        t.TempDir(),
		Migrations: fstest.MapFS{},
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := db.writer.PingContext(t.Context()); err == nil {
		t.Error("writer still usable after Close")
	}
}
