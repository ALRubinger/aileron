package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// PKCEPair is the verifier + challenge pair produced by NewPKCE per
// RFC 7636. The verifier is the secret the client retains and sends
// at token exchange; the challenge is the base64url(sha256(verifier))
// the client sends at the authorize step. The runtime uses S256 only
// in v1; the `plain` method is rejected.
type PKCEPair struct {
	// Verifier is the URL-safe-base64-encoded random secret. 43–128
	// chars per RFC 7636 §4.1; this implementation always produces
	// 43 chars (32 random bytes → 43 base64url-no-pad chars).
	Verifier string

	// Challenge is base64url(sha256(verifier)) — 43 chars without
	// padding, suitable for inclusion in the authorize URL.
	Challenge string

	// Method is the challenge method name. Always "S256" in v1.
	Method string
}

// NewPKCE returns a fresh PKCE verifier + challenge pair using S256.
// The verifier is 32 bytes of crypto-random entropy, base64url-
// encoded without padding (43 characters); the challenge is the
// base64url-encoded SHA-256 of the verifier per RFC 7636 §4.2.
func NewPKCE() (PKCEPair, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return PKCEPair{}, fmt.Errorf("oauth: generate PKCE verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return PKCEPair{
		Verifier:  verifier,
		Challenge: challenge,
		Method:    "S256",
	}, nil
}

// NewState returns a random URL-safe state token suitable for use as
// the OAuth `state` parameter. 32 bytes of entropy, base64url-encoded
// without padding. Callers compare the value verbatim across the
// authorize → callback hop.
func NewState() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("oauth: generate state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
