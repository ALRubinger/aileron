// Package gcs provides a TEE client and attestation verifier for Google
// Confidential Space. The client communicates with the enclave binary over
// HTTPS; the verifier validates Confidential Space OIDC attestation tokens.
package gcs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/internal/enclave"
)

// Client communicates with the enclave binary running inside a Google
// Confidential Space VM over HTTPS.
type Client struct {
	mu         sync.Mutex
	baseURL    string
	httpClient *http.Client
	sessionID  string
	authKey    []byte
}

// Config holds configuration for the GCS enclave client.
type Config struct {
	// BaseURL is the enclave binary's address, e.g. "https://enclave.internal:8443".
	BaseURL string
	// HTTPClient is an optional custom HTTP client. If nil, http.DefaultClient is used.
	HTTPClient *http.Client
}

// New creates a new Google Confidential Space enclave client.
func New(cfg Config) *Client {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{
		baseURL:    cfg.BaseURL,
		httpClient: hc,
	}
}

// Attest requests attestation evidence from the enclave.
func (c *Client) Attest(ctx context.Context, req enclave.AttestationRequest) (enclave.AttestationResponse, error) {
	var resp enclave.AttestationResponse
	if err := c.post(ctx, "/attest", req, &resp); err != nil {
		return enclave.AttestationResponse{}, fmt.Errorf("gcs: attest: %w", err)
	}
	return resp, nil
}

// EstablishSession completes the ECDH key exchange with the enclave.
func (c *Client) EstablishSession(ctx context.Context, req enclave.SessionRequest) (enclave.SessionResponse, error) {
	var resp enclave.SessionResponse
	if err := c.post(ctx, "/session", req, &resp); err != nil {
		return enclave.SessionResponse{}, fmt.Errorf("gcs: establish session: %w", err)
	}
	c.mu.Lock()
	c.sessionID = resp.SessionID
	c.authKey = copyBytes(resp.RequestAuthKey)
	c.mu.Unlock()
	return resp, nil
}

// Execute sends an execution request to the enclave.
func (c *Client) Execute(ctx context.Context, req enclave.ExecuteRequest) (enclave.ExecuteResponse, error) {
	var resp enclave.ExecuteResponse
	if err := c.post(ctx, "/execute", req, &resp); err != nil {
		return enclave.ExecuteResponse{}, fmt.Errorf("gcs: execute: %w", err)
	}
	return resp, nil
}

// EscrowStore sends an escrow store request to the enclave.
func (c *Client) EscrowStore(ctx context.Context, req enclave.EscrowStoreRequest) (enclave.EscrowStoreResponse, error) {
	var resp enclave.EscrowStoreResponse
	if err := c.post(ctx, "/escrow", req, &resp); err != nil {
		return enclave.EscrowStoreResponse{}, fmt.Errorf("gcs: escrow store: %w", err)
	}
	return resp, nil
}

// TransmitKEK sends a user's KEK to the enclave.
func (c *Client) TransmitKEK(ctx context.Context, req enclave.TransmitKEKRequest) (enclave.TransmitKEKResponse, error) {
	var resp enclave.TransmitKEKResponse
	if err := c.post(ctx, "/kek", req, &resp); err != nil {
		return enclave.TransmitKEKResponse{}, fmt.Errorf("gcs: transmit KEK: %w", err)
	}
	return resp, nil
}

// OAuthExchange asks the enclave to exchange an OAuth code for tokens.
func (c *Client) OAuthExchange(ctx context.Context, req enclave.OAuthExchangeRequest) (enclave.OAuthExchangeResponse, error) {
	var resp enclave.OAuthExchangeResponse
	if err := c.post(ctx, "/oauth/exchange", req, &resp); err != nil {
		return enclave.OAuthExchangeResponse{}, fmt.Errorf("gcs: OAuth exchange: %w", err)
	}
	return resp, nil
}

