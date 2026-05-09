//go:build windows

package discovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

// LockFileInode returns a stable identifier for <stateDir>/daemon.lock,
// equivalent to a POSIX inode for the purpose of detecting lock-file
// recreation. On Windows this is the BY_HANDLE_FILE_INFORMATION
// FileIndex (high+low combined) returned by GetFileInformationByHandle,
// which is unique-and-stable per file on NTFS / ReFS volumes.
//
// On POSIX the inode-watch loop guards against an external
// `rm -rf <stateDir>` followed by a fresh daemon recreating the lock
// file at the same path — a scenario where flock holds against an
// orphaned inode while a new daemon locks a fresh inode (#528 finding
// 3b). On Windows the same scenario is much rarer because LockFileEx
// blocks file deletion while the lock handle is open, but the file ID
// still serves as a stable identity check for any future code paths
// that share this contract.
//
// Returns the wrapping os.ErrNotExist when the file has been unlinked
// — callers can [errors.Is] against [os.ErrNotExist] to distinguish
// "file gone" from other stat errors.
func LockFileInode(stateDir string) (uint64, error) {
	path := filepath.Join(stateDir, LockFile)
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("discovery: utf16 %s: %w", path, err)
	}
	// FILE_SHARE_READ|WRITE|DELETE matches the locking handle's share
	// mode so this stat-style read does not fight the held lock.
	h, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return 0, fmt.Errorf("%s: %w", path, os.ErrNotExist)
		}
		return 0, fmt.Errorf("discovery: open %s: %w", path, err)
	}
	defer windows.CloseHandle(h)

	var bhfi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &bhfi); err != nil {
		return 0, fmt.Errorf("discovery: GetFileInformationByHandle %s: %w", path, err)
	}
	return uint64(bhfi.FileIndexHigh)<<32 | uint64(bhfi.FileIndexLow), nil
}

// Lock takes an exclusive lock on <stateDir>/daemon.lock via LockFileEx,
// blocking until the lock is acquired or ctx is canceled. Returns a
// release function the caller must invoke (typically via defer) to
// release the lock and close the file handle.
//
// Used by clients racing to spawn the daemon on first need: only the
// holder spawns; contenders re-check daemon.json after acquiring.
//
// LockFileEx is mandatory on Windows (POSIX flock is advisory), but the
// behavior is the same for our purpose: LOCKFILE_EXCLUSIVE_LOCK +
// LOCKFILE_FAIL_IMMEDIATELY mirrors LOCK_EX|LOCK_NB.
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
	// Lock the entire file (offset 0, length = max uint32 for both
	// halves — matches the canonical "lock everything" pattern from
	// MSDN's LockFileEx examples).
	const maxBytes uint32 = 0xFFFFFFFF
	for {
		var ol windows.Overlapped
		err := windows.LockFileEx(
			windows.Handle(f.Fd()),
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0,
			maxBytes,
			maxBytes,
			&ol,
		)
		if err == nil {
			return func() error {
				var ol windows.Overlapped
				_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, maxBytes, maxBytes, &ol)
				return f.Close()
			}, nil
		}
		// ERROR_LOCK_VIOLATION is the contention signal (someone else
		// holds the lock); anything else is fatal.
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			_ = f.Close()
			return nil, fmt.Errorf("discovery: LockFileEx: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
