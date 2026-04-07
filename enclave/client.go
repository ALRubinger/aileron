package enclave

import "context"

// Client is the SPI for communicating with a TEE enclave. The host control
// plane uses this interface to delegate execution, perform attestation, and
// manage credential escrow. Implementations exist for each supported TEE
// provider (e.g. local dev, Google Confidential Space).
type Client interface {
	// Attest initiates remote attestation and returns the enclave's evidence
	// along with its ephemeral ECDH public key.
	Attest(ctx context.Context, req AttestationRequest) (AttestationResponse, error)

	// EstablishSession completes the ECDH key exchange after the host has
	// verified the attestation evidence, creating an encrypted channel for
	// credential transmission.
	EstablishSession(ctx context.Context, req SessionRequest) (SessionResponse, error)

	// Execute sends an execution request to the enclave. The credential must
	// be encrypted with the session key before transmission (or an EscrowID
	// must be provided).
	Execute(ctx context.Context, req ExecuteRequest) (ExecuteResponse, error)

	// EscrowStore places a credential inside the enclave for asynchronous or
	// scheduled use. The credential is decrypted on receipt and held only in
	// enclave memory.
	EscrowStore(ctx context.Context, req EscrowStoreRequest) (EscrowStoreResponse, error)

	// EscrowRevoke removes a previously escrowed credential and zeros the
	// plaintext from enclave memory.
	EscrowRevoke(ctx context.Context, req EscrowRevokeRequest) error

	// Close releases any resources held by this client.
	Close() error
}
