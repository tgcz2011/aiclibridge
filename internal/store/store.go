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
	"encoding/json"
	_ "embed"
	"fmt"
	"runtime"
	"sort"
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
	// UsageJSON is the terminal result event's usage map serialised as
	// JSON ({"model_name":{"input_tokens":N,...}}). Empty until the run
	// finishes (FinishRunWithUsage). Callers parse it; the store does not
	// depend on the protocol package.
	UsageJSON string
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
// connection level. v0.3 enables WAL + busy_timeout + a small
// connection pool so reads (history replay, stats) do not block on
// writes (event persistence) under the no-cap concurrency model.
type Store struct {
	db *sql.DB
}

// Open returns a Store backed by the given DSN. Use ":memory:" for
// tests or a file path for the daemon. The schema is applied
// idempotently on every Open.
//
// Concurrency tuning (v0.3): WAL journal mode lets readers proceed
// concurrently with the single writer; busy_timeout=5s absorbs
// transient lock contention; synchronous=NORMAL trades a small
// durability window for ~2-5x write throughput, acceptable because
// the store is a persistence helper, not the source of truth (the
// adapter.Session is). MaxOpenConns is capped at max(4, NumCPU) so a
// burst of concurrent runs does not serialize on a single connection;
// the single-writer invariant is still honoured by SQLite itself.
// ":memory:" databases force MaxOpenConns=1 because modernc.org/sqlite
// gives each connection its own private in-memory database.
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", dsn, err)
	}

	// In-memory databases are per-connection in modernc.org/sqlite, so
	// a pool > 1 would silently give each connection a separate DB.
	// Keep the v0.1 single-connection behaviour for the ":memory:"
	// case (used by `run`/`agents`/`models` and tests).
	if dsn == ":memory:" {
		db.SetMaxOpenConns(1)
	} else {
		// WAL + busy_timeout only apply to file-backed DBs.
		pragmas := []string{
			"PRAGMA journal_mode=WAL",
			"PRAGMA busy_timeout=5000",
			"PRAGMA synchronous=NORMAL",
			"PRAGMA foreign_keys=ON",
		}
		for _, p := range pragmas {
			if _, err := db.ExecContext(context.Background(), p); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("apply pragma %q: %w", p, err)
			}
		}
		conns := runtime.NumCPU()
		if conns < 4 {
			conns = 4
		}
		db.SetMaxOpenConns(conns)
		db.SetMaxIdleConns(conns)
		db.SetConnMaxLifetime(0) // reuse forever; modernc handles staleness
	}

	if _, err := db.ExecContext(context.Background(), schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	// v0.3 migration: add usage_json column to runs if missing. CREATE
	// TABLE IF NOT EXISTS skips existing tables, so a pre-v0.3 DB does not
	// pick up the new column from the schema above. ALTER TABLE ADD COLUMN
	// has no IF NOT EXISTS form in SQLite, so guard with a pragma check.
	var colCount int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name='usage_json'").Scan(&colCount); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("check usage_json column: %w", err)
	}
	if colCount == 0 {
		if _, err := db.ExecContext(context.Background(),
			"ALTER TABLE runs ADD COLUMN usage_json TEXT NOT NULL DEFAULT ''"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("add usage_json column: %w", err)
		}
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

// FinishRunWithUsage is the v0.3 finish variant: it also persists the
// terminal result event's usage map (serialised as JSON) so the stats
// aggregation can price the run without re-reading the event timeline.
// usageJSON may be empty (e.g. a failed run with no usage); the column
// keeps its DEFAULT '' in that case. FinishRun is retained for callers
// that do not have usage yet; the facade now calls this variant.
func (s *Store) FinishRunWithUsage(ctx context.Context, runID, status, cliSessionID, errMsg, usageJSON string) error {
	const q = `UPDATE runs
	           SET status = ?, cli_session_id = ?, error = ?, finished_at = ?, usage_json = ?
	           WHERE id = ?`
	res, err := s.db.ExecContext(ctx, q, status, cliSessionID, errMsg, time.Now().Unix(), usageJSON, runID)
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
	                  COALESCE(finished_at, 0), cwd, cli_session_id, error, usage_json
	           FROM runs WHERE id = ?`
	var r Run
	err := s.db.QueryRowContext(ctx, q, runID).Scan(
		&r.ID, &r.Adapter, &r.Model, &r.Status, &r.StartedAt,
		&r.FinishedAt, &r.Cwd, &r.CLISessionID, &r.Error, &r.UsageJSON,
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

// UsageStatRow is one (adapter, model, status) bucket in the usage
// aggregation. Tokens are summed across every run in the bucket by
// parsing each run's usage_json. The store does not price rows — that
// is the api layer's job (it needs the catalog → provider mapping and
// the pricing table, neither of which belongs in the store).
type UsageStatRow struct {
	Adapter          string
	Model            string
	Status           string
	RunCount         int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// GetUsageStats aggregates token usage across runs whose started_at falls
// in [since, until] (unix seconds). Rows are grouped by (adapter, model,
// status) where model is the runs.model column (the bare model name the
// run was started with). Each run's usage_json is parsed in Go and its
// token counts are summed into the bucket — modernc.org/sqlite's JSON
// functions are uneven, so aggregation stays in Go for portability. A
// malformed usage_json is skipped (logged by the caller if desired)
// rather than aborting the whole aggregation. Rows are returned sorted
// by (adapter, model, status) for stable output.
func (s *Store) GetUsageStats(ctx context.Context, since, until int64) ([]UsageStatRow, error) {
	const q = `SELECT adapter, model, status, usage_json FROM runs
	           WHERE started_at >= ? AND started_at <= ?`
	rows, err := s.db.QueryContext(ctx, q, since, until)
	if err != nil {
		return nil, fmt.Errorf("query usage stats: %w", err)
	}
	defer rows.Close()

	type key struct{ adapter, model, status string }
	type agg struct {
		row              UsageStatRow
		input            int64
		output           int64
		cacheRead        int64
		cacheWrite       int64
	}
	buckets := make(map[key]*agg)
	for rows.Next() {
		var adapter, model, status, usageJSON string
		if err := rows.Scan(&adapter, &model, &status, &usageJSON); err != nil {
			return nil, fmt.Errorf("scan usage stats row: %w", err)
		}
		k := key{adapter, model, status}
		b := buckets[k]
		if b == nil {
			b = &agg{row: UsageStatRow{Adapter: adapter, Model: model, Status: status}}
			buckets[k] = b
		}
		b.row.RunCount++
		if usageJSON == "" {
			continue
		}
		// usage_json shape: {"model_name":{"input_tokens":N,...}}.
		// Aggregate every model_name's tokens into this bucket.
		var usage map[string]struct {
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
			CacheReadTokens  int `json:"cache_read_tokens"`
			CacheWriteTokens int `json:"cache_write_tokens"`
		}
		if err := json.Unmarshal([]byte(usageJSON), &usage); err != nil {
			// Skip malformed usage; a single bad row must not break stats.
			continue
		}
		for _, u := range usage {
			b.input += int64(u.InputTokens)
			b.output += int64(u.OutputTokens)
			b.cacheRead += int64(u.CacheReadTokens)
			b.cacheWrite += int64(u.CacheWriteTokens)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage stats rows: %w", err)
	}

	out := make([]UsageStatRow, 0, len(buckets))
	for _, b := range buckets {
		b.row.InputTokens = b.input
		b.row.OutputTokens = b.output
		b.row.CacheReadTokens = b.cacheRead
		b.row.CacheWriteTokens = b.cacheWrite
		out = append(out, b.row)
	}
	// Stable order for predictable output / test assertions.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Adapter != out[j].Adapter {
			return out[i].Adapter < out[j].Adapter
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Status < out[j].Status
	})
	return out, nil
}
