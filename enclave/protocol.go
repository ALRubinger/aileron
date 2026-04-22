// Package enclave defines the SPI for Trusted Execution Environment (TEE)
// providers and the wire protocol for host-enclave communication.
//
// The enclave module is intentionally lightweight — it contains only interface
// definitions and data types so that both the host control plane (core/) and
// the enclave binary (cmd/aileron-enclave/) can import it without pulling in
// each other's dependency trees.
package enclave

// TransmitKEKRequest sends a user's KEK to the enclave for secure storage.
// The KEK is encrypted with the ECDH session key for transit. Once received,
// the enclave holds the KEK in its hardware-isolated memory and uses it to
// decrypt vault credentials for that user.
type TransmitKEKRequest struct {
	// UserID identifies the user whose KEK is being transmitted.
	UserID string `json:"user_id"`
	// EncryptedKEK is the user's KEK, encrypted with the session key.
	EncryptedKEK []byte `json:"encrypted_kek"`
}

// TransmitKEKResponse confirms the enclave has stored the KEK.
type TransmitKEKResponse struct {
	Stored bool `json:"stored"`
}

// OAuthExchangeRequest asks the enclave to exchange an OAuth authorization
// code for tokens. The enclave calls the provider's token endpoint, encrypts
// the resulting refresh token with the user's KEK (already stored in the
// enclave), and returns only the ciphertext. The host server never sees the
// plaintext token.
type OAuthExchangeRequest struct {
	// UserID identifies the user whose KEK will encrypt the token.
	UserID string `json:"user_id"`
	// Provider is the OAuth provider, e.g. "google".
	Provider string `json:"provider"`
	// Code is the authorization code from the OAuth redirect.
	Code string `json:"code"`
	// RedirectURI is the callback URL registered with the provider.
	RedirectURI string `json:"redirect_uri"`
	// ClientID is Aileron's OAuth application client ID.
	ClientID string `json:"client_id"`
	// ClientSecret is Aileron's OAuth application client secret.
	ClientSecret string `json:"client_secret"`
	// Scopes are the OAuth scopes that were requested.
	Scopes []string `json:"scopes"`
	// TokenEndpoint is the provider's token exchange URL.
	TokenEndpoint string `json:"token_endpoint"`
	// UserInfoEndpoint is the URL to fetch the user's email after exchange.
	UserInfoEndpoint string `json:"user_info_endpoint"`
}

// OAuthExchangeResponse contains the KEK-encrypted token and non-secret
// metadata. The host stores the encrypted token in the vault without ever
// seeing the plaintext.
type OAuthExchangeResponse struct {
	// EncryptedToken is the OAuth token JSON, encrypted with the user's KEK.
	EncryptedToken []byte `json:"encrypted_token"`
	// Email is the account email fetched from the userinfo endpoint.
	Email string `json:"email"`
	// TokenType is the OAuth token type (typically "Bearer").
	TokenType string `json:"token_type"`
	// ExternalUserID is the provider-specific user ID (e.g. Slack user ID "U...").
	// Only populated for providers that return this in the token response.
	ExternalUserID string `json:"external_user_id,omitempty"`
	// ExternalTeamID is the provider-specific workspace/team ID (e.g. Slack "T...").
	// Only populated for providers that return this in the token response.
	ExternalTeamID string `json:"external_team_id,omitempty"`
}

// ExecuteRequest is sent from the host to the enclave to execute a connector
// action. The credential is the raw vault ciphertext (KEK-encrypted); the
// enclave decrypts it using the user's KEK that was previously transmitted
// via TransmitKEK.
type ExecuteRequest struct {
	// RequestID is the execution ID assigned by the host.
	RequestID string `json:"request_id"`
	// UserID identifies the user whose KEK decrypts the credential.
	UserID string `json:"user_id"`
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
	// EncryptedCredential is the KEK-encrypted ciphertext from the vault.
	// The enclave decrypts it using the user's stored KEK.
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
	UserID              string   `json:"user_id"`
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
