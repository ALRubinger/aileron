package audit_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/audit"
)

func TestAppendShellEntry_CreatesFileAndDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "audit.jsonl")

	entry := audit.ShellEntry{
		Timestamp:   time.Now(),
		SessionID:   "sess-1",
		Command:     "echo hello",
		Disposition: "allow",
		RuleID:      "allow_0",
		Agent:       "claude",
	}

	if err := audit.AppendShellEntry(path, entry); err != nil {
		t.Fatalf("AppendShellEntry failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("audit log is empty")
	}
}

func TestAppendAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	entries := []audit.ShellEntry{
		{Timestamp: time.Now(), SessionID: "s1", Command: "echo hello", Disposition: "allow", RuleID: "allow_0", Agent: "claude"},
		{Timestamp: time.Now(), SessionID: "s1", Command: "rm -rf /", Disposition: "deny", RuleID: "deny_0", Agent: "claude"},
		{Timestamp: time.Now(), SessionID: "s2", Command: "git push", Disposition: "ask_approved", RuleID: "ask_0", Agent: "claude"},
	}

	for _, e := range entries {
		if err := audit.AppendShellEntry(path, e); err != nil {
			t.Fatal(err)
		}
	}

	got, err := audit.ReadShellEntries(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}
	if got[0].Command != "echo hello" {
		t.Errorf("got[0].Command = %q, want 'echo hello'", got[0].Command)
	}
	if got[1].Disposition != "deny" {
		t.Errorf("got[1].Disposition = %q, want 'deny'", got[1].Disposition)
	}
}

func TestReadShellEntries_FileNotFound(t *testing.T) {
	_, err := audit.ReadShellEntries("/nonexistent/audit.jsonl")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestReadShellEntries_SkipsMalformedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	content := `{"timestamp":"2026-04-08T12:00:00Z","session_id":"s1","command":"echo ok","disposition":"allow","rule_id":"allow_0","agent":"claude"}
not valid json
{"timestamp":"2026-04-08T12:01:00Z","session_id":"s1","command":"ls","disposition":"allow","rule_id":"allow_1","agent":"claude"}
`
	os.WriteFile(path, []byte(content), 0o644)

	entries, err := audit.ReadShellEntries(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 entries (skipping malformed), got %d", len(entries))
	}
}

func TestReadShellEntriesFiltered_BySession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	audit.AppendShellEntry(path, audit.ShellEntry{SessionID: "s1", Command: "echo 1", Disposition: "allow"})
	audit.AppendShellEntry(path, audit.ShellEntry{SessionID: "s2", Command: "echo 2", Disposition: "allow"})
	audit.AppendShellEntry(path, audit.ShellEntry{SessionID: "s1", Command: "echo 3", Disposition: "deny"})

	got, err := audit.ReadShellEntriesFiltered(path, audit.ShellFilter{SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 entries for s1, got %d", len(got))
	}
}

func TestReadShellEntriesFiltered_ByDisposition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	audit.AppendShellEntry(path, audit.ShellEntry{SessionID: "s1", Command: "echo 1", Disposition: "allow"})
	audit.AppendShellEntry(path, audit.ShellEntry{SessionID: "s1", Command: "rm -rf /", Disposition: "deny"})

	got, err := audit.ReadShellEntriesFiltered(path, audit.ShellFilter{Disposition: "deny"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Command != "rm -rf /" {
		t.Errorf("expected 1 deny entry, got %v", got)
	}
}

func TestReadShellEntriesFiltered_ByCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	audit.AppendShellEntry(path, audit.ShellEntry{SessionID: "s1", Command: "echo hello", Disposition: "allow"})
	audit.AppendShellEntry(path, audit.ShellEntry{SessionID: "s1", Command: "git push origin main", Disposition: "deny"})

	got, err := audit.ReadShellEntriesFiltered(path, audit.ShellFilter{CommandPattern: "git"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Command != "git push origin main" {
		t.Errorf("expected 1 git entry, got %v", got)
	}
}

func TestReadShellEntriesFiltered_Combined(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	audit.AppendShellEntry(path, audit.ShellEntry{SessionID: "s1", Command: "echo hello", Disposition: "allow"})
	audit.AppendShellEntry(path, audit.ShellEntry{SessionID: "s1", Command: "rm -rf /", Disposition: "deny"})
	audit.AppendShellEntry(path, audit.ShellEntry{SessionID: "s2", Command: "rm tmp", Disposition: "deny"})

	got, err := audit.ReadShellEntriesFiltered(path, audit.ShellFilter{SessionID: "s1", Disposition: "deny"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Command != "rm -rf /" {
		t.Errorf("expected 1 entry matching s1+deny, got %v", got)
	}
}
