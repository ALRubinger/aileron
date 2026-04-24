package local

import (
	"crypto/rand"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/enclave"
)

func TestKEKStoreAndGet(t *testing.T) {
	s := newKEKStore(30 * time.Minute)
	kek := make([]byte, 32)
	rand.Read(kek)

	s.Store("user-1", kek)

	got, err := s.Get("user-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != 32 {
		t.Fatalf("expected 32-byte KEK, got %d", len(got))
	}

	// Should return a copy, not the original.
	got[0] ^= 0xFF
	got2, _ := s.Get("user-1")
	if got2[0] == got[0] {
		t.Fatal("Get should return a copy")
	}
}

func TestKEKStoreNotFound(t *testing.T) {
	s := newKEKStore(30 * time.Minute)
	_, err := s.Get("nonexistent")
	if err != enclave.ErrNoKEK {
		t.Fatalf("expected ErrNoKEK, got %v", err)
	}
}

func TestKEKStoreExpired(t *testing.T) {
	s := newKEKStore(30 * time.Minute)
	kek := make([]byte, 32)
	rand.Read(kek)

	s.Store("user-1", kek)

	// Manually expire the entry.
	s.mu.Lock()
	s.entries["user-1"].expiresAt = time.Now().Add(-time.Second)
	s.mu.Unlock()

	_, err := s.Get("user-1")
	if err != enclave.ErrSessionExpired {
		t.Fatalf("expected ErrSessionExpired, got %v", err)
	}

	// Entry should have been removed.
	_, err = s.Get("user-1")
	if err != enclave.ErrNoKEK {
		t.Fatalf("expected ErrNoKEK after expiry removal, got %v", err)
	}
}

func TestKEKStoreOverwrite(t *testing.T) {
	s := newKEKStore(30 * time.Minute)
	kek1 := make([]byte, 32)
	kek2 := make([]byte, 32)
	rand.Read(kek1)
	rand.Read(kek2)

	s.Store("user-1", kek1)
	s.Store("user-1", kek2)

	got, err := s.Get("user-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Should match kek2, not kek1.
	for i := range got {
		if got[i] != kek2[i] {
			t.Fatalf("byte %d mismatch: expected %d, got %d", i, kek2[i], got[i])
		}
	}

	// The store's internal copy of kek1 should have been zeroed,
	// but the caller's original kek1 slice is not affected since Store copies it.
}

func TestKEKStoreClear(t *testing.T) {
	s := newKEKStore(30 * time.Minute)
	kek := make([]byte, 32)
	rand.Read(kek)
	s.Store("user-1", kek)
	s.Store("user-2", make([]byte, 32))

	s.Clear()

	_, err := s.Get("user-1")
	if err != enclave.ErrNoKEK {
		t.Fatalf("expected ErrNoKEK after clear, got %v", err)
	}
	_, err = s.Get("user-2")
	if err != enclave.ErrNoKEK {
		t.Fatalf("expected ErrNoKEK after clear, got %v", err)
	}
}

func TestKEKStoreRemoveNonExistent(t *testing.T) {
	s := newKEKStore(30 * time.Minute)
	// Should not panic.
	s.remove("nonexistent")
}
