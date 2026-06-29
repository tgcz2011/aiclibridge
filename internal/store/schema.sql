-- aiclibridge SQLite schema. v0.1.0.
--
-- runs: one row per /v1/runs invocation. cli_session_id is the
-- adapter's session id (e.g. claude session_id, codex thread_id)
-- used for resume — populated when the adapter emits it, not
-- always present (e.g. some failures never reach session_id).
CREATE TABLE IF NOT EXISTS runs (
    id              TEXT PRIMARY KEY,
    adapter         TEXT NOT NULL,
    model           TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'pending',
    started_at      INTEGER NOT NULL,
    finished_at     INTEGER,
    cwd             TEXT NOT NULL DEFAULT '',
    cli_session_id  TEXT NOT NULL DEFAULT '',
    error           TEXT NOT NULL DEFAULT ''
);

-- events: per-run timeline. payload_json is the SSE event JSON
-- (the data line) so ListEvents can be piped straight back to
-- an EventSource client for replay. PRIMARY KEY (run_id, seq)
-- guarantees order and idempotency on retry.
CREATE TABLE IF NOT EXISTS events (
    run_id        TEXT NOT NULL,
    seq           INTEGER NOT NULL,
    type          TEXT NOT NULL,
    payload_json  TEXT NOT NULL,
    PRIMARY KEY(run_id, seq)
);

-- sessions: maps an aiclibridge session id (stable across
-- resume/pause) to the adapter's CLI session id. Used by
-- POST /v1/sessions/{id}/resume in todo 10.
CREATE TABLE IF NOT EXISTS sessions (
    id              TEXT PRIMARY KEY,
    adapter         TEXT NOT NULL,
    cli_session_id  TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_run ON events(run_id, seq);
