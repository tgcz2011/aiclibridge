package store

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestReplay proves the store can persist a mixed-type event stream for a
// run and replay it byte-identical — the same guarantee pkg/protocol/sse.go
// gives for the wire, the store gives for history. GET /v1/runs/{id} (todo
// 10) re-uses ListEvents to reconstruct the SSE stream on resume/replay.
func TestReplay(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	if err := s.CreateRun(ctx, "r1", "claude", "claude-sonnet-4", "/tmp/work"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Five mixed events matching the canonical run shape: assistant text,
	// thinking, tool_use, tool_result, terminal result. Each payload is
	// the wire form (one JSON object per line / per event).
	events := []struct {
		seq     int
		evType  string
		payload []byte
	}{
		{0, "text", []byte(`{"type":"text","seq":0,"content":"Reading the file..."}`)},
		{1, "thinking", []byte(`{"type":"thinking","seq":1,"content":"Need to check if it exists first."}`)},
		{2, "tool_use", []byte(`{"type":"tool_use","seq":2,"tool":"Bash","call_id":"call_1","input":{"cmd":"ls -la"}}`)},
		{3, "tool_result", []byte(`{"type":"tool_result","seq":3,"tool":"Bash","call_id":"call_1","output":"total 4\n"}`)},
		{4, "result", []byte(`{"type":"result","seq":4,"result":{"status":"completed","duration_ms":4321,"usage":{"claude-sonnet-4":{"input_tokens":100,"output_tokens":50}}}}`)},
	}
	for _, e := range events {
		if err := s.AppendEvent(ctx, "r1", e.seq, e.evType, e.payload); err != nil {
			t.Fatalf("AppendEvent seq=%d: %v", e.seq, err)
		}
	}

	if err := s.FinishRun(ctx, "r1", "completed", "cli-sess-abc", ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	// Replay: ListEvents must return the same rows in the same order with
	// the same byte payloads.
	rows, err := s.ListEvents(ctx, "r1")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(rows) != len(events) {
		t.Fatalf("ListEvents returned %d rows, want %d", len(rows), len(events))
	}
	for i, got := range rows {
		want := events[i]
		if got.Seq != want.seq {
			t.Errorf("row %d Seq: got %d, want %d", i, got.Seq, want.seq)
		}
		if got.Type != want.evType {
			t.Errorf("row %d Type: got %q, want %q", i, got.Type, want.evType)
		}
		if !bytes.Equal(got.Payload, want.payload) {
			t.Errorf("row %d payload differs:\n got  %s\n want %s", i, got.Payload, want.payload)
		}
	}

	// Run row should reflect the finish call.
	run, err := s.GetRun(ctx, "r1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != "completed" {
		t.Errorf("run.Status: got %q, want %q", run.Status, "completed")
	}
	if run.CLISessionID != "cli-sess-abc" {
		t.Errorf("run.CLISessionID: got %q, want %q", run.CLISessionID, "cli-sess-abc")
	}
	if run.Adapter != "claude" {
		t.Errorf("run.Adapter: got %q, want %q", run.Adapter, "claude")
	}
	if run.Model != "claude-sonnet-4" {
		t.Errorf("run.Model: got %q, want %q", run.Model, "claude-sonnet-4")
	}
	if run.Cwd != "/tmp/work" {
		t.Errorf("run.Cwd: got %q, want %q", run.Cwd, "/tmp/work")
	}
	if run.FinishedAt == 0 {
		t.Error("run.FinishedAt should be non-zero after FinishRun")
	}
}

// TestSessionRoundTrip: SaveSession + GetSession should round-trip the
// session row used by POST /v1/sessions/{id}/resume (todo 10) to map an
// aiclibridge session id to a CLI session id.
func TestSessionRoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	if err := s.SaveSession(ctx, "sess-1", "claude", "cli-xyz-789"); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	got, err := s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.ID != "sess-1" {
		t.Errorf("ID: got %q, want %q", got.ID, "sess-1")
	}
	if got.Adapter != "claude" {
		t.Errorf("Adapter: got %q, want %q", got.Adapter, "claude")
	}
	if got.CLISessionID != "cli-xyz-789" {
		t.Errorf("CLISessionID: got %q, want %q", got.CLISessionID, "cli-xyz-789")
	}
	if got.CreatedAt == 0 {
		t.Error("CreatedAt should be non-zero")
	}
}

// TestFinishRunWithUsage verifies the v0.3 usage-persisting finish path:
// FinishRunWithUsage stores usage_json on the run row, and GetRun reads
// it back verbatim. An empty usageJSON (failed run) keeps the column at
// its DEFAULT ''. This is the storage half of the stats pipeline; the
// facade forwards the terminal result event's usage through this method.
func TestFinishRunWithUsage(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	if err := s.CreateRun(ctx, "ru1", "claude", "claude-sonnet-4.5", "/tmp"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	usageJSON := `{"claude-sonnet-4.5":{"input_tokens":100,"output_tokens":50,"cache_read_tokens":10,"cache_write_tokens":5}}`
	if err := s.FinishRunWithUsage(ctx, "ru1", "completed", "sess-1", "", usageJSON); err != nil {
		t.Fatalf("FinishRunWithUsage: %v", err)
	}

	run, err := s.GetRun(ctx, "ru1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.UsageJSON != usageJSON {
		t.Errorf("UsageJSON:\n got  %q\n want %q", run.UsageJSON, usageJSON)
	}
	if run.Status != "completed" {
		t.Errorf("Status: got %q, want completed", run.Status)
	}

	// Empty usage (a failed run) persists as "" and round-trips.
	if err := s.CreateRun(ctx, "ru2", "codex", "gpt-5", "/tmp"); err != nil {
		t.Fatalf("CreateRun ru2: %v", err)
	}
	if err := s.FinishRunWithUsage(ctx, "ru2", "failed", "", "boom", ""); err != nil {
		t.Fatalf("FinishRunWithUsage ru2: %v", err)
	}
	run2, err := s.GetRun(ctx, "ru2")
	if err != nil {
		t.Fatalf("GetRun ru2: %v", err)
	}
	if run2.UsageJSON != "" {
		t.Errorf("empty UsageJSON: got %q, want \"\"", run2.UsageJSON)
	}
	if run2.Error != "boom" {
		t.Errorf("Error: got %q, want boom", run2.Error)
	}
}

// TestGetUsageStats verifies the stats aggregation: runs in the time
// window are grouped by (adapter, model, status) and each run's
// usage_json is parsed and summed into the bucket. A wide window covers
// the just-inserted rows. Two claude/claude-sonnet-4.5/completed runs
// land in one bucket (tokens summed, run_count=2); a codex run lands in
// another. Malformed usage_json is skipped without breaking the query.
func TestGetUsageStats(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()

	// Two completed claude runs using the same model — should aggregate
	// into one bucket with summed tokens and run_count=2.
	if err := s.CreateRun(ctx, "s1", "claude", "claude-sonnet-4.5", "/tmp"); err != nil {
		t.Fatalf("CreateRun s1: %v", err)
	}
	if err := s.FinishRunWithUsage(ctx, "s1", "completed", "", "",
		`{"claude-sonnet-4.5":{"input_tokens":100,"output_tokens":50}}`); err != nil {
		t.Fatalf("FinishRunWithUsage s1: %v", err)
	}
	if err := s.CreateRun(ctx, "s2", "claude", "claude-sonnet-4.5", "/tmp"); err != nil {
		t.Fatalf("CreateRun s2: %v", err)
	}
	if err := s.FinishRunWithUsage(ctx, "s2", "completed", "", "",
		`{"claude-sonnet-4.5":{"input_tokens":200,"output_tokens":150,"cache_read_tokens":20}}`); err != nil {
		t.Fatalf("FinishRunWithUsage s2: %v", err)
	}

	// A codex run in a separate bucket.
	if err := s.CreateRun(ctx, "s3", "codex", "gpt-5", "/tmp"); err != nil {
		t.Fatalf("CreateRun s3: %v", err)
	}
	if err := s.FinishRunWithUsage(ctx, "s3", "completed", "", "",
		`{"gpt-5":{"input_tokens":1000,"output_tokens":500}}`); err != nil {
		t.Fatalf("FinishRunWithUsage s3: %v", err)
	}

	// A run with malformed usage_json — must be skipped, not abort.
	if err := s.CreateRun(ctx, "s4", "claude", "claude-haiku-4.5", "/tmp"); err != nil {
		t.Fatalf("CreateRun s4: %v", err)
	}
	if err := s.FinishRunWithUsage(ctx, "s4", "completed", "", "",
		`{not-json`); err != nil {
		t.Fatalf("FinishRunWithUsage s4: %v", err)
	}

	// Wide window covers all rows (started_at = time.Now()).
	rows, err := s.GetUsageStats(ctx, 0, time.Now().Unix()+60)
	if err != nil {
		t.Fatalf("GetUsageStats: %v", err)
	}

	// Find the claude/claude-sonnet-4.5/completed bucket.
	var claudeBucket *UsageStatRow
	for i := range rows {
		if rows[i].Adapter == "claude" && rows[i].Model == "claude-sonnet-4.5" && rows[i].Status == "completed" {
			claudeBucket = &rows[i]
			break
		}
	}
	if claudeBucket == nil {
		t.Fatalf("claude/sonnet bucket missing from %v", rows)
	}
	if claudeBucket.RunCount != 2 {
		t.Errorf("RunCount: got %d, want 2", claudeBucket.RunCount)
	}
	if claudeBucket.InputTokens != 300 {
		t.Errorf("InputTokens: got %d, want 300", claudeBucket.InputTokens)
	}
	if claudeBucket.OutputTokens != 200 {
		t.Errorf("OutputTokens: got %d, want 200", claudeBucket.OutputTokens)
	}
	if claudeBucket.CacheReadTokens != 20 {
		t.Errorf("CacheReadTokens: got %d, want 20", claudeBucket.CacheReadTokens)
	}

	// The malformed-usage run (s4) still counts as a run but contributes
	// zero tokens.
	var haikuBucket *UsageStatRow
	for i := range rows {
		if rows[i].Adapter == "claude" && rows[i].Model == "claude-haiku-4.5" {
			haikuBucket = &rows[i]
			break
		}
	}
	if haikuBucket == nil {
		t.Fatalf("claude/haiku bucket missing from %v", rows)
	}
	if haikuBucket.RunCount != 1 {
		t.Errorf("haiku RunCount: got %d, want 1", haikuBucket.RunCount)
	}
	if haikuBucket.InputTokens != 0 {
		t.Errorf("haiku InputTokens: got %d, want 0 (malformed usage skipped)", haikuBucket.InputTokens)
	}

	// The codex bucket.
	var codexBucket *UsageStatRow
	for i := range rows {
		if rows[i].Adapter == "codex" {
			codexBucket = &rows[i]
			break
		}
	}
	if codexBucket == nil {
		t.Fatalf("codex bucket missing from %v", rows)
	}
	if codexBucket.RunCount != 1 {
		t.Errorf("codex RunCount: got %d, want 1", codexBucket.RunCount)
	}
	if codexBucket.InputTokens != 1000 {
		t.Errorf("codex InputTokens: got %d, want 1000", codexBucket.InputTokens)
	}

	// A window excluding all rows returns empty.
	empty, err := s.GetUsageStats(ctx, time.Now().Unix()+3600, time.Now().Unix()+7200)
	if err != nil {
		t.Fatalf("GetUsageStats future window: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("future window: got %d rows, want 0", len(empty))
	}
}
