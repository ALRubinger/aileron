package enclave

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
)

// LoadOrCreateKey loads a 32-byte encryption key from the given path, or
// generates one and writes it to disk if the file doesn't exist. The key
// file is created with 0600 permissions.
func LoadOrCreateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != 32 {
			return nil, fmt.Errorf("escrow key at %s: expected 32 bytes, got %d", path, len(data))
		}
		return data, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading escrow key: %w", err)
	}

	// Generate a new key.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generating escrow key: %w", err)
	}

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("creating escrow key directory: %w", err)
	}

	if err := os.WriteFile(path, key, 0600); err != nil {
		return nil, fmt.Errorf("writing escrow key: %w", err)
	}

	return key, nil
}