// EscrowList returns metadata for all non-expired escrow entries.
func (c *Client) EscrowList(ctx context.Context) (enclave.EscrowListResponse, error) {
	var resp enclave.EscrowListResponse
	if err := c.post(ctx, "/escrow/list", struct{}{}, &resp); err != nil {
		return enclave.EscrowListResponse{}, fmt.Errorf("gcs: escrow list: %w", err)
	}
	return resp, nil
}

// EscrowRevoke sends an escrow revoke request to the enclave.
func (c *Client) EscrowRevoke(ctx context.Context, req enclave.EscrowRevokeRequest) error {
	if err := c.post(ctx, "/escrow/revoke", req, nil); err != nil {
		return fmt.Errorf("gcs: escrow revoke: %w", err)
	}
	return nil
}

// SourceExecute sends a source connector tool execution request to the enclave.
// The credential never leaves the enclave — only the tool results are returned.
func (c *Client) SourceExecute(ctx context.Context, req enclave.SourceExecuteRequest) (enclave.SourceExecuteResponse, error) {
	var resp enclave.SourceExecuteResponse
	if err := c.post(ctx, "/source/execute", req, &resp); err != nil {
		return enclave.SourceExecuteResponse{}, fmt.Errorf("gcs: source execute: %w", err)
	}
	return resp, nil
}

// Ready checks whether the enclave is reachable by calling GET /health.
func (c *Client) Ready(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("gcs: creating health request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("enclave not reachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("enclave health check failed (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

// Close clears session-bound request authentication material.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	zeroBytes(c.authKey)
	c.authKey = nil
	c.sessionID = ""
	return nil
}

// post sends a JSON POST request to the enclave and decodes the response.
func (c *Client) post(ctx context.Context, path string, body any, result any) error {
	c.mu.Lock()
	sid := c.sessionID
	authKey := copyBytes(c.authKey)
	c.mu.Unlock()
	defer zeroBytes(authKey)

	if len(authKey) > 0 {
		var err error
		body, err = attachGrantCapability(body, authKey)
		if err != nil {
			return err
		}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshalling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	if sid != "" {
		req.Header.Set(enclave.HeaderSessionID, sid)
	}
	if sid != "" && len(authKey) > 0 && requiresRequestAuth(path) {
		timestamp := time.Now().UTC().Format(time.RFC3339)
		nonce, err := randomNonce()
		if err != nil {
			return fmt.Errorf("generating request nonce: %w", err)
		}
		req.Header.Set(enclave.HeaderRequestTimestamp, timestamp)
		req.Header.Set(enclave.HeaderRequestNonce, nonce)
		req.Header.Set(enclave.HeaderRequestMAC, enclave.RequestMAC(authKey, http.MethodPost, path, payload, sid, timestamp, nonce))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("enclave returned %d: %s", resp.StatusCode, string(b))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

func attachGrantCapability(body any, authKey []byte) (any, error) {
	nonce, err := randomNonce()
	if err != nil {
		return nil, fmt.Errorf("generating grant capability nonce: %w", err)
	}
	switch req := body.(type) {
	case enclave.ExecuteRequest:
		capability, err := enclave.SignGrantCapability(authKey, enclave.ExecuteGrantCapability(req, nonce))
		if err != nil {
			return nil, fmt.Errorf("signing execute capability: %w", err)
		}
		req.Capability = capability
		return req, nil
	case enclave.EscrowStoreRequest:
		capability, err := enclave.SignGrantCapability(authKey, enclave.EscrowStoreGrantCapability(req, nonce))
		if err != nil {
			return nil, fmt.Errorf("signing escrow capability: %w", err)
		}
		req.Capability = capability
		return req, nil
	case enclave.SourceExecuteRequest:
		capability, err := enclave.SignGrantCapability(authKey, enclave.SourceExecuteGrantCapability(req, nonce))
		if err != nil {
			return nil, fmt.Errorf("signing source capability: %w", err)
		}
		req.Capability = capability
		return req, nil
	default:
		return body, nil
	}
}

func requiresRequestAuth(path string) bool {
	switch path {
	case "/kek", "/oauth/exchange", "/execute", "/escrow", "/escrow/list", "/escrow/revoke", "/source/execute":
		return true
	default:
		return false
	}
}

func randomNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func copyBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
