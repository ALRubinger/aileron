package local

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/enclave"
	"github.com/ALRubinger/aileron/internal/source"
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

func TestEscrowListViaClient(t *testing.T) {
	c := New(nil)
	defer c.Close()

	ctx := context.Background()
	sessionKey := establishSession(t, c)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitKEK(t, c, sessionKey, "user-1", userKEK)

	// Empty list initially.
	resp, err := c.EscrowList(ctx)
	if err != nil {
		t.Fatalf("EscrowList: %v", err)
	}
	if len(resp.Entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(resp.Entries))
	}

	// Store a credential, then list.
	encrypted, _ := encryptAESGCM([]byte("test-cred"), userKEK)
	storeResp, err := c.EscrowStore(ctx, enclave.EscrowStoreRequest{
		UserID:              "user-1",
		GrantID:             "g1",
		EncryptedCredential: encrypted,
		CredentialType:      "oauth_token",
		ExpiresAt:           time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("EscrowStore: %v", err)
	}

	resp, err = c.EscrowList(ctx)
	if err != nil {
		t.Fatalf("EscrowList: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(resp.Entries))
	}
	if resp.Entries[0].EscrowID != storeResp.EscrowID {
		t.Fatalf("expected %q, got %q", storeResp.EscrowID, resp.Entries[0].EscrowID)
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

func TestTransmitKEKSuccess(t *testing.T) {
	c := New(nil)
	defer c.Close()

	sessionKey := establishSession(t, c)
	kek := make([]byte, 32)
	rand.Read(kek)

	transmitKEK(t, c, sessionKey, "user-1", kek)

	// Verify KEK was stored by encrypting something with it and having the
	// enclave decrypt it.
	plain := []byte("verify-kek")
	encrypted, err := encryptAESGCM(plain, kek)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	var gotCred []byte
	c2 := c
	c2.executeFn = stubExecuteFn(&gotCred)
	resp, err := c2.Execute(context.Background(), enclave.ExecuteRequest{
		RequestID:           "verify",
		UserID:              "user-1",
		ConnectorID:         "test/stub",
		EncryptedCredential: encrypted,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Status != "succeeded" {
		t.Fatalf("expected succeeded, got %q", resp.Status)
	}
	if string(gotCred) != string(plain) {
		t.Fatalf("credential mismatch: got %q", gotCred)
	}
}

func TestTransmitKEKBadCiphertext(t *testing.T) {
	c := New(nil)
	defer c.Close()

	establishSession(t, c)

	// Send garbage as encrypted KEK — should fail decryption.
	_, err := c.TransmitKEK(context.Background(), enclave.TransmitKEKRequest{
		UserID:       "user-1",
		EncryptedKEK: []byte("too-short"),
	})
	if err == nil {
		t.Fatal("expected error for bad ciphertext")
	}
}

func TestOAuthExchangeSuccess(t *testing.T) {
	// Mock token endpoint.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"access_token":  "test-access-token",
			"refresh_token": "test-refresh-token",
			"token_type":    "Bearer",
		})
	}))
	defer tokenServer.Close()

	// Mock userinfo endpoint.
	userinfoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"email": "user@example.com",
		})
	}))
	defer userinfoServer.Close()

	c := New(nil)
	defer c.Close()

	sessionKey := establishSession(t, c)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitKEK(t, c, sessionKey, "user-1", userKEK)

	resp, err := c.OAuthExchange(context.Background(), enclave.OAuthExchangeRequest{
		UserID:           "user-1",
		Provider:         "test",
		Code:             "auth-code-123",
		RedirectURI:      "http://localhost/callback",
		ClientID:         "client-id",
		ClientSecret:     "client-secret",
		TokenEndpoint:    tokenServer.URL,
		UserInfoEndpoint: userinfoServer.URL,
	})
	if err != nil {
		t.Fatalf("OAuthExchange: %v", err)
	}

	if resp.Email != "user@example.com" {
		t.Fatalf("expected email user@example.com, got %q", resp.Email)
	}
	if resp.TokenType != "Bearer" {
		t.Fatalf("expected Bearer, got %q", resp.TokenType)
	}
	if len(resp.EncryptedToken) == 0 {
		t.Fatal("expected non-empty encrypted token")
	}

	// Decrypt the token with the KEK to verify it was encrypted correctly.
	tokenJSON, err := decryptAESGCM(resp.EncryptedToken, userKEK)
	if err != nil {
		t.Fatalf("decrypting token: %v", err)
	}
	var tokenData map[string]string
	if err := json.Unmarshal(tokenJSON, &tokenData); err != nil {
		t.Fatalf("unmarshaling token: %v", err)
	}
	if tokenData["refresh_token"] != "test-refresh-token" {
		t.Fatalf("expected test-refresh-token, got %q", tokenData["refresh_token"])
	}
}

