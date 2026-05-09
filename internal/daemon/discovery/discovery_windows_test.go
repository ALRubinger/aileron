//go:build windows

package discovery_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/internal/daemon/discovery"
)

// TestLockFileInode_StableAcrossCalls verifies that LockFileInode
// returns the same identifier across repeated calls while the lock
// file is undisturbed. This is the Windows analogue of the POSIX
// inode-stability assertion in TestLockFileInode_StableThenChangesAfterUnlinkRecreate;
// the unlink-while-held scenario doesn't apply on Windows because
// LockFileEx prevents unlinking a locked file.
func TestLockFileInode_StableAcrossCalls(t *testing.T) {
	dir := t.TempDir()

	release, err := discovery.Lock(context.Background(), dir)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	t.Cleanup(func() { _ = release() })

	first, err := discovery.LockFileInode(dir)
	if err != nil {
		t.Fatalf("LockFileInode: %v", err)
	}
	if first == 0 {
		t.Fatal("inode = 0; want a real file ID")
	}
	second, err := discovery.LockFileInode(dir)
	if err != nil {
		t.Fatalf("LockFileInode (second call): %v", err)
	}
	if second != first {
		t.Errorf("file ID changed between calls: %d → %d", first, second)
	}
}

// TestLockFileInode_MissingFile verifies the os.ErrNotExist contract
// holds on Windows: a stat-style read of an absent lock file wraps
// os.ErrNotExist so callers can distinguish "file gone" from other
// errors via errors.Is.
func TestLockFileInode_MissingFile(t *testing.T) {
	dir := t.TempDir()
	// Don't create daemon.lock — verify the absent-file path.
	_, err := discovery.LockFileInode(dir)
	if err == nil {
		t.Fatal("LockFileInode on missing file: want error, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LockFileInode on missing file: want wrapped os.ErrNotExist, got %v", err)
	}
}

// TestLockBlocksRemoveWhileHeld documents the Windows-specific
// behavior that distinguishes Windows from POSIX: LockFileEx prevents
// unlinking a locked file. This is why the POSIX-only
// TestLockFileInode_StableThenChangesAfterUnlinkRecreate scenario
// doesn't apply here — the kernel rejects the unlink before the
// orphaned-inode race can occur.
func TestLockBlocksRemoveWhileHeld(t *testing.T) {
	dir := t.TempDir()
	release, err := discovery.Lock(context.Background(), dir)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	t.Cleanup(func() { _ = release() })

	if err := os.Remove(filepath.Join(dir, discovery.LockFile)); err == nil {
		t.Fatal("Remove of held lock file should fail on Windows")
	}
}
