package vault

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ALRubinger/aileron/enclave"
)

// mockEnclaveClient implements enclave.Client for testing EscrowVault.
type mockEnclaveClient struct {
	retrieveResp enclave.EscrowRetrieveResponse
	retrieveErr  error
}

func (m *mockEnclaveClient) Attest(_ context.Context, _ enclave.AttestationRequest) (enclave.AttestationResponse, error) {
	return enclave.AttestationResponse{}, nil
}
func (m *mockEnclaveClient) EstablishSession(_ context.Context, _ enclave.SessionRequest) (enclave.SessionResponse, error) {
	return enclave.SessionResponse{}, nil
}
func (m *mockEnclaveClient) TransmitKEK(_ context.Context, _ enclave.TransmitKEKRequest) (enclave.TransmitKEKResponse, error) {
	return enclave.TransmitKEKResponse{}, nil
}
func (m *mockEnclaveClient) OAuthExchange(_ context.Context, _ enclave.OAuthExchangeRequest) (enclave.OAuthExchangeResponse, error) {
	return enclave.OAuthExchangeResponse{}, nil
}
func (m *mockEnclaveClient) Execute(_ context.Context, _ enclave.ExecuteRequest) (enclave.ExecuteResponse, error) {
	return enclave.ExecuteResponse{}, nil
}
func (m *mockEnclaveClient) EscrowStore(_ context.Context, _ enclave.EscrowStoreRequest) (enclave.EscrowStoreResponse, error) {
	return enclave.EscrowStoreResponse{}, nil
}
func (m *mockEnclaveClient) EscrowRetrieve(_ context.Context, req enclave.EscrowRetrieveRequest) (enclave.EscrowRetrieveResponse, error) {
	return m.retrieveResp, m.retrieveErr
}
func (m *mockEnclaveClient) EscrowList(_ context.Context) (enclave.EscrowListResponse, error) {
	return enclave.EscrowListResponse{}, nil
}
func (m *mockEnclaveClient) EscrowRevoke(_ context.Context, _ enclave.EscrowRevokeRequest) error {
	return nil
}
func (m *mockEnclaveClient) Close() error { return nil }

func TestEscrowVault_Get_EscrowHit(t *testing.T) {
	client := &mockEnclaveClient{
		retrieveResp: enclave.EscrowRetrieveResponse{
			Credential: []byte("plaintext-token"),
		},
	}
	idx := &sync.Map{}
	idx.Store("connected-accounts/usr_1/slack", "esc_123")

	v := NewEscrowVault(client, idx, NewMemVault())
	secret, err := v.Get(context.Background(), "connected-accounts/usr_1/slack")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(secret.Value) != "plaintext-token" {
		t.Errorf("Value = %q, want plaintext-token", secret.Value)
	}
	if secret.Path != "connected-accounts/usr_1/slack" {
		t.Errorf("Path = %q, want connected-accounts/usr_1/slack", secret.Path)
	}
}

func TestEscrowVault_Get_EscrowMiss_FallsThrough(t *testing.T) {
	client := &mockEnclaveClient{}
	idx := &sync.Map{} // no entries

	inner := NewMemVault()
	inner.Put(context.Background(), "connected-accounts/usr_1/gmail", []byte("inner-token"), Metadata{Type: "oauth"})

	v := NewEscrowVault(client, idx, inner)
	secret, err := v.Get(context.Background(), "connected-accounts/usr_1/gmail")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(secret.Value) != "inner-token" {
		t.Errorf("Value = %q, want inner-token", secret.Value)
	}
}

func TestEscrowVault_Get_EscrowExpired(t *testing.T) {
	client := &mockEnclaveClient{
		retrieveErr: enclave.ErrEscrowExpired,
	}
	idx := &sync.Map{}
	idx.Store("connected-accounts/usr_1/slack", "esc_expired")

	v := NewEscrowVault(client, idx, NewMemVault())
	_, err := v.Get(context.Background(), "connected-accounts/usr_1/slack")
	if err == nil {
		t.Fatal("expected error for expired escrow")
	}
	if !errors.Is(err, enclave.ErrEscrowExpired) {
		// The error is wrapped, so check the message.
		if !containsStr(err.Error(), "escrow") {
			t.Errorf("error = %v, want escrow-related error", err)
		}
	}
}

func TestEscrowVault_Put_DelegatesToFallback(t *testing.T) {
	client := &mockEnclaveClient{}
	idx := &sync.Map{}
	inner := NewMemVault()

	v := NewEscrowVault(client, idx, inner)
	err := v.Put(context.Background(), "test/path", []byte("value"), Metadata{Type: "test"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	secret, err := inner.Get(context.Background(), "test/path")
	if err != nil {
		t.Fatalf("inner Get: %v", err)
	}
	if string(secret.Value) != "value" {
		t.Errorf("inner value = %q, want value", secret.Value)
	}
}

func TestEscrowVault_Delete_DelegatesToFallback(t *testing.T) {
	client := &mockEnclaveClient{}
	idx := &sync.Map{}
	inner := NewMemVault()
	inner.Put(context.Background(), "test/path", []byte("value"), Metadata{})

	v := NewEscrowVault(client, idx, inner)
	err := v.Delete(context.Background(), "test/path")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err = inner.Get(context.Background(), "test/path")
	if err == nil {
		t.Error("expected error after delete")
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && contains(s, sub))
}

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
