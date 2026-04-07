// Package enclave defines the SPI for Trusted Execution Environment (TEE)
// providers and the wire protocol for host-enclave communication.
//
// The enclave module is intentionally lightweight — it contains only interface
// definitions and data types so that both the host control plane (core/) and
// the enclave binary (cmd/aileron-enclave/) can import it without pulling in
// each other's dependency trees.
package enclave

// ExecuteRequest is sent from the host to the enclave to execute a connector
// action. The credential is encrypted with the session key established during
// attestation; the enclave decrypts it internally.
type ExecuteRequest struct {
	// RequestID is the execution ID assigned by the host.
	RequestID string `json:"request_id"`
	// GrantID is the execution grant that authorised this action.
	GrantID string `json:"grant_id"`
	// IntentID is the intent that originated the action.
	IntentID string `json:"intent_id"`
	// ActionType is the dot-namespaced action, e.g. "payment.charge".
	ActionType string `json:"action_type"`
	// ConnectorID is "<type>/<provider>", e.g. "payments/stripe".
	ConnectorID string `json:"connector_id"`
	// Parameters are the bounded, approved parameters for execution.
	Parameters map[string]any `json:"parameters"`
	// EncryptedCredential is the AES-256-GCM ciphertext from the vault,
	// re-encrypted with the ECDH session key for transmission.
	EncryptedCredential []byte `json:"encrypted_credential"`
	// CredentialType describes the credential kind, e.g. "api_key".
	CredentialType string `json:"credential_type"`
	// EscrowID, if set, tells the enclave to use an escrowed credential
	// instead of EncryptedCredential.
	EscrowID string `json:"escrow_id,omitempty"`
}

// ExecuteResponse is returned by the enclave after connector execution.
type ExecuteResponse struct {
	RequestID  string         `json:"request_id"`
	Status     string         `json:"status"` // "succeeded" or "failed"
	Output     map[string]any `json:"output,omitempty"`
	ReceiptRef string         `json:"receipt_ref,omitempty"`
	Error      string         `json:"error,omitempty"`
}

// AttestationRequest initiates the attestation handshake. The verifier sends
// a random nonce that must be reflected in the attestation evidence.
type AttestationRequest struct {
	Nonce    []byte `json:"nonce"`
	Audience string `json:"audience"`
}

// AttestationResponse contains the enclave's attestation evidence and its
// ephemeral ECDH public key for session key derivation.
type AttestationResponse struct {
	// Token is the attestation evidence. For Google Confidential Space this
	// is an OIDC JWT; for the local dev provider it is "dev-ok".
	Token string `json:"token"`
	// PublicKey is the enclave's ephemeral P-256 ECDH public key,
	// serialised in uncompressed form.
	PublicKey []byte `json:"public_key"`
}

// SessionRequest completes the ECDH key exchange after the host has verified
// the attestation evidence.
type SessionRequest struct {
	// PublicKey is the host's ephemeral P-256 ECDH public key.
	PublicKey []byte `json:"public_key"`
}

// SessionResponse confirms that the session is established and credentials
// can be transmitted.
type SessionResponse struct {
	SessionID string `json:"session_id"`
	ExpiresAt string `json:"expires_at"` // RFC 3339
}

// EscrowStoreRequest asks the enclave to escrow a credential for
// asynchronous or scheduled execution when the user is offline.
type EscrowStoreRequest struct {
	GrantID             string   `json:"grant_id"`
	EncryptedCredential []byte   `json:"encrypted_credential"`
	CredentialType      string   `json:"credential_type"`
	ExpiresAt           string   `json:"expires_at"` // RFC 3339
	ActionTypes         []string `json:"action_types"`
}

// EscrowStoreResponse confirms that the credential has been escrowed.
type EscrowStoreResponse struct {
	EscrowID string `json:"escrow_id"`
}

// EscrowRevokeRequest removes an escrowed credential from the enclave.
type EscrowRevokeRequest struct {
	EscrowID string `json:"escrow_id"`
	GrantID  string `json:"grant_id"`
}
