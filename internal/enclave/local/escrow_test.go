package local

import (
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/enclave"
)

func testEscrowRequest(grantID, credType string, actionTypes []string) enclave.EscrowStoreRequest {
	return enclave.EscrowStoreRequest{
		UserID:         "user-1",
		GrantID:        grantID,
		EnforceGrantID: true,
		VaultPath:      "connected-accounts/user-1/gmail",
		Provider:       "gmail",
		CredentialType: credType,
		ActionTypes:    actionTypes,
		SourceTools:    []string{"test_search", "gmail_search"},
	}
}

func TestEscrowStoreAndGet(t *testing.T) {
	s := newEscrowStore()

	cred := []byte("test-credential")
	id := s.Store(testEscrowRequest("grant-1", "api_key", []string{"payment.charge"}), cred, time.Now().Add(time.Hour))

	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(cred) {
		t.Fatalf("expected %q, got %q", cred, got)
	}

	// Returned value should be a copy.
	got[0] = 'X'
	got2, _ := s.Get(id)
	if got2[0] == 'X' {
		t.Fatal("Get should return a copy, not the original")
	}
}

func TestEscrowGetForExecuteEnforcesScope(t *testing.T) {
	s := newEscrowStore()
	id := s.Store(testEscrowRequest("grant-1", "api_key", []string{"payment.charge"}), []byte("test-credential"), time.Now().Add(time.Hour))

	valid := enclave.ExecuteRequest{
		EscrowID:       id,
		UserID:         "user-1",
		GrantID:        "grant-1",
		VaultPath:      "connected-accounts/user-1/gmail",
		Provider:       "gmail",
		CredentialType: "api_key",
		ActionType:     "payment.charge",
	}
	if _, err := s.GetForExecute(valid); err != nil {
		t.Fatalf("valid GetForExecute: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*enclave.ExecuteRequest)
	}{
		{"user", func(r *enclave.ExecuteRequest) { r.UserID = "user-2" }},
		{"grant", func(r *enclave.ExecuteRequest) { r.GrantID = "grant-2" }},
		{"vault", func(r *enclave.ExecuteRequest) { r.VaultPath = "connected-accounts/user-2/gmail" }},
		{"provider", func(r *enclave.ExecuteRequest) { r.Provider = "slack" }},
		{"credential type", func(r *enclave.ExecuteRequest) { r.CredentialType = "oauth2" }},
		{"action", func(r *enclave.ExecuteRequest) { r.ActionType = "payment.refund" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := valid
			tt.mutate(&req)
			_, err := s.GetForExecute(req)
			if err != enclave.ErrEscrowScopeMismatch {
				t.Fatalf("expected ErrEscrowScopeMismatch, got %v", err)
			}
		})
	}
}

func TestEscrowGetForSourceEnforcesScope(t *testing.T) {
	s := newEscrowStore()
	req := testEscrowRequest("grant-1", "oauth2", nil)
	req.AllowedParameters = map[string]any{"query": "from:boss"}
	id := s.Store(req, []byte("token"), time.Now().Add(time.Hour))

	valid := enclave.SourceExecuteRequest{
		EscrowID:  id,
		UserID:    "user-1",
		VaultPath: "connected-accounts/user-1/gmail",
		Provider:  "gmail",
		Tool:      "gmail_search",
		Params:    map[string]any{"query": "from:boss"},
	}
	if _, err := s.GetForSource(valid); err != nil {
		t.Fatalf("valid GetForSource: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*enclave.SourceExecuteRequest)
	}{
		{"user", func(r *enclave.SourceExecuteRequest) { r.UserID = "user-2" }},
		{"vault", func(r *enclave.SourceExecuteRequest) { r.VaultPath = "connected-accounts/user-2/gmail" }},
		{"provider", func(r *enclave.SourceExecuteRequest) { r.Provider = "slack" }},
		{"tool", func(r *enclave.SourceExecuteRequest) { r.Tool = "gmail_delete" }},
		{"params", func(r *enclave.SourceExecuteRequest) { r.Params = map[string]any{"query": "to:boss"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := valid
			tt.mutate(&req)
			_, err := s.GetForSource(req)
			if err != enclave.ErrEscrowScopeMismatch {
				t.Fatalf("expected ErrEscrowScopeMismatch, got %v", err)
			}
		})
	}
}

func TestEscrowNotFound(t *testing.T) {
	s := newEscrowStore()
	_, err := s.Get("nonexistent")
	if err != enclave.ErrEscrowNotFound {
		t.Fatalf("expected ErrEscrowNotFound, got %v", err)
	}
}

func TestEscrowExpired(t *testing.T) {
	s := newEscrowStore()
	cred := []byte("expired-cred")
	id := s.Store(testEscrowRequest("grant-1", "api_key", nil), cred, time.Now().Add(-time.Second))

	_, err := s.Get(id)
	if err != enclave.ErrEscrowExpired {
		t.Fatalf("expected ErrEscrowExpired, got %v", err)
	}

	// Entry should have been evicted.
	_, err = s.Get(id)
	if err != enclave.ErrEscrowNotFound {
		t.Fatalf("expected ErrEscrowNotFound after auto-eviction, got %v", err)
	}
}

func TestEscrowRevokeDirectly(t *testing.T) {
	s := newEscrowStore()
	cred := []byte("revoke-me")
	id := s.Store(testEscrowRequest("grant-1", "api_key", nil), cred, time.Now().Add(time.Hour))

	// Wrong grant ID.
	err := s.Revoke(id, "wrong-grant")
	if err != enclave.ErrEscrowNotFound {
		t.Fatalf("expected ErrEscrowNotFound for wrong grant, got %v", err)
	}

	// Correct grant ID.
	err = s.Revoke(id, "grant-1")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// Credential bytes should be zeroed.
	for _, b := range cred {
		if b != 0 {
			t.Fatal("credential bytes should be zeroed after revoke")
		}
	}
}

func TestEscrowEvictExpired(t *testing.T) {
	s := newEscrowStore()
	live := []byte("live")
	expired := []byte("dead")

	liveID := s.Store(testEscrowRequest("g1", "api_key", nil), live, time.Now().Add(time.Hour))
	s.Store(testEscrowRequest("g2", "api_key", nil), expired, time.Now().Add(-time.Second))

	s.EvictExpired()

	// Live entry should still exist.
	_, err := s.Get(liveID)
	if err != nil {
		t.Fatalf("live entry should still exist: %v", err)
	}

	// Expired bytes should be zeroed.
	for _, b := range expired {
		if b != 0 {
			t.Fatal("expired credential bytes should be zeroed")
		}
	}
}

func TestEscrowList(t *testing.T) {
	s := newEscrowStore()

	// Empty store returns nil/empty.
	entries := s.List()
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}

	// Add a mix of live and expired entries.
	s.Store(testEscrowRequest("g1", "api_key", nil), []byte("cred-1"), time.Now().Add(time.Hour))
	s.Store(testEscrowRequest("g2", "oauth", nil), []byte("cred-2"), time.Now().Add(time.Hour))
	s.Store(testEscrowRequest("g3", "api_key", nil), []byte("cred-3"), time.Now().Add(-time.Second)) // expired

	entries = s.List()
	if len(entries) != 2 {
		t.Fatalf("expected 2 non-expired entries, got %d", len(entries))
	}

	// Verify grant IDs are present.
	grants := map[string]bool{}
	for _, e := range entries {
		grants[e.GrantID] = true
		if e.EscrowID == "" {
			t.Fatal("EscrowID should not be empty")
		}
		if e.ExpiresAt == "" {
			t.Fatal("ExpiresAt should not be empty")
		}
	}
	if !grants["g1"] || !grants["g2"] {
		t.Fatalf("expected grants g1 and g2, got %v", grants)
	}
}

func TestEscrowUpdate(t *testing.T) {
	s := newEscrowStore()
	original := []byte(`{"access_token":"old","refresh_token":"r1"}`)
	id := s.Store(testEscrowRequest("g1", "oauth2", nil), original, time.Now().Add(time.Hour))

	// Update with refreshed token.
	updated := []byte(`{"access_token":"new","refresh_token":"r2"}`)
	if err := s.Update(id, updated); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Get should return the updated credential.
	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if string(got) != string(updated) {
		t.Errorf("got %q, want %q", got, updated)
	}

	// Original bytes should be zeroed.
	for _, b := range original {
		if b != 0 {
			t.Fatal("original credential bytes should be zeroed after update")
		}
	}
}

func TestEscrowUpdate_NotFound(t *testing.T) {
	s := newEscrowStore()
	err := s.Update("esc_nonexistent", []byte("data"))
	if err != enclave.ErrEscrowNotFound {
		t.Errorf("expected ErrEscrowNotFound, got %v", err)
	}
}

func TestEscrowUpdate_Expired(t *testing.T) {
	s := newEscrowStore()
	id := s.Store(testEscrowRequest("g1", "oauth2", nil), []byte("cred"), time.Now().Add(-time.Second))
	err := s.Update(id, []byte("new"))
	if err != enclave.ErrEscrowExpired {
		t.Errorf("expected ErrEscrowExpired, got %v", err)
	}
}

func TestEscrowClear(t *testing.T) {
	s := newEscrowStore()
	cred := []byte("clear-me")
	id := s.Store(testEscrowRequest("g1", "api_key", nil), cred, time.Now().Add(time.Hour))

	s.Clear()

	_, err := s.Get(id)
	if err != enclave.ErrEscrowNotFound {
		t.Fatalf("expected ErrEscrowNotFound after clear, got %v", err)
	}

	for _, b := range cred {
		if b != 0 {
			t.Fatal("credential bytes should be zeroed after clear")
		}
	}
}
