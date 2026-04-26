package enclave

import "testing"

func TestExecuteCapabilityVerifiesBoundFields(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	req := ExecuteRequest{
		GrantID:        "grant-1",
		IntentID:       "intent-1",
		UserID:         "user-1",
		VaultPath:      "connected-accounts/user-1/gmail",
		Provider:       "gmail",
		CredentialType: "oauth2",
		ActionType:     "email.send",
		Parameters:     map[string]any{"to": "a@example.com"},
	}

	capability, err := SignGrantCapability(key, ExecuteGrantCapability(req, "nonce-1"))
	if err != nil {
		t.Fatalf("SignGrantCapability: %v", err)
	}
	req.Capability = capability

	if !VerifyExecuteCapability(key, req) {
		t.Fatal("expected capability to verify")
	}

	req.UserID = "user-2"
	if VerifyExecuteCapability(key, req) {
		t.Fatal("expected user tamper to fail")
	}
}

func TestEscrowStoreCapabilityVerifiesBoundFields(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	req := EscrowStoreRequest{
		GrantID:           "grant-1",
		UserID:            "user-1",
		VaultPath:         "connected-accounts/user-1/gmail",
		Provider:          "gmail",
		CredentialType:    "oauth2",
		ExpiresAt:         "2026-04-26T09:00:00Z",
		ActionTypes:       []string{"email.send"},
		SourceTools:       []string{"gmail_search"},
		AllowedParameters: map[string]any{"to": "a@example.com"},
	}

	capability, err := SignGrantCapability(key, EscrowStoreGrantCapability(req, "nonce-1"))
	if err != nil {
		t.Fatalf("SignGrantCapability: %v", err)
	}
	req.Capability = capability

	if !VerifyEscrowStoreCapability(key, req) {
		t.Fatal("expected capability to verify")
	}

	req.AllowedParameters = map[string]any{"to": "b@example.com"}
	if VerifyEscrowStoreCapability(key, req) {
		t.Fatal("expected parameter tamper to fail")
	}
}

func TestSourceExecuteCapabilityVerifiesBoundFields(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	req := SourceExecuteRequest{
		UserID:    "user-1",
		VaultPath: "connected-accounts/user-1/gmail",
		Provider:  "gmail",
		Tool:      "gmail_search",
		Params:    map[string]any{"query": "from:a@example.com"},
	}

	capability, err := SignGrantCapability(key, SourceExecuteGrantCapability(req, "nonce-1"))
	if err != nil {
		t.Fatalf("SignGrantCapability: %v", err)
	}
	req.Capability = capability

	if !VerifySourceExecuteCapability(key, req) {
		t.Fatal("expected capability to verify")
	}

	req.Tool = "gmail_delete"
	if VerifySourceExecuteCapability(key, req) {
		t.Fatal("expected tool tamper to fail")
	}
}

func TestGrantCapabilityRejectsMissingSignature(t *testing.T) {
	req := ExecuteRequest{
		GrantID:    "grant-1",
		Capability: &SignedGrantCapability{Capability: GrantCapability{GrantID: "grant-1", Nonce: "nonce-1"}},
	}
	if VerifyExecuteCapability([]byte("key"), req) {
		t.Fatal("expected missing signature to fail")
	}
}
