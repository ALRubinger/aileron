// Package local provides an in-process TEE client for development and testing.
// No actual hardware isolation occurs; credential decryption and connector
// execution happen in the same process. The ECDH key exchange is still
// performed to maintain protocol parity with real TEE providers.
package local

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/internal/enclave"
	"github.com/ALRubinger/aileron/internal/source"
)

// ExecuteFn executes a connector action given the decrypted credential.
// This is injected at construction to avoid the enclave module depending
// on core/connector.
type ExecuteFn func(ctx context.Context, req enclave.ExecuteRequest, credential []byte) (enclave.ExecuteResponse, error)

// SourceExecuteFn executes a source connector tool given the decrypted
// credential. Returns the tool result as a map. Like ExecuteFn, this is
// injected to avoid coupling the enclave module to source connectors.
type SourceExecuteFn func(ctx context.Context, tool string, params map[string]any, credential []byte) (map[string]any, error)

// Client is an in-process enclave client for development and testing.
type Client struct {
	mu              sync.Mutex
	executeFn       ExecuteFn
	sourceExecuteFn SourceExecuteFn
	enclaveKey      *ecdh.PrivateKey // enclave's ephemeral key, generated on Attest
	sessionKey      []byte           // derived shared secret after EstablishSession
	sessionID       string
	expiresAt       time.Time
	sessionTTL      time.Duration
	keks            *kekStore   // per-user KEK storage
	escrow          *escrowStore
}

// Option configures a local enclave Client.
type Option func(*Client)

// WithSessionTTL sets the ECDH session lifetime. Default: 24h.
func WithSessionTTL(d time.Duration) Option {
	return func(c *Client) { c.sessionTTL = d }
}

// WithKEKTTL sets the per-user KEK storage lifetime. Default: 24h.
func WithKEKTTL(d time.Duration) Option {
	return func(c *Client) { c.keks = newKEKStore(d) }
}

// WithSourceExecuteFn sets the function used to execute source connector tools.
func WithSourceExecuteFn(fn SourceExecuteFn) Option {
	return func(c *Client) { c.sourceExecuteFn = fn }
}

