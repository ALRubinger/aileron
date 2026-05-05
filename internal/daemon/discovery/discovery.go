// Package discovery manages the on-disk advertisement of the running
// Aileron daemon. The daemon writes its URL, PID, version, and start
// time to <stateDir>/daemon.json after binding its TCP listener; CLI
// and launch read this file to find the daemon (with auto-spawn if
// missing). A separate <stateDir>/daemon.pid sidecar exists so
// `aileron stop` and shell tooling (`kill $(cat daemon.pid)`) can
// find the process by PID without parsing JSON.
//
// A sidecar <stateDir>/daemon.lock is taken via flock(2) by clients
// that race to spawn the daemon on first need; the holder spawns and
// releases, and contenders re-check daemon.json after acquiring.
//
// All files are mode 0600 in a stateDir the caller is expected to
// scope to the user's ~/.aileron/ (or t.TempDir() in tests).
//
// See ADR-0012.
package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// File names within stateDir.
const (
	InfoFile = "daemon.json"
	PIDFile  = "daemon.pid"
	LockFile = "daemon.lock"
)

// Info is the structured advertisement the daemon writes after binding.
type Info struct {
	URL       string    `json:"url"`
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
}

// ErrNotRunning is returned by Read when daemon.json does not exist.
// Callers can use [errors.Is] against this sentinel; the underlying
// cause is also wrapped so [errors.Is] against [os.ErrNotExist] works.
var ErrNotRunning = errors.New("daemon is not running")

// Read reads <stateDir>/daemon.json and returns the parsed Info.
// Returns ErrNotRunning (also wrapping [os.ErrNotExist]) when the file
// does not exist.
func Read(stateDir string) (Info, error) {
	path := filepath.Join(stateDir, InfoFile)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Info{}, fmt.Errorf("%w: %w", ErrNotRunning, err)
		}
		return Info{}, fmt.Errorf("discovery: read %s: %w", path, err)
	}
	var info Info
	if err := json.Unmarshal(b, &info); err != nil {
		return Info{}, fmt.Errorf("discovery: parse %s: %w", path, err)
	}
	return info, nil
}

// Write atomically replaces <stateDir>/daemon.json and <stateDir>/daemon.pid
// with the supplied Info. The two files are written via temp + rename;
// each rename is atomic on its own, but the pair is not — readers
// should treat daemon.json as authoritative. Both files are mode 0600.
//
// stateDir is created (mode 0700) if it does not exist.
func Write(stateDir string, info Info) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("discovery: mkdir %s: %w", stateDir, err)
	}

	b, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("discovery: marshal: %w", err)
	}
	if err := writeAtomic(stateDir, InfoFile, b); err != nil {
		return err
	}

	pidBytes := []byte(strconv.Itoa(info.PID) + "\n")
	if err := writeAtomic(stateDir, PIDFile, pidBytes); err != nil {
		return err
	}
	return nil
}

// Remove deletes daemon.json and daemon.pid from stateDir. Missing
// files are not errors. The lock file is left in place — its presence
// does not imply the daemon is running, and removing it could race
// with a concurrent Lock acquisition.
func Remove(stateDir string) error {
	for _, name := range []string{InfoFile, PIDFile} {
		if err := os.Remove(filepath.Join(stateDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("discovery: remove %s: %w", name, err)
		}
	}
	return nil
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

// writeAtomic writes data to <stateDir>/<name> via a temp file and
// rename(2). Mode is 0600. The rename is atomic on POSIX file systems.
// On any error before the rename succeeds, the temp file is cleaned up.
func writeAtomic(stateDir, name string, data []byte) error {
	final := filepath.Join(stateDir, name)
	tmp, err := os.CreateTemp(stateDir, name+".tmp.*")
	if err != nil {
		return fmt.Errorf("discovery: create temp for %s: %w", name, err)
	}
	tmpPath := tmp.Name()
	keepTemp := false
	defer func() {
		if !keepTemp {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("discovery: write %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("discovery: close temp %s: %w", name, err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("discovery: chmod %s: %w", name, err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		return fmt.Errorf("discovery: rename %s: %w", name, err)
	}
	keepTemp = true
	return nil
}

