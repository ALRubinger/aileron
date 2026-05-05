// Package jsonl is a [sessions.Store] backed by a JSONL file. The
// daemon owns one instance pointed at ~/.aileron/sessions.jsonl.
//
// The file is append-only during normal operation: Put appends a full
// record per line and updates an in-memory map for fast Get/List.
// Close rewrites the file from the map for compaction (atomic rename).
//
// On New, the file is replayed line by line into the in-memory map
// (last-write-wins per ID), then any session left in the running state
// is reaped — its EndedAt is stamped to now, ExitCode left nil. This
// is how the daemon honestly reports sessions that were live when it
// last shut down (whether cleanly or not).
package jsonl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/internal/sessions"
)

// Store is a JSONL-backed [sessions.Store]. Safe for concurrent use.
type Store struct {
	path string

	mu     sync.Mutex
	data   map[string]sessions.Session
	f      *os.File
	closed bool
}

// New opens or creates the JSONL file at path, replays it into memory,
// and reaps any running-when-last-shut-down sessions as orphaned.
func New(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("sessions/jsonl: mkdir: %w", err)
	}

	data, err := replay(path)
	if err != nil {
		return nil, err
	}

	reapOrphaned(data, time.Now().UTC())

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("sessions/jsonl: open: %w", err)
	}

	s := &Store{path: path, data: data, f: f}

	// Persist any reaped records so the orphaned state is durable.
	for _, sess := range data {
		if sess.Orphaned() {
			if err := s.appendLocked(sess); err != nil {
				_ = f.Close()
				return nil, err
			}
		}
	}
	return s, nil
}

// Put upserts a session record.
func (s *Store) Put(ctx context.Context, sess sessions.Session) error {
	if err := validate(sess); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("sessions/jsonl: store is closed")
	}
	if err := s.appendLocked(sess); err != nil {
		return err
	}
	s.data[sess.ID] = sess
	return nil
}

// Get returns the session with the given ID, or [sessions.ErrNotFound].
func (s *Store) Get(ctx context.Context, id string) (sessions.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sessions.Session{}, errors.New("sessions/jsonl: store is closed")
	}
	sess, ok := s.data[id]
	if !ok {
		return sessions.Session{}, sessions.ErrNotFound
	}
	return sess, nil
}

// List returns sessions matching f, ordered newest-first.
func (s *Store) List(ctx context.Context, f sessions.Filter) ([]sessions.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("sessions/jsonl: store is closed")
	}
	out := make([]sessions.Session, 0, len(s.data))
	for _, sess := range s.data {
		if matches(sess, f) {
			out = append(out, sess)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// Close compacts the JSONL file to one record per ID and releases the
// file descriptor. Subsequent calls to Put/Get/List return an error.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true

	if err := s.f.Close(); err != nil {
		return fmt.Errorf("sessions/jsonl: close fd: %w", err)
	}
	return s.compactLocked()
}

// appendLocked writes one JSON line for sess and flushes. Caller holds s.mu.
func (s *Store) appendLocked(sess sessions.Session) error {
	b, err := json.Marshal(sess)
	if err != nil {
		return fmt.Errorf("sessions/jsonl: marshal: %w", err)
	}
	b = append(b, '\n')
	if _, err := s.f.Write(b); err != nil {
		return fmt.Errorf("sessions/jsonl: write: %w", err)
	}
	return nil
}

// compactLocked rewrites the file from s.data using a temp file +
// atomic rename. Caller holds s.mu and has already closed s.f.
func (s *Store) compactLocked() error {
	tmp, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("sessions/jsonl: create temp: %w", err)
	}
	tmpPath := tmp.Name()

	w := bufio.NewWriter(tmp)
	for _, sess := range s.data {
		b, err := json.Marshal(sess)
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("sessions/jsonl: marshal compact: %w", err)
		}
		if _, err := w.Write(b); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("sessions/jsonl: write compact: %w", err)
		}
		if err := w.WriteByte('\n'); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("sessions/jsonl: write compact: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sessions/jsonl: flush compact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sessions/jsonl: close temp: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sessions/jsonl: chmod temp: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sessions/jsonl: rename: %w", err)
	}
	return nil
}

// replay reads path line by line, last-write-wins per ID, and returns
// the resulting map. A missing file returns an empty map. Malformed
// lines are skipped — the daemon may have been killed mid-write.
func replay(path string) (map[string]sessions.Session, error) {
	data := make(map[string]sessions.Session)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return data, nil
		}
		return nil, fmt.Errorf("sessions/jsonl: open for replay: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var sess sessions.Session
		if err := json.Unmarshal(line, &sess); err != nil {
			continue
		}
		if sess.ID == "" {
			continue
		}
		data[sess.ID] = sess
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("sessions/jsonl: scan: %w", err)
	}
	return data, nil
}

// reapOrphaned stamps EndedAt on any record left in the running state,
// leaving ExitCode nil — the absence is the orphaned signal.
func reapOrphaned(data map[string]sessions.Session, now time.Time) {
	for id, sess := range data {
		if sess.EndedAt == nil {
			t := now
			sess.EndedAt = &t
			data[id] = sess
		}
	}
}

func validate(s sessions.Session) error {
	if s.ID == "" {
		return fmt.Errorf("%w: missing ID", sessions.ErrInvalidSession)
	}
	if s.Agent == "" {
		return fmt.Errorf("%w: missing Agent", sessions.ErrInvalidSession)
	}
	if s.StartedAt.IsZero() {
		return fmt.Errorf("%w: zero StartedAt", sessions.ErrInvalidSession)
	}
	return nil
}

func matches(s sessions.Session, f sessions.Filter) bool {
	if f.ActiveOnly && s.EndedAt != nil {
		return false
	}
	if f.Agent != "" && s.Agent != f.Agent {
		return false
	}
	if !f.Since.IsZero() && s.StartedAt.Before(f.Since) {
		return false
	}
	return true
}