// New creates a local enclave client. The executeFn is called to perform
// connector execution after credential decryption.
func New(executeFn ExecuteFn, opts ...Option) *Client {
	c := &Client{
		executeFn:  executeFn,
		sessionTTL: 24 * time.Hour,
		keks:       newKEKStore(24 * time.Hour),
		escrow:     newEscrowStore(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Attest generates an ephemeral ECDH key pair and returns a dev attestation
// token. No real attestation occurs.
func (c *Client) Attest(_ context.Context, _ enclave.AttestationRequest) (enclave.AttestationResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return enclave.AttestationResponse{}, fmt.Errorf("local: generating ECDH key: %w", err)
	}
	c.enclaveKey = priv

	return enclave.AttestationResponse{
		Token:     "dev-ok",
		PublicKey: priv.PublicKey().Bytes(),
	}, nil
}

// EstablishSession completes the ECDH key exchange and derives the session
// key used for KEK transit encryption.
func (c *Client) EstablishSession(_ context.Context, req enclave.SessionRequest) (enclave.SessionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.enclaveKey == nil {
		return enclave.SessionResponse{}, enclave.ErrNotAttested
	}

	hostPub, err := ecdh.P256().NewPublicKey(req.PublicKey)
	if err != nil {
		return enclave.SessionResponse{}, fmt.Errorf("local: parsing host public key: %w", err)
	}

	raw, err := c.enclaveKey.ECDH(hostPub)
	if err != nil {
		return enclave.SessionResponse{}, fmt.Errorf("local: ECDH exchange: %w", err)
	}
	h := sha256.Sum256(raw)

	// Zero previous session key.
	zeroBytes(c.sessionKey)

	c.sessionKey = h[:]
	c.expiresAt = time.Now().Add(c.sessionTTL)

	b := make([]byte, 16)
	rand.Read(b)
	c.sessionID = hex.EncodeToString(b)

	return enclave.SessionResponse{
		SessionID: c.sessionID,
		ExpiresAt: c.expiresAt.Format(time.RFC3339),
	}, nil
}

// TransmitKEK receives a user's KEK encrypted with the session key,
// decrypts it, and stores the plaintext KEK for that user.
func (c *Client) TransmitKEK(_ context.Context, req enclave.TransmitKEKRequest) (enclave.TransmitKEKResponse, error) {
	c.mu.Lock()
	sessionKey := copyBytes(c.sessionKey)
	c.mu.Unlock()

	if sessionKey == nil {
		return enclave.TransmitKEKResponse{}, enclave.ErrNotAttested
	}
	defer zeroBytes(sessionKey)

	kek, err := decryptAESGCM(req.EncryptedKEK, sessionKey)
	if err != nil {
		return enclave.TransmitKEKResponse{}, fmt.Errorf("local: decrypting KEK: %w", err)
	}

	c.keks.Store(req.UserID, kek)
	zeroBytes(kek)

	return enclave.TransmitKEKResponse{Stored: true}, nil
}

// OAuthExchange exchanges an OAuth authorization code for tokens, encrypts
// the token with the user's stored KEK, and returns the ciphertext.
func (c *Client) OAuthExchange(ctx context.Context, req enclave.OAuthExchangeRequest) (enclave.OAuthExchangeResponse, error) {
	kek, err := c.keks.Get(req.UserID)
	if err != nil {
		return enclave.OAuthExchangeResponse{}, err
	}
	defer zeroBytes(kek)

	// Exchange authorization code for tokens.
	tokenResp, err := exchangeOAuthCode(ctx, req)
	if err != nil {
		return enclave.OAuthExchangeResponse{}, fmt.Errorf("local: OAuth exchange: %w", err)
	}
	defer zeroBytes(tokenResp.tokenJSON)

	// Fetch user email from userinfo endpoint.
	email, err := fetchUserEmail(ctx, req.UserInfoEndpoint, tokenResp.accessToken)
	if err != nil {
		return enclave.OAuthExchangeResponse{}, fmt.Errorf("local: fetching email: %w", err)
	}

	// Encrypt token JSON with user's KEK.
	encrypted, err := encryptAESGCM(tokenResp.tokenJSON, kek)
	if err != nil {
		return enclave.OAuthExchangeResponse{}, fmt.Errorf("local: encrypting token: %w", err)
	}

	return enclave.OAuthExchangeResponse{
		EncryptedToken: encrypted,
		Email:          email,
		TokenType:      tokenResp.tokenType,
		ExternalUserID: tokenResp.externalUserID,
		ExternalTeamID: tokenResp.externalTeamID,
	}, nil
}

// Execute decrypts the credential using the user's stored KEK and calls
// the injected ExecuteFn.
func (c *Client) Execute(ctx context.Context, req enclave.ExecuteRequest) (enclave.ExecuteResponse, error) {
	// If EscrowID is set, use the escrowed credential instead.
	if req.EscrowID != "" {
		credential, err := c.escrow.Get(req.EscrowID)
		if err != nil {
			return enclave.ExecuteResponse{}, err
		}
		cred := copyBytes(credential)
		defer zeroBytes(cred)
		return c.executeFn(ctx, req, cred)
	}

	// Decrypt credential with user's KEK.
	kek, err := c.keks.Get(req.UserID)
	if err != nil {
		return enclave.ExecuteResponse{}, err
	}
	defer zeroBytes(kek)

	plaintext, err := decryptAESGCM(req.EncryptedCredential, kek)
	if err != nil {
		return enclave.ExecuteResponse{}, fmt.Errorf("local: decrypting credential: %w", err)
	}
	defer zeroBytes(plaintext)

	return c.executeFn(ctx, req, plaintext)
}

// EscrowStore decrypts the credential using the user's KEK and stores it
// in the in-memory escrow.
func (c *Client) EscrowStore(_ context.Context, req enclave.EscrowStoreRequest) (enclave.EscrowStoreResponse, error) {
	kek, err := c.keks.Get(req.UserID)
	if err != nil {
		return enclave.EscrowStoreResponse{}, err
	}
	defer zeroBytes(kek)

	plaintext, err := decryptAESGCM(req.EncryptedCredential, kek)
	if err != nil {
		return enclave.EscrowStoreResponse{}, fmt.Errorf("local: decrypting credential for escrow: %w", err)
	}

	expiresAt, err := time.Parse(time.RFC3339, req.ExpiresAt)
	if err != nil {
		zeroBytes(plaintext)
		return enclave.EscrowStoreResponse{}, fmt.Errorf("local: parsing escrow expiry: %w", err)
	}

	id := c.escrow.Store(req.GrantID, plaintext, req.CredentialType, req.ActionTypes, expiresAt)
	return enclave.EscrowStoreResponse{EscrowID: id}, nil
}

// EscrowRetrieve returns the plaintext credential for a given escrow ID.
func (c *Client) EscrowRetrieve(_ context.Context, req enclave.EscrowRetrieveRequest) (enclave.EscrowRetrieveResponse, error) {
	plaintext, err := c.escrow.Get(req.EscrowID)
	if err != nil {
		return enclave.EscrowRetrieveResponse{}, err
	}
	return enclave.EscrowRetrieveResponse{Credential: plaintext}, nil
}

// EscrowList returns metadata for all non-expired escrow entries.
func (c *Client) EscrowList(_ context.Context) (enclave.EscrowListResponse, error) {
	return enclave.EscrowListResponse{Entries: c.escrow.List()}, nil
}

// EscrowRevoke removes an escrowed credential and zeros its plaintext.
func (c *Client) EscrowRevoke(_ context.Context, req enclave.EscrowRevokeRequest) error {
	return c.escrow.Revoke(req.EscrowID, req.GrantID)
}

// SourceExecute runs a source connector tool using an escrowed credential.
// The credential stays in-process (never returned to the caller). If the
// token is refreshed during execution, the escrowed copy is updated.
func (c *Client) SourceExecute(ctx context.Context, req enclave.SourceExecuteRequest) (enclave.SourceExecuteResponse, error) {
	if c.sourceExecuteFn == nil {
		return enclave.SourceExecuteResponse{}, fmt.Errorf("local: no source execute function configured")
	}

	credential, err := c.escrow.Get(req.EscrowID)
	if err != nil {
		return enclave.SourceExecuteResponse{}, err
	}

	// Inject a TokenSaver that updates the escrowed credential on refresh.
	escrowID := req.EscrowID
	ctx = source.WithTokenSaver(ctx, func(_ context.Context, newToken []byte) error {
		return c.escrow.Update(escrowID, newToken)
	})

	result, execErr := c.sourceExecuteFn(ctx, req.Tool, req.Params, credential)

	resp := enclave.SourceExecuteResponse{}
	if execErr != nil {
		resp.Error = execErr.Error()
	} else if result != nil {
		resultJSON, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return enclave.SourceExecuteResponse{}, fmt.Errorf("local: marshaling result: %w", marshalErr)
		}
		resp.Result = resultJSON
	}
	return resp, nil
}

// Ready always returns nil for the local client since it runs in-process.
func (c *Client) Ready(_ context.Context) error {
	return nil
}

// Close zeros the session key, KEKs, and clears the escrow store.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	zeroBytes(c.sessionKey)
	c.sessionKey = nil
	c.enclaveKey = nil
	c.keks.Clear()
	c.escrow.Clear()
	return nil
}

