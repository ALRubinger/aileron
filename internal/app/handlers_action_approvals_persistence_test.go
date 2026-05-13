package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/action"
	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/approval"
	approvaljsonl "github.com/ALRubinger/aileron/internal/approval/jsonl"
)

// TestPersistence_RestartSurvival is the load-bearing acceptance test
// for #649: register an approval-gated action, simulate daemon
// shutdown by closing the JSONL store, reopen the store as a new
// daemon would on startup, and verify the action runs exactly once
// after the user decides via the new daemon. Covers the criteria
// "Pending and resolved approval entries persist…" and "Daemon
// startup replays the file into the queue; pending entries get fresh
// executor goroutines."
func TestPersistence_RestartSurvival(t *testing.T) {
	tmp := t.TempDir()
	jsonlPath := filepath.Join(tmp, "approvals.jsonl")

	// --- Daemon 1: register an approval, shut down. ---
	srv1, store1, _ := newPersistedTestServer(t, tmp, persistedTestOpts{
		actionFiles: map[string]string{"ship-update.md": actionsTestManifestApproval},
	})

	body := bytes.NewReader([]byte(`{"args":{"channel":"#engineering"}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()
	srv1.RunAction(rec, req, "ship-update")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first daemon RunAction: status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var pending api.ActionRunPendingResponse
	if err := json.NewDecoder(rec.Body).Decode(&pending); err != nil {
		t.Fatalf("decode pending: %v", err)
	}
	approvalID := pending.ApprovalId
	if approvalID == "" {
		t.Fatal("approval id missing from 202 body")
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	// --- File-level: the JSONL was written. ---
	contents, err := os.ReadFile(jsonlPath)
	if err != nil {
		t.Fatalf("read jsonl: %v", err)
	}
	if !bytes.Contains(contents, []byte(approvalID)) {
		t.Fatalf("approval id not persisted to jsonl; contents: %s", contents)
	}

	// --- Daemon 2: reopen the store; verify the entry replays. ---
	srv2, store2, tracker2 := newPersistedTestServer(t, tmp, persistedTestOpts{
		actionFiles: map[string]string{"ship-update.md": actionsTestManifestApproval},
	})
	t.Cleanup(func() { _ = store2.Close() })

	listed := srv2.actionApprovals.List()
	if len(listed) != 1 || listed[0].ID != approvalID {
		t.Fatalf("post-restart pending list = %+v, want one entry with id %q", listed, approvalID)
	}

	// User decides via the webapp on the new daemon → background
	// executor runs the action exactly once.
	if err := srv2.actionApprovals.Decide(approvalID, true, "", nil); err != nil {
		t.Fatalf("Decide on new daemon: %v", err)
	}
	waitForOutcome(t, srv2.actionApprovals, approvalID, approval.OutcomeCompleted)
	if got := atomic.LoadInt32(&tracker2.atomicCalls); got != 1 {
		t.Errorf("executor calls on second daemon = %d, want 1", got)
	}
}

// TestPersistence_CrashDuringExecutionMarksFailed asserts that an
// entry left in OutcomeRunning on disk (simulating kill -9 mid-
// execute) replays as OutcomeFailed with a reconciliation reason —
// the runtime refuses to auto-retry a non-idempotent action. Covers
// "Crash-during-execution test (kill -9 mid-flight)…".
func TestPersistence_CrashDuringExecutionMarksFailed(t *testing.T) {
	tmp := t.TempDir()
	jsonlPath := filepath.Join(tmp, "approvals.jsonl")

	// Seed the file with an entry persisted at OutcomeRunning, as if
	// the daemon had crashed after SetRunning but before
	// SetCompleted. Writing directly to the file (rather than going
	// through a queue) is the test's analog of "kill -9".
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	decidedAt := now.Add(time.Minute)
	rec := approval.PersistedRecord{
		ID:          "act-crashed",
		Kind:        approval.ApprovalKindAction,
		ActionName:  "ship-update",
		Args:        map[string]any{"channel": "#engineering"},
		RequestedAt: now,
		Status:      approval.OutcomeRunning,
		DecidedAt:   &decidedAt,
	}
	mustWriteJSONLRecord(t, jsonlPath, rec)

	srv, store, tracker := newPersistedTestServer(t, tmp, persistedTestOpts{
		actionFiles: map[string]string{"ship-update.md": actionsTestManifestApproval},
	})
	t.Cleanup(func() { _ = store.Close() })

	outcome, ok := srv.actionApprovals.Outcome("act-crashed")
	if !ok {
		t.Fatal("crashed entry missing from queue after restart")
	}
	if outcome.Status != approval.OutcomeFailed {
		t.Errorf("status = %q, want %q", outcome.Status, approval.OutcomeFailed)
	}
	if outcome.ErrorMessage == "" {
		t.Error("ErrorMessage empty; want a reconciliation reason")
	}
	if got := atomic.LoadInt32(&tracker.atomicCalls); got != 0 {
		t.Errorf("executor invoked %d times during replay; want 0 (no auto-retry)", got)
	}
}

// TestPersistence_VaultLockedAtReplayBlocksExecutor asserts that a
// non-terminal entry replayed while the local vault is locked parks
// the executor goroutine in OutcomeAwaitingVault until the vault-
// unlock signal closes — then the goroutine wakes, SetRunning
// fires, and the action runs. Covers "Executor goroutines block on
// vault unlock when the vault is locked at replay."
func TestPersistence_VaultLockedAtReplayBlocksExecutor(t *testing.T) {
	tmp := t.TempDir()
	jsonlPath := filepath.Join(tmp, "approvals.jsonl")

	// Seed an approved-not-started entry: the user approved before
	// the previous daemon shut down.
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	decidedAt := now.Add(time.Minute)
	rec := approval.PersistedRecord{
		ID:          "act-vault",
		Kind:        approval.ApprovalKindAction,
		ActionName:  "ship-update",
		Args:        map[string]any{"channel": "#engineering"},
		RequestedAt: now,
		Status:      approval.OutcomeApprovedNotStarted,
		DecidedAt:   &decidedAt,
	}
	mustWriteJSONLRecord(t, jsonlPath, rec)

	// Start the daemon with vault locked. The respawn loop spawns an
	// executor goroutine that finds the vault locked and parks on
	// the unlock signal.
	srv, store, tracker := newPersistedTestServer(t, tmp, persistedTestOpts{
		actionFiles: map[string]string{"ship-update.md": actionsTestManifestApproval},
		vaultLocked: true,
	})
	t.Cleanup(func() { _ = store.Close() })

	waitForOutcome(t, srv.actionApprovals, "act-vault", approval.OutcomeAwaitingVault)
	if got := atomic.LoadInt32(&tracker.atomicCalls); got != 0 {
		t.Errorf("executor calls before unlock = %d, want 0", got)
	}

	// Simulate vault unlock — close the signal channel and flip the
	// state flag, matching what UnlockLocalVault does on success.
	srv.localVaultMu.Lock()
	srv.vaultLocked = false
	close(srv.vaultUnlockedCh)
	srv.localVaultMu.Unlock()

	waitForOutcome(t, srv.actionApprovals, "act-vault", approval.OutcomeCompleted)
	if got := atomic.LoadInt32(&tracker.atomicCalls); got != 1 {
		t.Errorf("executor calls after unlock = %d, want 1", got)
	}
}

// TestPersistence_RunningPersistedBeforeExecute asserts that
// SetRunning's persisted record is written BEFORE the action runs —
// the load-bearing invariant that makes a crash mid-execute
// observable on replay. Without it, the entry would still show
// approved_not_started on disk and the replay path would auto-rerun
// a non-idempotent action.
func TestPersistence_RunningPersistedBeforeExecute(t *testing.T) {
	tmp := t.TempDir()
	jsonlPath := filepath.Join(tmp, "approvals.jsonl")

	be := &blockingExecutor{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	srv, store, _ := newPersistedTestServer(t, tmp, persistedTestOpts{
		actionFiles: map[string]string{"ship-update.md": actionsTestManifestApproval},
		executor:    be,
	})
	t.Cleanup(func() { _ = store.Close() })

	body := bytes.NewReader([]byte(`{"args":{"channel":"#eng"}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")
	var pending api.ActionRunPendingResponse
	_ = json.NewDecoder(rec.Body).Decode(&pending)
	if pending.ApprovalId == "" {
		t.Fatalf("missing approval id; body=%s", rec.Body.String())
	}

	if err := srv.actionApprovals.Decide(pending.ApprovalId, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	select {
	case <-be.started:
	case <-time.After(2 * time.Second):
		t.Fatal("executor never entered Execute")
	}

	// At this point the file MUST contain a `running` record for the
	// approval id — SetRunning fires before Execute is called.
	if !fileContainsStatus(t, jsonlPath, pending.ApprovalId, approval.OutcomeRunning) {
		t.Errorf("jsonl missing running record for %q while Execute is in flight", pending.ApprovalId)
	}
	close(be.unblock)
	waitForOutcome(t, srv.actionApprovals, pending.ApprovalId, approval.OutcomeCompleted)
}

// TestPersistence_ManifestMissingFailsLoad asserts that a replayed
// approval whose action manifest has been uninstalled is marked
// failed (rather than left to crash in the executor goroutine). The
// failure message names the situation so the user can reconcile.
func TestPersistence_ManifestMissingFailsLoad(t *testing.T) {
	tmp := t.TempDir()
	jsonlPath := filepath.Join(tmp, "approvals.jsonl")

	rec := approval.PersistedRecord{
		ID:          "act-orphan",
		Kind:        approval.ApprovalKindAction,
		ActionName:  "uninstalled-action",
		RequestedAt: time.Now().UTC(),
		Status:      approval.OutcomePendingApproval,
	}
	mustWriteJSONLRecord(t, jsonlPath, rec)

	srv, store, _ := newPersistedTestServer(t, tmp, persistedTestOpts{
		// No action manifest installed — the replay path can't
		// resolve "uninstalled-action".
		actionFiles: map[string]string{},
	})
	t.Cleanup(func() { _ = store.Close() })

	// The respawn loop runs synchronously inside our test helper.
	// The orphaned entry should land in OutcomeFailed.
	o, ok := srv.actionApprovals.Outcome("act-orphan")
	if !ok {
		t.Fatal("orphan entry missing from queue")
	}
	if o.Status != approval.OutcomeFailed {
		t.Errorf("status = %q, want %q", o.Status, approval.OutcomeFailed)
	}
	if o.ErrorMessage == "" {
		t.Error("ErrorMessage empty for orphan")
	}
}

// --- Test helpers ---

// persistedTestOpts captures the optional knobs the persistence
// tests use to dial in their scenarios. The zero value gives an
// unlocked vault, a counting executor, and an empty action dir.
type persistedTestOpts struct {
	actionFiles map[string]string
	executor    action.Executor // defaults to a fresh countingExecutor
	vaultLocked bool            // when true, vaultUnlockedCh stays open
}

// newPersistedTestServer wires the apiServer with a real
// [approvaljsonl.Store] anchored at tmp/approvals.jsonl, reaps any
// pre-existing running entries, loads records into the queue, and
// respawns executor goroutines for non-terminal entries — the same
// dance NewHandlerWithConfig performs at daemon startup. The vault
// starts locked or unlocked per opts.
func newPersistedTestServer(t *testing.T, tmp string, opts persistedTestOpts) (*apiServer, *approvaljsonl.Store, *atomicCountingExecutor) {
	t.Helper()
	dir := filepath.Join(tmp, "actions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir actions: %v", err)
	}
	for name, body := range opts.actionFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	actStore := action.NewStore(dir)
	if _, err := actStore.Load(); err != nil {
		t.Fatalf("Load actions: %v", err)
	}

	jsonlPath := filepath.Join(tmp, "approvals.jsonl")
	store, records, err := approvaljsonl.New(jsonlPath)
	if err != nil {
		t.Fatalf("approval/jsonl.New: %v", err)
	}
	// Reap running entries — same policy the daemon's main applies.
	for i, rec := range records {
		if rec.Status != approval.OutcomeRunning {
			continue
		}
		reaped := rec
		reaped.Status = approval.OutcomeFailed
		reaped.ErrorMessage = "daemon crashed during execution; manual reconciliation required"
		if err := store.Append(reaped); err != nil {
			t.Fatalf("persist reap: %v", err)
		}
		records[i] = reaped
	}

	q := approval.NewActionApprovalQueue(nil, nil)
	q.SetPersister(store)
	q.LoadRecords(records)

	// Pick an executor. The tracker shim lets tests query call counts
	// without caring whether the executor is plain-counting or
	// blocking-counting.
	var tracker *atomicCountingExecutor
	executor := opts.executor
	if executor == nil {
		ace := &atomicCountingExecutor{}
		executor = ace
		tracker = ace
	} else {
		// For non-default executors the caller is responsible for any
		// tracking they want; produce a no-op tracker so tests that
		// don't care can ignore the third return value.
		switch e := executor.(type) {
		case *atomicCountingExecutor:
			tracker = e
		case *blockingExecutor:
			tracker = &e.atomicCountingExecutor
		default:
			tracker = &atomicCountingExecutor{}
		}
	}

	vaultUnlockedCh := make(chan struct{})
	if !opts.vaultLocked {
		close(vaultUnlockedCh)
	}

	srv := &apiServer{
		log:             slog.New(slog.NewJSONHandler(io.Discard, nil)),
		actions:         actStore,
		executor:        executor,
		newID:           func() string { return "audit_test_id" },
		actionApprovals: q,
		vaultLocked:     opts.vaultLocked,
		vaultUnlockedCh: vaultUnlockedCh,
	}

	// Respawn executor goroutines for non-terminal entries — same as
	// the wiring code in NewHandlerWithConfig.
	for _, id := range q.NonTerminalLoaded() {
		entry, ok := q.Get(id)
		if !ok {
			continue
		}
		loaded, err := actStore.Get(entry.ActionName)
		if err != nil {
			_ = q.SetFailed(entry.ID, nil, "action manifest no longer exists; cannot resume after daemon restart")
			continue
		}
		connFQN := entry.ConnectorFQN
		if connFQN == "" && len(loaded.Manifest.Execute) > 0 {
			connFQN = loaded.Manifest.Execute[0].Connector
		}
		go srv.executeApprovedAction(entry, entry.ActionName, connFQN, entry.Args, entry.SessionID)
	}
	return srv, store, tracker
}

func waitForOutcome(t *testing.T, q *approval.ActionApprovalQueue, id string, want approval.OutcomeStatus) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		o, ok := q.Outcome(id)
		if ok && o.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	o, _ := q.Outcome(id)
	t.Fatalf("waitForOutcome(%s): got %q, want %q", id, o.Status, want)
}

func mustWriteJSONLRecord(t *testing.T, path string, rec approval.PersistedRecord) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open jsonl: %v", err)
	}
	defer f.Close()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// fileContainsStatus scans the JSONL file for a record matching id
// at the requested status. Used by tests that want to assert "the
// transition was on disk by this point" without depending on
// last-write-wins replay semantics.
func fileContainsStatus(t *testing.T, path, id string, status approval.OutcomeStatus) bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read jsonl: %v", err)
	}
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var rec approval.PersistedRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.ID == id && rec.Status == status {
			return true
		}
	}
	return false
}

// atomicCountingExecutor mirrors countingExecutor but uses an int32
// + sync/atomic so concurrent goroutines in the persistence tests
// can safely read the call count.
type atomicCountingExecutor struct {
	atomicCalls int32
}

func (a *atomicCountingExecutor) Execute(_ context.Context, _ string, _ map[string]any) (action.Result, error) {
	atomic.AddInt32(&a.atomicCalls, 1)
	return action.Result{Content: `{"ok":true}`}, nil
}

// blockingExecutor blocks in Execute on `unblock` so tests can
// inspect intermediate state. Embeds atomicCountingExecutor so the
// call count assertions work across goroutines.
type blockingExecutor struct {
	atomicCountingExecutor
	started chan struct{}
	unblock chan struct{}
}

func (b *blockingExecutor) Execute(_ context.Context, _ string, _ map[string]any) (action.Result, error) {
	atomic.AddInt32(&b.atomicCalls, 1)
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-b.unblock
	return action.Result{Content: `{"ok":true}`}, nil
}
