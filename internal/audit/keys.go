package audit

// Reserved attribute keys for the audit-event payload.
//
// These names are declared as part of the ADR-0010 schema but no
// recorder emits them yet. They exist so future implementations of
// signed audit logs (#398) and the external policy hook (#396, #399)
// pick up the same spelling rather than choosing alternates that
// would force a downstream-visible rename later.
//
// Both names follow the OTel-namespaced convention shared with the
// rest of the audit payload (#390 Phase 6.5). When the corresponding
// features ship, recorders import these constants instead of writing
// the string literally.
//
// Reservations were committed in #404's "Schema lock-in" track and
// in the per-issue Phase 6.5 commitments on #398 and #399. The
// rename portion of Phase 6.5 shipped in #452; this file is the
// reservation portion.
const (
	// AttrSigningKeyID is the public-key fingerprint that signed an
	// audit event under the Stage-1.5 hash-chained audit log (#398).
	// Empty / absent on every event today; populated when signing
	// lands.
	AttrSigningKeyID = "aileron.signing.key_id"

	// AttrPolicyDecision is the verdict a pre-execution policy hook
	// returned for the action that produced this event: "allow",
	// "deny", "require_approval", or an opaque decision-source
	// reference (#396, #399). Empty / absent on every event today;
	// populated when the policy hook lands.
	AttrPolicyDecision = "aileron.policy.decision"
)
