package local

import (
	"context"

	"github.com/ALRubinger/aileron/enclave"
)

// DevVerifier accepts any attestation token. It is intended only for
// development and testing where no real TEE is available.
type DevVerifier struct{}

// Verify always succeeds and returns dev claims.
func (v *DevVerifier) Verify(_ context.Context, _ string, _ []byte) (enclave.AttestationClaims, error) {
	return enclave.AttestationClaims{
		ImageDigest: "dev",
		ProjectID:   "local",
	}, nil
}
