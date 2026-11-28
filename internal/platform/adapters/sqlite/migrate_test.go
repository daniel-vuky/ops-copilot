package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	msqlite "modernc.org/sqlite"
)

// fixedClock is a clock.Clock that always returns t, so snapshot names
// are known in advance.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// testNow is the time of fixedClock in these tests, and testStamp its form
// in a snapshot name.
var testNow = time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

const testStamp = "20261128T120000Z"

// Migration fixtures. Every table is STRICT (ADR 0005 §1); the second
// depends on the first, so applying them out of order fails.
const (
	sqlA = "CREATE TABLE a(id INTEGER PRIMARY KEY, name TEXT NOT NULL) STRICT;\n"
	sqlB = "CREATE TABLE b(id INTEGER PRIMARY KEY, a_id INTEGER REFERENCES a(id)) STRICT;\n" +
		"INSERT INTO a(id, name) VALUES (1, 'x');\n" +
		"INSERT INTO b(id, a_id) VALUES (1, 1);\n"
)

// set builds a migration set from name, SQL pairs.
func set(pairs ...string) fstest.MapFS {
	m := fstest.MapFS{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = &fstest.MapFile{Data: []byte(pairs[i+1])}
	}
	return m
}

// openSet opens dir with migrations and the fixed clock. Log lines go to
// buf when it is not nil. A DB that opens is closed when the test ends.
func openSet(t *testing.T, dir string, migrations fs.FS, buf *bytes.Buffer) (*DB, error) {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	if buf != nil {
		logger = slog.New(slog.NewTextHandler(buf, nil))
	}
	db, err := Open(t.Context(), Options{
		Dir:        dir,
		Migrations: migrations,
		Clock:      fixedClock{testNow},
		Logger:     logger,
	})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db, nil
}

// mustOpenSet is openSet that fails the test on an error.
func mustOpenSet(t *testing.T, dir string, migrations fs.FS, buf *bytes.Buffer) *DB {
	t.Helper()
	db, err := openSet(t, dir, migrations, buf)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
}

