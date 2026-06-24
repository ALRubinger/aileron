package runtime

import "testing"

func TestRequiresApproval_EffectRouting(t *testing.T) {
	cases := map[Effect]bool{
		EffectRead:         false,
		EffectWrite:        true,
		EffectDelete:       true,
		EffectSpend:        true,
		EffectExternalSend: true,
	}
	for eff, want := range cases {
		if got := requiresApproval(eff); got != want {
			t.Errorf("requiresApproval(%q) = %v, want %v", eff, got, want)
		}
	}
}

func TestRequiresApproval_UnknownFailsSafe(t *testing.T) {
	if !requiresApproval(Effect("mystery")) {
		t.Error("an unknown effect must fail safe (require approval), never run unattended")
	}
}
