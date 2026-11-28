# Migrations

Numbered SQL files embedded in the binary and applied at start, in order (ADR 0001 §3). They only move forward. Before applying any pending migration, the binary writes a pre-migration snapshot to `<DATA_DIR>/snapshots/` (ADR 0005 §3).

## Files

- One file per migration, named `NNNN_slug.sql` (`^[0-9]{4}_[a-z0-9_]+\.sql$`), for example `0001_jobs.sql`.
- Versions are numbered globally across all domains, start at `0001` and are contiguous. A duplicate version, a gap, or a `.sql` file with a bad name fails start. Files that do not end in `.sql` (like this README) are ignored.
- Never edit a migration once it is merged. Change the schema with a new migration.
- Each migration runs in its own `BEGIN IMMEDIATE` transaction, with foreign keys off and `PRAGMA foreign_key_check` before commit, so a table rebuild is safe. Do not write transaction control (`BEGIN`, `COMMIT`, `END TRANSACTION`, `ROLLBACK`, `SAVEPOINT`, `RELEASE`) in a file; validation rejects it. Start at most one statement per line: a bare `END;` cannot be told apart from the end of a trigger body.

## Tables and times (ADR 0005 §1)

- Ordinary tables are declared `STRICT`. A migration that leaves a non-`STRICT` table fails. Virtual tables (FTS5) are exempt.
- A time column is an `INTEGER` of Unix milliseconds in UTC named `<something>_at`; a duration is an `INTEGER` of milliseconds named `<something>_ms`. An absent time is `NULL`.
- No time defaults: `CURRENT_TIMESTAMP`, `CURRENT_DATE`, `CURRENT_TIME`, `'now'` or `"now"`, the `'subsec'` and `'subsecond'` modifiers, `unixepoch()`, `julianday()`, `datetime()`, `date()`, `time()` and `strftime('...')` without a time argument are rejected. The check is textual, so it also rejects these words in comments and string literals. "Now" comes from the clock port as a bound parameter.