// mustClose closes db before the test ends, so dir can be opened again.
func mustClose(t *testing.T, db *DB) {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// userVersion returns PRAGMA user_version read on a reader.
func userVersion(t *testing.T, db *DB) int {
	t.Helper()
	var v int
	if err := db.readers.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	return v
}

// hasTable reports whether the table name exists.
func hasTable(t *testing.T, db *DB, name string) bool {
	t.Helper()
	var n int
	err := db.readers.QueryRowContext(t.Context(),
		"SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?", name).Scan(&n)
	if err != nil {
		t.Fatalf("look up table %s: %v", name, err)
	}
	return n == 1
}

// snapshotNames returns the names in <dir>/snapshots, or nil when it does
// not exist.
func snapshotNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, SnapshotDirName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// writeSnapshotFile creates an empty file named name in <dir>/snapshots.
func writeSnapshotFile(t *testing.T, dir, name string) {
	t.Helper()
	snaps := filepath.Join(dir, SnapshotDirName)
	if err := os.MkdirAll(snaps, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snaps, name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMigrationsValid(t *testing.T) {
	t.Parallel()
	fsys := set("0002_b.sql", sqlB, "0001_a.sql", sqlA, "README.md", "# not a migration")

	got, err := loadMigrations(fsys)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	want := []migration{
		{Version: 1, Name: "0001_a.sql", SQL: sqlA},
		{Version: 2, Name: "0002_b.sql", SQL: sqlB},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d migrations, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("migration %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestLoadMigrationsInvalid(t *testing.T) {
	t.Parallel()
	const table = "CREATE TABLE t(x INTEGER) STRICT;\n"
	timeDefault := func(expr string) fstest.MapFS {
		return set("0001_t.sql", "CREATE TABLE t(x INTEGER DEFAULT ("+expr+")) STRICT;\n")
	}
	cases := []struct {
		name    string
		fsys    fstest.MapFS
		wantErr string
	}{
		{"short number", set("1_x.sql", table), "NNNN_slug.sql"},
		{"upper case slug", set("0001_X.sql", table), "NNNN_slug.sql"},
		{"upper case extension", set("0001_a.sql", table, "0002_x.SQL", table), "NNNN_slug.sql"},
		{"version zero", set("0000_x.sql", table), "versions start at 0001"},
		{"gap", set("0001_a.sql", table, "0003_c.sql", table), "version 2 is missing"},
		{"duplicate", set("0001_a.sql", table, "0001_b.sql", table), "same version 1"},
		{"starts at 2", set("0002_b.sql", table), "version 1 is missing"},
		{"BEGIN", set("0001_t.sql", table+"BEGIN;\n"), "transaction control"},
		{"begin immediate transaction", set("0001_t.sql", "begin immediate transaction;\n"+table), "transaction control"},
		{"COMMIT", set("0001_t.sql", table+"COMMIT;\n"), "transaction control"},
		{"COMMIT TRANSACTION", set("0001_t.sql", table+"COMMIT TRANSACTION;\n"), "transaction control"},
		{"END TRANSACTION", set("0001_t.sql", table+"  END TRANSACTION ;\n"), "transaction control"},
		{"ROLLBACK", set("0001_t.sql", table+"ROLLBACK;\n"), "transaction control"},
		{"ROLLBACK TO", set("0001_t.sql", table+"ROLLBACK TO s;\n"), "transaction control"},
		{"SAVEPOINT", set("0001_t.sql", "SAVEPOINT s;\n"+table), "transaction control"},
		{"RELEASE", set("0001_t.sql", table+"RELEASE s;\n"), "transaction control"},
		{"CURRENT_TIMESTAMP", set("0001_t.sql", "CREATE TABLE t(x INTEGER DEFAULT CURRENT_TIMESTAMP) STRICT;\n"), "time default"},
		{"current_date", timeDefault("current_date"), "time default"},
		{"CURRENT_TIME", timeDefault("CURRENT_TIME"), "time default"},
		{"datetime now", timeDefault("datetime('now')"), "time default"},
		{"unixepoch()", timeDefault("unixepoch()"), "time default"},
		{"julianday()", timeDefault("julianday( )"), "time default"},
		{"datetime()", timeDefault("datetime()"), "time default"},
		{"date()", timeDefault("date()"), "time default"},
		{"time()", timeDefault("time()"), "time default"},
		{"strftime without time", timeDefault("strftime('%s')"), "time default"},
		{"unixepoch subsec", timeDefault("unixepoch('subsec')"), "time default"},
		{"julianday subsec", timeDefault("julianday('subsec')"), "time default"},
		{"strftime subsec", timeDefault("strftime('%s', 'subsec')"), "time default"},
		{"datetime subsecond", timeDefault("datetime('SUBSECOND')"), "time default"},
		{"double-quoted now", timeDefault(`datetime("now")`), "time default"},
		{"time default in a comment", set("0001_t.sql", "-- defaults to CURRENT_TIMESTAMP\n"+table), "time default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := loadMigrations(tc.fsys)
			if err == nil {
				t.Fatalf("loadMigrations = %+v, want an error", got)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadMigrationsAccepted(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, sql string }{
		{"trigger body", "CREATE TABLE t(id INTEGER PRIMARY KEY, n INTEGER NOT NULL) STRICT;\n" +
			"CREATE TRIGGER t_bump AFTER INSERT ON t\n" +
			"BEGIN\n" +
			"  UPDATE t SET n = n + 1 WHERE id = NEW.id;\n" +
			"END;\n"},
		{"columns named commit and release", "CREATE TABLE deploys (\n" +
			"  id INTEGER PRIMARY KEY,\n" +
			"  commit TEXT NOT NULL,\n" +
			"  release TEXT NOT NULL\n" +
			") STRICT;\n"},
		{"time column from a bound parameter", "CREATE TABLE t(created_at INTEGER NOT NULL, updated_at INTEGER) STRICT;\n" +
			"CREATE INDEX t_created_at ON t(created_at);\n"},
		{"strftime with a time argument", "CREATE VIEW v AS SELECT strftime('%Y', 0) AS y;\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := loadMigrations(set("0001_t.sql", tc.sql)); err != nil {
				t.Errorf("loadMigrations: %v, want no error", err)
			}
		})
	}
}

func TestEmbeddedMigrationsValid(t *testing.T) {
	t.Parallel()
	if _, err := loadMigrations(EmbeddedMigrations()); err != nil {
		t.Fatalf("embedded migrations: %v", err)
	}
}

func TestMigrateAppliesInOrder(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer

	db := mustOpenSet(t, t.TempDir(), set("0002_b.sql", sqlB, "0001_a.sql", sqlA), &buf)

	if got := userVersion(t, db); got != 2 {
		t.Errorf("user_version = %d, want 2", got)
	}
	for _, name := range []string{"a", "b"} {
		if !hasTable(t, db, name) {
			t.Errorf("table %s missing", name)
		}
	}
	out := buf.String()
	if got := strings.Count(out, `msg="migration applied"`); got != 2 {
		t.Errorf("log has %d migration applied lines, want 2:\n%s", got, out)
	}
	if !strings.Contains(out, "version=1 name=0001_a.sql") || !strings.Contains(out, "version=2 name=0002_b.sql") {
		t.Errorf("log = %q, want version and name of both migrations", out)
	}
}

func TestNothingPendingNoSnapshot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	migrations := set("0001_a.sql", sqlA, "0002_b.sql", sqlB)
	want := "pre-migration-v0-to-v2-" + testStamp + ".db"

	mustClose(t, mustOpenSet(t, dir, migrations, nil))
	if got := snapshotNames(t, dir); len(got) != 1 || got[0] != want {
		t.Fatalf("after the first open, snapshots = %q, want [%s]", got, want)
	}

	var buf bytes.Buffer
	db := mustOpenSet(t, dir, migrations, &buf)
	if got := snapshotNames(t, dir); len(got) != 1 || got[0] != want {
		t.Errorf("after reopening, snapshots = %q, want [%s]", got, want)
	}
	if out := buf.String(); !strings.Contains(out, `msg="schema up to date" version=2`) {
		t.Errorf("log = %q, want schema up to date with version=2", out)
	}
	if got := userVersion(t, db); got != 2 {
		t.Errorf("user_version = %d, want 2", got)
	}

	empty := t.TempDir()
	mustOpenSet(t, empty, fstest.MapFS{}, nil)
	for _, name := range snapshotNames(t, empty) {
		if strings.HasSuffix(name, ".db") {
			t.Errorf("empty set wrote snapshot %s", name)
		}
	}
}

func TestPendingWritesOneSnapshot(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "a b?c%d&e#f[g")

	mustClose(t, mustOpenSet(t, dir, set("0001_a.sql", sqlA), nil))
	var buf bytes.Buffer
	db := mustOpenSet(t, dir, set("0001_a.sql", sqlA, "0002_b.sql", sqlB), &buf)

	if got := userVersion(t, db); got != 2 {
		t.Errorf("user_version = %d, want 2", got)
	}
	var matches []string
	for _, name := range snapshotNames(t, dir) {
		if strings.HasSuffix(name, ".tmp") {
			t.Errorf("temp file %s left behind", name)
		}
		if strings.HasPrefix(name, "pre-migration-v1-to-v2-") {
			matches = append(matches, name)
		}
	}
	want := "pre-migration-v1-to-v2-" + testStamp + ".db"
	if len(matches) != 1 || matches[0] != want {
		t.Fatalf("v1-to-v2 snapshots = %q, want [%s]", matches, want)
	}
	path := filepath.Join(dir, SnapshotDirName, want)
	if got := permOf(t, path); got != 0o600 {
		t.Errorf("snapshot mode = %#o, want 0600", got)
	}
	if got := permOf(t, filepath.Join(dir, SnapshotDirName)); got != 0o700 {
		t.Errorf("snapshots dir mode = %#o, want 0700", got)
	}
	out := buf.String()
	if !strings.Contains(out, `msg="pre-migration snapshot written"`) || !strings.Contains(out, "size_bytes=") ||
		!strings.Contains(out, "duration_ms=") {
		t.Errorf("log = %q, want the snapshot line with size_bytes and duration_ms", out)
	}

	snap, err := sql.Open(driverName, (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snap.Close() }()
	var v int
	if err := snap.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read snapshot user_version: %v", err)
	}
	if v != 1 {
		t.Errorf("snapshot user_version = %d, want 1", v)
	}
}

func TestMigrationFailureKeepsSnapshot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bad := "CREATE TABLE b(x INTEGER) STRICT;\nINSERT INTO nope VALUES(1);\n"
	migrations := set("0001_a.sql", sqlA, "0002_bad.sql", bad, "0003_c.sql", "CREATE TABLE c(x INTEGER) STRICT;\n")

	_, err := openSet(t, dir, migrations, nil)
	if err == nil {
		t.Fatal("Open succeeded, want the failure of migration 2")
	}
	snapPath := filepath.Join(dir, SnapshotDirName, "pre-migration-v0-to-v3-"+testStamp+".db")
	for _, want := range []string{"migration 2 (0002_bad.sql) failed", "nope", "snapshot kept at " + snapPath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}

	db := mustOpenSet(t, dir, set("0001_a.sql", sqlA), nil)
	if got := userVersion(t, db); got != 1 {
		t.Errorf("user_version = %d, want 1", got)
	}
	if hasTable(t, db, "b") {
		t.Error("table b of the failed migration exists")
	}
	if _, err := os.Stat(snapPath); err != nil {
		t.Errorf("snapshot gone: %v", err)
	}
}

func TestMigrationTableRebuildKeepsChildren(t *testing.T) {
	t.Parallel()
	m1 := "CREATE TABLE parent(id INTEGER PRIMARY KEY, name TEXT) STRICT;\n" +
		"CREATE TABLE child(id INTEGER PRIMARY KEY, pid INTEGER NOT NULL REFERENCES parent(id) ON DELETE CASCADE) STRICT;\n" +
		"INSERT INTO parent(id, name) VALUES (1, 'p');\n" +
		"INSERT INTO child(id, pid) VALUES (1, 1);\n"
	m2 := "CREATE TABLE parent_new(id INTEGER PRIMARY KEY, name TEXT NOT NULL DEFAULT '') STRICT;\n" +
		"INSERT INTO parent_new(id, name) SELECT id, coalesce(name, '') FROM parent;\n" +
		"DROP TABLE parent;\n" +
		"ALTER TABLE parent_new RENAME TO parent;\n"

	db := mustOpenSet(t, t.TempDir(), set("0001_parent.sql", m1, "0002_rebuild.sql", m2), nil)

	var n int
	if err := db.readers.QueryRowContext(t.Context(), "SELECT count(*) FROM child").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("child rows = %d, want 1 (the rebuild cascaded)", n)
	}
	var fk int
	if err := db.writer.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Errorf("writer foreign_keys = %d after migrating, want 1", fk)
	}
}

func TestMigrationForeignKeyCheckFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m1 := "CREATE TABLE parent(id INTEGER PRIMARY KEY) STRICT;\n" +
		"CREATE TABLE child(id INTEGER PRIMARY KEY, pid INTEGER NOT NULL REFERENCES parent(id)) STRICT;\n"
	m2 := "INSERT INTO child(id, pid) VALUES (1, 99);\n"
	mustClose(t, mustOpenSet(t, dir, set("0001_parent.sql", m1), nil))

	_, err := openSet(t, dir, set("0001_parent.sql", m1, "0002_orphan.sql", m2), nil)
	if err == nil {
		t.Fatal("Open succeeded, want the foreign key check to fail")
	}
	if !strings.Contains(err.Error(), "migration 2 (0002_orphan.sql) failed") || !strings.Contains(err.Error(), "foreign key") {
		t.Errorf("error = %q, want migration 2 to fail its foreign key check", err)
	}

	db := mustOpenSet(t, dir, set("0001_parent.sql", m1), nil)
	if got := userVersion(t, db); got != 1 {
		t.Errorf("user_version = %d, want 1", got)
	}
	var n int
	if err := db.readers.QueryRowContext(t.Context(), "SELECT count(*) FROM child").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("child rows = %d, want 0", n)
	}
}

func TestMigrationStrictCheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		sql     string
		wantErr string // empty: the migration applies
	}{
		{"non-STRICT table", "CREATE TABLE loose(x INTEGER);\n", "table loose is not STRICT"},
		{"STRICT table", "CREATE TABLE tight(x INTEGER) STRICT;\n", ""},
		{"FTS5 virtual table", "CREATE TABLE doc(id INTEGER PRIMARY KEY, body TEXT NOT NULL) STRICT;\n" +
			"CREATE VIRTUAL TABLE doc_fts USING fts5(body, content='doc', content_rowid='id');\n", ""},
		{"AUTOINCREMENT", "CREATE TABLE seq(id INTEGER PRIMARY KEY AUTOINCREMENT) STRICT;\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, err := openSet(t, t.TempDir(), set("0001_t.sql", tc.sql), nil)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				if got := userVersion(t, db); got != 1 {
					t.Errorf("user_version = %d, want 1", got)
				}
				return
			}
			if err == nil {
				t.Fatal("Open succeeded, want the STRICT check to fail")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := mustOpenSet(t, dir, fstest.MapFS{}, nil)
	if _, err := db.writer.ExecContext(t.Context(), "PRAGMA user_version = 5"); err != nil {
		t.Fatal(err)
	}
	mustClose(t, db)

	_, err := openSet(t, dir, set("0001_a.sql", sqlA, "0002_b.sql", sqlB), nil)
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("error = %v, want ErrSchemaTooNew", err)
	}
	const want = "database schema version 5 is newer than this binary knows (2); " +
		"run a newer image or restore a pre-migration snapshot"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
	if got := snapshotNames(t, dir); len(got) != 0 {
		t.Errorf("snapshots = %q, want none", got)
	}
}

func TestSnapshotFailureLeavesNoFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	final := "pre-migration-v0-to-v1-" + testStamp + ".db"
	writeSnapshotFile(t, dir, final)

	if _, err := openSet(t, dir, set("0001_a.sql", sqlA), nil); err == nil {
		t.Fatal("Open succeeded over an existing snapshot name, want an error")
	}

	if got := snapshotNames(t, dir); len(got) != 1 || got[0] != final {
		t.Errorf("snapshots = %q, want only the existing [%s]", got, final)
	}
	fi, err := os.Stat(filepath.Join(dir, SnapshotDirName, final))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Errorf("existing snapshot was overwritten (size %d)", fi.Size())
	}
	db := mustOpenSet(t, dir, fstest.MapFS{}, nil)
	if got := userVersion(t, db); got != 0 {
		t.Errorf("user_version = %d, want 0", got)
	}
	if hasTable(t, db, "a") {
		t.Error("migration 1 was applied")
	}
}

func TestSnapshotVacuumFailureRemovesTmp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := mustOpenSet(t, dir, fstest.MapFS{}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := db.snapshot(ctx, 0, 1); err == nil {
		t.Fatal("snapshot with a cancelled context succeeded, want an error")
	}

	if got := snapshotNames(t, dir); len(got) != 0 {
		t.Errorf("snapshots = %q, want none", got)
	}
}

