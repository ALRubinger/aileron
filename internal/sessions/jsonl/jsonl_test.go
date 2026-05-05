package jsonl_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/sessions"
	"github.com/ALRubinger/aileron/internal/sessions/jsonl"
	"github.com/ALRubinger/aileron/internal/sessions/sessionstest"
)

// TestContract runs the shared SPI suite against the JSONL store. Each
// subtest receives a factory whose state is scoped to t.TempDir() and
// can be reopened for persistence assertions.
func TestContract(t *testing.T) {
	sessionstest.RunSuite(t, func(t *testing.T) sessionstest.Factory {
		path := filepath.Join(t.TempDir(), "sessions.jsonl")
		return func() (sessions.Store, error) {
			return jsonl.New(path)
		}
	})
}

func TestCompactsOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.jsonl")
	s, err := jsonl.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	exit := 0
	ended := now.Add(time.Minute)
	id := "01HK0000000000000000000001"

	// Three Puts on the same ID — should produce three append lines.
	for i := 0; i < 3; i++ {
		sess := sessions.Session{
			ID: id, StartedAt: now, EndedAt: &ended,
			Agent: "claude", WorkingDir: "/x", ExitCode: &exit,
		}
		if err := s.Put(context.Background(), sess); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	// File should have at least 3 lines before Close.
	if got := lineCount(t, path); got < 3 {
		t.Fatalf("pre-Close: want ≥3 lines, got %d", got)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// After Close, compaction collapses to 1 line.
	if got := lineCount(t, path); got != 1 {
		t.Fatalf("post-Close: want 1 line, got %d", got)
	}
}

func TestSkipsMalformedLinesOnReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.jsonl")
	now := time.Now().UTC().Truncate(time.Second)

	// Pre-populate the file with valid + malformed lines.
	good := `{"id":"01HK0000000000000000000010","started_at":"` + now.Format(time.RFC3339) + `","agent":"claude","working_dir":"/x"}`
	contents := strings.Join([]string{
		good,
		"{not valid json",       // garbage
		"",                      // empty line
		`{"id":""}`,             // empty ID
		good,                    // duplicate, latest wins
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	s, err := jsonl.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	got, err := s.List(context.Background(), sessions.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != "01HK0000000000000000000010" {
		t.Fatalf("want one valid session, got %+v", got)
	}
}

func TestPutRejectsAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.jsonl")
	s, err := jsonl.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	err = s.Put(context.Background(), sessions.Session{
		ID: "01HK0000000000000000000020", StartedAt: now, Agent: "claude",
	})
	if err == nil {
		t.Fatal("Put after Close should return error")
	}
}

func TestFilePermissionsAfterCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.jsonl")
	s, err := jsonl.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.Put(context.Background(), sessions.Session{
		ID: "01HK0000000000000000000030", StartedAt: now, Agent: "claude",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("want 0o600, got %o", mode)
	}
}

func TestNewFailsWhenParentIsFile(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	// Path whose parent directory is a regular file — MkdirAll fails.
	_, err := jsonl.New(filepath.Join(blocker, "sub", "sessions.jsonl"))
	if err == nil {
		t.Fatal("New should fail when parent is a file")
	}
}

func TestNewFailsWhenPathIsDirectory(t *testing.T) {
	dir := t.TempDir() // path that exists as a directory
	_, err := jsonl.New(dir)
	if err == nil {
		t.Fatal("New should fail when path resolves to an existing directory")
	}
}

func TestNewSurvivesEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	s, err := jsonl.New(path)
	if err != nil {
		t.Fatalf("New on empty file: %v", err)
	}
	defer s.Close()

	got, err := s.List(context.Background(), sessions.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %d items", len(got))
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.jsonl")
	s, err := jsonl.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close should be a no-op, got %v", err)
	}
}

func TestCompactionFailsOnReadOnlyDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; chmod-based access denial is bypassed")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.jsonl")
	s, err := jsonl.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Put while dir is still writable — appends to the already-open fd.
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.Put(context.Background(), sessions.Session{
		ID: "01HK0000000000000000000040", StartedAt: now, Agent: "claude",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Make the directory read-only so compaction's CreateTemp fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := s.Close(); err == nil {
		t.Fatal("Close should fail when compaction cannot create temp file")
	}
}

func TestGetAfterCloseFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.jsonl")
	s, err := jsonl.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.Get(context.Background(), "any"); err == nil {
		t.Fatal("Get after Close should return error")
	}
	if _, err := s.List(context.Background(), sessions.Filter{}); err == nil {
		t.Fatal("List after Close should return error")
	}
}

func lineCount(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(b) == 0 {
		return 0
	}
	return strings.Count(string(b), "\n")
}
