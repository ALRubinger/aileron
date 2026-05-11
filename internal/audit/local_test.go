package audit_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/internal/audit"
)

func TestAppendMessageEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	err := audit.AppendMessageEntry(path, audit.MessageEntry{
		SessionID: "s1",
		Event:     "message_received",
		Service:   "slack",
		Channel:   "#backend",
		Author:    "Sarah",
		Body:      "Does the auth change JWT claims?",
	})
	if err != nil {
		t.Fatal(err)
	}

	err = audit.AppendMessageEntry(path, audit.MessageEntry{
		SessionID: "s1",
		Event:     "message_sent",
		Service:   "slack",
		Channel:   "#backend",
		Body:      "No, the claims are unchanged.",
	})
	if err != nil {
		t.Fatal(err)
	}

	entries, err := audit.ReadMessageEntries(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 message entries, got %d", len(entries))
	}
	if entries[0].Event != "message_received" {
		t.Errorf("expected message_received, got %q", entries[0].Event)
	}
	if entries[0].Author != "Sarah" {
		t.Errorf("expected author Sarah, got %q", entries[0].Author)
	}
	if entries[1].Event != "message_sent" {
		t.Errorf("expected message_sent, got %q", entries[1].Event)
	}
}

func TestReadMessageEntries_SkipsEntriesWithoutEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	// Mix a message entry with a JSON object lacking the "event" field
	// (e.g. a daemon EventStore record from ADR-0010). The reader filters
	// to entries whose Event field is non-empty.
	if err := os.WriteFile(path, []byte(
		`{"event":"message_received","service":"slack"}`+"\n"+
			`{"timestamp":"2026-05-11T10:00:00Z","other":"thing"}`+"\n"+
			`{"event":"message_sent","service":"slack"}`+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := audit.ReadMessageEntries(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries with event set, got %d", len(entries))
	}
}

func TestReadMessageEntries_EmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := audit.ReadMessageEntries(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestReadMessageEntries_NoFile(t *testing.T) {
	_, err := audit.ReadMessageEntries("/nonexistent/audit.jsonl")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestAppendMessageEntry_InReplyTo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	if err := audit.AppendMessageEntry(path, audit.MessageEntry{
		SessionID: "s1",
		Event:     "reply_sent",
		Service:   "slack",
		Channel:   "#backend",
		Body:      "my reply",
		InReplyTo: "msg-123",
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := audit.ReadMessageEntries(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].InReplyTo != "msg-123" {
		t.Errorf("expected InReplyTo=msg-123, got %q", entries[0].InReplyTo)
	}
}
