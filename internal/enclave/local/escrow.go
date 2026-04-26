package local

import (
	"crypto/rand"
	"encoding/hex"
	"reflect"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/internal/enclave"
)

type escrowEntry struct {
	userID            string
	grantID           string
	enforceGrantID    bool
	vaultPath         string
	provider          string
	credential        []byte // plaintext, held in memory
	credType          string
	actionTypes       []string
	sourceTools       []string
	allowedParameters map[string]any
	expiresAt         time.Time
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
func (s *escrowStore) Store(req enclave.EscrowStoreRequest, credential []byte, expiresAt time.Time) string {
	b := make([]byte, 16)
	rand.Read(b)
	id := "esc_" + hex.EncodeToString(b)

	s.mu.Lock()
	s.entries[id] = &escrowEntry{
		userID:            req.UserID,
		grantID:           req.GrantID,
		enforceGrantID:    req.EnforceGrantID,
		vaultPath:         req.VaultPath,
		provider:          req.Provider,
		credential:        credential,
		credType:          req.CredentialType,
		actionTypes:       append([]string(nil), req.ActionTypes...),
		sourceTools:       append([]string(nil), req.SourceTools...),
		allowedParameters: cloneMap(req.AllowedParameters),
		expiresAt:         expiresAt,
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

func (s *escrowStore) GetForExecute(req enclave.ExecuteRequest) ([]byte, error) {
	entry, err := s.entry(req.EscrowID)
	if err != nil {
		return nil, err
	}
	if entry.userID != "" && entry.userID != req.UserID {
		return nil, enclave.ErrEscrowScopeMismatch
	}
	if entry.enforceGrantID && entry.grantID != req.GrantID {
		return nil, enclave.ErrEscrowScopeMismatch
	}
	if entry.vaultPath != "" && entry.vaultPath != req.VaultPath {
		return nil, enclave.ErrEscrowScopeMismatch
	}
	if entry.provider != "" && entry.provider != req.Provider {
		return nil, enclave.ErrEscrowScopeMismatch
	}
	if entry.credType != "" && entry.credType != req.CredentialType {
		return nil, enclave.ErrEscrowScopeMismatch
	}
	if len(entry.actionTypes) > 0 && !containsString(entry.actionTypes, req.ActionType) {
		return nil, enclave.ErrEscrowScopeMismatch
	}
	if len(entry.allowedParameters) > 0 && !reflect.DeepEqual(entry.allowedParameters, req.Parameters) {
		return nil, enclave.ErrEscrowScopeMismatch
	}
	return copyBytes(entry.credential), nil
}

func (s *escrowStore) GetForSource(req enclave.SourceExecuteRequest) ([]byte, error) {
	entry, err := s.entry(req.EscrowID)
	if err != nil {
		return nil, err
	}
	if entry.userID != "" && entry.userID != req.UserID {
		return nil, enclave.ErrEscrowScopeMismatch
	}
	if entry.vaultPath != "" && entry.vaultPath != req.VaultPath {
		return nil, enclave.ErrEscrowScopeMismatch
	}
	if entry.provider != "" && entry.provider != req.Provider {
		return nil, enclave.ErrEscrowScopeMismatch
	}
	if len(entry.sourceTools) > 0 && !containsString(entry.sourceTools, req.Tool) {
		return nil, enclave.ErrEscrowScopeMismatch
	}
	if len(entry.allowedParameters) > 0 && !reflect.DeepEqual(entry.allowedParameters, req.Params) {
		return nil, enclave.ErrEscrowScopeMismatch
	}
	return copyBytes(entry.credential), nil
}

func (s *escrowStore) entry(escrowID string) (*escrowEntry, error) {
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
	return entry, nil
}

// List returns metadata for all non-expired escrow entries.
func (s *escrowStore) List() []enclave.EscrowListEntry {
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()

	var entries []enclave.EscrowListEntry
	for id, entry := range s.entries {
		if now.After(entry.expiresAt) {
			continue
		}
		entries = append(entries, enclave.EscrowListEntry{
			EscrowID:  id,
			GrantID:   entry.grantID,
			UserID:    entry.userID,
			VaultPath: entry.vaultPath,
			Provider:  entry.provider,
			ExpiresAt: entry.expiresAt.Format(time.RFC3339),
		})
	}
	return entries
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

// Update replaces the credential for an existing escrow entry. The old
// credential bytes are zeroed before replacement.
func (s *escrowStore) Update(escrowID string, credential []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[escrowID]
	if !ok {
		return enclave.ErrEscrowNotFound
	}
	if time.Now().After(entry.expiresAt) {
		return enclave.ErrEscrowExpired
	}
	zeroBytes(entry.credential)
	entry.credential = credential
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

func cloneMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
