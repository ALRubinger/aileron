package audit_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/audit"
)

func TestNewFileStore_FailsWhenParentIsFile(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Try to use a path under a regular file as the state dir —
	// MkdirAll for <blocker>/sub/audit must fail.
	_, err := audit.NewFileStore(filepath.Join(blocker, "sub"), slog.Default())
	if err == nil {
		t.Fatal("NewFileStore should fail when parent is a file")
	}
}

func TestNewFileStore_FailsWhenReplayCannotOpen(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; chmod-based access denial is bypassed")
	}
	dir := t.TempDir()
	auditDir := audit.DailyDir(dir)
	if err := os.MkdirAll(auditDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed a daily file then strip read perms — replay should error
	// rather than silently skip the file (we'd lose history).
	path := filepath.Join(auditDir, "audit-2026-05-04.jsonl")
	if err := os.WriteFile(path, []byte(`{"event":{"EventID":"x","Timestamp":"2026-05-04T00:00:00Z"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	_, err := audit.NewFileStore(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("NewFileStore should propagate replay open error")
	}
}

func TestNewFileStore_FailsWhenAuditDirCannotBeListed(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; chmod-based access denial is bypassed")
	}
	dir := t.TempDir()
	auditDir := audit.DailyDir(dir)
	if err := os.MkdirAll(auditDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Make the audit dir unreadable so ReadDir during replay fails.
	if err := os.Chmod(auditDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(auditDir, 0o755) })

	_, err := audit.NewFileStore(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("NewFileStore should propagate ReadDir failure")
	}
}

func TestFileStore_AppendFailsOnReadOnlyAuditDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; chmod-based access denial is bypassed")
	}
	dir := t.TempDir()
	store, err := audit.NewFileStore(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	// Lock the audit subdir so the per-write OpenFile in writeLine
	// (O_CREATE) cannot create today's file.
	auditDir := audit.DailyDir(dir)
	if err := os.Chmod(auditDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(auditDir, 0o755) })

	err = store.Append(context.Background(), audit.Event{EventID: "x", Timestamp: time.Now().UTC()})
	if err == nil {
		t.Fatal("Append should fail when writeLine cannot open the daily file")
	}
}

func TestFileStore_AppendToTraceFailsOnReadOnlyAuditDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; chmod-based access denial is bypassed")
	}
	dir := t.TempDir()
	store, err := audit.NewFileStore(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	auditDir := audit.DailyDir(dir)
	if err := os.Chmod(auditDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(auditDir, 0o755) })

	err = store.AppendToTrace(context.Background(), "tid", "iid", "wid",
		audit.Event{EventID: "x", Timestamp: time.Now().UTC()})
	if err == nil {
		t.Fatal("AppendToTrace should fail when writeLine cannot open the daily file")
	}
}
