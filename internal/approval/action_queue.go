package approval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// ApprovalKind discriminates the user-facing card layout the webapp
// renders for a queue entry. Defaults to [ApprovalKindAction] for
// historic action-manifest gated calls; the comms-MCP send-shaped
// tools (#428) and the shell-shim approval surface (#427) register
// entries with their own kinds so the webapp can surface kind-
// specific fields (editable reply body, command + reason, method +
// URL + matched secret name).
//
// New kinds extend the enum; the default-when-empty fallback to
// [ApprovalKindAction] keeps older Register call sites compiling
// without forcing every caller to spell the kind out.
type ApprovalKind string

const (
	// ApprovalKindAction is the historic "an action declares
	// `[approval] required = true`" gate. Card layout: action name,
	// optional connector FQN, args dump, approve / deny.
	ApprovalKindAction ApprovalKind = "action"

	// ApprovalKindCommsSend is `aileron-mcp`'s `send_message` tool
	// (#428). Card layout: service + channel + message body, approve
	// / deny.
	ApprovalKindCommsSend ApprovalKind = "comms_send"

	// ApprovalKindCommsDraft is `aileron-mcp`'s `draft_reply` tool
	// (#428). Card layout: original message + draft reply (editable),
	// approve / approve-with-edits / discard.
	ApprovalKindCommsDraft ApprovalKind = "comms_draft"

	// ApprovalKindHTTPRequest is `aileron-mcp`'s `http_request` tool
	// (#428). Card layout: method + URL + body + which credential
	// would be injected (name only, never the value), approve / deny.
	ApprovalKindHTTPRequest ApprovalKind = "http_request"

	// ApprovalKindShell is the policy-enforced shell shim's per-
	// command approval (#427). aileron-sh dials the launch session's
	// approval socket when a command matches an `ask:` rule; the
	// launcher registers a [ApprovalKindShell] entry on this queue,
	// the webapp prompts the user with four options (approve once /
	// deny / approve and save for project / approve and save for me),
	// and the decision flows back over the socket as the wire string
	// aileron-sh expects. The "save" decisions ride through
	// [ActionDecision.EditedPayload] under the `save_policy` key
	// (`""` | `"project"` | `"user"`).
	ApprovalKindShell ApprovalKind = "shell"
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

	// Kind discriminates the user-facing card layout. Empty values
	// are treated as [ApprovalKindAction] by the webapp and the API
	// layer to keep older callers working without changes.
	Kind ApprovalKind

	// ActionName is the manifest name of the gated action (e.g.
	// "send-email"). For non-action kinds, carries a short label
	// describing what's being approved ("send_message", "draft_reply",
	// "http_request"). Read-only after Register.
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

	// EditedPayload carries kind-specific fields the user changed
	// before approving — most prominently the edited reply body for
	// [ApprovalKindCommsDraft] (`{ "body": "...new..." }`). Nil when
	// the user approved without edits, or when the kind doesn't
	// support editing. Surfaced to the dispatcher (e.g. CommsServer)
	// so the user-edited bytes are what actually go on the wire.
	EditedPayload map[string]any

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

// ActionApprovalEventType discriminates the union of events the queue
// publishes to subscribers. The webapp consumes these via SSE
// ([#418]) so the pending list updates live without polling.
//
// [#418]: https://github.com/ALRubinger/aileron/issues/418
type ActionApprovalEventType string

const (
	// ActionApprovalEventPending fires when [ActionApprovalQueue.Register]
	// adds a new pending entry. Event.Pending is the entry; Event.Resolved
	// is nil.
	ActionApprovalEventPending ActionApprovalEventType = "pending"

	// ActionApprovalEventResolved fires when [ActionApprovalQueue.Decide]
	// resolves an entry. Event.Resolved carries the user's verdict;
	// Event.Pending is nil.
	ActionApprovalEventResolved ActionApprovalEventType = "resolved"
)

// ActionApprovalEvent is the shape carried on the channel returned by
// [ActionApprovalQueue.Subscribe]. Exactly one of Pending / Resolved
// is populated per the Type discriminant.
type ActionApprovalEvent struct {
	Type     ActionApprovalEventType
	Pending  *ActionApproval
	Resolved *ResolvedActionApproval
}

// ResolvedActionApproval describes a queue entry that has been
// decided. Carried on [ActionApprovalEvent] so the webapp can drop
// the matching card without re-fetching the list.
type ResolvedActionApproval struct {
	ID        string
	Kind      ApprovalKind
	Approved  bool
	Reason    string
	DecidedAt time.Time
}

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

	// subscribers receive ActionApprovalEvent on Register / Decide.
	// Surface for SSE/streaming consumers (the webapp). Each subscriber
	// owns one channel; broadcast is non-blocking — a slow consumer
	// drops events past the buffer rather than back-pressuring Register
	// or Decide.
	subscribers map[chan ActionApprovalEvent]struct{}
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
		pending:     map[string]*ActionApproval{},
		idGen:       idGen,
		now:         now,
		subscribers: map[chan ActionApprovalEvent]struct{}{},
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

// Subscribe registers a streaming consumer of queue events and
// returns the receive channel plus a cancel function the caller MUST
// invoke on disconnect. The channel is buffered; if a subscriber
// stops draining it, broadcast drops events for that subscriber
// rather than blocking Register / Decide. Slow consumers can resync
// from [List] when they reconnect.
//
// Used by the SSE handler (`GET /v1/action-approvals/watch`, [#418])
// so the webapp's pending list updates live without polling. Each
// open SSE connection holds one subscription for the duration of the
// HTTP request.
//
// [#418]: https://github.com/ALRubinger/aileron/issues/418
func (q *ActionApprovalQueue) Subscribe() (<-chan ActionApprovalEvent, func()) {
	ch := make(chan ActionApprovalEvent, actionApprovalSubscriberBuffer)
	q.mu.Lock()
	q.subscribers[ch] = struct{}{}
	q.mu.Unlock()
	cancel := func() {
		q.mu.Lock()
		if _, ok := q.subscribers[ch]; ok {
			delete(q.subscribers, ch)
			close(ch)
		}
		q.mu.Unlock()
	}
	return ch, cancel
}

// actionApprovalSubscriberBuffer is the per-subscriber channel
// buffer. v0.x sees a handful of pending approvals at a time; 32
// keeps a slow webapp tab from missing events under any realistic
// load and is still small enough to bound memory under a runaway
// register storm.
const actionApprovalSubscriberBuffer = 32

// broadcast fans an event out to every current subscriber. Non-
// blocking: if a subscriber's buffer is full the event is dropped
// for that subscriber only — the queue's invariants and the runtime
// (Register / Decide) must not be held up by a slow webapp tab.
//
// Called with q.mu unheld so a misbehaving subscriber can't deadlock
// the queue; the snapshot of subscribers is taken under the lock.
func (q *ActionApprovalQueue) broadcast(e ActionApprovalEvent) {
	q.mu.Lock()
	subs := make([]chan ActionApprovalEvent, 0, len(q.subscribers))
	for ch := range q.subscribers {
		subs = append(subs, ch)
	}
	q.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- e:
		default:
			// Drop for this subscriber; they'll resync on reconnect.
		}
	}
}

