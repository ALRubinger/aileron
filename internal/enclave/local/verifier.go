package local

import (
	"context"
	"time"

	"github.com/ALRubinger/aileron/internal/enclave"
)

// DevVerifier accepts any attestation token. It is intended only for
// development and testing where no real TEE is available.
type DevVerifier struct{}

// Verify always succeeds and returns dev claims.
func (v *DevVerifier) Verify(_ context.Context, _ string, _ []byte) (enclave.AttestationClaims, error) {
	now := time.Now()
	return enclave.AttestationClaims{
		ImageDigest: "dev",
		ProjectID:   "local",
		Issuer:      "local-dev",
		HWModel:     "none",
		IssuedAt:    now,
		ExpiresAt:   now.Add(24 * time.Hour),
	}, nil
}
