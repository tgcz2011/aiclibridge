package store

import (
	"bytes"
	"context"
	"testing"
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
