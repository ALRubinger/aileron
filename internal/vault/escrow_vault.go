package vault

import (
	"context"
	"fmt"
)

// DenyPlaintextVault is a Vault implementation that refuses to return
// credential data. It is used as the pipeline's vault when source
// connectors execute inside the TEE enclave — any attempt to retrieve
// credentials on the host is a zero-knowledge violation and must fail
// loudly rather than silently leak plaintext.
type DenyPlaintextVault struct{}

// NewDenyPlaintextVault returns a vault that blocks all credential access.
func NewDenyPlaintextVault() *DenyPlaintextVault {
	return &DenyPlaintextVault{}
}

func (v *DenyPlaintextVault) Get(_ context.Context, path string) (Secret, error) {
	return Secret{}, fmt.Errorf("vault: credential retrieval denied for %q — source connectors must execute inside the enclave: %w", path, ErrCredentialUnavailable)
}

func (v *DenyPlaintextVault) Put(_ context.Context, path string, _ []byte, _ Metadata) error {
	return fmt.Errorf("vault: credential write denied for %q — source connectors must execute inside the enclave", path)
}

func (v *DenyPlaintextVault) Delete(_ context.Context, path string) error {
	return fmt.Errorf("vault: credential delete denied for %q — source connectors must execute inside the enclave", path)
}
