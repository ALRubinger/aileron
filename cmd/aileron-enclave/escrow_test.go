package main

import (
	"os"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/crypto"
	"github.com/ALRubinger/aileron/enclave"
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

func TestEscrowStoreAndGet(t *testing.T) {
	s := mustNewEscrowStore(t)
	cred := []byte("test-cred")
	id := s.Store("grant-1", cred, "api_key", []string{"action"}, time.Now().Add(time.Hour))

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

func TestEscrowNotFound(t *testing.T) {
	s := mustNewEscrowStore(t)
	_, err := s.Get("nonexistent")
	if err != enclave.ErrEscrowNotFound {
		t.Fatalf("expected ErrEscrowNotFound, got %v", err)
	}
}

func TestEscrowExpired(t *testing.T) {
	s := mustNewEscrowStore(t)
	id := s.Store("g1", []byte("dead"), "api_key", nil, time.Now().Add(-time.Second))
	_, err := s.Get(id)
	if err != enclave.ErrEscrowExpired {
		t.Fatalf("expected ErrEscrowExpired, got %v", err)
	}
}

func TestEscrowRevokeWrongGrant(t *testing.T) {
	s := mustNewEscrowStore(t)
	id := s.Store("grant-1", []byte("cred"), "api_key", nil, time.Now().Add(time.Hour))
	err := s.Revoke(id, "wrong-grant")
	if err != enclave.ErrEscrowNotFound {
		t.Fatalf("expected ErrEscrowNotFound, got %v", err)
	}
}

func TestEscrowEvictExpired(t *testing.T) {
	s := mustNewEscrowStore(t)
	live := []byte("live")
	dead := []byte("dead")
	liveID := s.Store("g1", live, "api_key", nil, time.Now().Add(time.Hour))
	s.Store("g2", dead, "api_key", nil, time.Now().Add(-time.Second))

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
	id1 := s1.Store("grant-1", []byte("cred-one"), "oauth", nil, time.Now().Add(time.Hour))
	id2 := s1.Store("grant-2", []byte("cred-two"), "api_key", []string{"email.send"}, time.Now().Add(2*time.Hour))

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
	id := s1.Store("g1", []byte("expired-cred"), "oauth", nil, time.Now().Add(-time.Second))

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
	s1.Store("g1", []byte("cred"), "api_key", nil, time.Now().Add(time.Hour))

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

func TestEscrowList(t *testing.T) {
	s := mustNewEscrowStore(t)
	s.Store("g1", []byte("c1"), "oauth", nil, time.Now().Add(time.Hour))
	s.Store("g2", []byte("c2"), "api_key", nil, time.Now().Add(time.Hour))
	s.Store("g3", []byte("expired"), "api_key", nil, time.Now().Add(-time.Second))

	entries := s.List()
	if len(entries) != 2 {
		t.Fatalf("expected 2 non-expired entries, got %d", len(entries))
	}
}
