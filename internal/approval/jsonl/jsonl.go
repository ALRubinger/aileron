// Package jsonl is the JSONL-backed [approval.Persister] for the
// action-approval queue. The daemon owns one instance pointed at
// ~/.aileron/approvals.jsonl, plumbed through the wiring layer in
// internal/server.
//
// The file is append-only during normal operation: Append writes one
// JSON line per queue state transition. Loaders replay the file
// last-write-wins per ID (the most recent record per approval id is
// the entry's final on-disk state). Close compacts the file by
// rewriting it from the in-memory map and renaming atomically.
//
// Mirrors the shape of internal/sessions/jsonl deliberately — the
// two stores share the same crash-safe append + compact pattern.
package jsonl

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/ALRubinger/aileron/internal/approval"
)

// Store is a JSONL-backed [approval.Persister]. Safe for concurrent
// use.
type Store struct {
	path string

	mu     sync.Mutex
	data   map[string]approval.PersistedRecord
	f      *os.File
	closed bool
}

// New opens or creates the JSONL file at path and replays it into an
// in-memory map. Returns the store (callers install via
// [approval.ActionApprovalQueue.SetPersister]) and the loaded records
// in oldest-first registration order. A missing file is a non-error
// fresh-start; malformed lines are skipped (the daemon may have been
// killed mid-write).
//
// Loaded records are returned by value so the caller can mutate them
// before seeding the queue (e.g. reaping [approval.OutcomeRunning]
// entries to [approval.OutcomeFailed] with a reconciliation message —
// the runtime refuses to auto-retry a non-idempotent action whose
// execution may have produced side effects before the crash).
func New(path string) (*Store, []approval.PersistedRecord, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, fmt.Errorf("approval/jsonl: mkdir: %w", err)
	}
	data, order, err := replay(path)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("approval/jsonl: open: %w", err)
	}
	records := make([]approval.PersistedRecord, 0, len(order))
	for _, id := range order {
		records = append(records, data[id])
	}
	return &Store{path: path, data: data, f: f}, records, nil
}

// Append durably writes one record. Implements [approval.Persister].
func (s *Store) Append(rec approval.PersistedRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("approval/jsonl: store is closed")
	}
	if err := s.appendLocked(rec); err != nil {
		return err
	}
	s.data[rec.ID] = rec
	return nil
}

// Close compacts the JSONL file to one record per ID and releases
// the file descriptor. Subsequent calls to Append return an error.
// Idempotent.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.f.Close(); err != nil {
		return fmt.Errorf("approval/jsonl: close fd: %w", err)
	}
	return s.compactLocked()
}

// appendLocked writes one JSON line for rec and flushes. Caller
// holds s.mu.
func (s *Store) appendLocked(rec approval.PersistedRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("approval/jsonl: marshal: %w", err)
	}
	b = append(b, '\n')
	if _, err := s.f.Write(b); err != nil {
		return fmt.Errorf("approval/jsonl: write: %w", err)
	}
	return nil
}

// compactLocked rewrites the file from s.data via a temp file +
// atomic rename. Caller holds s.mu and has already closed s.f.
func (s *Store) compactLocked() error {
	tmp, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("approval/jsonl: create temp: %w", err)
	}
	tmpPath := tmp.Name()

	w := bufio.NewWriter(tmp)
	for _, rec := range s.data {
		b, err := json.Marshal(rec)
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("approval/jsonl: marshal compact: %w", err)
		}
		if _, err := w.Write(b); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("approval/jsonl: write compact: %w", err)
		}
		if err := w.WriteByte('\n'); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("approval/jsonl: write compact: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("approval/jsonl: flush compact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("approval/jsonl: close temp: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("approval/jsonl: chmod temp: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("approval/jsonl: rename: %w", err)
	}
	return nil
}

// replay reads path line by line. Returns the latest record per ID
// keyed by ID, plus the order in which IDs first appeared (so the
// caller can hand records back to the queue in registration order
// for deterministic respawn).
//
// A missing file returns an empty map and nil slice. Malformed
// lines are skipped — the daemon may have been killed mid-write,
// and the next append-then-compact recovers cleanly.
func replay(path string) (map[string]approval.PersistedRecord, []string, error) {
	data := make(map[string]approval.PersistedRecord)
	var order []string
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return data, nil, nil
		}
		return nil, nil, fmt.Errorf("approval/jsonl: open for replay: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec approval.PersistedRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.ID == "" {
			continue
		}
		if _, seen := data[rec.ID]; !seen {
			order = append(order, rec.ID)
		}
		data[rec.ID] = rec
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("approval/jsonl: scan: %w", err)
	}
	return data, order, nil
}
