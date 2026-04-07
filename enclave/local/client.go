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
	"fmt"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/enclave"
)

const sessionTTL = 30 * time.Minute

// ExecuteFn executes a connector action given the decrypted credential.
// This is injected at construction to avoid the enclave module depending
// on core/connector.
type ExecuteFn func(ctx context.Context, req enclave.ExecuteRequest, credential []byte) (enclave.ExecuteResponse, error)

// Client is an in-process enclave client for development and testing.
type Client struct {
	mu         sync.Mutex
	executeFn  ExecuteFn
	enclaveKey *ecdh.PrivateKey // enclave's ephemeral key, generated on Attest
	sessionKey []byte           // derived shared secret after EstablishSession
	sessionID  string
	expiresAt  time.Time
	escrow     *escrowStore
}

// New creates a local enclave client. The executeFn is called to perform
// connector execution after credential decryption.
func New(executeFn ExecuteFn) *Client {
	return &Client{
		executeFn: executeFn,
		escrow:    newEscrowStore(),
	}
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
// key used to decrypt credentials sent via Execute.
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
	c.expiresAt = time.Now().Add(sessionTTL)

	b := make([]byte, 16)
	rand.Read(b)
	c.sessionID = hex.EncodeToString(b)

	return enclave.SessionResponse{
		SessionID: c.sessionID,
		ExpiresAt: c.expiresAt.Format(time.RFC3339),
	}, nil
}

// Execute decrypts the credential using the session key and calls the
// injected ExecuteFn.
func (c *Client) Execute(ctx context.Context, req enclave.ExecuteRequest) (enclave.ExecuteResponse, error) {
	c.mu.Lock()
	sessionKey := copyBytes(c.sessionKey)
	expiresAt := c.expiresAt
	c.mu.Unlock()

	if sessionKey == nil {
		return enclave.ExecuteResponse{}, enclave.ErrNotAttested
	}
	if time.Now().After(expiresAt) {
		return enclave.ExecuteResponse{}, enclave.ErrSessionExpired
	}

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

	// Decrypt credential with session key.
	plaintext, err := decryptAESGCM(req.EncryptedCredential, sessionKey)
	if err != nil {
		return enclave.ExecuteResponse{}, fmt.Errorf("local: decrypting credential: %w", err)
	}
	defer zeroBytes(plaintext)
	defer zeroBytes(sessionKey)

	return c.executeFn(ctx, req, plaintext)
}

// EscrowStore decrypts the credential and stores it in the in-memory escrow.
func (c *Client) EscrowStore(_ context.Context, req enclave.EscrowStoreRequest) (enclave.EscrowStoreResponse, error) {
	c.mu.Lock()
	sessionKey := copyBytes(c.sessionKey)
	c.mu.Unlock()

	if sessionKey == nil {
		return enclave.EscrowStoreResponse{}, enclave.ErrNotAttested
	}
	defer zeroBytes(sessionKey)

	plaintext, err := decryptAESGCM(req.EncryptedCredential, sessionKey)
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

// EscrowRevoke removes an escrowed credential and zeros its plaintext.
func (c *Client) EscrowRevoke(_ context.Context, req enclave.EscrowRevokeRequest) error {
	return c.escrow.Revoke(req.EscrowID, req.GrantID)
}

// Close zeros the session key and clears the escrow store.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	zeroBytes(c.sessionKey)
	c.sessionKey = nil
	c.enclaveKey = nil
	c.escrow.Clear()
	return nil
}

// decryptAESGCM decrypts AES-256-GCM ciphertext. The first 12 bytes are the
// nonce, followed by the ciphertext + GCM tag.
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

// encryptAESGCM encrypts plaintext with AES-256-GCM. Returns nonce || ciphertext || tag.
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
