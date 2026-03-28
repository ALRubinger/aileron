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

import "context"

// Vault stores and retrieves secrets by path.
type Vault interface {
	// Get retrieves a secret by its vault path.
	Get(ctx context.Context, path string) (Secret, error)

	// Put stores a secret at the given path, creating or overwriting it.
	Put(ctx context.Context, path string, value []byte, meta Metadata) error

	// Delete permanently removes a secret.
	Delete(ctx context.Context, path string) error
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
