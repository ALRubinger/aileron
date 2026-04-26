package enclave

import "errors"

var (
	// ErrNotAttested indicates that the enclave has not completed attestation.
	ErrNotAttested = errors.New("enclave: not attested")

	// ErrSessionExpired indicates that the ECDH session has expired and a new
	// attestation + key exchange is required.
	ErrSessionExpired = errors.New("enclave: session expired")

	// ErrEscrowNotFound indicates that the requested escrow entry does not exist.
	ErrEscrowNotFound = errors.New("enclave: escrow entry not found")

	// ErrEscrowExpired indicates that the escrow entry has passed its expiry time.
	ErrEscrowExpired = errors.New("enclave: escrow entry expired")

	// ErrEscrowScopeMismatch indicates that the requested credential use does
	// not match the ownership or scope bound to the escrow entry.
	ErrEscrowScopeMismatch = errors.New("enclave: escrow scope mismatch")

	// ErrNoKEK indicates that no KEK has been transmitted for the given user.
	ErrNoKEK = errors.New("enclave: no KEK stored for user")
)
