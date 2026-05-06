package audit_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/audit"
)

// ReadShellEntries / ReadMessageEntries delegate to resolveAuditFiles
// for the dir-vs-file decision. These tests exercise that decision tree
// through the public API.

func TestReadShellEntries_DirectoryScansAuditFiles(t *testing.T) {
	dir := t.TempDir()
	// Two daily files plus a file that should be ignored (not "audit-...jsonl").
	good1 := filepath.Join(dir, "audit-2026-05-04.jsonl")
	good2 := filepath.Join(dir, "audit-2026-05-05.jsonl")
	junk := filepath.Join(dir, "README.md")
	if err := os.WriteFile(good1, []byte(`{"session_id":"s1","disposition":"deny"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(good2, []byte(`{"session_id":"s2","disposition":"allow"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(junk, []byte("ignore me"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A subdirectory in the same place should not crash the scan.
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	entries, err := audit.ReadShellEntries(dir)
	if err != nil {
		t.Fatalf("ReadShellEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	// Sorted by name → 2026-05-04 first, 2026-05-05 second.
	if entries[0].SessionID != "s1" || entries[1].SessionID != "s2" {
		t.Errorf("entries out of order: %+v", entries)
	}
}

func TestReadShellEntries_NonExistentPathReturnsErrorOnOpen(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.jsonl")
	_, err := audit.ReadShellEntries(missing)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want os.ErrNotExist, got %v", err)
	}
}

func TestReadShellEntries_EmptyDirectoryReturnsEmpty(t *testing.T) {
	entries, err := audit.ReadShellEntries(t.TempDir())
	if err != nil {
		t.Fatalf("ReadShellEntries: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("got %d entries, want 0", len(entries))
	}
}

func TestReadMessageEntries_DirectoryScans(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit-2026-05-05.jsonl")
	// Mix message entries (have "event") and shell entries (no "event") in one file.
	contents := strings.Join([]string{
		`{"session_id":"s","event":"message_received","service":"slack","channel":"#x"}`,
		`{"session_id":"s","disposition":"allow"}`,
		`{"session_id":"s","event":"draft_requested","service":"slack","channel":"#x"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := audit.ReadMessageEntries(dir)
	if err != nil {
		t.Fatalf("ReadMessageEntries: %v", err)
	}
	// Only the two with event should be returned.
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
}

func TestAppendShellEntry_FailsWhenParentIsFile(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// path's parent (`blocker/sub`) cannot be created because `blocker` is a file.
	err := audit.AppendShellEntry(filepath.Join(blocker, "sub", "audit.jsonl"), audit.ShellEntry{
		Timestamp:   time.Now(),
		SessionID:   "s",
		Command:     "echo",
		Disposition: "allow",
	})
	if err == nil {
		t.Fatal("AppendShellEntry should fail when parent dir cannot be created")
	}
}

func TestAppendMessageEntry_FailsWhenParentIsFile(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := audit.AppendMessageEntry(filepath.Join(blocker, "sub", "audit.jsonl"), audit.MessageEntry{
		Timestamp: time.Now(),
		SessionID: "s",
		Event:     "message_received",
		Service:   "slack",
		Channel:   "#x",
	})
	if err == nil {
		t.Fatal("AppendMessageEntry should fail when parent dir cannot be created")
	}
}

func TestAppendShellEntry_FailsWhenPathIsDirectory(t *testing.T) {
	dir := t.TempDir() // existing directory; OpenFile with O_WRONLY fails
	err := audit.AppendShellEntry(dir, audit.ShellEntry{
		Timestamp:   time.Now(),
		SessionID:   "s",
		Command:     "echo",
		Disposition: "allow",
	})
	if err == nil {
		t.Fatal("AppendShellEntry should fail when path resolves to a directory")
	}
}

func TestAppendMessageEntry_FailsWhenPathIsDirectory(t *testing.T) {
	dir := t.TempDir()
	err := audit.AppendMessageEntry(dir, audit.MessageEntry{
		Timestamp: time.Now(),
		SessionID: "s",
		Event:     "message_received",
		Service:   "slack",
		Channel:   "#x",
	})
	if err == nil {
		t.Fatal("AppendMessageEntry should fail when path resolves to a directory")
	}
}

func TestReadShellEntriesFiltered_DirectoryAndFilters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit-2026-05-05.jsonl")
	contents := strings.Join([]string{
		`{"session_id":"s1","command":"git push origin main","disposition":"deny","rule_id":"never-push-main"}`,
		`{"session_id":"s2","command":"echo hi","disposition":"allow"}`,
		`{"session_id":"s1","command":"echo bye","disposition":"allow"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := audit.ReadShellEntriesFiltered(dir, audit.ShellFilter{
		SessionID:      "s1",
		CommandPattern: "echo",
	})
	if err != nil {
		t.Fatalf("ReadShellEntriesFiltered: %v", err)
	}
	if len(got) != 1 || got[0].Command != "echo bye" {
		t.Fatalf("got %+v; want one entry: echo bye", got)
	}
}

func TestReadShellEntries_DirectoryWithUnreadableEntryReturnsError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; chmod-based access denial is bypassed")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "audit-2026-05-05.jsonl")
	if err := os.WriteFile(path, []byte(`{"session_id":"s"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Strip read permission from the file; ReadShellEntries should
	// surface the open error rather than silently succeed.
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	_, err := audit.ReadShellEntries(dir)
	if err == nil {
		t.Fatal("expected error reading unreadable file")
	}
}
