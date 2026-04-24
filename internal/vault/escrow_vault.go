package vault

import (
	"context"
	"fmt"
	"sync"

	"github.com/ALRubinger/aileron/internal/enclave"
)

// EscrowVault is a Vault adapter that retrieves credentials from the TEE's
// escrow store when available. For paths that have been escrowed (tracked in
// the escrowIndex), it calls EscrowRetrieve to get the plaintext from the
// enclave. For other paths, it delegates to the fallback vault.
//
// This enables source (read-only) connectors to work during the escrow window
// (typically 7 days) without requiring the user's vault to be unlocked.
type EscrowVault struct {
	client      enclave.Client
	escrowIndex *sync.Map // vault path (string) → escrow ID (string)
	fallback    Vault
}

// NewEscrowVault creates a vault that checks the escrow index before falling
// back to the inner vault. The escrowIndex maps vault paths to escrow IDs.
func NewEscrowVault(client enclave.Client, escrowIndex *sync.Map, fallback Vault) *EscrowVault {
	return &EscrowVault{
		client:      client,
		escrowIndex: escrowIndex,
		fallback:    fallback,
	}
}

// Get retrieves a secret. If the path has an escrowed credential, it retrieves
// the plaintext from the enclave. Otherwise it delegates to the fallback vault.
//
// If the enclave no longer has the escrow entry (restart, expiry, etc.), Get
// removes the stale index entry so that future lookups fall through to the
// fallback vault, and returns ErrEscrowStale.
func (v *EscrowVault) Get(ctx context.Context, path string) (Secret, error) {
	escrowID, ok := v.escrowIndex.Load(path)
	if !ok {
		return v.fallback.Get(ctx, path)
	}

	resp, err := v.client.EscrowRetrieve(ctx, enclave.EscrowRetrieveRequest{
		EscrowID: escrowID.(string),
	})
	if err != nil {
		// The enclave no longer has this entry — prune the stale index so
		// subsequent requests don't repeat the failed lookup.
		v.escrowIndex.Delete(path)
		return Secret{}, fmt.Errorf("vault: escrow retrieve for %q: %w", path, ErrEscrowStale)
	}

	return Secret{
		Path:  path,
		Value: resp.Credential,
		Metadata: Metadata{
			Labels: map[string]string{EncryptedLabel: "false"},
		},
	}, nil
}

// Put delegates to the fallback vault.
func (v *EscrowVault) Put(ctx context.Context, path string, value []byte, meta Metadata) error {
	return v.fallback.Put(ctx, path, value, meta)
}

// Delete delegates to the fallback vault.
func (v *EscrowVault) Delete(ctx context.Context, path string) error {
	return v.fallback.Delete(ctx, path)
}
