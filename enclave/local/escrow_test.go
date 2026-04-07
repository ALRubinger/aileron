package local

import (
	"testing"
	"time"

	"github.com/ALRubinger/aileron/enclave"
)

func TestEscrowStoreAndGet(t *testing.T) {
	s := newEscrowStore()

	cred := []byte("test-credential")
	id := s.Store("grant-1", cred, "api_key", []string{"payment.charge"}, time.Now().Add(time.Hour))

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
	id := s.Store("grant-1", cred, "api_key", nil, time.Now().Add(-time.Second))

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
	id := s.Store("grant-1", cred, "api_key", nil, time.Now().Add(time.Hour))

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

	liveID := s.Store("g1", live, "api_key", nil, time.Now().Add(time.Hour))
	s.Store("g2", expired, "api_key", nil, time.Now().Add(-time.Second))

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

func TestEscrowClear(t *testing.T) {
	s := newEscrowStore()
	cred := []byte("clear-me")
	id := s.Store("g1", cred, "api_key", nil, time.Now().Add(time.Hour))

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
