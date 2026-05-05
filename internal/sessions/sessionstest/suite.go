// Package sessionstest provides a shared behavioural-contract test
// suite that any [sessions.Store] implementation must satisfy.
//
// Implementations exercise the suite by passing a Factory that opens a
// Store backed by per-test isolated state. Repeated calls to the
// Factory must return Stores backed by the same underlying state so
// persistence tests can close and reopen.
//
// This package is a test harness, not production code. Coverage for it
// is excluded from the meaningful coverage figure — the err-check
// branches inside the must-helpers only fire when an implementation
// misbehaves, which is the framework's job to detect, not its job to
// exercise.
package sessionstest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/sessions"
)

// Factory opens a Store. Repeated calls within a single test must
// return Stores backed by the same underlying state so the suite can
// verify persistence across Close/reopen.
type Factory func() (sessions.Store, error)

// RunSuite executes the contract suite against the implementation
// produced by newFactory. newFactory is invoked once per subtest; the
// implementation is expected to scope state to t.TempDir() (or similar)
// so subtests do not share state.
func RunSuite(t *testing.T, newFactory func(*testing.T) Factory) {
	t.Helper()

	t.Run("PutGetRoundTrip", func(t *testing.T) { testPutGetRoundTrip(t, newFactory(t)) })
	t.Run("PutValidates", func(t *testing.T) { testPutValidates(t, newFactory(t)) })
	t.Run("PutUpserts", func(t *testing.T) { testPutUpserts(t, newFactory(t)) })
	t.Run("GetNotFound", func(t *testing.T) { testGetNotFound(t, newFactory(t)) })
	t.Run("ListEmpty", func(t *testing.T) { testListEmpty(t, newFactory(t)) })
	t.Run("ListOrdersNewestFirst", func(t *testing.T) { testListOrdersNewestFirst(t, newFactory(t)) })
	t.Run("ListActiveOnly", func(t *testing.T) { testListActiveOnly(t, newFactory(t)) })
	t.Run("ListAgentFilter", func(t *testing.T) { testListAgentFilter(t, newFactory(t)) })
	t.Run("ListSinceFilter", func(t *testing.T) { testListSinceFilter(t, newFactory(t)) })
	t.Run("ListLimit", func(t *testing.T) { testListLimit(t, newFactory(t)) })
	t.Run("ListCombinedFilters", func(t *testing.T) { testListCombinedFilters(t, newFactory(t)) })
	t.Run("PersistsAcrossReopen", func(t *testing.T) { testPersistsAcrossReopen(t, newFactory(t)) })
	t.Run("ReapsOrphanedOnReopen", func(t *testing.T) { testReapsOrphanedOnReopen(t, newFactory(t)) })
	t.Run("ConcurrentPutGet", func(t *testing.T) { testConcurrentPutGet(t, newFactory(t)) })
}

