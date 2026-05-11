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

	if err := q.Decide(a.ID, true, "", nil); err != nil {
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

	if err := q.Decide(a.ID, false, "wrong recipient", nil); err != nil {
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

	if err := q.Decide(a.ID, true, "", nil); err != nil {
		t.Fatalf("first Decide: %v", err)
	}
	err := q.Decide(a.ID, true, "", nil)
	if !errors.Is(err, ErrActionApprovalNotFound) {
		t.Errorf("second Decide err = %v, want ErrActionApprovalNotFound", err)
	}
}

// TestActionApprovalQueue_DecideUnknownIDReturnsNotFound covers the
// CLI/webapp path where a stale id (already resolved on another
// channel, or never existed) should produce a clear error.
func TestActionApprovalQueue_DecideUnknownIDReturnsNotFound(t *testing.T) {
	q := NewActionApprovalQueue(nil, nil)
	err := q.Decide("act-does-not-exist", true, "", nil)
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
	if err := q.Decide(a.ID, true, "", nil); err != nil {
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

// TestActionApprovalQueue_OnRegisterCallbackFires asserts that the
// hook installed via SetOnRegister is invoked for every Register, with
// the exact pending entry as its argument. Production wiring uses this
// to fire a desktop notification so the user knows the agent is
// blocked; tests inject a recorder to drive the same path.
func TestActionApprovalQueue_OnRegisterCallbackFires(t *testing.T) {
	q := NewActionApprovalQueue(nil, nil)
	var received []*ActionApproval
	q.SetOnRegister(func(a *ActionApproval) {
		received = append(received, a)
	})

	first := q.Register("send-email", "github://x/y", "sess-1", map[string]any{"to": "alice"})
	second := q.Register("send-email", "github://x/y", "sess-2", nil)

	if len(received) != 2 {
		t.Fatalf("received len = %d, want 2", len(received))
	}
	if received[0].ID != first.ID || received[1].ID != second.ID {
		t.Errorf("callback ids = [%s, %s], want [%s, %s]",
			received[0].ID, received[1].ID, first.ID, second.ID)
	}
	// The callback gets the same pointer the caller does — the queue
	// shouldn't be defensively copying. The webapp's notification
	// payload reads ActionName / ConnectorFQN / Args directly off it.
	if received[0].ActionName != "send-email" {
		t.Errorf("callback received[0].ActionName = %q", received[0].ActionName)
	}
}

// TestActionApprovalQueue_OnRegisterPanicDoesNotPropagate asserts the
// fail-soft contract: a misbehaving callback (panic, slow, whatever)
// does not break Register. The queue's invariants must hold regardless
// of what the notifier does. Without this guarantee, a buggy notifier
// would prevent the agent's tool call from registering — much worse
// than no notification.
func TestActionApprovalQueue_OnRegisterPanicDoesNotPropagate(t *testing.T) {
	q := NewActionApprovalQueue(nil, nil)
	q.SetOnRegister(func(_ *ActionApproval) {
		panic("notifier blew up")
	})

	// If panic propagates, this Register call panics out of the test;
	// the recover'd path returns normally.
	a := q.Register("send-email", "github://x/y", "", nil)
	if a == nil || a.ID == "" {
		t.Fatal("Register returned nil despite recovered panic")
	}
	// Entry is still in the queue.
	if got, ok := q.Get(a.ID); !ok || got.ID != a.ID {
		t.Errorf("Get(%s) ok=%v, expected entry to be registered despite callback panic", a.ID, ok)
	}
}

// TestActionApprovalQueue_SetOnRegisterClearsCallback covers the
// SetOnRegister(nil) path: clearing the callback returns Register to
// its no-op-on-side-effects behavior.
func TestActionApprovalQueue_SetOnRegisterClearsCallback(t *testing.T) {
	q := NewActionApprovalQueue(nil, nil)
	called := 0
	q.SetOnRegister(func(_ *ActionApproval) { called++ })
	q.Register("a", "x", "", nil)
	if called != 1 {
		t.Fatalf("after first Register: called = %d, want 1", called)
	}

	q.SetOnRegister(nil)
	q.Register("b", "x", "", nil)
	if called != 1 {
		t.Errorf("after SetOnRegister(nil) + Register: called = %d, want still 1", called)
	}
}

// TestActionApprovalQueue_SubscribeReceivesPendingAndResolved is the
// primary regression for the streaming surface (#418): a subscriber
// receives one `pending` event per Register and one `resolved` event
// per Decide, in order, with the right payload. The webapp's SSE
// handler is built on this contract.
func TestActionApprovalQueue_SubscribeReceivesPendingAndResolved(t *testing.T) {
	q := NewActionApprovalQueue(nil, nil)
	events, cancel := q.Subscribe()
	defer cancel()

	a := q.Register("send-email", "github://x/y", "sess-1", map[string]any{"to": "alice"})

	select {
	case e := <-events:
		if e.Type != ActionApprovalEventPending {
			t.Errorf("event[0].Type = %q, want pending", e.Type)
		}
		if e.Pending == nil || e.Pending.ID != a.ID {
			t.Errorf("event[0].Pending = %+v, want id %s", e.Pending, a.ID)
		}
		if e.Resolved != nil {
			t.Errorf("event[0].Resolved should be nil for pending event")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for pending event")
	}

	if err := q.Decide(a.ID, false, "wrong recipient", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	select {
	case e := <-events:
		if e.Type != ActionApprovalEventResolved {
			t.Errorf("event[1].Type = %q, want resolved", e.Type)
		}
		if e.Resolved == nil || e.Resolved.ID != a.ID {
			t.Errorf("event[1].Resolved = %+v, want id %s", e.Resolved, a.ID)
		}
		if e.Resolved.Approved {
			t.Errorf("event[1].Resolved.Approved = true, want false")
		}
		if e.Resolved.Reason != "wrong recipient" {
			t.Errorf("event[1].Resolved.Reason = %q", e.Resolved.Reason)
		}
		if e.Pending != nil {
			t.Errorf("event[1].Pending should be nil for resolved event")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for resolved event")
	}
}

// TestActionApprovalQueue_SubscribeFanOut asserts that two
// independent subscribers each receive every event. This is the
// shape that holds when multiple webapp tabs are open at once: each
// tab opens its own SSE connection and must see the same updates.
func TestActionApprovalQueue_SubscribeFanOut(t *testing.T) {
	q := NewActionApprovalQueue(nil, nil)
	a, cancelA := q.Subscribe()
	defer cancelA()
	b, cancelB := q.Subscribe()
	defer cancelB()

	q.Register("send-email", "x", "", nil)
	for i, ch := range []<-chan ActionApprovalEvent{a, b} {
		select {
		case e := <-ch:
			if e.Type != ActionApprovalEventPending {
				t.Errorf("subscriber %d: Type = %q, want pending", i, e.Type)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d: timed out on pending", i)
		}
	}
}

// TestActionApprovalQueue_SubscribeCancelStopsDelivery asserts that
// the cancel returned by Subscribe removes the subscriber and closes
// the channel. The SSE handler relies on this for cleanup on client
// disconnect — a leaked subscriber would buffer forever and
// (eventually, after broadcast's drop semantics) be silently dropped
// from but never garbage collected.
func TestActionApprovalQueue_SubscribeCancelStopsDelivery(t *testing.T) {
	q := NewActionApprovalQueue(nil, nil)
	events, cancel := q.Subscribe()
	cancel()
	// Channel should be closed.
	if _, ok := <-events; ok {
		t.Errorf("channel still open after cancel; want closed")
	}
	// Calling cancel a second time must not panic — defensive against
	// the SSE handler's `defer cancel()` racing with an explicit
	// cancel inside the loop.
	cancel()
	// New events after cancel are silently dropped; Register must
	// still succeed.
	q.Register("a", "x", "", nil)
}

// TestActionApprovalQueue_SubscribeDropsWhenSlow asserts the
// broadcast contract: if a subscriber stops draining its channel,
// further events are dropped for that subscriber rather than
// blocking Register / Decide. The buffer is finite; flooding past
// it must not stall the runtime.
func TestActionApprovalQueue_SubscribeDropsWhenSlow(t *testing.T) {
	q := NewActionApprovalQueue(nil, nil)
	_, cancel := q.Subscribe()
	defer cancel()

	// Send more events than the buffer (32) without draining; broadcast
	// must not block. If it does, this test will hang — the test
	// timeout is the assertion.
	for i := 0; i < actionApprovalSubscriberBuffer*2; i++ {
		q.Register("a", "x", "", nil)
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
			if err := q.Decide(a.ID, true, "", nil); err != nil {
				t.Errorf("Decide(%s): %v", a.ID, err)
			}
		}(approvals[i])
	}
	wg.Wait()

	if got := q.List(); len(got) != 0 {
		t.Errorf("after concurrent Decide of all entries, List len = %d, want 0", len(got))
	}
}

