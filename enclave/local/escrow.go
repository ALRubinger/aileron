package local

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/enclave"
)

type escrowEntry struct {
	grantID     string
	credential  []byte // plaintext, held in memory
	credType    string
	actionTypes []string
	expiresAt   time.Time
}

type escrowStore struct {
	mu      sync.RWMutex
	entries map[string]*escrowEntry
}

func newEscrowStore() *escrowStore {
	return &escrowStore{entries: make(map[string]*escrowEntry)}
}

// Store adds a credential to the escrow. The credential is already decrypted
// and is held in plaintext in memory. Returns the escrow ID.
func (s *escrowStore) Store(grantID string, credential []byte, credType string, actionTypes []string, expiresAt time.Time) string {
	b := make([]byte, 16)
	rand.Read(b)
	id := "esc_" + hex.EncodeToString(b)

	s.mu.Lock()
	s.entries[id] = &escrowEntry{
		grantID:     grantID,
		credential:  credential,
		credType:    credType,
		actionTypes: actionTypes,
		expiresAt:   expiresAt,
	}
	s.mu.Unlock()
	return id
}

// Get retrieves the plaintext credential for an escrow entry. Returns a copy.
func (s *escrowStore) Get(escrowID string) ([]byte, error) {
	s.mu.RLock()
	entry, ok := s.entries[escrowID]
	s.mu.RUnlock()

	if !ok {
		return nil, enclave.ErrEscrowNotFound
	}
	if time.Now().After(entry.expiresAt) {
		s.revokeByID(escrowID)
		return nil, enclave.ErrEscrowExpired
	}
	return copyBytes(entry.credential), nil
}

// Revoke removes an escrow entry and zeros the credential bytes. The grantID
// must match the original grant that created the escrow.
func (s *escrowStore) Revoke(escrowID, grantID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[escrowID]
	if !ok {
		return enclave.ErrEscrowNotFound
	}
	if entry.grantID != grantID {
		return enclave.ErrEscrowNotFound
	}
	zeroBytes(entry.credential)
	delete(s.entries, escrowID)
	return nil
}

// EvictExpired removes and zeros all expired entries.
func (s *escrowStore) EvictExpired() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, entry := range s.entries {
		if now.After(entry.expiresAt) {
			zeroBytes(entry.credential)
			delete(s.entries, id)
		}
	}
}

// Clear removes and zeros all entries.
func (s *escrowStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, entry := range s.entries {
		zeroBytes(entry.credential)
		delete(s.entries, id)
	}
}

func (s *escrowStore) revokeByID(escrowID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.entries[escrowID]; ok {
		zeroBytes(entry.credential)
		delete(s.entries, escrowID)
	}
}
