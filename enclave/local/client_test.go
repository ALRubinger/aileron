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
// session key.
func establishSession(t *testing.T, c *Client) []byte {
	t.Helper()
	ctx := context.Background()

	attestResp, err := c.Attest(ctx, enclave.AttestationRequest{
		Nonce: []byte("test-nonce"), Audience: "test",
	})
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if attestResp.Token != "dev-ok" {
		t.Fatalf("expected dev-ok token, got %q", attestResp.Token)
	}

	hostKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating host key: %v", err)
	}

	sessResp, err := c.EstablishSession(ctx, enclave.SessionRequest{
		PublicKey: hostKey.PublicKey().Bytes(),
	})
	if err != nil {
		t.Fatalf("EstablishSession: %v", err)
	}
	if sessResp.SessionID == "" {
		t.Fatal("expected non-empty session ID")
	}

	enclavePub, _ := ecdh.P256().NewPublicKey(attestResp.PublicKey)
	raw, _ := hostKey.ECDH(enclavePub)
	h := sha256.Sum256(raw)
	return h[:]
}

// transmitKEK encrypts and sends a KEK to the enclave for a given user.
func transmitKEK(t *testing.T, c *Client, sessionKey []byte, userID string, kek []byte) {
	t.Helper()
	encrypted, err := encryptAESGCM(kek, sessionKey)
	if err != nil {
		t.Fatalf("encrypting KEK: %v", err)
	}
	resp, err := c.TransmitKEK(context.Background(), enclave.TransmitKEKRequest{
		UserID:       userID,
		EncryptedKEK: encrypted,
	})
	if err != nil {
		t.Fatalf("TransmitKEK: %v", err)
	}
	if !resp.Stored {
		t.Fatal("expected Stored=true")
	}
}

func TestFullExecuteFlow(t *testing.T) {
	var gotCredential []byte
	c := New(stubExecuteFn(&gotCredential))
	defer c.Close()

	sessionKey := establishSession(t, c)

	// Transmit a user KEK.
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitKEK(t, c, sessionKey, "user-1", userKEK)

	// Encrypt a credential with the user's KEK (simulating vault storage).
	plainCred := []byte("secret-api-key-12345")
	kekEncrypted, err := encryptAESGCM(plainCred, userKEK)
	if err != nil {
		t.Fatalf("encrypting credential with KEK: %v", err)
	}

	// Execute — enclave decrypts with stored KEK.
	resp, err := c.Execute(context.Background(), enclave.ExecuteRequest{
		RequestID:           "exec-1",
		UserID:              "user-1",
		GrantID:             "grant-1",
		ActionType:          "payment.charge",
		ConnectorID:         "payments/stripe",
		EncryptedCredential: kekEncrypted,
		CredentialType:      "api_key",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Status != "succeeded" {
		t.Fatalf("expected succeeded, got %q", resp.Status)
	}
	if string(gotCredential) != string(plainCred) {
		t.Fatalf("expected credential %q, got %q", plainCred, gotCredential)
	}
}

func TestExecuteWithoutKEK(t *testing.T) {
	c := New(func(_ context.Context, _ enclave.ExecuteRequest, _ []byte) (enclave.ExecuteResponse, error) {
		t.Fatal("should not be called")
		return enclave.ExecuteResponse{}, nil
	})
	defer c.Close()

	establishSession(t, c)

	// Execute without transmitting KEK.
	_, err := c.Execute(context.Background(), enclave.ExecuteRequest{
		UserID:              "no-kek-user",
		EncryptedCredential: []byte("data"),
	})
	if err != enclave.ErrNoKEK {
		t.Fatalf("expected ErrNoKEK, got %v", err)
	}
}

func TestExecuteWithoutAttestation(t *testing.T) {
	c := New(func(_ context.Context, _ enclave.ExecuteRequest, _ []byte) (enclave.ExecuteResponse, error) {
		t.Fatal("should not be called")
		return enclave.ExecuteResponse{}, nil
	})
	defer c.Close()

	_, err := c.TransmitKEK(context.Background(), enclave.TransmitKEKRequest{
		UserID:       "user-1",
		EncryptedKEK: []byte("data"),
	})
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

func TestEscrowStoreAndExecute(t *testing.T) {
	var gotCredential []byte
	c := New(stubExecuteFn(&gotCredential))
	defer c.Close()

	sessionKey := establishSession(t, c)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitKEK(t, c, sessionKey, "user-1", userKEK)

	ctx := context.Background()

	// Encrypt and escrow a credential.
	plainCred := []byte("escrowed-secret")
	kekEncrypted, _ := encryptAESGCM(plainCred, userKEK)

	storeResp, err := c.EscrowStore(ctx, enclave.EscrowStoreRequest{
		UserID:              "user-1",
		GrantID:             "grant-1",
		EncryptedCredential: kekEncrypted,
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
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitKEK(t, c, sessionKey, "user-1", userKEK)

	ctx := context.Background()
	encrypted, _ := encryptAESGCM([]byte("secret"), userKEK)

	storeResp, err := c.EscrowStore(ctx, enclave.EscrowStoreRequest{
		UserID:              "user-1",
		GrantID:             "grant-1",
		EncryptedCredential: encrypted,
		CredentialType:      "api_key",
		ExpiresAt:           time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("EscrowStore: %v", err)
	}

	// Wrong grant.
	err = c.EscrowRevoke(ctx, enclave.EscrowRevokeRequest{
		EscrowID: storeResp.EscrowID, GrantID: "wrong-grant",
	})
	if err != enclave.ErrEscrowNotFound {
		t.Fatalf("expected ErrEscrowNotFound, got %v", err)
	}

	// Correct grant.
	err = c.EscrowRevoke(ctx, enclave.EscrowRevokeRequest{
		EscrowID: storeResp.EscrowID, GrantID: "grant-1",
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
	sessionKey := establishSession(t, c)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitKEK(t, c, sessionKey, "user-1", userKEK)

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// After close, KEK should be gone.
	_, err := c.Execute(context.Background(), enclave.ExecuteRequest{
		UserID: "user-1", EncryptedCredential: []byte("data"),
	})
	if err != enclave.ErrNoKEK {
		t.Fatalf("expected ErrNoKEK after close, got %v", err)
	}
}
