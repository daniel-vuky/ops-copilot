# 5. Set conventions for the SQLite store

Date: 2026-11-28

## Status

Accepted

Amends [0001](0001-build-ops-copilot-as-one-go-binary-on-sqlite.md) §3, §4 and §15, and clarifies 0001 §2.

## Context

Building the SQLite store ([SU-05 #11](https://github.com/daniel-vuky/ops-copilot/issues/11)) raised four points that 0001 leaves open. Every later ticket that stores data depends on them, and changing one later means a data migration or a rewrite of several domains:

1. 0001 §2 says times are stored in UTC, but not in what form. The driver `modernc.org/sqlite` writes a bound `time.Time` as Go's `String()` text by default, which neither sorts nor compares. The job scheduler ([SU-06 #12](https://github.com/daniel-vuky/ops-copilot/issues/12)) and retention ([SU-12 #43](https://github.com/daniel-vuky/ops-copilot/issues/43)) compare times in SQL.
2. 0001 §3 requires that every write transaction starts with `BEGIN IMMEDIATE`, and 0001 §1 keeps SQL in adapters, so SU-05 puts write transactions behind a port. Nothing says what that port looks like. [0004](0004-settle-open-points-in-adrs-0001-0003.md) §5 needs findings of one domain and a notification of another stored in the same write transaction.
3. 0001 §3 and §15 say the binary writes a pre-migration snapshot and the updater restores "the pre-migration snapshot" on a failed update. They do not name the file, say which one to restore, or say how. After a partial run (migration 1 applied, migration 2 failed) and a restart, a second snapshot is written at the newer schema, which the previous image refuses. Restoring that one would break the rollback.
4. 0001 §4 keeps daily backups 14 days and §5 sets retention for tokens (30 days) and investigations (90 days), but nothing deletes pre-migration snapshots. They are full copies of the database, so they would keep token hashes, stored webhook payloads and investigation transcripts forever.

### Option A: Settle each point in the code of SU-05

- Pro: no new document.
- Con: later tickets would follow conventions no ADR records, and the snapshot retention is a retention decision, which 0001 §5 and 0004 §7 already treat as an ADR matter.

### Option B: One ADR for the four points (chosen)

- Pro: later tickets have one place to read; the updater (SU-11) and backups (SU-10) build on a written contract.
- Con: one more ADR.

Daniel chose option B and approved the four decisions below.

## Decision

**Layout.** The data volume is the directory in the env variable `DATA_DIR` (default `/data`). The database is `<DATA_DIR>/ops-copilot.db`, and pre-migration snapshots live in `<DATA_DIR>/snapshots/`, created with mode `0700`. The database file and every snapshot have mode `0600`.

1. **Times are stored as INTEGER Unix milliseconds in UTC (clarifies 0001 §2, amends 0001 §3).**
   - Every time column is an `INTEGER` holding milliseconds since the Unix epoch, named `<something>_at`. A duration is an `INTEGER` of milliseconds named `<something>_ms`.
   - An absent time is `NULL`, never the zero `time.Time` (which would store as a large negative number and read as set).
   - Ordinary tables are declared `STRICT`, so a time that arrives as text is refused instead of stored.
   - "Now" always comes from the clock port as a bound parameter. Migrations use no time defaults (`CURRENT_TIMESTAMP`, `datetime('now')`), which would store text.
   - Both connection pools are opened with the driver option `_time_integer_format=unix_milli`, so a bound `time.Time` is stored in that form, and the store checks this at start. Adapters read the integer and convert it with `time.UnixMilli(v).UTC()`.
   - Times are still shown in `DISPLAY_TZ` (0001 §2); only storage is fixed here.
2. **Domain code writes through one transaction port carried in the context (amends 0001 §3).**
   - The port is `Write(ctx, fn func(ctx context.Context) error) error`. It starts one `BEGIN IMMEDIATE` transaction on the writer connection and passes `fn` a context that carries it. Repository adapters of every domain take the transaction from that context, so repositories of two domains called inside one `Write` share one transaction (0004 §5). Reads made with `fn`'s context run on the transaction and see its uncommitted rows.
   - It commits when `fn` returns nil and rolls back otherwise. It also rolls back if `fn` panics (the panic is not swallowed) or exits its goroutine.
   - A `Write` called inside a `Write` joins the outer transaction. If the inner one fails, the transaction stays usable until the outer `fn` returns and then rolls back: the outer `Write` returns `ErrRolledBack` even when its own `fn` returns nil.
   - A repository that writes outside a `Write`, or with a context whose transaction already ended, gets an error and writes nothing. Reads outside a `Write` use the four reader connections.
   - Waiting for the writer connection is bounded by 30 s, or by the caller's deadline if that is sooner. A `Write` that cannot get the connection in time fails with `ErrWriterBusy`. This is the in-process queue for the one writer connection; the 5 s `busy_timeout` of 0001 §3 covers SQLite's file lock and is separate.
   - `fn` does only database work: no network calls, no extraction, no waiting on other jobs (0001 Consequences). Other long work, such as the daily backup's `VACUUM INTO`, never runs inside a `Write`; the backup ticket decides which connection runs it, since the readers are read-only.
3. **Pre-migration snapshots have a fixed name and a fixed restore rule (amends 0001 §3 and §15).**
   - The file is `snapshots/pre-migration-v<from>-to-v<to>-<UTC time as 20060102T150405Z>.db`. `<from>` is the schema version before migrating and `<to>` the highest version the binary knows, both unpadded decimals.
   - It is first written to the same name plus `.tmp`, a file created empty with mode `0600` before `VACUUM INTO` writes into it. It is renamed when complete, so a file under a final name is always a whole snapshot. At start the binary removes leftover `.tmp` files in `snapshots/`.
   - The migration runner never deletes or overwrites a snapshot; only the backup job deletes them (decision 4).
   - If at least 3 snapshots with the same `<from>` and `<to>` were written in the last hour, the binary refuses to start without writing another. A failing migration under a restart policy therefore cannot fill the volume.
   - **Which one to restore.** On a failed update the updater takes, among the snapshots whose name time is at or after the UTC second at which it started the new image, the one with the numerically lowest `<from>`, then the earliest name time. That one holds the schema of the previous image; later ones may hold a partly migrated schema. If there is none, the failed image applied no migration and nothing is restored.
   - **How to restore.** With the container stopped, remove the database's `-wal` and `-shm` files, copy (not move) the snapshot to a temporary name in `DATA_DIR`, and rename it over the database, keeping its owner and mode `0600`.
4. **Pre-migration snapshots are kept 14 days (amends 0001 §4).** The daily backup job deletes pre-migration snapshots whose name time is older than the backup period of 0001 §4 (14 days), as it does daily backups. So a snapshot does not undo the retention of 0001 §5 for longer than backups already do.

## Consequences

**Easier:**

- Times compare and sort in SQL, so the scheduler's "due now" and every retention query are plain integer comparisons on an index.
- A domain can store its rows and queue another domain's work in one transaction without either importing the other's adapter.
- A crashed or repeated start cannot leave a half-written snapshot under a final name, and the updater knows which snapshot to restore and how.
- No copy of the database outlives the backup retention.

**Harder:**

- Times are not readable in the `sqlite3` shell without `datetime(x / 1000, 'unixepoch')`. Stored times lose sub-millisecond precision, and two rows in the same millisecond tie, so lists order by `(x_at, id)`.
- The transaction is implicit in the context. Code that drops the context inside `fn` (for example by using `context.Background()`) does not join the transaction:
  - a repository write fails at once with the no-transaction error;
  - a nested `Write` waits for the writer connection and fails with `ErrWriterBusy`;
  - a read goes to a reader connection and does not see the transaction's own rows.
- A rollback done by hand more than 14 days after an update finds no pre-migration snapshot; daily backups from that period are gone too.
- Restoring a snapshot brings back anything the failed image changed, including sessions, tokens or keys it revoked. After a rollback, revocations made in that window must be checked again.
- After 3 failed starts in an hour, the binary refuses to start even if the cause was fixed outside it (for example, disk space). Removing the newest of those snapshots, or waiting the hour, lets it start again.
- The same holds for a corrected image with the same `<from>` and `<to>` shipped within that hour: it is refused before it tries, fails `/healthz`, and the updater stops until the next image (0001 §15). The updater ([SU-11 #26](https://github.com/daniel-vuky/ops-copilot/issues/26)) decides how a rollback clears the bound.
- Pre-migration snapshots are deleted only by the backup job ([SU-10 #25](https://github.com/daniel-vuky/ops-copilot/issues/25)), which must land before the first deployment that keeps real data.
