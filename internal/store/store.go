// Package store is part of aiclibridge.
//
// It hosts persistence: session history, run records, and the small
// SQLite-backed store. The schema and migrations land in
// internal/store/schema.sql and are applied on Open via //go:embed.
//
// The store is pure-Go (modernc.org/sqlite, no CGO) so the daemon
// stays a single static binary across macOS/Linux/Windows.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, registers as "sqlite"
)

//go:embed schema.sql
var schemaSQL string

// Run is one /v1/runs invocation.
type Run struct {
	ID           string
	Adapter      string
	Model        string
	Status       string
	StartedAt    int64 // unix seconds
	FinishedAt   int64 // unix seconds; 0 until FinishRun
	Cwd          string
	CLISessionID string
	Error        string
}

// EventRow is one frame in a run's timeline. Payload is the SSE
// data-line JSON (the event body, NOT including the `event:` header).
type EventRow struct {
	Seq     int
	Type    string
	Payload []byte
}

// SessionRow maps an aiclibridge session id to an adapter's CLI session
// id. Resume (POST /v1/sessions/{id}/resume in todo 10) uses this.
type SessionRow struct {
	ID           string
	Adapter      string
	CLISessionID string
	CreatedAt    int64
}

// Store is the SQLite-backed persistence handle. Safe for concurrent
// use; database/sql + modernc.org/sqlite serialize writes at the
// connection level and we use a single *sql.DB (fine for the v1
// concurrency cap of 4 runs).
type Store struct {
	db *sql.DB
}

// Open returns a Store backed by the given DSN. Use ":memory:" for
// tests or a file path for the daemon. The schema is applied
// idempotently on every Open.
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", dsn, err)
	}
	// modernc.org/sqlite is single-writer; cap conns to 1 writer +
	// a few readers to avoid spurious SQLITE_BUSY under the v1
	// 4-concurrent-runs cap.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(context.Background(), schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the underlying connection pool.
func (s *Store) Close() error { return s.db.Close() }

// CreateRun inserts a new run row in `pending` status with the
// current unix timestamp. Adapter and cwd are required; model may
// be empty (the adapter discovers it for some CLIs).
func (s *Store) CreateRun(ctx context.Context, id, adapter, model, cwd string) error {
	const q = `INSERT INTO runs (id, adapter, model, status, started_at, cwd)
	           VALUES (?, ?, ?, 'pending', ?, ?)`
	_, err := s.db.ExecContext(ctx, q, id, adapter, model, time.Now().Unix(), cwd)
	if err != nil {
		return fmt.Errorf("create run %q: %w", id, err)
	}
	return nil
}

// AppendEvent records one event in a run's timeline. seq must be
// unique per run; the (run_id, seq) primary key enforces that.
func (s *Store) AppendEvent(ctx context.Context, runID string, seq int, eventType string, payload []byte) error {
	const q = `INSERT INTO events (run_id, seq, type, payload_json) VALUES (?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, q, runID, seq, eventType, string(payload))
	if err != nil {
		return fmt.Errorf("append event run=%q seq=%d: %w", runID, seq, err)
	}
	return nil
}

// FinishRun marks a run complete with the given terminal status
// (completed/failed/cancelled/timeout), the adapter's CLI session id
// (may be empty), and an error message (empty on success).
func (s *Store) FinishRun(ctx context.Context, runID, status, cliSessionID, errMsg string) error {
	const q = `UPDATE runs
	           SET status = ?, cli_session_id = ?, error = ?, finished_at = ?
	           WHERE id = ?`
	res, err := s.db.ExecContext(ctx, q, status, cliSessionID, errMsg, time.Now().Unix(), runID)
	if err != nil {
		return fmt.Errorf("finish run %q: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("finish run %q rows affected: %w", runID, err)
	}
	if n == 0 {
		return fmt.Errorf("finish run %q: not found", runID)
	}
	return nil
}

// GetRun returns the run row for id, or an error if not found.
func (s *Store) GetRun(ctx context.Context, runID string) (*Run, error) {
	const q = `SELECT id, adapter, model, status, started_at,
	                  COALESCE(finished_at, 0), cwd, cli_session_id, error
	           FROM runs WHERE id = ?`
	var r Run
	err := s.db.QueryRowContext(ctx, q, runID).Scan(
		&r.ID, &r.Adapter, &r.Model, &r.Status, &r.StartedAt,
		&r.FinishedAt, &r.Cwd, &r.CLISessionID, &r.Error,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("run %q: not found", runID)
	}
	if err != nil {
		return nil, fmt.Errorf("get run %q: %w", runID, err)
	}
	return &r, nil
}

// ListEvents returns all events for a run in seq order. Used by
// GET /v1/runs/{id} (todo 10) to replay the SSE stream verbatim.
func (s *Store) ListEvents(ctx context.Context, runID string) ([]EventRow, error) {
	const q = `SELECT seq, type, payload_json FROM events WHERE run_id = ? ORDER BY seq ASC`
	rows, err := s.db.QueryContext(ctx, q, runID)
	if err != nil {
		return nil, fmt.Errorf("list events run=%q: %w", runID, err)
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var er EventRow
		var payload string
		if err := rows.Scan(&er.Seq, &er.Type, &payload); err != nil {
			return nil, fmt.Errorf("scan event row: %w", err)
		}
		er.Payload = []byte(payload)
		out = append(out, er)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate event rows: %w", err)
	}
	return out, nil
}

// SaveSession records the mapping from an aiclibridge session id
// (stable across resume) to the adapter's CLI session id.
func (s *Store) SaveSession(ctx context.Context, id, adapter, cliSessionID string) error {
	const q = `INSERT INTO sessions (id, adapter, cli_session_id, created_at)
	           VALUES (?, ?, ?, ?)
	           ON CONFLICT(id) DO UPDATE SET
	             adapter = excluded.adapter,
	             cli_session_id = excluded.cli_session_id`
	_, err := s.db.ExecContext(ctx, q, id, adapter, cliSessionID, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("save session %q: %w", id, err)
	}
	return nil
}

// GetSession returns the session row for id, or an error if not found.
func (s *Store) GetSession(ctx context.Context, id string) (*SessionRow, error) {
	const q = `SELECT id, adapter, cli_session_id, created_at FROM sessions WHERE id = ?`
	var s2 SessionRow
	err := s.db.QueryRowContext(ctx, q, id).Scan(
		&s2.ID, &s2.Adapter, &s2.CLISessionID, &s2.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("session %q: not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("get session %q: %w", id, err)
	}
	return &s2, nil
}
