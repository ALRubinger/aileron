package sessions_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/sessions"
)

func TestNewID(t *testing.T) {
	id := sessions.NewID()
	if len(id) != 26 {
		t.Fatalf("ULID length want 26, got %d (%q)", len(id), id)
	}
	// ULID is Crockford base32 — uppercase + digits, no '0', '1' for I/L lookalikes
	for _, r := range id {
		if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", r) {
			t.Fatalf("invalid ULID character %q in %q", r, id)
		}
	}
}

func TestNewIDIsUniqueAndTimeSortable(t *testing.T) {
	const N = 100
	ids := make([]string, N)
	for i := 0; i < N; i++ {
		ids[i] = sessions.NewID()
	}
	seen := make(map[string]struct{}, N)
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ULID: %q", id)
		}
		seen[id] = struct{}{}
	}
	// IDs generated within the same monotonic stream sort lexicographically
	// in generation order. A handful of consecutive IDs may share a
	// millisecond timestamp prefix; the monotonic counter still keeps
	// them strictly increasing.
	for i := 1; i < N; i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("ULID at index %d (%q) does not sort after %q", i, ids[i], ids[i-1])
		}
	}
}

func TestSessionRunning(t *testing.T) {
	now := time.Now().UTC()
	running := sessions.Session{ID: "x", StartedAt: now, Agent: "claude"}
	ended := now
	finished := sessions.Session{ID: "x", StartedAt: now, Agent: "claude", EndedAt: &ended}

	if !running.Running() {
		t.Fatal("Running session should report Running()")
	}
	if finished.Running() {
		t.Fatal("Ended session should not report Running()")
	}
}

func TestSessionOrphaned(t *testing.T) {
	now := time.Now().UTC()
	exit := 0
	cases := []struct {
		name string
		s    sessions.Session
		want bool
	}{
		{
			"running",
			sessions.Session{StartedAt: now, Agent: "c"},
			false,
		},
		{
			"ended cleanly",
			sessions.Session{StartedAt: now, Agent: "c", EndedAt: &now, ExitCode: &exit},
			false,
		},
		{
			"orphaned (ended without exit code)",
			sessions.Session{StartedAt: now, Agent: "c", EndedAt: &now},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.Orphaned(); got != tc.want {
				t.Fatalf("Orphaned()=%v, want %v", got, tc.want)
			}
		})
	}
}
