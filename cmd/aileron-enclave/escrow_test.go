package main

import (
	"testing"
	"time"

	"github.com/ALRubinger/aileron/enclave"
)

func TestEscrowStoreAndGet(t *testing.T) {
	s := newEscrowStore()
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
	s := newEscrowStore()
	_, err := s.Get("nonexistent")
	if err != enclave.ErrEscrowNotFound {
		t.Fatalf("expected ErrEscrowNotFound, got %v", err)
	}
}

func TestEscrowExpired(t *testing.T) {
	s := newEscrowStore()
	id := s.Store("g1", []byte("dead"), "api_key", nil, time.Now().Add(-time.Second))
	_, err := s.Get(id)
	if err != enclave.ErrEscrowExpired {
		t.Fatalf("expected ErrEscrowExpired, got %v", err)
	}
}

func TestEscrowRevokeWrongGrant(t *testing.T) {
	s := newEscrowStore()
	id := s.Store("grant-1", []byte("cred"), "api_key", nil, time.Now().Add(time.Hour))
	err := s.Revoke(id, "wrong-grant")
	if err != enclave.ErrEscrowNotFound {
		t.Fatalf("expected ErrEscrowNotFound, got %v", err)
	}
}

func TestEscrowEvictExpired(t *testing.T) {
	s := newEscrowStore()
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
	s := newEscrowStore()
	err := s.Revoke("nonexistent", "grant")
	if err != enclave.ErrEscrowNotFound {
		t.Fatalf("expected ErrEscrowNotFound, got %v", err)
	}
}
