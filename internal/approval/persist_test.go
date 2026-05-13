package approval_test

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/approval"
	"github.com/ALRubinger/aileron/internal/failure"
)

// recordingPersister captures every Append in declaration order for
// test assertion. Concurrent-safe so the queue's broadcast goroutine
// can race with the test thread without false negatives.
type recordingPersister struct {
	mu      sync.Mutex
	records []approval.PersistedRecord
}

func (r *recordingPersister) Append(rec approval.PersistedRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Copy maps so later mutations in the queue don't change what we
	// recorded. encoding/json would do this implicitly for a JSONL
	// persister; the test fake needs to do it explicitly.
	if rec.Args != nil {
		args := make(map[string]any, len(rec.Args))
		for k, v := range rec.Args {
			args[k] = v
		}
		rec.Args = args
	}
	r.records = append(r.records, rec)
	return nil
}

func (r *recordingPersister) snapshot() []approval.PersistedRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]approval.PersistedRecord, len(r.records))
	copy(out, r.records)
	return out
}

// erroringPersister always errors. Used to assert the queue tolerates
// persist failures (logs but does not crash; in-memory state remains
// correct).
type erroringPersister struct{}

func (erroringPersister) Append(approval.PersistedRecord) error {
	return errors.New("disk full")
}

// TestQueue_PersistsEveryTransition is the load-bearing assertion for
// "Pending and resolved approval entries persist to ~/.aileron/
// approvals.jsonl on every state transition" — the queue must call
// Append for each of: Register, Decide, SetRunning, SetCompleted.
func TestQueue_PersistsEveryTransition(t *testing.T) {
	rp := &recordingPersister{}
	q := approval.NewActionApprovalQueue(nil, nil)
	q.SetPersister(rp)

	a := q.Register("send-email", "github://x/y", "sess-1", map[string]any{"to": "alice"})
	if err := q.Decide(a.ID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if err := q.SetRunning(a.ID); err != nil {
		t.Fatalf("SetRunning: %v", err)
	}
	if err := q.SetCompleted(a.ID, "audit-1", `{"sent":true}`); err != nil {
		t.Fatalf("SetCompleted: %v", err)
	}

	records := rp.snapshot()
	if len(records) != 4 {
		t.Fatalf("record count = %d, want 4", len(records))
	}
	want := []approval.OutcomeStatus{
		approval.OutcomePendingApproval,
		approval.OutcomeApprovedNotStarted,
		approval.OutcomeRunning,
		approval.OutcomeCompleted,
	}
	for i, w := range want {
		if records[i].Status != w {
			t.Errorf("records[%d].Status = %q, want %q", i, records[i].Status, w)
		}
		if records[i].ID != a.ID {
			t.Errorf("records[%d].ID = %q, want %q", i, records[i].ID, a.ID)
		}
	}
	// The completed record should carry audit_id and result.
	if records[3].AuditID != "audit-1" {
		t.Errorf("completed.AuditID = %q", records[3].AuditID)
	}
	if records[3].Result != `{"sent":true}` {
		t.Errorf("completed.Result = %q", records[3].Result)
	}
}

// TestQueue_PersistsDenial asserts the deny path lands on disk too —
// the user's deny commentary is the kind of forensic data that
// matters across daemon restart.
func TestQueue_PersistsDenial(t *testing.T) {
	rp := &recordingPersister{}
	q := approval.NewActionApprovalQueue(nil, nil)
	q.SetPersister(rp)

	a := q.Register("send-email", "", "", nil)
	if err := q.Decide(a.ID, false, "wrong recipient", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	records := rp.snapshot()
	if len(records) != 2 {
		t.Fatalf("record count = %d, want 2", len(records))
	}
	if records[1].Status != approval.OutcomeDenied {
		t.Errorf("status = %q, want denied", records[1].Status)
	}
	if records[1].DenyReason != "wrong recipient" {
		t.Errorf("deny_reason = %q", records[1].DenyReason)
	}
	if records[1].DecidedAt == nil {
		t.Errorf("decided_at = nil, want non-nil")
	}
}

// TestQueue_PersistsFailedFailure asserts that a structured failure
// envelope round-trips through the persistence layer.
func TestQueue_PersistsFailedFailure(t *testing.T) {
	rp := &recordingPersister{}
	q := approval.NewActionApprovalQueue(nil, nil)
	q.SetPersister(rp)

	a := q.Register("send-email", "", "", nil)
	_ = q.Decide(a.ID, true, "", nil)
	_ = q.SetRunning(a.ID)
	f := failure.Network("dns lookup failed")
	if err := q.SetFailed(a.ID, f, ""); err != nil {
		t.Fatalf("SetFailed: %v", err)
	}
	records := rp.snapshot()
	last := records[len(records)-1]
	if last.Status != approval.OutcomeFailed {
		t.Errorf("status = %q", last.Status)
	}
	if last.Failure == nil {
		t.Fatal("failure envelope = nil")
	}
	if last.Failure.Error.Class != failure.NetworkError {
		t.Errorf("class = %q", last.Failure.Error.Class)
	}
	if last.Failure.Error.Message != "dns lookup failed" {
		t.Errorf("message = %q", last.Failure.Error.Message)
	}
}

// TestQueue_PersistFailureDoesNotBreakState asserts the in-memory
// invariants hold when the persister errors — the queue surfaces the
// log line but the state machine is unaffected.
func TestQueue_PersistFailureDoesNotBreakState(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	q.SetPersister(erroringPersister{})

	a := q.Register("send-email", "", "", nil)
	if a.ID == "" {
		t.Fatal("Register returned empty id")
	}
	if err := q.Decide(a.ID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	o, ok := q.Outcome(a.ID)
	if !ok {
		t.Fatal("Outcome missing after Decide")
	}
	if o.Status != approval.OutcomeApprovedNotStarted {
		t.Errorf("status = %q, want approved_not_started", o.Status)
	}
}

// TestLoadRecords_ApprovedRespawnsWithPrefilledChannel asserts that a
// replay-loaded approved entry's Wait returns immediately with an
// approved decision — that's how a respawned executor goroutine
// resumes without the user having to re-approve.
func TestLoadRecords_ApprovedRespawnsWithPrefilledChannel(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	decidedAt := now.Add(time.Minute)
	q.LoadRecords([]approval.PersistedRecord{
		{
			ID:          "act-1",
			Kind:        approval.ApprovalKindAction,
			ActionName:  "send-email",
			Args:        map[string]any{"to": "alice"},
			RequestedAt: now,
			Status:      approval.OutcomeApprovedNotStarted,
			DecidedAt:   &decidedAt,
		},
	})

	entry, ok := q.Get("act-1")
	if !ok {
		t.Fatal("entry missing after LoadRecords")
	}
	d, err := entry.Wait(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !d.Approved {
		t.Error("decision.Approved = false, want true")
	}
	if !d.DecidedAt.Equal(decidedAt) {
		t.Errorf("decided_at = %v, want %v", d.DecidedAt, decidedAt)
	}
}

// TestLoadRecords_PendingDoesNotPrefillChannel asserts replay of an
// undecided entry leaves the channel empty so a respawned executor
// blocks on the user's decision via the webapp.
func TestLoadRecords_PendingDoesNotPrefillChannel(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	q.LoadRecords([]approval.PersistedRecord{
		{
			ID:          "act-1",
			Kind:        approval.ApprovalKindAction,
			ActionName:  "send-email",
			RequestedAt: time.Now().UTC(),
			Status:      approval.OutcomePendingApproval,
		},
	})
	entry, ok := q.Get("act-1")
	if !ok {
		t.Fatal("entry missing")
	}
	_, err := entry.Wait(context.Background(), 50*time.Millisecond)
	if err == nil {
		t.Error("Wait returned without timeout; decision channel should be empty")
	}
}

// TestLoadRecords_TerminalEntriesAreSurfacedViaOutcome asserts that
// completed/denied/failed entries reload with their full payload so
// /v1/action-approvals/{id}/result can serve them across restart.
func TestLoadRecords_TerminalEntriesAreSurfacedViaOutcome(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	env := failure.ToEnvelope(failure.Network("dns failure"))
	q.LoadRecords([]approval.PersistedRecord{
		{
			ID:           "act-1",
			ActionName:   "send-email",
			RequestedAt:  time.Now().UTC(),
			Status:       approval.OutcomeCompleted,
			AuditID:      "audit-1",
			Result:       `{"sent":true}`,
		},
		{
			ID:          "act-2",
			ActionName:  "send-email",
			RequestedAt: time.Now().UTC(),
			Status:      approval.OutcomeDenied,
			DenyReason:  "wrong recipient",
		},
		{
			ID:          "act-3",
			ActionName:  "send-email",
			RequestedAt: time.Now().UTC(),
			Status:      approval.OutcomeFailed,
			Failure:     &env,
		},
	})
	cases := []struct {
		id     string
		status approval.OutcomeStatus
	}{
		{"act-1", approval.OutcomeCompleted},
		{"act-2", approval.OutcomeDenied},
		{"act-3", approval.OutcomeFailed},
	}
	for _, c := range cases {
		o, ok := q.Outcome(c.id)
		if !ok {
			t.Errorf("%s: Outcome missing", c.id)
			continue
		}
		if o.Status != c.status {
			t.Errorf("%s: status = %q, want %q", c.id, o.Status, c.status)
		}
	}
	// Failure round-trip preserves class + message.
	o, _ := q.Outcome("act-3")
	if o.Failure == nil {
		t.Fatal("act-3 failure = nil")
	}
	if o.Failure.Class() != failure.NetworkError {
		t.Errorf("class = %q", o.Failure.Class())
	}
}

// TestNonTerminalLoaded_ReturnsRespawnSet asserts the helper that the
// wiring layer uses to find entries needing executor respawn.
func TestNonTerminalLoaded_ReturnsRespawnSet(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	q.LoadRecords([]approval.PersistedRecord{
		{ID: "act-pending", ActionName: "a", RequestedAt: now, Status: approval.OutcomePendingApproval},
		{ID: "act-approved", ActionName: "a", RequestedAt: now.Add(time.Second), Status: approval.OutcomeApprovedNotStarted},
		{ID: "act-vault", ActionName: "a", RequestedAt: now.Add(2 * time.Second), Status: approval.OutcomeAwaitingVault},
		{ID: "act-done", ActionName: "a", RequestedAt: now.Add(3 * time.Second), Status: approval.OutcomeCompleted},
		{ID: "act-denied", ActionName: "a", RequestedAt: now.Add(4 * time.Second), Status: approval.OutcomeDenied},
		{ID: "act-failed", ActionName: "a", RequestedAt: now.Add(5 * time.Second), Status: approval.OutcomeFailed},
	})
	got := q.NonTerminalLoaded()
	sort.Strings(got)
	want := []string{"act-approved", "act-pending", "act-vault"}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NonTerminalLoaded() = %v, want %v", got, want)
	}
}

// TestSetRunning_FromAwaitingVaultIsAllowed asserts the executor's
// after-vault-unlock transition path: an entry left in
// OutcomeAwaitingVault must accept SetRunning so the executor can
// proceed past the unlock wait.
func TestSetRunning_FromAwaitingVaultIsAllowed(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	q.LoadRecords([]approval.PersistedRecord{
		{ID: "act-1", ActionName: "a", RequestedAt: time.Now().UTC(), Status: approval.OutcomeAwaitingVault},
	})
	if err := q.SetRunning("act-1"); err != nil {
		t.Fatalf("SetRunning: %v", err)
	}
	o, _ := q.Outcome("act-1")
	if o.Status != approval.OutcomeRunning {
		t.Errorf("status = %q", o.Status)
	}
}

// TestSetRunning_UnknownIDReturnsNotFound covers the queue's
// missing-entry branch — a goroutine racing replay with a
// post-Close re-open could call SetRunning for an id the new queue
// hasn't loaded yet; the error must be surfaced rather than masked
// as success.
func TestSetRunning_UnknownIDReturnsNotFound(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	err := q.SetRunning("act-does-not-exist")
	if !errors.Is(err, approval.ErrActionApprovalNotFound) {
		t.Errorf("err = %v, want ErrActionApprovalNotFound", err)
	}
}

// TestSetRunning_OnTerminalIsNoop ensures the queue's single-shot
// terminal contract holds — SetRunning on a completed entry doesn't
// flip status back to running.
func TestSetRunning_OnTerminalIsNoop(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	a := q.Register("send-email", "", "", nil)
	_ = q.Decide(a.ID, true, "", nil)
	_ = q.SetRunning(a.ID)
	_ = q.SetCompleted(a.ID, "audit-1", "")
	if err := q.SetRunning(a.ID); err != nil {
		t.Errorf("SetRunning on completed: %v", err)
	}
	o, _ := q.Outcome(a.ID)
	if o.Status != approval.OutcomeCompleted {
		t.Errorf("status = %q, want completed (SetRunning must not flip terminal back)", o.Status)
	}
}

// TestSetRunning_IdempotentFromRunning asserts a repeated SetRunning
// call (e.g. respawn retries) is a no-op rather than an error.
func TestSetRunning_IdempotentFromRunning(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	a := q.Register("send-email", "", "", nil)
	_ = q.Decide(a.ID, true, "", nil)
	if err := q.SetRunning(a.ID); err != nil {
		t.Fatalf("first SetRunning: %v", err)
	}
	if err := q.SetRunning(a.ID); err != nil {
		t.Errorf("second SetRunning: %v", err)
	}
	o, _ := q.Outcome(a.ID)
	if o.Status != approval.OutcomeRunning {
		t.Errorf("status = %q, want running", o.Status)
	}
}

// TestSetPersister_NilRevertsToNoop asserts passing nil restores
// the queue's in-memory-only behavior — a swap-back path that test
// suites rely on when they want to disable persistence mid-run.
func TestSetPersister_NilRevertsToNoop(t *testing.T) {
	rp := &recordingPersister{}
	q := approval.NewActionApprovalQueue(nil, nil)
	q.SetPersister(rp)
	a := q.Register("send-email", "", "", nil)
	if len(rp.snapshot()) != 1 {
		t.Fatalf("after Register: snapshot len = %d", len(rp.snapshot()))
	}
	q.SetPersister(nil)
	_ = q.Decide(a.ID, true, "", nil)
	if len(rp.snapshot()) != 1 {
		t.Errorf("Decide after nil persister wrote to recorder; len = %d", len(rp.snapshot()))
	}
}

// TestSetAwaitingVault_OnlyFromApprovedNotStarted asserts the new
// transition is gated by source state — calling on pending_approval
// must NOT silently downgrade the entry (a concurrent Decide could
// have raced; the executor's view should not corrupt the queue).
func TestSetAwaitingVault_OnlyFromApprovedNotStarted(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	a := q.Register("send-email", "", "", nil)
	// Pending state: SetAwaitingVault is a no-op.
	if err := q.SetAwaitingVault(a.ID); err != nil {
		t.Fatalf("SetAwaitingVault on pending: %v", err)
	}
	o, _ := q.Outcome(a.ID)
	if o.Status != approval.OutcomePendingApproval {
		t.Errorf("status after no-op = %q, want pending_approval", o.Status)
	}
	// Approve, then SetAwaitingVault: should flip.
	_ = q.Decide(a.ID, true, "", nil)
	if err := q.SetAwaitingVault(a.ID); err != nil {
		t.Fatalf("SetAwaitingVault on approved: %v", err)
	}
	o, _ = q.Outcome(a.ID)
	if o.Status != approval.OutcomeAwaitingVault {
		t.Errorf("status after flip = %q, want awaiting_vault", o.Status)
	}
}
