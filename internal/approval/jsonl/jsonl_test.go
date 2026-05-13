package jsonl_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/approval"
	"github.com/ALRubinger/aileron/internal/approval/jsonl"
)

// TestNew_MissingFileFreshStart asserts that opening a non-existent
// file returns an empty store, no records, and a usable file
// descriptor. The daemon's first launch hits this path.
func TestNew_MissingFileFreshStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approvals.jsonl")
	s, records, err := jsonl.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if len(records) != 0 {
		t.Errorf("records = %d, want 0", len(records))
	}
}

// TestAppend_PersistsAcrossReopen is the load-bearing crash-survival
// test: write transitions, close the store, reopen, and verify the
// last-written status for each id is what comes back.
func TestAppend_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approvals.jsonl")
	s, _, err := jsonl.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	rec1 := approval.PersistedRecord{
		ID:          "act-1",
		Kind:        approval.ApprovalKindAction,
		ActionName:  "send-email",
		Args:        map[string]any{"to": "alice"},
		RequestedAt: now,
		Status:      approval.OutcomePendingApproval,
	}
	if err := s.Append(rec1); err != nil {
		t.Fatalf("Append pending: %v", err)
	}
	decidedAt := now.Add(30 * time.Second)
	rec1Approved := rec1
	rec1Approved.Status = approval.OutcomeApprovedNotStarted
	rec1Approved.DecidedAt = &decidedAt
	if err := s.Append(rec1Approved); err != nil {
		t.Fatalf("Append approved: %v", err)
	}
	rec1Running := rec1Approved
	rec1Running.Status = approval.OutcomeRunning
	if err := s.Append(rec1Running); err != nil {
		t.Fatalf("Append running: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen — the latest record per id wins.
	s2, records, err := jsonl.New(path)
	if err != nil {
		t.Fatalf("reopen New: %v", err)
	}
	defer s2.Close()
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	got := records[0]
	if got.ID != "act-1" {
		t.Errorf("id = %q, want act-1", got.ID)
	}
	if got.Status != approval.OutcomeRunning {
		t.Errorf("status = %q, want %q", got.Status, approval.OutcomeRunning)
	}
	if got.DecidedAt == nil || !got.DecidedAt.Equal(decidedAt) {
		t.Errorf("decided_at = %v, want %v", got.DecidedAt, decidedAt)
	}
	if got.Args["to"] != "alice" {
		t.Errorf("args[to] = %v, want alice", got.Args["to"])
	}
}

// TestNew_MalformedLineSkipped asserts that a half-written line
// (the daemon was killed in the middle of a write) doesn't fail the
// replay — the line is dropped and the file is otherwise loadable.
func TestNew_MalformedLineSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approvals.jsonl")
	// Write one good line then a truncated one.
	good := `{"id":"act-1","action_name":"send-email","status":"pending_approval","requested_at":"2026-05-12T10:00:00Z"}`
	bad := `{"id":"act-2","action_name":"so` // truncated
	if err := os.WriteFile(path, []byte(good+"\n"+bad+"\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	s, records, err := jsonl.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1 (malformed line should be skipped)", len(records))
	}
	if records[0].ID != "act-1" {
		t.Errorf("id = %q, want act-1", records[0].ID)
	}
}

// TestClose_CompactsFile asserts that Close rewrites the file to one
// record per ID, dropping intermediate records. Without compaction
// the file would grow unbounded across a daemon's lifetime.
func TestClose_CompactsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approvals.jsonl")
	s, _, err := jsonl.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := approval.PersistedRecord{
		ID:          "act-1",
		ActionName:  "send-email",
		RequestedAt: time.Now().UTC(),
		Status:      approval.OutcomePendingApproval,
	}
	for i := 0; i < 5; i++ {
		if err := s.Append(rec); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Count lines.
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines != 1 {
		t.Errorf("post-compact line count = %d, want 1", lines)
	}
}

// TestAppend_AfterCloseFails ensures the closed-after invariant.
func TestAppend_AfterCloseFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approvals.jsonl")
	s, _, err := jsonl.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err = s.Append(approval.PersistedRecord{ID: "act-1", Status: approval.OutcomePendingApproval})
	if err == nil {
		t.Error("Append after Close should error")
	}
}
