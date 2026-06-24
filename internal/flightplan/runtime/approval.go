package runtime

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
