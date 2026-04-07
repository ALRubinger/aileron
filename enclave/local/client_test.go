package local

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/enclave"
)

// stubExecuteFn returns a fixed response and records the credential it received.
func stubExecuteFn(gotCredential *[]byte) ExecuteFn {
	return func(_ context.Context, req enclave.ExecuteRequest, credential []byte) (enclave.ExecuteResponse, error) {
		*gotCredential = copyBytes(credential)
		return enclave.ExecuteResponse{
			RequestID:  req.RequestID,
			Status:     "succeeded",
			Output:     map[string]any{"result": "ok"},
			ReceiptRef: "receipt-123",
		}, nil
	}
}

// establishSession performs the full attest → session flow and returns the
// host's private key and the derived session key.
func establishSession(t *testing.T, c *Client) (sessionKey []byte) {
	t.Helper()
	ctx := context.Background()

	// Attest.
	attestResp, err := c.Attest(ctx, enclave.AttestationRequest{
		Nonce:    []byte("test-nonce"),
		Audience: "test",
	})
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if attestResp.Token != "dev-ok" {
		t.Fatalf("expected dev-ok token, got %q", attestResp.Token)
	}

	// Host generates its own ephemeral key.
	hostKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating host key: %v", err)
	}

	// Establish session.
	sessResp, err := c.EstablishSession(ctx, enclave.SessionRequest{
		PublicKey: hostKey.PublicKey().Bytes(),
	})
	if err != nil {
		t.Fatalf("EstablishSession: %v", err)
	}
	if sessResp.SessionID == "" {
		t.Fatal("expected non-empty session ID")
	}

	// Derive the same shared secret on the host side.
	enclavePub, err := ecdh.P256().NewPublicKey(attestResp.PublicKey)
	if err != nil {
		t.Fatalf("parsing enclave public key: %v", err)
	}
	raw, err := hostKey.ECDH(enclavePub)
	if err != nil {
		t.Fatalf("host ECDH: %v", err)
	}
	h := sha256.Sum256(raw)
	return h[:]
}

