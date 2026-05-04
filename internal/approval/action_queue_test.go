package approval

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestActionApprovalQueue_RegisterAndDecideUnblocksWaiter is the
// primary regression for the approve path: Register creates a pending
// entry, a goroutine waits on Wait, the user calls Decide(approved=true),
// the waiter unblocks with the same decision. This is the load-bearing
// shape of the action-run handler under approval gating.
func TestActionApprovalQueue_RegisterAndDecideUnblocksWaiter(t *testing.T) {
	q := NewActionApprovalQueue(nil, nil)
	a := q.Register("send-email", "github://x/y", "sess-1", map[string]any{"to": "alice"})
	if a.ID == "" {
		t.Fatal("Register returned empty id")
	}

	var got ActionDecision
	var waitErr error
	done := make(chan struct{})
	go func() {
		got, waitErr = a.Wait(context.Background(), 5*time.Second)
		close(done)
	}()

	if err := q.Decide(a.ID, true, ""); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	<-done
	if waitErr != nil {
		t.Fatalf("Wait err = %v, want nil", waitErr)
	}
	if !got.Approved {
		t.Errorf("decision.Approved = false, want true")
	}
}

// TestActionApprovalQueue_DenyCarriesReason asserts that the deny
// path forwards the user's reason to the waiter, so the runtime can
// surface it in the failure envelope returned to the agent.
func TestActionApprovalQueue_DenyCarriesReason(t *testing.T) {
	q := NewActionApprovalQueue(nil, nil)
	a := q.Register("send-email", "github://x/y", "", nil)

	type result struct {
		decision ActionDecision
		err      error
	}
	resultCh := make(chan result, 1)
	go func() {
		d, err := a.Wait(context.Background(), time.Second)
		resultCh <- result{d, err}
	}()

	if err := q.Decide(a.ID, false, "wrong recipient"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	r := <-resultCh
	if r.err != nil {
		t.Fatalf("Wait err = %v", r.err)
	}
	if r.decision.Approved {
		t.Errorf("decision.Approved = true, want false")
	}
	if r.decision.Reason != "wrong recipient" {
		t.Errorf("decision.Reason = %q", r.decision.Reason)
	}
}

// TestActionApprovalQueue_DecideTwiceReturnsNotFound asserts that
// resolving a single approval twice fails on the second call. This
// is what guards against double-invocation when the user clicks
// twice or two webapp tabs decide the same item simultaneously.
func TestActionApprovalQueue_DecideTwiceReturnsNotFound(t *testing.T) {
	q := NewActionApprovalQueue(nil, nil)
	a := q.Register("send-email", "github://x/y", "", nil)

	if err := q.Decide(a.ID, true, ""); err != nil {
		t.Fatalf("first Decide: %v", err)
	}
	err := q.Decide(a.ID, true, "")
	if !errors.Is(err, ErrActionApprovalNotFound) {
		t.Errorf("second Decide err = %v, want ErrActionApprovalNotFound", err)
	}
}

// TestActionApprovalQueue_DecideUnknownIDReturnsNotFound covers the
// CLI/webapp path where a stale id (already resolved on another
// channel, or never existed) should produce a clear error.
func TestActionApprovalQueue_DecideUnknownIDReturnsNotFound(t *testing.T) {
	q := NewActionApprovalQueue(nil, nil)
	err := q.Decide("act-does-not-exist", true, "")
	if !errors.Is(err, ErrActionApprovalNotFound) {
		t.Errorf("err = %v, want ErrActionApprovalNotFound", err)
	}
}

// TestActionApprovalQueue_WaitTimesOutWhenNoDecision asserts that the
// runtime's Wait returns ErrActionApprovalTimeout when the user
// doesn't decide in the allotted window. The handler turns this into
// the agent-facing approval_timeout failure envelope so the agent can
// recover gracefully.
func TestActionApprovalQueue_WaitTimesOutWhenNoDecision(t *testing.T) {
	q := NewActionApprovalQueue(nil, nil)
	a := q.Register("send-email", "github://x/y", "", nil)

	_, err := a.Wait(context.Background(), 50*time.Millisecond)
	if !errors.Is(err, ErrActionApprovalTimeout) {
		t.Errorf("err = %v, want ErrActionApprovalTimeout", err)
	}
	// After Wait timeout, Decide on the same id should still work
	// (Decide and Wait are independent; the entry stays pending until
	// resolved or the queue process exits). This matters for the
	// race where a user finally clicks just as the runtime gives up
	// — we don't want a spurious "not found" in the webapp.
	if err := q.Decide(a.ID, true, ""); err != nil {
		t.Errorf("Decide after timeout = %v, want nil", err)
	}
}

// TestActionApprovalQueue_WaitContextCancelReleases asserts that the
// runtime can abandon the wait when the originating HTTP request
// cancels (client disconnect, deadline exceeded). The pending entry
// stays in the queue — Decide remains valid for the same race window
// described above.
func TestActionApprovalQueue_WaitContextCancelReleases(t *testing.T) {
	q := NewActionApprovalQueue(nil, nil)
	a := q.Register("send-email", "github://x/y", "", nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := a.Wait(ctx, 5*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// TestActionApprovalQueue_ListReturnsStableOrder asserts that List
// returns approvals in RequestedAt order, oldest first. Stable
// ordering matters for the user surface — both the CLI's
// `aileron approval list` and the webapp's pending column read from
// this method.
func TestActionApprovalQueue_ListReturnsStableOrder(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	q := NewActionApprovalQueue(nil, func() time.Time {
		now = now.Add(time.Second)
		return now
	})

	first := q.Register("a-first", "x", "", nil)
	second := q.Register("a-second", "x", "", nil)
	third := q.Register("a-third", "x", "", nil)

	got := q.List()
	if len(got) != 3 {
		t.Fatalf("List len = %d, want 3", len(got))
	}
	if got[0].ID != first.ID || got[1].ID != second.ID || got[2].ID != third.ID {
		t.Errorf("List order = [%s, %s, %s], want [first, second, third]",
			got[0].ActionName, got[1].ActionName, got[2].ActionName)
	}
}

// TestActionApprovalQueue_GetReturnsRegisteredEntry rounds out the
// surface that the webapp/CLI need: list, get-by-id, decide.
func TestActionApprovalQueue_GetReturnsRegisteredEntry(t *testing.T) {
	q := NewActionApprovalQueue(nil, nil)
	a := q.Register("send-email", "github://x/y", "", nil)

	got, ok := q.Get(a.ID)
	if !ok {
		t.Fatal("Get(registered id) ok = false, want true")
	}
	if got.ID != a.ID || got.ActionName != "send-email" {
		t.Errorf("Get returned wrong entry: %+v", got)
	}

	if _, ok := q.Get("act-does-not-exist"); ok {
		t.Errorf("Get(unknown id) ok = true, want false")
	}
}

// TestActionApprovalQueue_ConcurrentDecides verifies that Register +
// Decide is safe under concurrent access. The queue lives in the
// apiServer and is shared by every action-run handler; locking has
// to be correct.
func TestActionApprovalQueue_ConcurrentDecides(t *testing.T) {
	q := NewActionApprovalQueue(nil, nil)

	const N = 50
	approvals := make([]*ActionApproval, N)
	for i := range approvals {
		approvals[i] = q.Register("a", "x", "", nil)
	}

	var wg sync.WaitGroup
	for i := range approvals {
		wg.Add(1)
		go func(a *ActionApproval) {
			defer wg.Done()
			if err := q.Decide(a.ID, true, ""); err != nil {
				t.Errorf("Decide(%s): %v", a.ID, err)
			}
		}(approvals[i])
	}
	wg.Wait()

	if got := q.List(); len(got) != 0 {
		t.Errorf("after concurrent Decide of all entries, List len = %d, want 0", len(got))
	}
}
