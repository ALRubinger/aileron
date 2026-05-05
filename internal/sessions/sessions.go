// Package sessions defines the storage interface for Aileron's launch
// session records. The daemon owns one concrete implementation;
// callers depend on the interface here. See ADR-0012.
package sessions

import (
	"context"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"
)

// Session is one record of an agent invocation under `aileron launch`.
//
// The (EndedAt, ExitCode) pair distinguishes three states:
//
//   - Running:       EndedAt == nil.
//   - Ended cleanly: EndedAt != nil && ExitCode != nil.
//   - Orphaned:      EndedAt != nil && ExitCode == nil. The daemon was
//     restarted while the session was running; the absence of an exit
//     code is the honest signal that we don't know how it ended.
type Session struct {
	ID         string     `json:"id"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	Agent      string     `json:"agent"`
	WorkingDir string     `json:"working_dir"`
	ExitCode   *int       `json:"exit_code,omitempty"`
}

// Running reports whether the session is still running (EndedAt is nil).
func (s Session) Running() bool { return s.EndedAt == nil }

// Orphaned reports whether the session was reaped by a daemon restart
// (EndedAt is set but ExitCode is not).
func (s Session) Orphaned() bool { return s.EndedAt != nil && s.ExitCode == nil }

// Filter constrains List queries. Results are ordered newest-first by StartedAt.
type Filter struct {
	ActiveOnly bool      // EndedAt == nil
	Agent      string    // "" matches any
	Since      time.Time // zero = no lower bound
	Limit      int       // 0 = no limit
}

// ErrNotFound is returned by Store.Get when no session matches the supplied ID.
var ErrNotFound = errors.New("session not found")

// ErrInvalidSession is returned by Store.Put when the session is missing
// required fields (ID, StartedAt, Agent).
var ErrInvalidSession = errors.New("invalid session")

// Store persists session records. Implementations must be safe for
// concurrent use.
type Store interface {
	// Put upserts a session keyed by ID. Returns ErrInvalidSession when
	// the session is missing required fields.
	Put(ctx context.Context, s Session) error
	// Get returns the session with the given ID or ErrNotFound.
	Get(ctx context.Context, id string) (Session, error)
	// List returns sessions matching f, ordered newest-first.
	List(ctx context.Context, f Filter) ([]Session, error)
	// Close releases resources. Implementations may perform compaction
	// on Close.
	Close() error
}

// NewID returns a new ULID-based session ID. ULIDs are time-sortable
// lexicographically, which the JSONL implementation relies on for
// last-write-wins replay ordering.
func NewID() string {
	return ulid.Make().String()
}
