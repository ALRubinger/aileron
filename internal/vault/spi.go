// Package vault defines the SPI for credential and secret storage.
//
// The vault stores sensitive values (API keys, OAuth tokens, service account
// credentials) and injects them into connector execution requests without
// ever exposing them to agents. Agents see only a credential reference ID;
// the vault resolves the actual secret at execution time.
//
// The built-in implementation encrypts secrets at rest using AES-GCM with
// a key stored in the environment. Alternative implementations can delegate
// to HashiCorp Vault, AWS Secrets Manager, GCP Secret Manager, etc.
package vault

import (
	"context"
	"errors"
)

// ErrCredentialUnavailable indicates that a credential could not be retrieved
// from the vault — the vault session may be locked or the KEK invalid.
// Callers should prompt the user to unlock their vault.
var ErrCredentialUnavailable = errors.New("vault: credential unavailable")

// Vault stores and retrieves secrets by path.
type Vault interface {
	// Get retrieves a secret by its vault path.
	Get(ctx context.Context, path string) (Secret, error)

	// Put stores a secret at the given path, creating or overwriting it.
	Put(ctx context.Context, path string, value []byte, meta Metadata) error

	// Delete permanently removes a secret.
	Delete(ctx context.Context, path string) error

	// List returns plaintext metadata for every entry in the vault. The
	// credential bytes are NOT returned — listing must work without
	// decrypting any value, so the operation succeeds on a locked vault.
	// Per ADR-0011, this is the load-bearing property that makes
	// `aileron binding list` and the binding-resolution lookup work
	// without prompting for a passphrase. Order is unspecified.
	List(ctx context.Context) ([]Entry, error)
}

// Entry is a vault listing row: the path plus its plaintext metadata,
// without the encrypted value.
type Entry struct {
	Path     string
	Metadata Metadata
}

// Secret is a resolved secret value.
type Secret struct {
	Path     string
	Value    []byte
	Metadata Metadata
}

// Metadata carries non-secret attributes stored alongside a secret.
type Metadata struct {
	// Type classifies the credential (e.g. "api_key", "oauth_refresh_token").
	Type        string
	Environment string
	// Labels are arbitrary key-value pairs for organization.
	Labels map[string]string
}