func TestOAuthExchangeNoKEK(t *testing.T) {
	c := New(nil)
	defer c.Close()

	establishSession(t, c)

	_, err := c.OAuthExchange(context.Background(), enclave.OAuthExchangeRequest{
		UserID: "no-kek-user",
		Code:   "code",
	})
	if err != enclave.ErrNoKEK {
		t.Fatalf("expected ErrNoKEK, got %v", err)
	}
}

func TestOAuthExchangeTokenEndpointError(t *testing.T) {
	// Mock token endpoint that returns an error.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer tokenServer.Close()

	c := New(nil)
	defer c.Close()

	sessionKey := establishSession(t, c)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitKEK(t, c, sessionKey, "user-1", userKEK)

	_, err := c.OAuthExchange(context.Background(), enclave.OAuthExchangeRequest{
		UserID:        "user-1",
		Code:          "bad-code",
		TokenEndpoint: tokenServer.URL,
	})
	if err == nil {
		t.Fatal("expected error for failed token exchange")
	}
}

func TestOAuthExchangeNoRefreshToken(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"access_token": "test-access-token",
			"token_type":   "Bearer",
			// No refresh_token.
		})
	}))
	defer tokenServer.Close()

	c := New(nil)
	defer c.Close()

	sessionKey := establishSession(t, c)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitKEK(t, c, sessionKey, "user-1", userKEK)

	_, err := c.OAuthExchange(context.Background(), enclave.OAuthExchangeRequest{
		UserID:        "user-1",
		Code:          "code",
		TokenEndpoint: tokenServer.URL,
	})
	if err == nil {
		t.Fatal("expected error for missing refresh token")
	}
}

func TestOAuthExchangeNoUserInfoEndpoint(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"access_token":  "test-access-token",
			"refresh_token": "test-refresh-token",
			"token_type":    "Bearer",
		})
	}))
	defer tokenServer.Close()

	c := New(nil)
	defer c.Close()

	sessionKey := establishSession(t, c)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitKEK(t, c, sessionKey, "user-1", userKEK)

	resp, err := c.OAuthExchange(context.Background(), enclave.OAuthExchangeRequest{
		UserID:        "user-1",
		Code:          "code",
		TokenEndpoint: tokenServer.URL,
		// No UserInfoEndpoint — should return empty email.
	})
	if err != nil {
		t.Fatalf("OAuthExchange: %v", err)
	}
	if resp.Email != "" {
		t.Fatalf("expected empty email, got %q", resp.Email)
	}
}

func TestOAuthExchangeUserInfoError(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"access_token":  "test-access-token",
			"refresh_token": "test-refresh-token",
			"token_type":    "Bearer",
		})
	}))
	defer tokenServer.Close()

	userinfoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("server error"))
	}))
	defer userinfoServer.Close()

	c := New(nil)
	defer c.Close()

	sessionKey := establishSession(t, c)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitKEK(t, c, sessionKey, "user-1", userKEK)

	_, err := c.OAuthExchange(context.Background(), enclave.OAuthExchangeRequest{
		UserID:           "user-1",
		Code:             "code",
		TokenEndpoint:    tokenServer.URL,
		UserInfoEndpoint: userinfoServer.URL,
	})
	if err == nil {
		t.Fatal("expected error for userinfo failure")
	}
}

