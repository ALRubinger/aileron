package main

import (
	"os"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/crypto"
	"github.com/ALRubinger/aileron/internal/enclave"
)

func encryptForTest(data, key []byte) ([]byte, error) {
	return crypto.Encrypt(data, key)
}

func mustNewEscrowStore(t *testing.T) *escrowStore {
	t.Helper()
	s, err := newEscrowStore("") // in-memory only
	if err != nil {
		t.Fatalf("newEscrowStore: %v", err)
	}
	return s
}

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
	s := mustNewEscrowStore(t)
	cred := []byte("test-cred")
	id := s.Store(testEscrowRequest("grant-1", "api_key", []string{"action"}), cred, time.Now().Add(time.Hour))

	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "test-cred" {
		t.Fatalf("expected test-cred, got %q", got)
	}

	// Should return a copy.
	got[0] = 'X'
	got2, _ := s.Get(id)
	if got2[0] == 'X' {
		t.Fatal("should return a copy")
	}
}

func TestEscrowGetForExecuteEnforcesScope(t *testing.T) {
	s := mustNewEscrowStore(t)
	req := testEscrowRequest("grant-1", "api_key", []string{"email.send"})
	req.AllowedParameters = map[string]any{"to": "a@example.com"}
	id := s.Store(req, []byte("scoped-cred"), time.Now().Add(time.Hour))

	valid := enclave.ExecuteRequest{
		EscrowID:       id,
		UserID:         "user-1",
		GrantID:        "grant-1",
		VaultPath:      "connected-accounts/user-1/gmail",
		Provider:       "gmail",
		CredentialType: "api_key",
		ActionType:     "email.send",
		Parameters:     map[string]any{"to": "a@example.com"},
	}
	if _, err := s.GetForExecute(valid); err != nil {
		t.Fatalf("GetForExecute valid scope: %v", err)
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
		{"action", func(r *enclave.ExecuteRequest) { r.ActionType = "email.delete" }},
		{"params", func(r *enclave.ExecuteRequest) { r.Parameters = map[string]any{"to": "b@example.com"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := valid
			tt.mutate(&candidate)
			_, err := s.GetForExecute(candidate)
			if err != enclave.ErrEscrowScopeMismatch {
				t.Fatalf("expected ErrEscrowScopeMismatch, got %v", err)
			}
		})
	}
}

func TestEscrowGetForExecuteEnforcesIssuedCapabilityBinding(t *testing.T) {
	baseCap := enclave.GrantCapability{
		GrantID:        "grant-1",
		UserID:         "user-1",
		VaultPath:      "connected-accounts/user-1/gmail",
		Provider:       "gmail",
		CredentialType: "oauth2",
		ActionType:     "email.send",
		Nonce:          "nonce-1",
	}
	s := mustNewEscrowStore(t)
	id := s.Store(enclave.EscrowStoreRequest{
		IssuedCapability: &enclave.SignedGrantCapability{Capability: baseCap, Signature: "sig"},
	}, []byte("scoped-cred"), time.Now().Add(time.Hour))

	valid := enclave.ExecuteRequest{
		EscrowID:       id,
		GrantID:        "grant-1",
		UserID:         "user-1",
		VaultPath:      "connected-accounts/user-1/gmail",
		Provider:       "gmail",
		CredentialType: "oauth2",
		ActionType:     "email.send",
	}
	if _, err := s.GetForExecute(valid); err != nil {
		t.Fatalf("GetForExecute valid issued scope: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*enclave.ExecuteRequest)
	}{
		{"grant", func(r *enclave.ExecuteRequest) { r.GrantID = "grant-2" }},
		{"user", func(r *enclave.ExecuteRequest) { r.UserID = "user-2" }},
		{"vault", func(r *enclave.ExecuteRequest) { r.VaultPath = "connected-accounts/user-2/gmail" }},
		{"provider", func(r *enclave.ExecuteRequest) { r.Provider = "drive" }},
		{"credential type", func(r *enclave.ExecuteRequest) { r.CredentialType = "api_key" }},
		{"action", func(r *enclave.ExecuteRequest) { r.ActionType = "email.delete" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := valid
			tt.mutate(&candidate)
			_, err := s.GetForExecute(candidate)
			if err != enclave.ErrEscrowScopeMismatch {
				t.Fatalf("expected ErrEscrowScopeMismatch, got %v", err)
			}
		})
	}
}

func TestEscrowGetForExecuteEnforcesIssuedCapabilityActionTypes(t *testing.T) {
	s := mustNewEscrowStore(t)
	id := s.Store(enclave.EscrowStoreRequest{
		IssuedCapability: &enclave.SignedGrantCapability{
			Capability: enclave.GrantCapability{
				ActionTypes: []string{"email.send", "email.archive"},
				Nonce:       "nonce-1",
			},
			Signature: "sig",
		},
	}, []byte("scoped-cred"), time.Now().Add(time.Hour))

	valid := enclave.ExecuteRequest{EscrowID: id, ActionType: "email.archive"}
	if _, err := s.GetForExecute(valid); err != nil {
		t.Fatalf("GetForExecute valid action type: %v", err)
	}
	invalid := enclave.ExecuteRequest{EscrowID: id, ActionType: "email.delete"}
	if _, err := s.GetForExecute(invalid); err != enclave.ErrEscrowScopeMismatch {
		t.Fatalf("expected ErrEscrowScopeMismatch, got %v", err)
	}
}

func TestEscrowGetForSourceEnforcesScope(t *testing.T) {
	s := mustNewEscrowStore(t)
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
		t.Fatalf("GetForSource valid scope: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*enclave.SourceExecuteRequest)
	}{
		{"user", func(r *enclave.SourceExecuteRequest) { r.UserID = "user-2" }},
		{"vault", func(r *enclave.SourceExecuteRequest) { r.VaultPath = "connected-accounts/user-2/gmail" }},
		{"provider", func(r *enclave.SourceExecuteRequest) { r.Provider = "slack" }},
		{"tool", func(r *enclave.SourceExecuteRequest) { r.Tool = "gmail_delete_everything" }},
		{"params", func(r *enclave.SourceExecuteRequest) { r.Params = map[string]any{"query": "to:boss"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := valid
			tt.mutate(&candidate)
			_, err := s.GetForSource(candidate)
			if err != enclave.ErrEscrowScopeMismatch {
				t.Fatalf("expected ErrEscrowScopeMismatch, got %v", err)
			}
		})
	}
}

func TestEscrowGetForSourceEnforcesIssuedCapabilityBinding(t *testing.T) {
	baseCap := enclave.GrantCapability{
		UserID:    "user-1",
		VaultPath: "connected-accounts/user-1/gmail",
		Provider:  "gmail",
		Tool:      "gmail_search",
		Nonce:     "nonce-1",
	}
	s := mustNewEscrowStore(t)
	id := s.Store(enclave.EscrowStoreRequest{
		IssuedCapability: &enclave.SignedGrantCapability{Capability: baseCap, Signature: "sig"},
	}, []byte("token"), time.Now().Add(time.Hour))

	valid := enclave.SourceExecuteRequest{
		EscrowID:  id,
		UserID:    "user-1",
		VaultPath: "connected-accounts/user-1/gmail",
		Provider:  "gmail",
		Tool:      "gmail_search",
	}
	if _, err := s.GetForSource(valid); err != nil {
		t.Fatalf("GetForSource valid issued scope: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*enclave.SourceExecuteRequest)
	}{
		{"user", func(r *enclave.SourceExecuteRequest) { r.UserID = "user-2" }},
		{"vault", func(r *enclave.SourceExecuteRequest) { r.VaultPath = "connected-accounts/user-2/gmail" }},
		{"provider", func(r *enclave.SourceExecuteRequest) { r.Provider = "drive" }},
		{"tool", func(r *enclave.SourceExecuteRequest) { r.Tool = "gmail_delete" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := valid
			tt.mutate(&candidate)
			_, err := s.GetForSource(candidate)
			if err != enclave.ErrEscrowScopeMismatch {
				t.Fatalf("expected ErrEscrowScopeMismatch, got %v", err)
			}
		})
	}
}

func TestEscrowGetForSourceEnforcesIssuedCapabilitySourceTools(t *testing.T) {
	s := mustNewEscrowStore(t)
	id := s.Store(enclave.EscrowStoreRequest{
		IssuedCapability: &enclave.SignedGrantCapability{
			Capability: enclave.GrantCapability{
				SourceTools: []string{"gmail_search", "gmail_threads"},
				Nonce:       "nonce-1",
			},
			Signature: "sig",
		},
	}, []byte("token"), time.Now().Add(time.Hour))

	valid := enclave.SourceExecuteRequest{EscrowID: id, Tool: "gmail_threads"}
	if _, err := s.GetForSource(valid); err != nil {
		t.Fatalf("GetForSource valid source tool: %v", err)
	}
	invalid := enclave.SourceExecuteRequest{EscrowID: id, Tool: "gmail_delete"}
	if _, err := s.GetForSource(invalid); err != enclave.ErrEscrowScopeMismatch {
		t.Fatalf("expected ErrEscrowScopeMismatch, got %v", err)
	}
}

func TestEscrowNotFound(t *testing.T) {
	s := mustNewEscrowStore(t)
	_, err := s.Get("nonexistent")
	if err != enclave.ErrEscrowNotFound {
		t.Fatalf("expected ErrEscrowNotFound, got %v", err)
	}
}

func TestEscrowExpired(t *testing.T) {
	s := mustNewEscrowStore(t)
	id := s.Store(testEscrowRequest("g1", "api_key", nil), []byte("dead"), time.Now().Add(-time.Second))
	_, err := s.Get(id)
	if err != enclave.ErrEscrowExpired {
		t.Fatalf("expected ErrEscrowExpired, got %v", err)
	}
}

func TestEscrowRevokeWrongGrant(t *testing.T) {
	s := mustNewEscrowStore(t)
	id := s.Store(testEscrowRequest("grant-1", "api_key", nil), []byte("cred"), time.Now().Add(time.Hour))
	err := s.Revoke(id, "wrong-grant")
	if err != enclave.ErrEscrowNotFound {
		t.Fatalf("expected ErrEscrowNotFound, got %v", err)
	}
}

func TestEscrowEvictExpired(t *testing.T) {
	s := mustNewEscrowStore(t)
	live := []byte("live")
	dead := []byte("dead")
	liveID := s.Store(testEscrowRequest("g1", "api_key", nil), live, time.Now().Add(time.Hour))
	s.Store(testEscrowRequest("g2", "api_key", nil), dead, time.Now().Add(-time.Second))

	s.EvictExpired()

	_, err := s.Get(liveID)
	if err != nil {
		t.Fatalf("live should exist: %v", err)
	}
	for _, b := range dead {
		if b != 0 {
			t.Fatal("dead bytes should be zeroed")
		}
	}
}

func TestEscrowRevokeNotFound(t *testing.T) {
	s := mustNewEscrowStore(t)
	err := s.Revoke("nonexistent", "grant")
	if err != enclave.ErrEscrowNotFound {
		t.Fatalf("expected ErrEscrowNotFound, got %v", err)
	}
}

func TestEscrowPersistence_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Store entries in one escrow store.
	s1, err := newEscrowStore(dir)
	if err != nil {
		t.Fatalf("newEscrowStore: %v", err)
	}
	id1 := s1.Store(testEscrowRequest("grant-1", "oauth", nil), []byte("cred-one"), time.Now().Add(time.Hour))
	id2 := s1.Store(testEscrowRequest("grant-2", "api_key", []string{"email.send"}), []byte("cred-two"), time.Now().Add(2*time.Hour))

	// Create a new escrow store from the same directory — simulates restart.
	s2, err := newEscrowStore(dir)
	if err != nil {
		t.Fatalf("newEscrowStore after restart: %v", err)
	}

	got1, err := s2.Get(id1)
	if err != nil {
		t.Fatalf("Get id1 after restart: %v", err)
	}
	if string(got1) != "cred-one" {
		t.Errorf("id1 = %q, want cred-one", got1)
	}

	got2, err := s2.Get(id2)
	if err != nil {
		t.Fatalf("Get id2 after restart: %v", err)
	}
	if string(got2) != "cred-two" {
		t.Errorf("id2 = %q, want cred-two", got2)
	}
}

func TestEscrowPersistence_ExpiredNotLoaded(t *testing.T) {
	dir := t.TempDir()

	s1, err := newEscrowStore(dir)
	if err != nil {
		t.Fatalf("newEscrowStore: %v", err)
	}
	// Store an already-expired entry.
	id := s1.Store(testEscrowRequest("g1", "oauth", nil), []byte("expired-cred"), time.Now().Add(-time.Second))

	// Restart — expired entry should be evicted on load.
	s2, err := newEscrowStore(dir)
	if err != nil {
		t.Fatalf("newEscrowStore after restart: %v", err)
	}
	_, err = s2.Get(id)
	if err == nil {
		t.Fatal("expected error for expired entry after restart")
	}
}

func TestEscrowPersistence_CorruptFile(t *testing.T) {
	dir := t.TempDir()

	// Write garbage to the data file.
	os.WriteFile(dir+"/escrow.dat", []byte("not encrypted"), 0600)

	// Generate a valid key so newEscrowStore doesn't fail on key load.
	enclave.LoadOrCreateKey(dir + "/escrow.key")

	// Should start fresh, not error out.
	s, err := newEscrowStore(dir)
	if err != nil {
		t.Fatalf("newEscrowStore with corrupt file: %v", err)
	}

	// Should be empty.
	entries := s.List()
	if len(entries) != 0 {
		t.Errorf("expected 0 entries after corrupt file, got %d", len(entries))
	}
}

func TestNewEscrowStore_MkdirFailure(t *testing.T) {
	// Use a file as the "directory" path — MkdirAll will fail.
	dir := t.TempDir()
	blockingFile := dir + "/blocker"
	os.WriteFile(blockingFile, []byte("x"), 0600)

	_, err := newEscrowStore(blockingFile + "/subdir")
	if err == nil {
		t.Fatal("expected error when data dir path is blocked by a file")
	}
}

func TestEscrowPersistence_ReadError(t *testing.T) {
	dir := t.TempDir()

	// Create a valid key.
	enclave.LoadOrCreateKey(dir + "/escrow.key")

	// Make escrow.dat a directory so ReadFile fails with a non-NotExist error.
	os.Mkdir(dir+"/escrow.dat", 0700)

	_, err := newEscrowStore(dir)
	if err == nil {
		t.Fatal("expected error when escrow.dat is a directory")
	}
}

func TestEscrowPersistence_CorruptJSON(t *testing.T) {
	dir := t.TempDir()

	// Create store and persist an entry to generate key + valid encrypted blob.
	s1, err := newEscrowStore(dir)
	if err != nil {
		t.Fatalf("newEscrowStore: %v", err)
	}
	s1.Store(testEscrowRequest("g1", "api_key", nil), []byte("cred"), time.Now().Add(time.Hour))

	// Now overwrite escrow.dat with data that decrypts to invalid JSON.
	// We use the existing key to encrypt garbage JSON.
	key, _ := os.ReadFile(dir + "/escrow.key")
	badJSON := []byte("{invalid json")
	encrypted, err := encryptForTest(badJSON, key)
	if err != nil {
		t.Fatalf("encrypting bad JSON: %v", err)
	}
	os.WriteFile(dir+"/escrow.dat", encrypted, 0600)

	// Should start fresh (not error).
	s2, err := newEscrowStore(dir)
	if err != nil {
		t.Fatalf("newEscrowStore with corrupt JSON: %v", err)
	}
	entries := s2.List()
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestEscrowUpdate_HappyPath(t *testing.T) {
	s := mustNewEscrowStore(t)
	original := []byte(`{"access_token":"old","refresh_token":"r1"}`)
	id := s.Store(testEscrowRequest("g1", "oauth2", nil), original, time.Now().Add(time.Hour))

	updated := []byte(`{"access_token":"new","refresh_token":"r2"}`)
	if err := s.Update(id, updated); err != nil {
		t.Fatalf("Update: %v", err)
	}

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
	s := mustNewEscrowStore(t)
	err := s.Update("esc_nonexistent", []byte("data"))
	if err != enclave.ErrEscrowNotFound {
		t.Errorf("expected ErrEscrowNotFound, got %v", err)
	}
}

func TestEscrowUpdate_Expired(t *testing.T) {
	s := mustNewEscrowStore(t)
	id := s.Store(testEscrowRequest("g1", "oauth2", nil), []byte("cred"), time.Now().Add(-time.Second))
	err := s.Update(id, []byte("new"))
	if err != enclave.ErrEscrowExpired {
		t.Errorf("expected ErrEscrowExpired, got %v", err)
	}
}

func TestEscrowUpdate_Persistence(t *testing.T) {
	dir := t.TempDir()
	s1, err := newEscrowStore(dir)
	if err != nil {
		t.Fatalf("newEscrowStore: %v", err)
	}

	id := s1.Store(testEscrowRequest("g1", "oauth2", nil), []byte("old"), time.Now().Add(time.Hour))
	if err := s1.Update(id, []byte("refreshed")); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Reload from disk.
	s2, err := newEscrowStore(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, err := s2.Get(id)
	if err != nil {
		t.Fatalf("Get after reload: %v", err)
	}
	if string(got) != "refreshed" {
		t.Errorf("persisted value = %q, want %q", got, "refreshed")
	}
}

func TestEscrowList(t *testing.T) {
	s := mustNewEscrowStore(t)
	s.Store(testEscrowRequest("g1", "oauth", nil), []byte("c1"), time.Now().Add(time.Hour))
	s.Store(testEscrowRequest("g2", "api_key", nil), []byte("c2"), time.Now().Add(time.Hour))
	s.Store(testEscrowRequest("g3", "api_key", nil), []byte("expired"), time.Now().Add(-time.Second))

	entries := s.List()
	if len(entries) != 2 {
		t.Fatalf("expected 2 non-expired entries, got %d", len(entries))
	}
}
