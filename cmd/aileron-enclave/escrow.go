package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/internal/crypto"
	"github.com/ALRubinger/aileron/internal/enclave"
)

type escrowEntry struct {
	userID            string
	grantID           string
	enforceGrantID    bool
	vaultPath         string
	provider          string
	credential        []byte // plaintext, held in enclave memory
	credType          string
	actionTypes       []string
	sourceTools       []string
	allowedParameters map[string]any
	expiresAt         time.Time
}

type escrowStore struct {
	mu      sync.RWMutex
	entries map[string]*escrowEntry
	dataDir string // empty = in-memory only (no persistence)
	encKey  []byte // 32-byte DEK for at-rest encryption
}

// persistedEntry is the JSON-serializable form of an escrow entry.
type persistedEntry struct {
	UserID            string         `json:"user_id"`
	GrantID           string         `json:"grant_id"`
	EnforceGrantID    bool           `json:"enforce_grant_id,omitempty"`
	VaultPath         string         `json:"vault_path"`
	Provider          string         `json:"provider"`
	Credential        []byte         `json:"credential"`
	CredType          string         `json:"cred_type"`
	ActionTypes       []string       `json:"action_types"`
	SourceTools       []string       `json:"source_tools,omitempty"`
	AllowedParameters map[string]any `json:"allowed_parameters,omitempty"`
	ExpiresAt         time.Time      `json:"expires_at"`
}

func newEscrowStore(dataDir string) (*escrowStore, error) {
	s := &escrowStore{entries: make(map[string]*escrowEntry)}

	if dataDir == "" {
		return s, nil
	}

	s.dataDir = dataDir
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("creating escrow data dir: %w", err)
	}

	key, err := enclave.LoadOrCreateKey(filepath.Join(dataDir, "escrow.key"))
	if err != nil {
		return nil, err
	}
	s.encKey = key

	// Load existing entries from disk.
	if err := s.load(); err != nil {
		return nil, err
	}

	// Evict any expired entries loaded from disk.
	s.EvictExpired()

	return s, nil
}

// Store adds a plaintext credential to the escrow. Returns the escrow ID.
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
	s.persistLocked()
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

// GetForExecute retrieves a credential only when the execution request matches
// the owner and scope bound to the escrow entry.
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

// GetForSource retrieves a credential only when the source tool request matches
// the owner and scope bound to the escrow entry.
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

// Revoke removes an escrow entry and zeros the credential bytes.
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
	s.persistLocked()
	return nil
}

// Update replaces the credential for an existing escrow entry. The old
// credential bytes are zeroed before replacement. This is used when an OAuth
// token is refreshed inside the enclave — the enclave updates its own copy
// without involving the host.
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
	s.persistLocked()
	return nil
}

// EvictExpired removes and zeros all expired entries.
func (s *escrowStore) EvictExpired() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	evicted := false
	for id, entry := range s.entries {
		if now.After(entry.expiresAt) {
			zeroBytes(entry.credential)
			delete(s.entries, id)
			evicted = true
		}
	}
	if evicted {
		s.persistLocked()
	}
}

func (s *escrowStore) revokeByID(escrowID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.entries[escrowID]; ok {
		zeroBytes(entry.credential)
		delete(s.entries, escrowID)
		s.persistLocked()
	}
}

// persistLocked writes all entries to disk encrypted. Caller must hold mu.
func (s *escrowStore) persistLocked() {
	if s.dataDir == "" {
		return
	}

	persisted := make(map[string]*persistedEntry, len(s.entries))
	for id, e := range s.entries {
		persisted[id] = &persistedEntry{
			UserID:            e.userID,
			GrantID:           e.grantID,
			EnforceGrantID:    e.enforceGrantID,
			VaultPath:         e.vaultPath,
			Provider:          e.provider,
			Credential:        e.credential,
			CredType:          e.credType,
			ActionTypes:       e.actionTypes,
			SourceTools:       e.sourceTools,
			AllowedParameters: e.allowedParameters,
			ExpiresAt:         e.expiresAt,
		}
	}

	data, err := json.Marshal(persisted)
	if err != nil {
		return // best-effort; in-memory state is still correct
	}

	ciphertext, err := crypto.Encrypt(data, s.encKey)
	if err != nil {
		return
	}

	path := filepath.Join(s.dataDir, "escrow.dat")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, ciphertext, 0600); err != nil {
		return
	}
	os.Rename(tmp, path)
}

// load reads and decrypts entries from disk into the in-memory map.
func (s *escrowStore) load() error {
	path := filepath.Join(s.dataDir, "escrow.dat")
	ciphertext, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil // no data yet
	}
	if err != nil {
		return fmt.Errorf("reading escrow data: %w", err)
	}

	data, err := crypto.Decrypt(ciphertext, s.encKey)
	if err != nil {
		// Corrupt or wrong key — start fresh rather than fail.
		return nil
	}

	var persisted map[string]*persistedEntry
	if err := json.Unmarshal(data, &persisted); err != nil {
		return nil // corrupt JSON — start fresh
	}

	for id, pe := range persisted {
		s.entries[id] = &escrowEntry{
			userID:            pe.UserID,
			grantID:           pe.GrantID,
			enforceGrantID:    pe.EnforceGrantID,
			vaultPath:         pe.VaultPath,
			provider:          pe.Provider,
			credential:        pe.Credential,
			credType:          pe.CredType,
			actionTypes:       pe.ActionTypes,
			sourceTools:       pe.SourceTools,
			allowedParameters: pe.AllowedParameters,
			expiresAt:         pe.ExpiresAt,
		}
	}
	return nil
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
