package runtime

import "fmt"

// PendingApprovalError reports that an effect-gated action's Approver returned
// Decision.Pending: no decision has landed yet, so the run SUSPENDS at this
// step rather than blocking (#2100). It is a sentinel the executor recognizes
// and converts into a SuspendResult without re-deriving the approval request.
// Unlike DenyError, it is NOT a run failure: the executor unwinds to a suspend,
// and the effect never fires (dispatch short-circuits before Dispatch).
type PendingApprovalError struct {
	// ActionRef is the action awaiting a decision.
	ActionRef string
	// Effect is the operation effect that routed this action to approval.
	Effect Effect
	// Args is the redacted args summary the approver was shown, carried through
	// so the suspend result presents the same request without re-deriving it.
	Args map[string]any
}

func (e *PendingApprovalError) Error() string {
	return fmt.Sprintf("flightplan: action %q awaits an approval decision (run suspended)", e.ActionRef)
}

// requiresApproval reports whether an effect routes through the out-of-band
// approval channel (ADR-0009). A read observes state and runs unattended;
// write, delete, spend, and external-send all mutate state, money, or reach a
// third party, so they raise an approval gate and block on the decision.
//
// This is the single effect→route decision point. Keeping it one function
// means the routing rule is stated once and the executor reads it, never
// re-deriving it per call site.
func requiresApproval(effect Effect) bool {
	switch effect {
	case EffectRead:
		return false
	case EffectWrite, EffectDelete, EffectSpend, EffectExternalSend:
		return true
	default:
		// An unknown effect should never reach here (decode validates the
		// closed enum). Fail safe: require approval rather than run unattended.
		return true
	}
}
