package sqlite

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Pre-migration snapshot names (ADR 0005 §3):
// pre-migration-v<from>-to-v<to>-<UTC time>.db.
const (
	snapshotSuffix     = ".db"
	snapshotTimeLayout = "20060102T150405Z"
	// snapshotJournalSuffix follows a temp file's name on the rollback
	// journal VACUUM INTO writes next to it.
	snapshotJournalSuffix = "-journal"
)

// Restart bound (ADR 0005 §3): at most this many snapshots for the same
// from and to in the window before a start refuses to write another.
const (
	snapshotMaxRecent = 3
	snapshotWindow    = time.Hour
)

// snapshot writes a copy of the database to
// <Dir>/snapshots/pre-migration-v<from>-to-v<to>-<UTC time>.db and returns
// its path (ADR 0005 §3). The copy is written by VACUUM INTO to a .tmp
// file, synced and renamed, so a file under the final name is always
// whole. It never overwrites a snapshot, and it refuses to write one when
// snapshotMaxRecent for the same from and to were written in the last
// snapshotWindow.
//
// VACUUM INTO runs on the writer pool outside any transaction. Like the
// file preparation in Open, it assumes one process per DATA_DIR (ADR 0001
// §6): the gap between checking the final name and the rename is accepted.
func (db *DB) snapshot(ctx context.Context, from, to int) (string, error) {
	dir := filepath.Join(db.dir, SnapshotDirName)
	_, statErr := os.Lstat(dir)
	created := errors.Is(statErr, fs.ErrNotExist)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return "", fmt.Errorf("store: create snapshot dir: %w", err)
	}
	if created {
		// Make the new directory's entry in DATA_DIR durable too.
		if err := syncPath(db.dir); err != nil {
			return "", fmt.Errorf("store: sync data dir: %w", err)
		}
	}
	now := db.clock.Now().UTC()
	if err := checkRestartBound(dir, from, to, now); err != nil {
		return "", err
	}

	final := filepath.Join(dir, fmt.Sprintf("%sv%d-to-v%d-%s%s",
		snapshotPrefix, from, to, now.Format(snapshotTimeLayout), snapshotSuffix))
	if err := refuseExisting(final); err != nil {
		return "", err
	}

	start := time.Now()
	tmp := final + ".tmp"
	size, err := db.vacuumInto(ctx, tmp)
	if err != nil {
		return "", errors.Join(err, removeTmp(tmp))
	}
	if err := refuseExisting(final); err != nil {
		return "", errors.Join(err, removeTmp(tmp))
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", errors.Join(fmt.Errorf("store: rename snapshot: %w", err), removeTmp(tmp))
	}
	if err := syncPath(dir); err != nil {
		return "", fmt.Errorf("store: sync snapshot dir: %w; snapshot kept at %s", err, final)
	}
	db.logger.Info("pre-migration snapshot written",
		"path", final, "size_bytes", size, "duration_ms", time.Since(start).Milliseconds())
	return final, nil
}

// vacuumInto creates tmp empty with mode 0600, fills it with VACUUM INTO
// (which accepts an existing empty file), syncs it and returns its size.
func (db *DB) vacuumInto(ctx context.Context, tmp string) (int64, error) {
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode) //nolint:gosec // G304: path is DATA_DIR plus a generated name
	if err != nil {
		return 0, fmt.Errorf("store: create snapshot temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return 0, errors.Join(fmt.Errorf("store: create snapshot temp file: %w", err), removeTmp(tmp))
	}
	if _, err := db.writer.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		return 0, fmt.Errorf("store: write snapshot: %w", err)
	}
	if err := syncPath(tmp); err != nil {
		return 0, fmt.Errorf("store: sync snapshot: %w", err)
	}
	fi, err := os.Stat(tmp)
	if err != nil {
		return 0, fmt.Errorf("store: stat snapshot: %w", err)
	}
	return fi.Size(), nil
}

// refuseExisting fails if anything exists at path.
func refuseExisting(path string) error {
	_, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("store: stat snapshot: %w", err)
	}
	return fmt.Errorf("store: snapshot %s already exists", path)
}

// removeTmp removes a snapshot temp file and the rollback journal VACUUM
// INTO may leave next to it; a missing file is not an error.
func removeTmp(tmp string) error {
	var errs []error
	for _, p := range []string{tmp, tmp + snapshotJournalSuffix} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("store: remove snapshot temp file: %w", err))
		}
	}
	return errors.Join(errs...)
}

// syncPath flushes the file or directory at path to disk.
func syncPath(path string) (err error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is DATA_DIR plus a generated name
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	return f.Sync()
}

// checkRestartBound refuses when snapshotMaxRecent snapshots from v<from>
// to v<to> have a name time in (now-snapshotWindow, now] (ADR 0005 §3): a
// migration failing under a restart policy cannot fill the volume. Only
// regular files with a final name count; temp files and names that do
// not parse are ignored.
func checkRestartBound(dir string, from, to int, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("store: read snapshot dir: %w", err)
	}
	prefix := fmt.Sprintf("%sv%d-to-v%d-", snapshotPrefix, from, to)
	recent := 0
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, snapshotSuffix) {
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(name, prefix), snapshotSuffix)
		at, err := time.Parse(snapshotTimeLayout, stamp)
		if err != nil {
			continue
		}
		if at.After(now.Add(-snapshotWindow)) && !at.After(now) {
			recent++
		}
	}
	if recent >= snapshotMaxRecent {
		return fmt.Errorf("store: migration from v%d to v%d already failed %d times in the last hour; "+
			"not writing another snapshot", from, to, recent)
	}
	return nil
}
