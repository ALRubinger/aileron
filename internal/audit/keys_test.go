package audit

import "testing"

// The reserved keys aren't behavior, they're contract. The contract
// is that these specific spellings are stable. The test guards the
// spellings against accidental rename. When a recorder eventually
// emits one of these, change it here AND in any consumer reading the
// audit log; this test is the single point of truth for the wire
// strings.

func TestAttrSigningKeyIDSpelling(t *testing.T) {
	if got, want := AttrSigningKeyID, "aileron.signing.key_id"; got != want {
		t.Errorf("AttrSigningKeyID = %q, want %q", got, want)
	}
}

func TestAttrPolicyDecisionSpelling(t *testing.T) {
	if got, want := AttrPolicyDecision, "aileron.policy.decision"; got != want {
		t.Errorf("AttrPolicyDecision = %q, want %q", got, want)
	}
}
