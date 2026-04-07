package enclave

import "context"

// AttestationClaims are the verified claims extracted from attestation
// evidence. The exact contents depend on the TEE provider.
type AttestationClaims struct {
	// ImageDigest is the container image digest (e.g. "sha256:abc...").
	// For the local dev provider this is "dev".
	ImageDigest string
	// ProjectID is the cloud project that owns the enclave workload.
	// For the local dev provider this is "local".
	ProjectID string
}

// Verifier validates attestation evidence from an enclave. Each TEE provider
// supplies its own Verifier implementation (e.g. OIDC token verification for
// Google Confidential Space, or a pass-through for local development).
type Verifier interface {
	// Verify validates the attestation token and returns the verified claims.
	// The nonce must match the value originally sent in AttestationRequest to
	// ensure freshness. Returns an error if the token is invalid, expired, or
	// does not match the expected workload identity.
	Verify(ctx context.Context, token string, nonce []byte) (AttestationClaims, error)
}
