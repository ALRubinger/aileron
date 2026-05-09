//go:build unix

package discovery_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/daemon/discovery"
)

// TestWriteSetsFileMode0600 pins the POSIX file-mode contract (mode
// 0600 — owner read/write, no group or other access). Windows does not
// model these bits the same way (mode is a translation of the
// read-only attribute), so this test is unix-tagged.
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

// TestWriteFailsOnReadOnlyStateDir verifies Write surfaces the
// underlying mkdir/create error when the state dir is not writable.
// Uses chmod 0500 — POSIX-only semantics.
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

// TestLockFailsOnReadOnlyStateDir verifies Lock surfaces a useful
// error when the state dir is not writable. Uses chmod 0500 —
// POSIX-only semantics.
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

// TestLockFileInode_StableThenChangesAfterUnlinkRecreate pins the
// contract LockFileInode exposes to the daemon's inode-watch loop
// (#528 finding 3b): repeated calls return the same inode while the
// file is undisturbed; an unlink + recreate at the same path while the
// original lock fd is still held returns a different inode (the
// failure mode the watcher detects).
//
// The fd-still-held detail matters: if the original fd is closed
// before re-acquiring, Linux ext4 may reuse the now-free inode for
// the new file, producing a false-negative inode comparison. The
// real-world bug scenario keeps the original daemon's fd open
// throughout (the daemon is still running), pinning the inode's
// refcount > 0 so the kernel must allocate a fresh inode for the
// fresh file. This test mirrors that — release1 fires only at
// cleanup, after the second Lock has captured a comparison inode.
//
// POSIX-only: Windows LockFileEx prevents unlinking a locked file, so
// the recreate scenario this pins doesn't apply on Windows.
func TestLockFileInode_StableThenChangesAfterUnlinkRecreate(t *testing.T) {
	dir := t.TempDir()

	// Acquire a lock so daemon.lock exists. The fd is held until test
	// cleanup — mimicking the original (still-running) daemon.
	release1, err := discovery.Lock(context.Background(), dir)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	t.Cleanup(func() { _ = release1() })

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

	// External `rm -rf ~/.aileron`: unlink the lock file. Original
	// fd still holds flock on the orphaned inode (refcount > 0), so
	// the kernel cannot reuse that inode for whatever lands here next.
	if err := os.Remove(filepath.Join(dir, discovery.LockFile)); err != nil {
		t.Fatalf("remove lock file: %v", err)
	}

	// Path missing → LockFileInode wraps os.ErrNotExist.
	if _, err := discovery.LockFileInode(dir); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LockFileInode after remove: err = %v, want wrapped os.ErrNotExist", err)
	}

	// A "fresh daemon" comes up at the same path. discovery.Lock
	// creates daemon.lock anew and flock's its fd — the original fd
	// is on a different inode, so flock(LOCK_EX|LOCK_NB) succeeds
	// without contention.
	release2, err := discovery.Lock(context.Background(), dir)
	if err != nil {
		t.Fatalf("Lock (re-acquire while original fd still held): %v", err)
	}
	t.Cleanup(func() { _ = release2() })

	third, err := discovery.LockFileInode(dir)
	if err != nil {
		t.Fatalf("LockFileInode (post-recreate): %v", err)
	}
	if third == first {
		t.Errorf("inode unchanged after held-unlink-recreate (got %d both times); the watcher cannot detect 3b on this filesystem", third)
	}
}