// Register creates and tracks a new pending approval. The returned
// pointer is the caller's handle for [ActionApproval.Wait]. When an
// onRegister callback has been installed via SetOnRegister, it is
// invoked synchronously after the entry is in the map and before
// Register returns. Callback errors / panics are not propagated.
//
// Kind defaults to [ApprovalKindAction]. Use [RegisterKind] (or one
// of the typed wrappers like [RegisterCommsSend]) when the queue
// entry should render as something other than the default action card.
func (q *ActionApprovalQueue) Register(actionName, connectorFQN, sessionID string, args map[string]any) *ActionApproval {
	return q.RegisterKind(ApprovalKindAction, actionName, connectorFQN, sessionID, args)
}

// RegisterKind is the kind-aware Register. Production callers use the
// typed wrappers below; tests and callers in feature packages take
// this directly when they want full control of the queue entry's
// fields.
func (q *ActionApprovalQueue) RegisterKind(kind ApprovalKind, actionName, connectorFQN, sessionID string, args map[string]any) *ActionApproval {
	if kind == "" {
		kind = ApprovalKindAction
	}
	q.mu.Lock()
	a := &ActionApproval{
		ID:           q.idGen(),
		Kind:         kind,
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
	q.broadcast(ActionApprovalEvent{Type: ActionApprovalEventPending, Pending: a})
	return a
}

// RegisterCommsSend creates a [ApprovalKindCommsSend] entry (#428).
// The webapp renders a card with service / channel / body and an
// approve / deny pair; on approve, the CommsServer dispatches the
// message via the matched [comms.Listener].
func (q *ActionApprovalQueue) RegisterCommsSend(service, channel, body, sessionID string) *ActionApproval {
	args := map[string]any{
		"service": service,
		"channel": channel,
		"body":    body,
	}
	return q.RegisterKind(ApprovalKindCommsSend, "send_message", "", sessionID, args)
}

// RegisterCommsDraft creates a [ApprovalKindCommsDraft] entry (#428).
// The card surfaces the original incoming message alongside the
// draft body in an editable field; the user can approve as-is, edit
// then approve (the queue ships the edited body to the dispatcher
// via [ActionDecision.EditedPayload]), or discard.
func (q *ActionApprovalQueue) RegisterCommsDraft(service, channel, originalAuthor, originalBody, draftBody, replyTo, sessionID string) *ActionApproval {
	args := map[string]any{
		"service":         service,
		"channel":         channel,
		"original_author": originalAuthor,
		"original_body":   originalBody,
		"draft_body":      draftBody,
		"reply_to":        replyTo,
	}
	return q.RegisterKind(ApprovalKindCommsDraft, "draft_reply", "", sessionID, args)
}

// RegisterHTTPRequest creates a [ApprovalKindHTTPRequest] entry
// (#428). secretName is the name of the matched api_key binding the
// daemon would inject as a Bearer token, surfaced so the user can
// decide whether to authorise the call. The card never carries the
// secret value — only the binding name.
func (q *ActionApprovalQueue) RegisterHTTPRequest(method, url, body, secretName, sessionID string) *ActionApproval {
	args := map[string]any{
		"method":      method,
		"url":         url,
		"body":        body,
		"secret_name": secretName,
	}
	return q.RegisterKind(ApprovalKindHTTPRequest, "http_request", "", sessionID, args)
}

// RegisterShell creates a [ApprovalKindShell] entry (#427) for the
// policy-enforced shell shim's per-command approval. command is the
// literal command line aileron-sh saw; reason is the matched policy
// rule's description; cwd is the working directory the agent was in
// when the command ran. All three are surfaced verbatim on the
// webapp card.
//
// The launch-side [ApprovalSocketServer] consumes the resulting
// [ActionDecision]; on approve, it reads `save_policy` from
// [ActionDecision.EditedPayload] (`""` | `"project"` | `"user"`) to
// pick the wire string aileron-sh writes back as the rule's
// disposition.
func (q *ActionApprovalQueue) RegisterShell(command, reason, sessionID, cwd string) *ActionApproval {
	args := map[string]any{
		"command": command,
		"reason":  reason,
	}
	if cwd != "" {
		args["cwd"] = cwd
	}
	return q.RegisterKind(ApprovalKindShell, "shell", "", sessionID, args)
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
//
// editedPayload carries kind-specific fields the user changed before
// approving — e.g. the edited reply body for [ApprovalKindCommsDraft]
// (`{ "body": "...new..." }`). Pass nil when there are no edits.
func (q *ActionApprovalQueue) Decide(id string, approved bool, reason string, editedPayload map[string]any) error {
	q.mu.Lock()
	a, ok := q.pending[id]
	if !ok {
		q.mu.Unlock()
		return ErrActionApprovalNotFound
	}
	delete(q.pending, id)
	kind := a.Kind
	q.mu.Unlock()
	decidedAt := q.now()
	a.decision <- ActionDecision{
		Approved:      approved,
		Reason:        reason,
		EditedPayload: editedPayload,
		DecidedAt:     decidedAt,
	}
	q.broadcast(ActionApprovalEvent{
		Type: ActionApprovalEventResolved,
		Resolved: &ResolvedActionApproval{
			ID:        id,
			Kind:      kind,
			Approved:  approved,
			Reason:    reason,
			DecidedAt: decidedAt,
		},
	})
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
