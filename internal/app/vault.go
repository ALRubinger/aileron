package app

import (
	"fmt"

	"github.com/ALRubinger/aileron/internal/crypto"
	"github.com/ALRubinger/aileron/internal/vault"
)

// newLocalEncryptedVault creates an in-memory vault wrapped with AES-256-GCM
// encryption using a randomly generated KEK. The KEK lives only in process
// memory — it is regenerated on each server start. This ensures all secrets
// are encrypted at rest even in local/dev mode.
func newLocalEncryptedVault() (vault.Vault, error) {
	kek, err := crypto.GenerateRandomKEK()
	if err != nil {
		return nil, fmt.Errorf("generating local vault KEK: %w", err)
	}
	ev, err := vault.NewEncryptedVault(vault.NewMemVault(), kek)
	if err != nil {
		return nil, fmt.Errorf("creating encrypted vault: %w", err)
	}
	return ev, nil
}