func testPutGetRoundTrip(t *testing.T, f Factory) {
	t.Helper()
	s := mustOpen(t, f)
	defer mustClose(t, s)

	want := newRunning("01HK0000000000000000000000", "claude", "/work/proj", time.Now().UTC().Truncate(time.Second))
	mustPut(t, s, want)
	got := mustGet(t, s, want.ID)
	if !sessionEquals(got, want) {
		t.Fatalf("round-trip mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func testPutValidates(t *testing.T, f Factory) {
	t.Helper()
	s := mustOpen(t, f)
	defer mustClose(t, s)

	cases := []struct {
		name string
		s    sessions.Session
	}{
		{"missing ID", sessions.Session{StartedAt: time.Now(), Agent: "claude"}},
		{"missing Agent", sessions.Session{ID: "01HK0000000000000000000000", StartedAt: time.Now()}},
		{"zero StartedAt", sessions.Session{ID: "01HK0000000000000000000000", Agent: "claude"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.Put(context.Background(), tc.s)
			if !errors.Is(err, sessions.ErrInvalidSession) {
				t.Fatalf("want ErrInvalidSession, got %v", err)
			}
		})
	}
}

func testPutUpserts(t *testing.T, f Factory) {
	t.Helper()
	s := mustOpen(t, f)
	defer mustClose(t, s)

	id := "01HK0000000000000000000001"
	started := time.Now().UTC().Truncate(time.Second)
	mustPut(t, s, newRunning(id, "claude", "/a", started))

	ended := started.Add(5 * time.Minute)
	exit := 0
	updated := sessions.Session{
		ID: id, StartedAt: started, EndedAt: &ended,
		Agent: "claude", WorkingDir: "/a", ExitCode: &exit,
	}
	mustPut(t, s, updated)

	got := mustGet(t, s, id)
	if !sessionEquals(got, updated) {
		t.Fatalf("upsert mismatch:\n got=%+v\nwant=%+v", got, updated)
	}
}

func testGetNotFound(t *testing.T, f Factory) {
	t.Helper()
	s := mustOpen(t, f)
	defer mustClose(t, s)

	_, err := s.Get(context.Background(), "01HK0000000000000000000099")
	if !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func testListEmpty(t *testing.T, f Factory) {
	t.Helper()
	s := mustOpen(t, f)
	defer mustClose(t, s)

	got := mustList(t, s, sessions.Filter{})
	if len(got) != 0 {
		t.Fatalf("want empty, got %d items", len(got))
	}
}

func testListOrdersNewestFirst(t *testing.T, f Factory) {
	t.Helper()
	s := mustOpen(t, f)
	defer mustClose(t, s)

	base := time.Now().UTC().Truncate(time.Second)
	ids := []string{"01HK0000000000000000000010", "01HK0000000000000000000011", "01HK0000000000000000000012"}
	starts := []time.Time{base.Add(-2 * time.Hour), base.Add(-1 * time.Hour), base}

	for i, id := range ids {
		mustPut(t, s, newRunning(id, "claude", "/x", starts[i]))
	}

	got := mustList(t, s, sessions.Filter{})
	if len(got) != 3 {
		t.Fatalf("want 3 items, got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].StartedAt.Before(got[i].StartedAt) {
			t.Fatalf("not newest-first at index %d: %v before %v", i-1, got[i-1].StartedAt, got[i].StartedAt)
		}
	}
}

func testListActiveOnly(t *testing.T, f Factory) {
	t.Helper()
	s := mustOpen(t, f)
	defer mustClose(t, s)

	now := time.Now().UTC().Truncate(time.Second)
	exit := 0
	ended := now
	running := newRunning("01HK0000000000000000000020", "claude", "/x", now)
	done := sessions.Session{
		ID: "01HK0000000000000000000021", StartedAt: now.Add(-time.Hour),
		EndedAt: &ended, Agent: "claude", WorkingDir: "/x", ExitCode: &exit,
	}
	mustPut(t, s, running)
	mustPut(t, s, done)

	got := mustList(t, s, sessions.Filter{ActiveOnly: true})
	if len(got) != 1 || got[0].ID != running.ID {
		t.Fatalf("want only running session, got %+v", got)
	}
}

func testListAgentFilter(t *testing.T, f Factory) {
	t.Helper()
	s := mustOpen(t, f)
	defer mustClose(t, s)

	now := time.Now().UTC().Truncate(time.Second)
	for i, agent := range []string{"claude", "claude", "pi"} {
		id := []string{"01HK0000000000000000000030", "01HK0000000000000000000031", "01HK0000000000000000000032"}[i]
		mustPut(t, s, newRunning(id, agent, "/x", now.Add(-time.Duration(i)*time.Minute)))
	}

	got := mustList(t, s, sessions.Filter{Agent: "claude"})
	if len(got) != 2 {
		t.Fatalf("want 2 claude sessions, got %d: %+v", len(got), got)
	}
	for _, sess := range got {
		if sess.Agent != "claude" {
			t.Fatalf("filter leaked %q session", sess.Agent)
		}
	}
}

func testListSinceFilter(t *testing.T, f Factory) {
	t.Helper()
	s := mustOpen(t, f)
	defer mustClose(t, s)

	now := time.Now().UTC().Truncate(time.Second)
	old := newRunning("01HK0000000000000000000040", "claude", "/x", now.Add(-2*time.Hour))
	recent := newRunning("01HK0000000000000000000041", "claude", "/x", now)
	mustPut(t, s, old)
	mustPut(t, s, recent)

	got := mustList(t, s, sessions.Filter{Since: now.Add(-time.Hour)})
	if len(got) != 1 || got[0].ID != recent.ID {
		t.Fatalf("want only recent session, got %+v", got)
	}
}

func testListLimit(t *testing.T, f Factory) {
	t.Helper()
	s := mustOpen(t, f)
	defer mustClose(t, s)

	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 5; i++ {
		id := "01HK000000000000000000005" + string(rune('0'+i))
		mustPut(t, s, newRunning(id, "claude", "/x", now.Add(-time.Duration(i)*time.Minute)))
	}

	got := mustList(t, s, sessions.Filter{Limit: 2})
	if len(got) != 2 {
		t.Fatalf("want 2 (limited), got %d", len(got))
	}
}

func testListCombinedFilters(t *testing.T, f Factory) {
	t.Helper()
	s := mustOpen(t, f)
	defer mustClose(t, s)

	now := time.Now().UTC().Truncate(time.Second)
	exit := 0
	ended := now
	cases := []sessions.Session{
		newRunning("01HK0000000000000000000060", "claude", "/x", now),                                                          // claude, running, recent
		newRunning("01HK0000000000000000000061", "pi", "/x", now),                                                              // pi, running, recent — filtered by agent
		newRunning("01HK0000000000000000000062", "claude", "/x", now.Add(-3*time.Hour)),                                        // claude, running, old — filtered by since
		{ID: "01HK0000000000000000000063", StartedAt: now, EndedAt: &ended, Agent: "claude", WorkingDir: "/x", ExitCode: &exit}, // claude, ended — filtered by ActiveOnly
	}
	for _, sess := range cases {
		mustPut(t, s, sess)
	}

	got := mustList(t, s, sessions.Filter{
		ActiveOnly: true,
		Agent:      "claude",
		Since:      now.Add(-time.Hour),
		Limit:      10,
	})
	if len(got) != 1 || got[0].ID != "01HK0000000000000000000060" {
		t.Fatalf("want only the recent running claude session, got %+v", got)
	}
}

func testPersistsAcrossReopen(t *testing.T, f Factory) {
	t.Helper()
	s := mustOpen(t, f)

	now := time.Now().UTC().Truncate(time.Second)
	exit := 0
	ended := now.Add(time.Minute)
	id := "01HK0000000000000000000070"
	want := sessions.Session{
		ID: id, StartedAt: now, EndedAt: &ended,
		Agent: "claude", WorkingDir: "/x", ExitCode: &exit,
	}
	mustPut(t, s, want)
	mustClose(t, s)

	s2 := mustOpen(t, f)
	defer mustClose(t, s2)

	got := mustGet(t, s2, id)
	if !sessionEquals(got, want) {
		t.Fatalf("persistence mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func testReapsOrphanedOnReopen(t *testing.T, f Factory) {
	t.Helper()
	s := mustOpen(t, f)

	now := time.Now().UTC().Truncate(time.Second)
	id := "01HK0000000000000000000080"
	mustPut(t, s, newRunning(id, "claude", "/x", now))
	mustClose(t, s)

	s2 := mustOpen(t, f)
	defer mustClose(t, s2)

	got := mustGet(t, s2, id)
	if got.EndedAt == nil {
		t.Fatalf("orphan reap did not stamp EndedAt: %+v", got)
	}
	if got.ExitCode != nil {
		t.Fatalf("orphan reap fabricated ExitCode (%v); want nil", *got.ExitCode)
	}
	if !got.Orphaned() {
		t.Fatalf("session not classified as Orphaned: %+v", got)
	}
}

func testConcurrentPutGet(t *testing.T, f Factory) {
	t.Helper()
	s := mustOpen(t, f)
	defer mustClose(t, s)

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			id := sessions.NewID()
			sess := newRunning(id, "claude", "/x", now.Add(time.Duration(i)*time.Microsecond))
			if err := s.Put(context.Background(), sess); err != nil {
				t.Errorf("Put: %v", err)
				return
			}
			got, err := s.Get(context.Background(), id)
			if err != nil {
				t.Errorf("Get: %v", err)
				return
			}
			if got.ID != id {
				t.Errorf("got ID %q, want %q", got.ID, id)
			}
		}(i)
	}
	wg.Wait()

	got := mustList(t, s, sessions.Filter{Limit: 0})
	if len(got) != N {
		t.Fatalf("want %d sessions after concurrent Put, got %d", N, len(got))
	}
}

// must-helpers consolidate err-check branches into a small number of
// places. The test functions above are then mostly branch-free, which
// keeps their statement coverage near 100% in normal runs. The
// helpers' err-check branches are dead in correct implementations; see
// the package doc for why this is the right shape.

func mustOpen(t *testing.T, f Factory) sessions.Store {
	t.Helper()
	s, err := f()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s
}

func mustClose(t *testing.T, s sessions.Store) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func mustPut(t *testing.T, s sessions.Store, sess sessions.Session) {
	t.Helper()
	if err := s.Put(context.Background(), sess); err != nil {
		t.Fatalf("Put %s: %v", sess.ID, err)
	}
}

func mustGet(t *testing.T, s sessions.Store, id string) sessions.Session {
	t.Helper()
	got, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	return got
}

func mustList(t *testing.T, s sessions.Store, f sessions.Filter) []sessions.Session {
	t.Helper()
	got, err := s.List(context.Background(), f)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return got
}

func newRunning(id, agent, dir string, started time.Time) sessions.Session {
	return sessions.Session{
		ID: id, StartedAt: started, Agent: agent, WorkingDir: dir,
	}
}

// sessionEquals compares two sessions by value, treating nil pointers
// as equal and non-nil pointers as equal when their values match.
// time.Time fields are compared via Equal to ignore monotonic-clock
// differences after a JSON round-trip.
func sessionEquals(a, b sessions.Session) bool {
	if a.ID != b.ID || a.Agent != b.Agent || a.WorkingDir != b.WorkingDir {
		return false
	}
	if !a.StartedAt.Equal(b.StartedAt) {
		return false
	}
	if (a.EndedAt == nil) != (b.EndedAt == nil) {
		return false
	}
	if a.EndedAt != nil && !a.EndedAt.Equal(*b.EndedAt) {
		return false
	}
	if (a.ExitCode == nil) != (b.ExitCode == nil) {
		return false
	}
	if a.ExitCode != nil && *a.ExitCode != *b.ExitCode {
		return false
	}
	return true
}
