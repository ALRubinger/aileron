package discovery_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/daemon/discovery"
)

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := discovery.Info{
		URL:       "http://127.0.0.1:54321",
		PID:       12345,
		Version:   "0.0.7-dev+abc123",
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := discovery.Write(dir, want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := discovery.Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.URL != want.URL || got.PID != want.PID || got.Version != want.Version {
		t.Fatalf("round-trip mismatch:\n got=%+v\nwant=%+v", got, want)
	}
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Fatalf("StartedAt mismatch: got=%v want=%v", got.StartedAt, want.StartedAt)
	}
}

func TestWriteCreatesStateDir(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "nested", "state")
	info := discovery.Info{URL: "http://127.0.0.1:1", PID: 1, Version: "v", StartedAt: time.Now().UTC()}
	if err := discovery.Write(dir, info); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, discovery.InfoFile)); err != nil {
		t.Fatalf("Stat: %v", err)
	}
}

func TestWriteSetsFileMode0600(t *testing.T) {
	dir := t.TempDir()
	info := discovery.Info{URL: "http://127.0.0.1:1", PID: 1, Version: "v", StartedAt: time.Now().UTC()}
	if err := discovery.Write(dir, info); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for _, name := range []string{discovery.InfoFile, discovery.PIDFile} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("Stat %s: %v", name, err)
		}
		if mode := fi.Mode().Perm(); mode != 0o600 {
			t.Fatalf("%s mode: want 0o600, got %o", name, mode)
		}
	}
}

func TestWritePIDFileContainsPID(t *testing.T) {
	dir := t.TempDir()
	info := discovery.Info{URL: "http://127.0.0.1:1", PID: 99887, Version: "v", StartedAt: time.Now().UTC()}
	if err := discovery.Write(dir, info); err != nil {
		t.Fatalf("Write: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, discovery.PIDFile))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("parse PID: %v", err)
	}
	if pid != info.PID {
		t.Fatalf("got PID %d, want %d", pid, info.PID)
	}
}

func TestReadMissingReturnsErrNotRunning(t *testing.T) {
	dir := t.TempDir()
	_, err := discovery.Read(dir)
	if !errors.Is(err, discovery.ErrNotRunning) {
		t.Fatalf("want ErrNotRunning, got %v", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ErrNotRunning should also wrap os.ErrNotExist; got %v", err)
	}
}

func TestReadMalformedJSONFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, discovery.InfoFile), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	_, err := discovery.Read(dir)
	if err == nil {
		t.Fatal("Read should fail on malformed JSON")
	}
	if errors.Is(err, discovery.ErrNotRunning) {
		t.Fatalf("malformed JSON should not be reported as ErrNotRunning; got %v", err)
	}
}

func TestRemoveDeletesBothFiles(t *testing.T) {
	dir := t.TempDir()
	info := discovery.Info{URL: "http://127.0.0.1:1", PID: 1, Version: "v", StartedAt: time.Now().UTC()}
	if err := discovery.Write(dir, info); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := discovery.Remove(dir); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	for _, name := range []string{discovery.InfoFile, discovery.PIDFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still exists after Remove (err: %v)", name, err)
		}
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := discovery.Remove(dir); err != nil {
		t.Fatalf("Remove on empty dir: %v", err)
	}
	if err := discovery.Remove(dir); err != nil {
		t.Fatalf("second Remove on empty dir: %v", err)
	}
}

func TestLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	release1, err := discovery.Lock(ctx, dir)
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	defer release1()

	// Second acquire with a short ctx must time out — first holder still has the lock.
	ctx2, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	release2, err := discovery.Lock(ctx2, dir)
	if err == nil {
		release2()
		t.Fatal("second Lock should not succeed while first is held")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

func TestLockReleasePermitsReacquire(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	release1, err := discovery.Lock(ctx, dir)
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	if err := release1(); err != nil {
		t.Fatalf("release1: %v", err)
	}

	release2, err := discovery.Lock(ctx, dir)
	if err != nil {
		t.Fatalf("second Lock after release: %v", err)
	}
	if err := release2(); err != nil {
		t.Fatalf("release2: %v", err)
	}
}

// TestLockSerializesContenders mimics the daemon-spawn race: many
// goroutines try to acquire the lock; exactly one runs the protected
// section at any time, and all of them eventually succeed.
func TestLockSerializesContenders(t *testing.T) {
	dir := t.TempDir()
	const N = 8
	var (
		concurrent atomic.Int32
		maxSeen    atomic.Int32
		wg         sync.WaitGroup
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			release, err := discovery.Lock(ctx, dir)
			if err != nil {
				t.Errorf("Lock: %v", err)
				return
			}
			now := concurrent.Add(1)
			for {
				prev := maxSeen.Load()
				if now <= prev || maxSeen.CompareAndSwap(prev, now) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			concurrent.Add(-1)
			if err := release(); err != nil {
				t.Errorf("release: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := maxSeen.Load(); got != 1 {
		t.Fatalf("max concurrent holders: want 1, got %d", got)
	}
}

func TestLockCancelBeforeAcquire(t *testing.T) {
	dir := t.TempDir()

	// Hold the lock so the second Lock blocks.
	hold, err := discovery.Lock(context.Background(), dir)
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	defer hold()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	_, err = discovery.Lock(ctx, dir)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want Canceled, got %v", err)
	}
}

func TestLockCreatesStateDir(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "nested")
	release, err := discovery.Lock(context.Background(), dir)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer release()
	if _, err := os.Stat(filepath.Join(dir, discovery.LockFile)); err != nil {
		t.Fatalf("Stat lock file: %v", err)
	}
}

// Compile-time assertion that discovery.Info JSON shape is what the
// ADR documents — exact field names, lower-snake-case for started_at.
func TestInfoJSONShape(t *testing.T) {
	info := discovery.Info{
		URL:       "http://127.0.0.1:54321",
		PID:       42,
		Version:   "v1",
		StartedAt: time.Date(2026, 5, 5, 14, 22, 3, 0, time.UTC),
	}
	dir := t.TempDir()
	if err := discovery.Write(dir, info); err != nil {
		t.Fatalf("Write: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, discovery.InfoFile))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := `{"url":"http://127.0.0.1:54321","pid":42,"version":"v1","started_at":"2026-05-05T14:22:03Z"}`
	if got := string(b); got != want {
		t.Fatalf("json shape mismatch:\n got=%s\nwant=%s", got, want)
	}
}

func TestWriteFailsOnReadOnlyStateDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; chmod-based access denial is bypassed")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := discovery.Write(dir, discovery.Info{
		URL: "u", PID: 1, Version: "v", StartedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("Write should fail when stateDir is read-only")
	}
}

func TestLockFailsOnReadOnlyStateDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; chmod-based access denial is bypassed")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err := discovery.Lock(context.Background(), dir)
	if err == nil {
		t.Fatal("Lock should fail when stateDir is read-only")
	}
}

func TestLockMkdirFailsWhenParentIsFile(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	_, err := discovery.Lock(context.Background(), filepath.Join(blocker, "sub"))
	if err == nil {
		t.Fatal("Lock should fail when parent is a regular file")
	}
}

func TestWriteFailsWhenStateDirIsFile(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	// stateDir = blocker, which is a regular file — MkdirAll should fail.
	err := discovery.Write(blocker, discovery.Info{URL: "u", PID: 1, Version: "v", StartedAt: time.Now()})
	if err == nil {
		t.Fatal("Write should fail when stateDir is a regular file")
	}
}

// TestLockFileInode_StableThenChangesAfterUnlinkRecreate pins the
// contract LockFileInode exposes to the daemon's inode-watch loop
// (#528 finding 3b): repeated calls return the same inode while the
// file is undisturbed; an unlink + recreate at the same path returns
// a different inode (the failure mode the watcher detects).
func TestLockFileInode_StableThenChangesAfterUnlinkRecreate(t *testing.T) {
	dir := t.TempDir()

	// Acquire a lock so daemon.lock exists with a stable inode.
	release, err := discovery.Lock(context.Background(), dir)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	first, err := discovery.LockFileInode(dir)
	if err != nil {
		t.Fatalf("LockFileInode: %v", err)
	}
	if first == 0 {
		t.Fatal("inode = 0; want a real inode")
	}

	// Repeated calls return the same inode.
	second, err := discovery.LockFileInode(dir)
	if err != nil {
		t.Fatalf("LockFileInode (second call): %v", err)
	}
	if second != first {
		t.Errorf("inode changed between calls: %d → %d", first, second)
	}

	// Release our flock and remove the lock file. (The release closes
	// the fd; the held inode is freed once removed.)
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, discovery.LockFile)); err != nil {
		t.Fatalf("remove lock file: %v", err)
	}

	// File missing → error wrapping os.ErrNotExist.
	if _, err := discovery.LockFileInode(dir); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LockFileInode after remove: err = %v, want wrapped os.ErrNotExist", err)
	}

	// Acquire a fresh lock (creates daemon.lock anew) and verify the
	// inode has changed. This is the empirical underpinning of finding
	// 3b: a fresh inode at the same path is the signal a parallel
	// daemon now owns the lock.
	release2, err := discovery.Lock(context.Background(), dir)
	if err != nil {
		t.Fatalf("Lock (re-acquire): %v", err)
	}
	defer release2()

	third, err := discovery.LockFileInode(dir)
	if err != nil {
		t.Fatalf("LockFileInode (post-recreate): %v", err)
	}
	if third == first {
		t.Errorf("inode unchanged after unlink+recreate (got %d both times); the watcher cannot detect 3b", third)
	}
}

