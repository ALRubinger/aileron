package gcs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALRubinger/aileron/internal/enclave"
)

func TestClientAttest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/attest" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method %q", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected content type %q", r.Header.Get("Content-Type"))
		}

		var req enclave.AttestationRequest
		json.NewDecoder(r.Body).Decode(&req)

		json.NewEncoder(w).Encode(enclave.AttestationResponse{
			Token:     "test-token",
			PublicKey: []byte("test-pubkey"),
		})
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	resp, err := c.Attest(context.Background(), enclave.AttestationRequest{
		Nonce:    []byte("nonce"),
		Audience: "test",
	})
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if resp.Token != "test-token" {
		t.Fatalf("expected test-token, got %q", resp.Token)
	}
}

func TestClientDoesNotSignUnauthenticatedEndpoints(t *testing.T) {
	authKey := []byte("01234567890123456789012345678901")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(enclave.HeaderRequestMAC) != "" {
			t.Fatal("did not expect request MAC on attestation request")
		}
		if r.Header.Get(enclave.HeaderRequestTimestamp) != "" {
			t.Fatal("did not expect request timestamp on attestation request")
		}
		if r.Header.Get(enclave.HeaderRequestNonce) != "" {
			t.Fatal("did not expect request nonce on attestation request")
		}
		json.NewEncoder(w).Encode(enclave.AttestationResponse{
			Token:     "test-token",
			PublicKey: []byte("test-pubkey"),
		})
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	c.sessionID = "sess-abc"
	c.authKey = authKey

	if _, err := c.Attest(context.Background(), enclave.AttestationRequest{}); err != nil {
		t.Fatalf("Attest: %v", err)
	}
}

func TestClientEstablishSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(enclave.SessionResponse{
			SessionID:      "sess-123",
			ExpiresAt:      "2026-12-31T23:59:59Z",
			RequestAuthKey: []byte("request-auth-key"),
		})
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	resp, err := c.EstablishSession(context.Background(), enclave.SessionRequest{
		PublicKey: []byte("host-key"),
	})
	if err != nil {
		t.Fatalf("EstablishSession: %v", err)
	}
	if resp.SessionID != "sess-123" {
		t.Fatalf("expected sess-123, got %q", resp.SessionID)
	}
	if string(c.authKey) != "request-auth-key" {
		t.Fatalf("expected request auth key to be stored")
	}
}

