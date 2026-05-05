package approval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// ActionApproval is a pending request to gate one action invocation.
//
// Distinct from the rich governance flow modeled by the [Orchestrator]
// SPI (intents → multi-approver workflows → execution grants). This is
// the runtime-blocking shape for "ask the user yes or no before this
// specific action runs" — single user, single decision, the answer
// unblocks a held-open action-run HTTP response. Applies to actions
// whose manifest declares `[approval] required = true`
// (see action.Manifest.ApprovalRequired).
//
// Convergence with the rich Orchestrator surface is post-MVP and
// belongs in #418's webapp work; for v0.x the simpler queue keeps
// the runtime-block path easy to reason about.
type ActionApproval struct {
	// ID is opaque, server-minted, unique within this process. Embedded
	// in the agent-facing tool-call hold and in webapp/CLI list views.
	ID string

	// ActionName is the manifest name of the gated action (e.g.
	// "send-email"). Read-only after Register.
	ActionName string

	// ConnectorFQN is the connector the action's first execute step
	// targets — useful in the user surface for "send_email via
	// github://ALRubinger/aileron-connector-google". Read-only.
	ConnectorFQN string

	// Args are the call-time arguments the agent passed in. Surfaced
	// to the user so they can see what would actually be sent. Read-only.
	Args map[string]any

	// SessionID is the launch session that initiated the request,
	// when one is in scope. Empty for daemon-direct callers.
	SessionID string

	// RequestedAt is when the queue minted this request. Read-only.
	RequestedAt time.Time

	// decision is closed when Decide resolves the approval; the
	// caller listening on it receives the user's verdict. Buffered
	// so a Decide call never blocks even if the runtime moved on
	// (e.g. context cancelled before the user answered).
	decision chan ActionDecision
}

// ActionDecision is the user's verdict on an [ActionApproval].
type ActionDecision struct {
	// Approved is true when the user permits the action to run.
	Approved bool

	// Reason is optional commentary the user attached to the deny
	// path (e.g. "wrong recipient"). Empty for approve.
	Reason string

	// DecidedAt is when Decide was called.
	DecidedAt time.Time
}

// Wait blocks until the user decides, the timeout fires, or ctx is
// done. Returns the user's decision on resolution; an error wrapping
// [ErrActionApprovalTimeout] on timeout; ctx.Err() if the caller's
// context cancels first.
//
// The runtime calls this from its action-run handler while the
// HTTP client (the agent's MCP server, ultimately) waits on the
// held-open response. When this returns, the handler proceeds to
// either execute the action (Approved) or write an `approval_denied`
// failure envelope to the response.
func (a *ActionApproval) Wait(ctx context.Context, timeout time.Duration) (ActionDecision, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case d := <-a.decision:
		return d, nil
	case <-timer.C:
		return ActionDecision{}, ErrActionApprovalTimeout
	case <-ctx.Done():
		return ActionDecision{}, ctx.Err()
	}
}

// ErrActionApprovalTimeout is returned by [ActionApproval.Wait] when
// the user did not decide within the supplied timeout. The handler
// surfaces this to the agent as a structured `approval_timeout`
// failure envelope so the agent can recover gracefully.
var ErrActionApprovalTimeout = errors.New("action approval timed out")

// ErrActionApprovalNotFound is returned by [ActionApprovalQueue.Decide]
// when the requested approval id is unknown or already resolved.
var ErrActionApprovalNotFound = errors.New("action approval not found")

// ActionApprovalQueue is the in-memory store of pending action-level
// approvals. v0.x is per-process; restarts drop pending requests.
// Persistent backing parallels the audit-log persistence work in
// #412 — same architectural decision deferred for the same reasons.
//
// All methods are safe for concurrent use.
type ActionApprovalQueue struct {
	mu      sync.Mutex
	pending map[string]*ActionApproval

	// idGen mints opaque ids. Tests inject a deterministic generator;
	// production wiring uses a UUID-style helper.
	idGen func() string

	// now returns "now" for RequestedAt and DecidedAt. Tests inject a
	// fake clock.
	now func() time.Time

	// onRegister is invoked synchronously by Register after a new
	// pending entry has been added. Production wiring fires a desktop
	// notification + log line so the user knows to look at the webapp;
	// tests inject a recorder. Nil = no-op.
	//
	// The callback runs on the Register caller's goroutine, so the
	// runtime's RunAction is blocked behind it — it should be fast.
	// Side-effects that may block (network, slow shell-out) should
	// dispatch their own goroutine. Errors returned (or panics) are
	// not propagated to Register's caller; this is a notification path,
	// not a gating one.
	onRegister func(*ActionApproval)
}

