//go:build unix

package discovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// LockFileInode returns the current inode of <stateDir>/daemon.lock.
// Used by the daemon to capture the inode of its held flock at startup,
// for periodic comparison against the current inode at the same path.
//
// The flock(2) primitive holds against an open file descriptor — i.e.
// against an inode — not a path, so an external `rm -rf <stateDir>`
// (or any other unlink + recreate at the same path) can leave a still-
// running daemon flock'd against an orphaned inode while a fresh daemon
// flock's a brand-new inode at the same path. Both processes hold valid
// locks on different underlying inodes, breaking the singleton invariant.
// A daemon that watches its lock-file inode can self-terminate when it
// detects the mismatch (#528 finding 3b).
//
// Returns the wrapping os.ErrNotExist when the file has been unlinked
// — callers can [errors.Is] against [os.ErrNotExist] to distinguish
// "file gone" from other stat errors.
func LockFileInode(stateDir string) (uint64, error) {
	path := filepath.Join(stateDir, LockFile)
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("discovery: syscall.Stat_t unavailable for %s", path)
	}
	return uint64(sys.Ino), nil
}

// Lock takes an exclusive flock(2) on <stateDir>/daemon.lock, blocking
// until the lock is acquired or ctx is canceled. Returns a release
// function the caller must invoke (typically via defer) to release the
// lock and close the file descriptor.
//
// Used by clients racing to spawn the daemon on first need: only the
// holder spawns; contenders re-check daemon.json after acquiring.
func Lock(ctx context.Context, stateDir string) (release func() error, err error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("discovery: mkdir %s: %w", stateDir, err)
	}
	path := filepath.Join(stateDir, LockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("discovery: open lock: %w", err)
	}

	const pollInterval = 25 * time.Millisecond
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() error {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				return f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("discovery: flock: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
