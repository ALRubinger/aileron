package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
)

// /v1/sessions watch CLI contract (#532 finding 7c):
//
//   - `aileron sessions watch <id>` resolves the session to its
//     working_dir via GET /v1/sessions/{id}, computes the per-project
//     session.log path, and tails it. Existing content prints first;
//     new lines stream until SIGINT/SIGTERM.
//   - 404 surfaces as "session ... not found" on stderr with exit 1.
//   - `--no-follow` prints existing content and exits.
//   - A missing session.log at start is polled for, not an error —
//     the watcher may legitimately race the launcher creating the file.

// --- runSessionsWatch ---

func TestRunSessionsWatch_NotFoundExitsOne(t *testing.T) {
	withSessionsFetchers(t, nil,
		func(string) (*api.Session, int, error) {
			return nil, http.StatusNotFound, nil
		})

	var stdout, stderr bytes.Buffer
	code := runSessionsWatch([]string{"missing"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `session "missing" not found`) {
		t.Errorf("stderr = %q, want session-not-found message", stderr.String())
	}
}

func TestRunSessionsWatch_RequiresSessionID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSessionsWatch(nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for missing id", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("stderr = %q, want usage line", stderr.String())
	}
}

func TestRunSessionsWatch_NoFollowPrintsExistingAndExits(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".aileron", "session.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	withSessionsFetchers(t, nil,
		func(id string) (*api.Session, int, error) {
			return &api.Session{Id: id, WorkingDir: dir, Agent: "claude"}, http.StatusOK, nil
		})

	var stdout, stderr bytes.Buffer
	code := runSessionsWatch([]string{"--no-follow", "abc123"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); got != "line1\nline2\n" {
		t.Errorf("stdout = %q, want existing log content verbatim", got)
	}
}

// --- tailSessionLog ---

func TestTailSessionLog_PrintsExistingThenAppends(t *testing.T) {
	withFastTailInterval(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "session.log")
	if err := os.WriteFile(path, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out lockingBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- tailSessionLog(ctx, &out, path, false) }()

	// Wait for the existing content to be printed.
	waitForOutput(t, &out, "first\n")

	// Append new bytes; the follow loop should pick them up.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("second\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	waitForOutput(t, &out, "second\n")

	cancel()
	if err := <-done; err != nil {
		t.Errorf("tailSessionLog returned err = %v, want nil after ctx cancel", err)
	}
}

func TestTailSessionLog_NoFollowReturnsAfterExisting(t *testing.T) {
	withFastTailInterval(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "session.log")
	if err := os.WriteFile(path, []byte("only line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out lockingBuffer
	if err := tailSessionLog(context.Background(), &out, path, true); err != nil {
		t.Fatalf("tailSessionLog: %v", err)
	}
	if got := out.String(); got != "only line\n" {
		t.Errorf("got %q, want %q", got, "only line\n")
	}
}

func TestTailSessionLog_PollsForFileCreation(t *testing.T) {
	withFastTailInterval(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "session.log")
	// File does not exist yet — the watch must poll, not error.

	var out lockingBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- tailSessionLog(ctx, &out, path, false) }()

	// Race the watcher: create the file shortly after the call.
	time.Sleep(40 * time.Millisecond)
	if err := os.WriteFile(path, []byte("appeared\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	waitForOutput(t, &out, "appeared\n")
	cancel()
	if err := <-done; err != nil {
		t.Errorf("tailSessionLog returned err = %v, want nil after ctx cancel", err)
	}
}

func TestTailSessionLog_OpenFailureSurfaced(t *testing.T) {
	withFastTailInterval(t)
	// A path under a non-readable parent yields a non-ErrNotExist
	// open error; tailSessionLog must surface it rather than
	// retrying forever.
	dir := t.TempDir()
	parent := filepath.Join(dir, "noperm")
	if err := os.Mkdir(parent, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) }) // let TempDir clean up
	path := filepath.Join(parent, "session.log")

	if os.Geteuid() == 0 {
		t.Skip("root bypasses 0o000 — skipping")
	}

	var out lockingBuffer
	err := tailSessionLog(context.Background(), &out, path, true)
	if err == nil {
		t.Fatal("expected error from unreadable parent dir, got nil")
	}
}

// --- helpers ---

// withFastTailInterval shortens the tail polling interval for the
// duration of the test so the suite finishes promptly. Restores on
// cleanup.
func withFastTailInterval(t *testing.T) {
	t.Helper()
	orig := sessionsWatchTailInterval
	sessionsWatchTailInterval = 5 * time.Millisecond
	t.Cleanup(func() { sessionsWatchTailInterval = orig })
}

// lockingBuffer is a goroutine-safe bytes.Buffer; the tail loop
// writes to it from a goroutine while the test reads it.
type lockingBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockingBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockingBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForOutput polls the buffer up to 1.5s for substr to appear,
// failing the test if it doesn't. Replaces sleep+assert which would
// either be racy (too short) or slow (too long).
func waitForOutput(t *testing.T, out *lockingBuffer, substr string) {
	t.Helper()
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), substr) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("output never contained %q; got:\n%s", substr, out.String())
}