// NewActionApprovalQueue returns an empty queue using the supplied
// id generator and clock. Pass nil for either to use the package
// defaults — useful in production wiring.
func NewActionApprovalQueue(idGen func() string, now func() time.Time) *ActionApprovalQueue {
	if idGen == nil {
		idGen = defaultActionApprovalID
	}
	if now == nil {
		now = time.Now
	}
	return &ActionApprovalQueue{
		pending: map[string]*ActionApproval{},
		idGen:   idGen,
		now:     now,
	}
}

// SetOnRegister installs a callback fired synchronously after each
// successful Register. Pass nil to clear. Safe to call concurrently
// with Register/List/Decide/Get; the swap is mutex-protected.
//
// Production wiring uses this to fire a desktop notification + log
// line on each new pending approval so the user knows to look at the
// webapp. Test code uses it to record call sequences.
func (q *ActionApprovalQueue) SetOnRegister(fn func(*ActionApproval)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.onRegister = fn
}

// Register creates and tracks a new pending approval. The returned
// pointer is the caller's handle for [ActionApproval.Wait]. When an
// onRegister callback has been installed via SetOnRegister, it is
// invoked synchronously after the entry is in the map and before
// Register returns. Callback errors / panics are not propagated.
func (q *ActionApprovalQueue) Register(actionName, connectorFQN, sessionID string, args map[string]any) *ActionApproval {
	q.mu.Lock()
	a := &ActionApproval{
		ID:           q.idGen(),
		ActionName:   actionName,
		ConnectorFQN: connectorFQN,
		Args:         args,
		SessionID:    sessionID,
		RequestedAt:  q.now(),
		decision:     make(chan ActionDecision, 1),
	}
	q.pending[a.ID] = a
	cb := q.onRegister
	q.mu.Unlock()
	if cb != nil {
		// Defer the panic recover so a misbehaving notifier never
		// takes down RunAction. Notifications are nice-to-have; the
		// queue's invariants must hold regardless.
		func() {
			defer func() { _ = recover() }()
			cb(a)
		}()
	}
	return a
}

// List returns the snapshot of currently pending approvals, ordered
// by RequestedAt ascending (oldest first). The slice is freshly
// allocated; the caller may mutate it without affecting the queue.
func (q *ActionApprovalQueue) List() []*ActionApproval {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]*ActionApproval, 0, len(q.pending))
	for _, a := range q.pending {
		out = append(out, a)
	}
	// Stable order: by RequestedAt ascending. Insertion order would
	// be cheaper but maps don't preserve it; sorting by a stable
	// timestamp keeps the user surface deterministic across calls.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].RequestedAt.After(out[j].RequestedAt); j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Get returns the pending approval for id, or nil + false when the id
// is unknown or already resolved.
func (q *ActionApprovalQueue) Get(id string) (*ActionApproval, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	a, ok := q.pending[id]
	return a, ok
}

// Decide resolves a pending approval with the user's verdict. The
// pending entry is removed atomically — a second Decide call on the
// same id returns ErrActionApprovalNotFound. The runtime's blocked
// Wait unblocks on the next tick; the channel is buffered so this
// call never blocks even if the runtime has already moved on (ctx
// cancelled, timeout fired) before the user clicked.
func (q *ActionApprovalQueue) Decide(id string, approved bool, reason string) error {
	q.mu.Lock()
	a, ok := q.pending[id]
	if !ok {
		q.mu.Unlock()
		return ErrActionApprovalNotFound
	}
	delete(q.pending, id)
	q.mu.Unlock()
	a.decision <- ActionDecision{
		Approved:  approved,
		Reason:    reason,
		DecidedAt: q.now(),
	}
	return nil
}

// defaultActionApprovalID returns an opaque identifier suitable for
// log scraping. Eight bytes of crypto/rand suffix keep ids unique
// under tight loops without depending on the wall clock having
// sub-microsecond resolution. Not security-relevant — the surface is
// localhost-only at v0.x; randomness is only here to prevent
// register-time collisions when two requests land in the same
// timestamp tick.
func defaultActionApprovalID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail on the supported platforms.
		// If it ever does, fall back to a timestamp-only id and
		// accept the (very small) collision risk; never panic on a
		// startup-path helper.
		return "act-" + time.Now().UTC().Format("20060102T150405.000000000")
	}
	return "act-" + time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}
