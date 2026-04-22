package enclave

import "context"

// Client is the SPI for communicating with a TEE enclave. The host control
// plane uses this interface to delegate execution, perform attestation,
// transmit user KEKs, exchange OAuth tokens, and manage credential escrow.
// Implementations exist for each supported TEE provider (e.g. local dev,
// Google Confidential Space).
type Client interface {
	// Attest initiates remote attestation and returns the enclave's evidence
	// along with its ephemeral ECDH public key.
	Attest(ctx context.Context, req AttestationRequest) (AttestationResponse, error)

	// EstablishSession completes the ECDH key exchange after the host has
	// verified the attestation evidence, creating an encrypted channel for
	// credential transmission.
	EstablishSession(ctx context.Context, req SessionRequest) (SessionResponse, error)

	// TransmitKEK sends a user's Key Encryption Key to the enclave, encrypted
	// with the session key. The enclave stores the KEK in hardware-isolated
	// memory and uses it to decrypt vault credentials for that user. The host
	// must zero the KEK from its own memory immediately after transmission.
	TransmitKEK(ctx context.Context, req TransmitKEKRequest) (TransmitKEKResponse, error)

	// OAuthExchange asks the enclave to exchange an OAuth authorization code
	// for tokens. The enclave calls the provider's token endpoint, encrypts
	// the resulting tokens with the user's stored KEK, and returns only the
	// ciphertext. The host never sees the plaintext tokens.
	OAuthExchange(ctx context.Context, req OAuthExchangeRequest) (OAuthExchangeResponse, error)

	// Execute sends an execution request to the enclave. The credential is
	// the raw vault ciphertext (KEK-encrypted). The enclave decrypts it using
	// the user's stored KEK, executes the connector, and returns the result.
	Execute(ctx context.Context, req ExecuteRequest) (ExecuteResponse, error)

	// EscrowStore places a credential inside the enclave for asynchronous or
	// scheduled use. The credential is decrypted on receipt and held only in
	// enclave memory.
	EscrowStore(ctx context.Context, req EscrowStoreRequest) (EscrowStoreResponse, error)

	// EscrowRetrieve returns the plaintext credential for a given escrow ID.
	// Used by source (read-only) connectors that run on the host. Write
	// actions should use Execute with EscrowID instead.
	EscrowRetrieve(ctx context.Context, req EscrowRetrieveRequest) (EscrowRetrieveResponse, error)

	// EscrowRevoke removes a previously escrowed credential and zeros the
	// plaintext from enclave memory.
	EscrowRevoke(ctx context.Context, req EscrowRevokeRequest) error

	// Close releases any resources held by this client.
	Close() error
}
