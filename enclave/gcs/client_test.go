package gcs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALRubinger/aileron/enclave"
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

func TestClientEstablishSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(enclave.SessionResponse{
			SessionID: "sess-123",
			ExpiresAt: "2026-12-31T23:59:59Z",
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
}

func TestClientExecute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify session header is sent after session establishment.
		if sid := r.Header.Get("X-Session-ID"); sid != "sess-abc" {
			t.Fatalf("expected session ID sess-abc, got %q", sid)
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

func TestClientEscrowRetrieve(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/escrow/retrieve" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		var req enclave.EscrowRetrieveRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.EscrowID != "esc-456" {
			t.Fatalf("expected escrow ID esc-456, got %q", req.EscrowID)
		}
		json.NewEncoder(w).Encode(enclave.EscrowRetrieveResponse{
			Credential: []byte("plaintext-cred"),
		})
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	resp, err := c.EscrowRetrieve(context.Background(), enclave.EscrowRetrieveRequest{
		EscrowID: "esc-456",
	})
	if err != nil {
		t.Fatalf("EscrowRetrieve: %v", err)
	}
	if string(resp.Credential) != "plaintext-cred" {
		t.Fatalf("expected plaintext-cred, got %q", resp.Credential)
	}
}

func TestClientEscrowRetrieveError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("not found"))
	}))
	defer server.Close()

	c := New(Config{BaseURL: server.URL})
	_, err := c.EscrowRetrieve(context.Background(), enclave.EscrowRetrieveRequest{EscrowID: "bad"})
	if err == nil {
		t.Fatal("expected error for 404 response on EscrowRetrieve")
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
