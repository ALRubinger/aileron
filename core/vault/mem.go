package vault

import (
	"context"
	"sync"
)

// MemVault is a thread-safe in-memory implementation of the Vault interface.
// Suitable for development and testing only.
type MemVault struct {
	mu      sync.RWMutex
	secrets map[string]Secret
}

// NewMemVault returns an empty in-memory vault.
func NewMemVault() *MemVault {
	return &MemVault{secrets: make(map[string]Secret)}
}

func (v *MemVault) Get(_ context.Context, path string) (Secret, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	s, ok := v.secrets[path]
	if !ok {
		return Secret{}, &errNotFound{path: path}
	}
	return s, nil
}

func (v *MemVault) Put(_ context.Context, path string, value []byte, meta Metadata) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.secrets[path] = Secret{Path: path, Value: value, Metadata: meta}
	return nil
}

func (v *MemVault) Delete(_ context.Context, path string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.secrets, path)
	return nil
}

type errNotFound struct {
	path string
}

func (e *errNotFound) Error() string {
	return "vault: secret not found: " + e.path
}