// --- OAuth helpers ---

type oauthTokenResponse struct {
	accessToken    string
	tokenType      string
	tokenJSON      []byte // full JSON for vault storage
	externalUserID string // provider-specific user ID (Slack: authed_user.id)
	externalTeamID string // provider-specific team ID (Slack: team.id)
}

func exchangeOAuthCode(ctx context.Context, req enclave.OAuthExchangeRequest) (*oauthTokenResponse, error) {
	form := "grant_type=authorization_code" +
		"&code=" + req.Code +
		"&redirect_uri=" + req.RedirectURI +
		"&client_id=" + req.ClientID +
		"&client_secret=" + req.ClientSecret

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.TokenEndpoint, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Body = io.NopCloser(stringReader(form))

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	// Parse the token response. Slack uses a non-standard format with
	// nested authed_user and team fields; standard OAuth uses top-level fields.
	var tokenData struct {
		OK           bool   `json:"ok"`                      // Slack-specific
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
		AuthedUser   struct {
			ID          string `json:"id"`
			AccessToken string `json:"access_token"`
			TokenType   string `json:"token_type"`
		} `json:"authed_user"` // Slack-specific
		Team struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"team"` // Slack-specific
	}
	if err := json.Unmarshal(body, &tokenData); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}

	result := &oauthTokenResponse{
		tokenJSON: body,
	}

	// Slack OAuth v2: user token is under authed_user, no refresh_token.
	if tokenData.AuthedUser.AccessToken != "" {
		result.accessToken = tokenData.AuthedUser.AccessToken
		result.tokenType = tokenData.AuthedUser.TokenType
		result.externalUserID = tokenData.AuthedUser.ID
		result.externalTeamID = tokenData.Team.ID
		return result, nil
	}

	// Standard OAuth: top-level access_token + refresh_token.
	if tokenData.RefreshToken == "" {
		return nil, fmt.Errorf("no refresh token returned; user may need to re-consent")
	}
	result.accessToken = tokenData.AccessToken
	result.tokenType = tokenData.TokenType
	return result, nil
}

func fetchUserEmail(ctx context.Context, userinfoEndpoint, accessToken string) (string, error) {
	if userinfoEndpoint == "" {
		return "", nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userinfoEndpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("userinfo returned %d: %s", resp.StatusCode, body)
	}

	var claims struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&claims); err != nil {
		return "", fmt.Errorf("decoding userinfo: %w", err)
	}
	return claims.Email, nil
}

// --- Crypto helpers ---

func decryptAESGCM(ciphertext, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("local: invalid key length %d, want 32", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("local: ciphertext too short")
	}
	nonce, ct := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return gcm.Open(nil, nonce, ct, nil)
}

func encryptAESGCM(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func copyBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

type stringReaderType struct{ s string; i int }

func (r *stringReaderType) Read(b []byte) (int, error) {
	if r.i >= len(r.s) { return 0, io.EOF }
	n := copy(b, r.s[r.i:]); r.i += n; return n, nil
}

func stringReader(s string) io.Reader { return &stringReaderType{s: s} }
