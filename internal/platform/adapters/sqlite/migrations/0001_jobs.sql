-- Jobs of the in-process scheduler (ADR 0001 §6, ADR 0006).
-- attempts counts attempts started (incremented at claim); started_at is
-- the latest attempt's start; finished_at is set on succeeded, failed and
-- cancelled. jobs_due serves the due-kinds read, the claim, the waiting
-- count, the startup requeue and the orphan sweep.
CREATE TABLE jobs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    kind         TEXT    NOT NULL CHECK (length(kind) BETWEEN 1 AND 64),
    payload      BLOB    NOT NULL CHECK (length(payload) <= 65536),
    state        TEXT    NOT NULL CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    attempts     INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    run_after_at INTEGER NOT NULL,
    period_at    INTEGER,
    unique_key   TEXT    CHECK (unique_key IS NULL OR length(unique_key) BETWEEN 1 AND 128),
    last_error   TEXT,
    created_at   INTEGER NOT NULL,
    started_at   INTEGER,
    finished_at  INTEGER,
    updated_at   INTEGER NOT NULL
) STRICT;
CREATE INDEX jobs_due ON jobs (state, kind, run_after_at, id);
CREATE UNIQUE INDEX jobs_period ON jobs (kind, period_at) WHERE period_at IS NOT NULL;
CREATE UNIQUE INDEX jobs_active_key ON jobs (kind, unique_key)
    WHERE unique_key IS NOT NULL AND state IN ('queued', 'running');
