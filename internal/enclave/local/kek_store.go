package local

import (
	"sync"
	"time"

	"github.com/ALRubinger/aileron/internal/enclave"
)

type kekEntry struct {
	kek       []byte
	expiresAt time.Time
}

// kekStore holds per-user KEKs in the enclave's memory.
type kekStore struct {
	mu      sync.RWMutex
	entries map[string]*kekEntry // keyed by user ID
	ttl     time.Duration
}

func newKEKStore(ttl time.Duration) *kekStore {
	return &kekStore{
		entries: make(map[string]*kekEntry),
		ttl:     ttl,
	}
}

// Store saves a KEK for a user. Any previous KEK for the user is zeroed.
func (s *kekStore) Store(userID string, kek []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if old, ok := s.entries[userID]; ok {
		zeroBytes(old.kek)
	}

	cpy := make([]byte, len(kek))
	copy(cpy, kek)
	s.entries[userID] = &kekEntry{
		kek:       cpy,
		expiresAt: time.Now().Add(s.ttl),
	}
}

// Get returns a copy of the user's KEK. Returns ErrNoKEK if not found or expired.
func (s *kekStore) Get(userID string) ([]byte, error) {
	s.mu.RLock()
	entry, ok := s.entries[userID]
	s.mu.RUnlock()

	if !ok {
		return nil, enclave.ErrNoKEK
	}
	if time.Now().After(entry.expiresAt) {
		s.remove(userID)
		return nil, enclave.ErrSessionExpired
	}
	return copyBytes(entry.kek), nil
}

// Clear zeros and removes all entries.
func (s *kekStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, entry := range s.entries {
		zeroBytes(entry.kek)
		delete(s.entries, id)
	}
}

func (s *kekStore) remove(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.entries[userID]; ok {
		zeroBytes(entry.kek)
		delete(s.entries, userID)
	}
}
