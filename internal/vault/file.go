package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/ALRubinger/aileron/internal/crypto"
)

// FileVault is a Vault backed by a JSON file on disk. Secrets are stored
// as raw bytes (callers should wrap with EncryptedVault for encryption).
// The file also stores an Argon2id salt for key derivation.
type FileVault struct {
	path string
	mu   sync.Mutex
	data fileData
}

// fileData is the on-disk schema ratified by ADR-0011:
//
//	{
//	  "version":      1,
//	  "salt":         "<argon2id salt>",
//	  "verification": "<KEK-encrypted constant>",
//	  "secrets":      { "<path>": { value: ciphertext, metadata: ... } }
//	}
//
// `version` lets the loader gate behavior on schema evolution.
// `verification` lets a caller prove the supplied passphrase derives
// the right KEK before any secret is read — wrong passphrase or a
// tampered file fail fast with a single AEAD decryption attempt.
type fileData struct {
	Version      int                   `json:"version,omitempty"`
	Salt         []byte                `json:"salt"`
	Verification []byte                `json:"verification,omitempty"`
	Secrets      map[string]fileSecret `json:"secrets"`
}

// VerificationPlaintext is the well-known string the FileVault
// encrypts under the KEK on first creation. On unlock, decrypting
// the stored verification ciphertext must yield this exact value;
// any other result means wrong passphrase or tampered vault.
const VerificationPlaintext = "aileron-vault-v1"

// CurrentFileFormatVersion is the format version this build writes.
// Existing files without a version field are treated as v0 and
// upgraded on the next write.
const CurrentFileFormatVersion = 1

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

// List returns plaintext metadata for every entry. The encrypted
// `Value` is not returned — listing must work without decrypting any
// secret per ADR-0011.
func (fv *FileVault) List(_ context.Context) ([]Entry, error) {
	fv.mu.Lock()
	defer fv.mu.Unlock()

	entries := make([]Entry, 0, len(fv.data.Secrets))
	for path, s := range fv.data.Secrets {
		entries = append(entries, Entry{Path: path, Metadata: s.Metadata})
	}
	return entries, nil
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

// Verification returns the ciphertext stored under the `verification`
// key, or nil if the vault hasn't been initialised with one. Callers
// validate by decrypting with the candidate KEK and comparing the
// plaintext to [VerificationPlaintext].
func (fv *FileVault) Verification() []byte {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	if len(fv.data.Verification) == 0 {
		return nil
	}
	out := make([]byte, len(fv.data.Verification))
	copy(out, fv.data.Verification)
	return out
}

// SetVerification stores the KEK-encrypted verification blob and
// marks the file as the current format version. Called once on
// vault creation; subsequent writes preserve it. Returns an error
// when called on a vault that already has a verification blob (the
// caller should use Verification() to read it instead, never
// overwrite it — overwriting would silently invalidate every
// existing encrypted secret).
func (fv *FileVault) SetVerification(ciphertext []byte) error {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	if len(fv.data.Verification) > 0 {
		return fmt.Errorf("vault: verification blob is already set")
	}
	fv.data.Verification = append([]byte(nil), ciphertext...)
	fv.data.Version = CurrentFileFormatVersion
	return fv.flush()
}

// HasContents reports whether the vault file held any data when it
// was opened. False means the file did not exist (or contained an
// empty schema), so the caller is in the "first run" path and
// should populate the salt + verification blob before using it.
func (fv *FileVault) HasContents() bool {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	return len(fv.data.Salt) > 0 || len(fv.data.Verification) > 0 || len(fv.data.Secrets) > 0
}

func (fv *FileVault) flush() error {
	// Always bump to the current format on write. v0 (legacy) files
	// without a version field are upgraded transparently the first
	// time they're modified — readers that don't understand v1
	// won't be reached because v0 callers don't set a verification
	// blob, leaving the format additively backward-compatible.
	if fv.data.Version < CurrentFileFormatVersion {
		fv.data.Version = CurrentFileFormatVersion
	}
	raw, err := json.MarshalIndent(fv.data, "", "  ")
	if err != nil {
		return fmt.Errorf("vault: encoding: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(fv.path), 0o700); err != nil {
		return fmt.Errorf("vault: creating directory: %w", err)
	}

	return os.WriteFile(fv.path, raw, 0o600)
}