func TestSnapshotRestartBound(t *testing.T) {
	t.Parallel()
	name := func(ago time.Duration) string {
		return "pre-migration-v0-to-v1-" + testNow.Add(-ago).Format("20060102T150405Z") + ".db"
	}
	cases := []struct {
		name    string
		files   []string
		dirs    []string
		refused bool
	}{
		{name: "two in the last hour", files: []string{name(10 * time.Minute), name(20 * time.Minute)}},
		{
			name:    "three in the last hour",
			files:   []string{name(10 * time.Minute), name(20 * time.Minute), name(59 * time.Minute)},
			refused: true,
		},
		{
			name:  "three, one older than an hour",
			files: []string{name(10 * time.Minute), name(20 * time.Minute), name(61 * time.Minute)},
		},
		{
			name:  "three, one in the future",
			files: []string{name(10 * time.Minute), name(20 * time.Minute), name(-10 * time.Minute)},
		},
		{
			name:  "three, one exactly an hour old",
			files: []string{name(10 * time.Minute), name(20 * time.Minute), name(time.Hour)},
		},
		{
			name: "others ignored",
			files: []string{
				name(10 * time.Minute), name(20 * time.Minute),
				name(30*time.Minute) + ".tmp",
				"pre-migration-v0-to-v2-" + testStamp + ".db",
				"pre-migration-v0-to-v10-" + testStamp + ".db",
				"pre-migration-v0-to-v1-garbage.db",
				"pre-migration-v0-to-v1-20261128T1159.db",
			},
			dirs: []string{name(40 * time.Minute)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for _, f := range tc.files {
				writeSnapshotFile(t, dir, f)
			}
			for _, d := range tc.dirs {
				if err := os.MkdirAll(filepath.Join(dir, SnapshotDirName, d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotNames(t, dir)

			db, err := openSet(t, dir, set("0001_a.sql", sqlA), nil)

			if !tc.refused {
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				if got := userVersion(t, db); got != 1 {
					t.Errorf("user_version = %d, want 1", got)
				}
				return
			}
			const want = "migration from v0 to v1 already failed 3 times in the last hour; not writing another snapshot"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want it to contain %q", err, want)
			}
			if got := snapshotNames(t, dir); strings.Join(got, ",") != strings.Join(before, ",") {
				t.Errorf("snapshots = %q, want unchanged %q", got, before)
			}
		})
	}
}

func TestSnapshotRestartBoundIgnoredWhenUpToDate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	migrations := set("0001_a.sql", sqlA)
	mustClose(t, mustOpenSet(t, dir, migrations, nil))
	for _, ago := range []time.Duration{10 * time.Minute, 20 * time.Minute} {
		writeSnapshotFile(t, dir, "pre-migration-v0-to-v1-"+testNow.Add(-ago).Format("20060102T150405Z")+".db")
	}
	if got := len(snapshotNames(t, dir)); got != 3 {
		t.Fatalf("%d snapshots, want 3", got)
	}

	db := mustOpenSet(t, dir, migrations, nil)

	if got := userVersion(t, db); got != 1 {
		t.Errorf("user_version = %d, want 1", got)
	}
}

// cancelFuncs holds the cancel functions oc_test_cancel(key) calls.
var cancelFuncs sync.Map // int64 -> context.CancelFunc

// The driver adds functions to connections opened after registration, so
// it is registered before any test opens a database.
func init() {
	msqlite.MustRegisterScalarFunction("oc_test_cancel", 1,
		func(_ *msqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			key, ok := args[0].(int64)
			if !ok {
				return nil, fmt.Errorf("oc_test_cancel: key %v is not an integer", args[0])
			}
			if cancel, ok := cancelFuncs.Load(key); ok {
				cancel.(context.CancelFunc)()
			}
			return int64(1), nil
		})
}

func TestMigrationCancelledRestoresForeignKeys(t *testing.T) {
	t.Parallel()
	db := mustOpenSet(t, t.TempDir(), set("0001_a.sql", sqlA), nil)
	for i := range 20 {
		key := int64(i) + time.Now().UnixNano()
		ctx, cancel := context.WithCancel(t.Context())
		cancelFuncs.Store(key, cancel)
		m := migration{
			Version: 2,
			Name:    "0002_cancel.sql",
			SQL: fmt.Sprintf("SELECT oc_test_cancel(%d);\n", key) +
				"WITH RECURSIVE n(i) AS (SELECT 1000 UNION ALL SELECT i + 1 FROM n WHERE i < 200000)\n" +
				"INSERT INTO a(id, name) SELECT i, 'x' FROM n;\n",
		}

		err := db.applyPending(ctx, []migration{m}, "snap")
		cancelFuncs.Delete(key)
		cancel()

		if err == nil {
			t.Fatalf("run %d: applyPending succeeded after a cancel, want an error", i)
		}
		var fk int
		if err := db.writer.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatalf("run %d: read foreign_keys: %v", i, err)
		}
		if fk != 1 {
			t.Fatalf("run %d: writer foreign_keys = %d after a cancelled migration, want 1", i, fk)
		}
		if got := userVersion(t, db); got != 1 {
			t.Fatalf("run %d: user_version = %d, want 1", i, got)
		}
	}
}

func TestMigrationThatCommitsFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// The commit is not at the start of a line, so the textual check
	// misses it; the savepoint guard must catch it.
	committing := "CREATE TABLE t(x INTEGER) STRICT; COMMIT;\n"
	if _, err := loadMigrations(set("0001_t.sql", committing)); err != nil {
		t.Fatalf("loadMigrations: %v; the test needs a commit the regex misses", err)
	}

	_, err := openSet(t, dir, set("0001_t.sql", committing), nil)
	if err == nil {
		t.Fatal("Open succeeded, want the migration that commits to fail")
	}
	if !strings.Contains(err.Error(), "migration 1 (0001_t.sql) failed") || !strings.Contains(err.Error(), "must not end its transaction") {
		t.Errorf("error = %q, want migration 1 to fail for ending its transaction", err)
	}

	db := mustOpenSet(t, dir, fstest.MapFS{}, nil)
	if got := userVersion(t, db); got != 0 {
		t.Errorf("user_version = %d, want 0", got)
	}
}

func TestNegativeSchemaVersionRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := mustOpenSet(t, dir, fstest.MapFS{}, nil)
	if _, err := db.writer.ExecContext(t.Context(), "PRAGMA user_version = -1"); err != nil {
		t.Fatal(err)
	}
	mustClose(t, db)

	_, err := openSet(t, dir, set("0001_a.sql", sqlA), nil)
	if err == nil || !strings.Contains(err.Error(), "schema version -1 is not valid") {
		t.Fatalf("error = %v, want schema version -1 is not valid", err)
	}
	if got := snapshotNames(t, dir); len(got) != 0 {
		t.Errorf("snapshots = %q, want none", got)
	}
}