func TestClientExecute(t *testing.T) {
	authKey := []byte("01234567890123456789012345678901")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify session header is sent after session establishment.
		if sid := r.Header.Get(enclave.HeaderSessionID); sid != "sess-abc" {
			t.Fatalf("expected session ID sess-abc, got %q", sid)
		}
		timestamp := r.Header.Get(enclave.HeaderRequestTimestamp)
		nonce := r.Header.Get(enclave.HeaderRequestNonce)
		mac := r.Header.Get(enclave.HeaderRequestMAC)
		if timestamp == "" || nonce == "" || mac == "" {
			t.Fatalf("expected request authentication headers")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading body: %v", err)
		}
		if !enclave.VerifyRequestMAC(authKey, r.Method, r.URL.Path, body, "sess-abc", timestamp, nonce, mac) {
			t.Fatalf("request MAC did not verify")
		}
		var req enclave.ExecuteRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshaling request: %v", err)
		}
		if !enclave.VerifyExecuteCapability(authKey, req) {
			t.Fatalf("execute capability did not verify")
		}
		json.NewEncoder(w).Encode(enclave.ExecuteResponse{
			RequestID:  "exec-1",
			Status:     "succeeded",
			Output:     map[string]any{"result": "ok"},
			ReceiptRef: "receipt-1",
		})
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	// Set session ID manually for this test.
	c.sessionID = "sess-abc"
	c.authKey = authKey

	resp, err := c.Execute(context.Background(), enclave.ExecuteRequest{
		RequestID: "exec-1",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Status != "succeeded" {
		t.Fatalf("expected succeeded, got %q", resp.Status)
	}
}

func TestRequiresRequestAuth(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "/kek", want: true},
		{path: "/oauth/exchange", want: true},
		{path: "/execute", want: true},
		{path: "/escrow", want: true},
		{path: "/escrow/list", want: true},
		{path: "/escrow/revoke", want: true},
		{path: "/source/execute", want: true},
		{path: "/attest", want: false},
		{path: "/session", want: false},
		{path: "/health", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := requiresRequestAuth(tt.path); got != tt.want {
				t.Fatalf("requiresRequestAuth(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestAttachGrantCapability(t *testing.T) {
	authKey := []byte("01234567890123456789012345678901")

	execute, err := attachGrantCapability(enclave.ExecuteRequest{
		GrantID:        "grant-1",
		IntentID:       "intent-1",
		UserID:         "user-1",
		VaultPath:      "connected-accounts/user-1/gmail",
		Provider:       "gmail",
		CredentialType: "oauth2",
		ActionType:     "email.send",
		Parameters:     map[string]any{"to": "a@example.com"},
	}, authKey)
	if err != nil {
		t.Fatalf("attach execute capability: %v", err)
	}
	executeReq := execute.(enclave.ExecuteRequest)
	if !enclave.VerifyExecuteCapability(authKey, executeReq) {
		t.Fatal("expected execute capability to verify")
	}

	escrow, err := attachGrantCapability(enclave.EscrowStoreRequest{
		GrantID:           "grant-1",
		UserID:            "user-1",
		VaultPath:         "connected-accounts/user-1/gmail",
		Provider:          "gmail",
		CredentialType:    "oauth2",
		ExpiresAt:         "2026-04-26T09:00:00Z",
		ActionTypes:       []string{"email.send"},
		SourceTools:       []string{"gmail_search"},
		AllowedParameters: map[string]any{"to": "a@example.com"},
	}, authKey)
	if err != nil {
		t.Fatalf("attach escrow capability: %v", err)
	}
	escrowReq := escrow.(enclave.EscrowStoreRequest)
	if !enclave.VerifyEscrowStoreCapability(authKey, escrowReq) {
		t.Fatal("expected escrow capability to verify")
	}

	source, err := attachGrantCapability(enclave.SourceExecuteRequest{
		UserID:    "user-1",
		VaultPath: "connected-accounts/user-1/gmail",
		Provider:  "gmail",
		Tool:      "gmail_search",
		Params:    map[string]any{"query": "from:a@example.com"},
	}, authKey)
	if err != nil {
		t.Fatalf("attach source capability: %v", err)
	}
	sourceReq := source.(enclave.SourceExecuteRequest)
	if !enclave.VerifySourceExecuteCapability(authKey, sourceReq) {
		t.Fatal("expected source capability to verify")
	}

	unchanged, err := attachGrantCapability(enclave.TransmitKEKRequest{UserID: "user-1"}, authKey)
	if err != nil {
		t.Fatalf("attach unsupported capability: %v", err)
	}
	if _, ok := unchanged.(enclave.TransmitKEKRequest); !ok {
		t.Fatalf("expected unsupported request type to be returned unchanged")
	}
}

func TestClientErrorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("something broke"))
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	_, err := c.Attest(context.Background(), enclave.AttestationRequest{})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestClientCloseClearsSessionAuth(t *testing.T) {
	c := New(Config{BaseURL: "http://example.test"})
	c.sessionID = "sess-abc"
	c.authKey = []byte("01234567890123456789012345678901")

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if c.sessionID != "" {
		t.Fatalf("expected session ID to be cleared")
	}
	if c.authKey != nil {
		t.Fatalf("expected auth key to be cleared")
	}
}

func TestClientEscrowStore(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/escrow" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(enclave.EscrowStoreResponse{
			EscrowID: "esc-123",
		})
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	resp, err := c.EscrowStore(context.Background(), enclave.EscrowStoreRequest{
		GrantID: "grant-1",
	})
	if err != nil {
		t.Fatalf("EscrowStore: %v", err)
	}
	if resp.EscrowID != "esc-123" {
		t.Fatalf("expected esc-123, got %q", resp.EscrowID)
	}
}

func TestClientTransmitKEK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/kek" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(enclave.TransmitKEKResponse{Stored: true})
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	resp, err := c.TransmitKEK(context.Background(), enclave.TransmitKEKRequest{
		UserID:       "user-1",
		EncryptedKEK: []byte("encrypted-kek"),
	})
	if err != nil {
		t.Fatalf("TransmitKEK: %v", err)
	}
	if !resp.Stored {
		t.Fatal("expected Stored=true")
	}
}

func TestClientOAuthExchange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/exchange" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(enclave.OAuthExchangeResponse{
			EncryptedToken: []byte("encrypted-token"),
			Email:          "user@example.com",
			TokenType:      "Bearer",
		})
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	resp, err := c.OAuthExchange(context.Background(), enclave.OAuthExchangeRequest{
		UserID:   "user-1",
		Provider: "google",
		Code:     "auth-code",
	})
	if err != nil {
		t.Fatalf("OAuthExchange: %v", err)
	}
	if resp.Email != "user@example.com" {
		t.Fatalf("expected user@example.com, got %q", resp.Email)
	}
	if resp.TokenType != "Bearer" {
		t.Fatalf("expected Bearer, got %q", resp.TokenType)
	}
}