func TestFullExecuteFlow(t *testing.T) {
	var gotCredential []byte
	c := New(stubExecuteFn(&gotCredential))
	defer c.Close()

	sessionKey := establishSession(t, c)

	// Encrypt a test credential with the session key.
	plainCred := []byte("secret-api-key-12345")
	encryptedCred, err := encryptAESGCM(plainCred, sessionKey)
	if err != nil {
		t.Fatalf("encrypting credential: %v", err)
	}

	// Execute.
	resp, err := c.Execute(context.Background(), enclave.ExecuteRequest{
		RequestID:           "exec-1",
		GrantID:             "grant-1",
		ActionType:          "payment.charge",
		ConnectorID:         "payments/stripe",
		EncryptedCredential: encryptedCred,
		CredentialType:      "api_key",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Status != "succeeded" {
		t.Fatalf("expected succeeded, got %q", resp.Status)
	}
	if resp.RequestID != "exec-1" {
		t.Fatalf("expected request ID exec-1, got %q", resp.RequestID)
	}
	if resp.ReceiptRef != "receipt-123" {
		t.Fatalf("expected receipt ref receipt-123, got %q", resp.ReceiptRef)
	}

	// Verify the execute function received the correct plaintext.
	if string(gotCredential) != string(plainCred) {
		t.Fatalf("expected credential %q, got %q", plainCred, gotCredential)
	}
}

func TestExecuteWithoutAttestation(t *testing.T) {
	c := New(func(_ context.Context, _ enclave.ExecuteRequest, _ []byte) (enclave.ExecuteResponse, error) {
		t.Fatal("should not be called")
		return enclave.ExecuteResponse{}, nil
	})
	defer c.Close()

	_, err := c.Execute(context.Background(), enclave.ExecuteRequest{})
	if err != enclave.ErrNotAttested {
		t.Fatalf("expected ErrNotAttested, got %v", err)
	}
}

func TestEstablishSessionWithoutAttestation(t *testing.T) {
	c := New(nil)
	defer c.Close()

	_, err := c.EstablishSession(context.Background(), enclave.SessionRequest{
		PublicKey: make([]byte, 65),
	})
	if err != enclave.ErrNotAttested {
		t.Fatalf("expected ErrNotAttested, got %v", err)
	}
}

func TestSessionReAttestation(t *testing.T) {
	var gotCredential []byte
	c := New(stubExecuteFn(&gotCredential))
	defer c.Close()

	// First session.
	key1 := establishSession(t, c)

	// Re-attest (new session).
	key2 := establishSession(t, c)

	// Old session key should not work.
	plainCred := []byte("cred")
	encrypted, _ := encryptAESGCM(plainCred, key1)
	_, err := c.Execute(context.Background(), enclave.ExecuteRequest{
		EncryptedCredential: encrypted,
	})
	if err == nil {
		t.Fatal("expected decryption to fail with old session key")
	}

	// New session key should work.
	encrypted2, _ := encryptAESGCM(plainCred, key2)
	_, err = c.Execute(context.Background(), enclave.ExecuteRequest{
		RequestID:           "exec-2",
		EncryptedCredential: encrypted2,
	})
	if err != nil {
		t.Fatalf("Execute with new key: %v", err)
	}
}

func TestEscrowStoreAndExecute(t *testing.T) {
	var gotCredential []byte
	c := New(stubExecuteFn(&gotCredential))
	defer c.Close()

	sessionKey := establishSession(t, c)
	ctx := context.Background()

	// Encrypt and escrow a credential.
	plainCred := []byte("escrowed-secret")
	encrypted, _ := encryptAESGCM(plainCred, sessionKey)

	storeResp, err := c.EscrowStore(ctx, enclave.EscrowStoreRequest{
		GrantID:             "grant-1",
		EncryptedCredential: encrypted,
		CredentialType:      "api_key",
		ExpiresAt:           time.Now().Add(time.Hour).Format(time.RFC3339),
		ActionTypes:         []string{"payment.charge"},
	})
	if err != nil {
		t.Fatalf("EscrowStore: %v", err)
	}

	// Execute using the escrowed credential.
	resp, err := c.Execute(ctx, enclave.ExecuteRequest{
		RequestID: "exec-escrow",
		EscrowID:  storeResp.EscrowID,
	})
	if err != nil {
		t.Fatalf("Execute with escrow: %v", err)
	}
	if resp.Status != "succeeded" {
		t.Fatalf("expected succeeded, got %q", resp.Status)
	}
	if string(gotCredential) != string(plainCred) {
		t.Fatalf("expected credential %q, got %q", plainCred, gotCredential)
	}
}

func TestEscrowRevoke(t *testing.T) {
	c := New(func(_ context.Context, _ enclave.ExecuteRequest, _ []byte) (enclave.ExecuteResponse, error) {
		return enclave.ExecuteResponse{Status: "succeeded"}, nil
	})
	defer c.Close()

	sessionKey := establishSession(t, c)
	ctx := context.Background()

	encrypted, _ := encryptAESGCM([]byte("secret"), sessionKey)
	storeResp, err := c.EscrowStore(ctx, enclave.EscrowStoreRequest{
		GrantID:             "grant-1",
		EncryptedCredential: encrypted,
		CredentialType:      "api_key",
		ExpiresAt:           time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("EscrowStore: %v", err)
	}

	// Revoke with wrong grant should fail.
	err = c.EscrowRevoke(ctx, enclave.EscrowRevokeRequest{
		EscrowID: storeResp.EscrowID,
		GrantID:  "wrong-grant",
	})
	if err != enclave.ErrEscrowNotFound {
		t.Fatalf("expected ErrEscrowNotFound for wrong grant, got %v", err)
	}

	// Revoke with correct grant.
	err = c.EscrowRevoke(ctx, enclave.EscrowRevokeRequest{
		EscrowID: storeResp.EscrowID,
		GrantID:  "grant-1",
	})
	if err != nil {
		t.Fatalf("EscrowRevoke: %v", err)
	}

	// Execute should now fail.
	_, err = c.Execute(ctx, enclave.ExecuteRequest{EscrowID: storeResp.EscrowID})
	if err != enclave.ErrEscrowNotFound {
		t.Fatalf("expected ErrEscrowNotFound after revoke, got %v", err)
	}
}

func TestClose(t *testing.T) {
	c := New(nil)
	establishSession(t, c)

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// After close, operations should fail.
	_, err := c.Execute(context.Background(), enclave.ExecuteRequest{})
	if err != enclave.ErrNotAttested {
		t.Fatalf("expected ErrNotAttested after close, got %v", err)
	}
}