func TestRemoveTmpRemovesJournal(t *testing.T) {
	t.Parallel()
	tmp := filepath.Join(t.TempDir(), "pre-migration-v0-to-v1-"+testStamp+".db.tmp")
	for _, p := range []string{tmp, tmp + "-journal"} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := removeTmp(tmp); err != nil {
		t.Fatalf("removeTmp: %v", err)
	}
	for _, p := range []string{tmp, tmp + "-journal"} {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s still there (stat err %v)", filepath.Base(p), err)
		}
	}
	if err := removeTmp(tmp); err != nil {
		t.Errorf("removeTmp of missing files: %v", err)
	}
}

// TestRestoreForeignKeysDiscardsConnInTx covers, deterministically, the
// state a cancelled migration can leave: database/sql has given up the
// transaction but SQLite still holds it open, so PRAGMA foreign_keys = ON
// is silently ignored. The conn must be discarded, and the next writer
// conn must have foreign keys on and no open transaction.
func TestRestoreForeignKeysDiscardsConnInTx(t *testing.T) {
	t.Parallel()
	db := mustOpenSet(t, t.TempDir(), set("0001_a.sql", sqlA), nil)
	ctx := t.Context()
	conn, err := db.writer.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"PRAGMA foreign_keys = OFF", "BEGIN IMMEDIATE"} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	err = restoreForeignKeys(ctx, conn)
	if err == nil || !strings.Contains(err.Error(), "foreign keys still off") {
		t.Errorf("restoreForeignKeys = %v, want foreign keys still off", err)
	}
	if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Errorf("Close: %v", err)
	}

	var fk int
	if err := db.writer.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Errorf("writer foreign_keys = %d, want 1 on a fresh conn", fk)
	}
	if _, err := db.writer.ExecContext(ctx, "INSERT INTO a(id, name) VALUES (7, 'y')"); err != nil {
		t.Errorf("write on the fresh writer conn: %v", err)
	}
}
