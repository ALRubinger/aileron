package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/ALRubinger/aileron/core/crypto"
)

// FileVault is a Vault backed by a JSON file on disk. Secrets are stored
// as raw bytes (callers should wrap with EncryptedVault for encryption).
// The file also stores an Argon2id salt for key derivation.
type FileVault struct {
	path string
	mu   sync.Mutex
	data fileData
}

type fileData struct {
	Salt    []byte                `json:"salt"`
	Secrets map[string]fileSecret `json:"secrets"`
}

type fileSecret struct {
	Value    []byte   `json:"value"`
	Metadata Metadata `json:"metadata"`
}

// NewFileVault opens or creates a vault file at path. If the file exists,
// its contents are loaded into memory. If not, an empty vault is created.
func NewFileVault(path string) (*FileVault, error) {
	fv := &FileVault{
		path: path,
		data: fileData{Secrets: make(map[string]fileSecret)},
	}

	raw, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(raw, &fv.data); err != nil {
			return nil, fmt.Errorf("vault: parsing %s: %w", path, err)
		}
		if fv.data.Secrets == nil {
			fv.data.Secrets = make(map[string]fileSecret)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("vault: reading %s: %w", path, err)
	}

	return fv, nil
}

// Salt returns the stored salt, generating one if it doesn't exist yet.
func (fv *FileVault) Salt() ([]byte, error) {
	fv.mu.Lock()
	defer fv.mu.Unlock()

	if len(fv.data.Salt) >= crypto.SaltLen {
		return fv.data.Salt, nil
	}

	salt, err := crypto.GenerateSalt()
	if err != nil {
		return nil, err
	}
	fv.data.Salt = salt
	if err := fv.flush(); err != nil {
		return nil, err
	}
	return salt, nil
}

func (fv *FileVault) Get(_ context.Context, path string) (Secret, error) {
	fv.mu.Lock()
	defer fv.mu.Unlock()

	s, ok := fv.data.Secrets[path]
	if !ok {
		return Secret{}, &errNotFound{path: path}
	}
	return Secret{Path: path, Value: s.Value, Metadata: s.Metadata}, nil
}

func (fv *FileVault) Put(_ context.Context, path string, value []byte, meta Metadata) error {
	fv.mu.Lock()
	defer fv.mu.Unlock()

	fv.data.Secrets[path] = fileSecret{Value: value, Metadata: meta}
	return fv.flush()
}

func (fv *FileVault) Delete(_ context.Context, path string) error {
	fv.mu.Lock()
	defer fv.mu.Unlock()

	delete(fv.data.Secrets, path)
	return fv.flush()
}

// Names returns the paths of all stored secrets.
func (fv *FileVault) Names() []string {
	fv.mu.Lock()
	defer fv.mu.Unlock()

	names := make([]string, 0, len(fv.data.Secrets))
	for k := range fv.data.Secrets {
		names = append(names, k)
	}
	return names
}

func (fv *FileVault) flush() error {
	raw, err := json.MarshalIndent(fv.data, "", "  ")
	if err != nil {
		return fmt.Errorf("vault: encoding: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(fv.path), 0o700); err != nil {
		return fmt.Errorf("vault: creating directory: %w", err)
	}

	return os.WriteFile(fv.path, raw, 0o600)
}