func TestClientExecuteError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("execute failed"))
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	_, err := c.Execute(context.Background(), enclave.ExecuteRequest{RequestID: "exec-1"})
	if err == nil {
		t.Fatal("expected error for 500 response on Execute")
	}
}

func TestClientEscrowStoreError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("escrow store failed"))
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	_, err := c.EscrowStore(context.Background(), enclave.EscrowStoreRequest{GrantID: "grant-1"})
	if err == nil {
		t.Fatal("expected error for 500 response on EscrowStore")
	}
}

func TestClientTransmitKEKError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("kek failed"))
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	_, err := c.TransmitKEK(context.Background(), enclave.TransmitKEKRequest{UserID: "user-1"})
	if err == nil {
		t.Fatal("expected error for 500 response on TransmitKEK")
	}
}

func TestClientOAuthExchangeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("oauth exchange failed"))
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	_, err := c.OAuthExchange(context.Background(), enclave.OAuthExchangeRequest{UserID: "user-1", Code: "bad"})
	if err == nil {
		t.Fatal("expected error for 500 response on OAuthExchange")
	}
}

func TestClientEscrowList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/escrow/list" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(enclave.EscrowListResponse{
			Entries: []enclave.EscrowListEntry{
				{EscrowID: "esc-1", GrantID: "g1", ExpiresAt: "2026-12-31T00:00:00Z"},
				{EscrowID: "esc-2", GrantID: "g2", ExpiresAt: "2026-12-31T00:00:00Z"},
			},
		})
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	resp, err := c.EscrowList(context.Background())
	if err != nil {
		t.Fatalf("EscrowList: %v", err)
	}
	if len(resp.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(resp.Entries))
	}
	if resp.Entries[0].EscrowID != "esc-1" {
		t.Fatalf("expected esc-1, got %q", resp.Entries[0].EscrowID)
	}
}

func TestClientEscrowListError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("list failed"))
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	_, err := c.EscrowList(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response on EscrowList")
	}
}

func TestClientEscrowRevokeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("revoke failed"))
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	err := c.EscrowRevoke(context.Background(), enclave.EscrowRevokeRequest{EscrowID: "esc-123"})
	if err == nil {
		t.Fatal("expected error for 500 response on EscrowRevoke")
	}
}

func TestClientEstablishSessionError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("session failed"))
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	_, err := c.EstablishSession(context.Background(), enclave.SessionRequest{PublicKey: []byte("key")})
	if err == nil {
		t.Fatal("expected error for 500 response on EstablishSession")
	}
}

func TestClientEscrowRevoke(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/escrow/revoke" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	err := c.EscrowRevoke(context.Background(), enclave.EscrowRevokeRequest{
		EscrowID: "esc-123",
		GrantID:  "grant-1",
	})
	if err != nil {
		t.Fatalf("EscrowRevoke: %v", err)
	}
}

func TestClientReady_Healthy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || r.Method != http.MethodGet {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	}))
	defer server.Close()

	client := New(Config{BaseURL: server.URL})
	if err := client.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
}

func TestClientReady_Unhealthy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("enclave error"))
	}))
	defer server.Close()

	client := New(Config{BaseURL: server.URL})
	err := client.Ready(context.Background())
	if err == nil {
		t.Fatal("expected error for unhealthy enclave")
	}
}

func TestClientReady_Unreachable(t *testing.T) {
	client := New(Config{BaseURL: "http://127.0.0.1:1"})
	err := client.Ready(context.Background())
	if err == nil {
		t.Fatal("expected error for unreachable enclave")
	}
}

func TestClientSourceExecute(t *testing.T) {
	resultJSON, _ := json.Marshal(map[string]any{"messages": []string{"hello"}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/source/execute" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method %q", r.Method)
		}

		var req enclave.SourceExecuteRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.EscrowID != "esc_123" {
			t.Errorf("expected escrow ID esc_123, got %q", req.EscrowID)
		}
		if req.Tool != "gmail_search" {
			t.Errorf("expected tool gmail_search, got %q", req.Tool)
		}

		json.NewEncoder(w).Encode(enclave.SourceExecuteResponse{
			Result: resultJSON,
		})
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	resp, err := c.SourceExecute(context.Background(), enclave.SourceExecuteRequest{
		EscrowID: "esc_123",
		Tool:     "gmail_search",
		Params:   map[string]any{"query": "test"},
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

func TestClientSourceExecute_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("enclave crashed"))
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	_, err := c.SourceExecute(context.Background(), enclave.SourceExecuteRequest{
		EscrowID: "esc_123",
		Tool:     "gmail_search",
	})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}
