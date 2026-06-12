-- schema version 1

CREATE TABLE IF NOT EXISTS pr_progress (
    pr_number      INTEGER PRIMARY KEY,
    package_name   TEXT    NOT NULL DEFAULT '',
    bump_type      TEXT    NOT NULL DEFAULT '',
    stage          TEXT    NOT NULL DEFAULT '',
    session_id     TEXT    NOT NULL DEFAULT '',
    worktree_path  TEXT    NOT NULL DEFAULT '',
    impl_branch    TEXT    NOT NULL DEFAULT '',
    replacement_pr INTEGER,                       -- nullable
    last_updated   INTEGER NOT NULL DEFAULT 0,    -- unix nanoseconds, UTC

    -- version metadata (v2)
    old_version    TEXT    NOT NULL DEFAULT '',
    new_version    TEXT    NOT NULL DEFAULT '',
    ecosystem      TEXT    NOT NULL DEFAULT '',
    budget_spent   REAL    NOT NULL DEFAULT 0,

    -- CI aggregate counts (v2; checks detail in ci_checks table)
    ci_state       TEXT    NOT NULL DEFAULT '',
    ci_total       INTEGER NOT NULL DEFAULT 0,
    ci_passed      INTEGER NOT NULL DEFAULT 0,
    ci_failed      INTEGER NOT NULL DEFAULT 0,
    ci_pending     INTEGER NOT NULL DEFAULT 0,

    -- analyser verdict as JSON blob (v2)
    analysis_json  TEXT    NOT NULL DEFAULT '',

    -- terminal outcome idempotency (v3): once set, the next scan at the same
    -- head_sha skips re-processing via a DB lookup instead of reading back a
    -- PR comment (Bug #23).
    head_sha       TEXT    NOT NULL DEFAULT '',
    outcome        TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS stage_events (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    pr_number INTEGER NOT NULL REFERENCES pr_progress(pr_number) ON DELETE CASCADE,
    stage     TEXT    NOT NULL,
    at        INTEGER NOT NULL,                    -- unix nanoseconds, UTC
    detail    TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_stage_events_pr ON stage_events(pr_number, id);

-- Per-check CI details (v2). Replaced wholesale each SetCI call (DELETE+INSERT).
CREATE TABLE IF NOT EXISTS ci_checks (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    pr_number   INTEGER NOT NULL REFERENCES pr_progress(pr_number) ON DELETE CASCADE,
    name        TEXT    NOT NULL DEFAULT '',
    status      TEXT    NOT NULL DEFAULT '',
    conclusion  TEXT,                              -- nullable (*string in Go)
    details_url TEXT    NOT NULL DEFAULT '',
    output      TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL DEFAULT 0         -- unix nanoseconds, UTC
);
CREATE INDEX IF NOT EXISTS idx_ci_checks_pr ON ci_checks(pr_number, id);

-- Single-row status written by the worker process and read by the web process.
CREATE TABLE IF NOT EXISTS scan_status (
    id        INTEGER PRIMARY KEY CHECK (id = 1),  -- enforces single row
    last_scan INTEGER NOT NULL DEFAULT 0,          -- unix nanoseconds
    next_scan INTEGER NOT NULL DEFAULT 0,
    in_flight INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO scan_status (id, last_scan, next_scan, in_flight) VALUES (1, 0, 0, 0);