func TestEscrowStoreWithoutKEK(t *testing.T) {
	c := New(nil)
	defer c.Close()

	establishSession(t, c)

	_, err := c.EscrowStore(context.Background(), enclave.EscrowStoreRequest{
		UserID:              "no-kek",
		EncryptedCredential: []byte("data"),
		ExpiresAt:           time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	if err != enclave.ErrNoKEK {
		t.Fatalf("expected ErrNoKEK, got %v", err)
	}
}

func TestEscrowStoreBadDecryption(t *testing.T) {
	c := New(nil)
	defer c.Close()

	sessionKey := establishSession(t, c)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitKEK(t, c, sessionKey, "user-1", userKEK)

	_, err := c.EscrowStore(context.Background(), enclave.EscrowStoreRequest{
		UserID:              "user-1",
		EncryptedCredential: []byte("bad-ciphertext"),
		ExpiresAt:           time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	if err == nil {
		t.Fatal("expected error for bad decryption")
	}
}

func TestEscrowStoreBadExpiry(t *testing.T) {
	c := New(nil)
	defer c.Close()

	sessionKey := establishSession(t, c)
	userKEK := make([]byte, 32)
	rand.Read(userKEK)
	transmitKEK(t, c, sessionKey, "user-1", userKEK)

	encrypted, _ := encryptAESGCM([]byte("cred"), userKEK)
	_, err := c.EscrowStore(context.Background(), enclave.EscrowStoreRequest{
		UserID:              "user-1",
		EncryptedCredential: encrypted,
		ExpiresAt:           "not-a-date",
	})
	if err == nil {
		t.Fatal("expected error for bad expiry")
	}
}

func TestDecryptAESGCMErrors(t *testing.T) {
	// Bad key length.
	_, err := decryptAESGCM([]byte("data"), make([]byte, 16))
	if err == nil {
		t.Fatal("expected error for bad key length")
	}

	// Ciphertext too short.
	_, err = decryptAESGCM([]byte("short"), make([]byte, 32))
	if err == nil {
		t.Fatal("expected error for short ciphertext")
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

func TestWithSessionTTL(t *testing.T) {
	c := New(nil, WithSessionTTL(10*time.Minute))
	defer c.Close()

	if c.sessionTTL != 10*time.Minute {
		t.Fatalf("sessionTTL = %v, want 10m", c.sessionTTL)
	}

	// Verify it's used in EstablishSession.
	sessionKey := establishSession(t, c)
	if sessionKey == nil {
		t.Fatal("expected session to be established")
	}

	// Session expiry should be ~10 minutes from now, not 24h.
	c.mu.Lock()
	expiresAt := c.expiresAt
	c.mu.Unlock()

	remaining := time.Until(expiresAt)
	if remaining > 11*time.Minute || remaining < 9*time.Minute {
		t.Fatalf("session expires in %v, expected ~10m", remaining)
	}
}

func TestWithKEKTTL(t *testing.T) {
	c := New(nil, WithKEKTTL(2*time.Minute))
	defer c.Close()

	if c.keks.ttl != 2*time.Minute {
		t.Fatalf("keks.ttl = %v, want 2m", c.keks.ttl)
	}
}

func TestNewDefaultTTLs(t *testing.T) {
	c := New(nil)
	defer c.Close()

	if c.sessionTTL != 24*time.Hour {
		t.Fatalf("default sessionTTL = %v, want 24h", c.sessionTTL)
	}
	if c.keks.ttl != 24*time.Hour {
		t.Fatalf("default keks.ttl = %v, want 24h", c.keks.ttl)
	}
}

func TestClient_Ready(t *testing.T) {
	c := New(nil)
	if err := c.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v (local client should always be ready)", err)
	}
}

func TestClient_SourceExecute_HappyPath(t *testing.T) {
	executeFn := func(_ context.Context, tool string, params map[string]any, cred []byte) (map[string]any, error) {
		if tool != "test_search" {
			t.Errorf("expected tool test_search, got %s", tool)
		}
		return map[string]any{"results": []string{"found it"}}, nil
	}
	c := New(nil, WithSourceExecuteFn(executeFn))

	// Store credential in escrow.
	storeResp, err := c.EscrowStore(context.Background(), enclave.EscrowStoreRequest{
		// EscrowStore needs encrypted credential + KEK — use a simpler approach:
		// store directly via the internal escrow.
	})
	// EscrowStore requires KEK — let's store directly.
	_ = storeResp
	_ = err

	// Use the escrow store directly since EscrowStore requires KEK decryption.
	escrowID := c.escrow.Store("g1", []byte(`{"access_token":"test"}`), "oauth2", nil, time.Now().Add(time.Hour))

	resp, err := c.SourceExecute(context.Background(), enclave.SourceExecuteRequest{
		EscrowID: escrowID,
		Tool:     "test_search",
		Params:   map[string]any{"query": "hello"},
	})
	if err != nil {
		t.Fatalf("SourceExecute: %v", err)
	}
	if resp.Error != "" {
		t.Errorf("unexpected error: %s", resp.Error)
	}
	if resp.Result == nil {
		t.Fatal("expected result, got nil")
	}
}

func TestClient_SourceExecute_NotConfigured(t *testing.T) {
	c := New(nil) // no WithSourceExecuteFn
	_, err := c.SourceExecute(context.Background(), enclave.SourceExecuteRequest{
		EscrowID: "esc_123",
		Tool:     "test_search",
	})
	if err == nil {
		t.Fatal("expected error when SourceExecuteFn not configured")
	}
}

func TestClient_SourceExecute_EscrowNotFound(t *testing.T) {
	executeFn := func(_ context.Context, _ string, _ map[string]any, _ []byte) (map[string]any, error) {
		return nil, nil
	}
	c := New(nil, WithSourceExecuteFn(executeFn))
	_, err := c.SourceExecute(context.Background(), enclave.SourceExecuteRequest{
		EscrowID: "esc_nonexistent",
		Tool:     "test_search",
	})
	if err == nil {
		t.Fatal("expected error for non-existent escrow ID")
	}
}

func TestClient_SourceExecute_TokenRefresh(t *testing.T) {
	// When the source executor triggers token refresh via the TokenSaver,
	// the escrow should be updated with the new credential.
	executeFn := func(ctx context.Context, _ string, _ map[string]any, _ []byte) (map[string]any, error) {
		// Simulate token refresh by calling the TokenSaver.
		saver := source.GetTokenSaver(ctx)
		if saver == nil {
			t.Fatal("expected TokenSaver in context")
		}
		newToken := []byte(`{"access_token":"refreshed","refresh_token":"new_r"}`)
		if err := saver(ctx, newToken); err != nil {
			t.Fatalf("TokenSaver: %v", err)
		}
		return map[string]any{"ok": true}, nil
	}
	c := New(nil, WithSourceExecuteFn(executeFn))

	escrowID := c.escrow.Store("g1", []byte(`{"access_token":"old"}`), "oauth2", nil, time.Now().Add(time.Hour))

	_, err := c.SourceExecute(context.Background(), enclave.SourceExecuteRequest{
		EscrowID: escrowID,
		Tool:     "test_search",
		Params:   map[string]any{"query": "hello"},
	})
	if err != nil {
		t.Fatalf("SourceExecute: %v", err)
	}

	// Verify escrow was updated.
	updated, err := c.escrow.Get(escrowID)
	if err != nil {
		t.Fatalf("escrow.Get: %v", err)
	}
	if string(updated) != `{"access_token":"refreshed","refresh_token":"new_r"}` {
		t.Errorf("escrow not updated: got %q", updated)
	}
}
