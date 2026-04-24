// Package gcs provides a TEE client and attestation verifier for Google
// Confidential Space. The client communicates with the enclave binary over
// HTTPS; the verifier validates Confidential Space OIDC attestation tokens.
package gcs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/ALRubinger/aileron/internal/enclave"
)

// Client communicates with the enclave binary running inside a Google
// Confidential Space VM over HTTPS.
type Client struct {
	mu         sync.Mutex
	baseURL    string
	httpClient *http.Client
	sessionID  string
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

// EscrowRetrieve retrieves a plaintext credential from escrow.
func (c *Client) EscrowRetrieve(ctx context.Context, req enclave.EscrowRetrieveRequest) (enclave.EscrowRetrieveResponse, error) {
	var resp enclave.EscrowRetrieveResponse
	if err := c.post(ctx, "/escrow/retrieve", req, &resp); err != nil {
		return enclave.EscrowRetrieveResponse{}, fmt.Errorf("gcs: escrow retrieve: %w", err)
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

// Close is a no-op for the HTTP client.
func (c *Client) Close() error {
	return nil
}

// post sends a JSON POST request to the enclave and decodes the response.
func (c *Client) post(ctx context.Context, path string, body any, result any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshalling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	if sid != "" {
		req.Header.Set("X-Session-ID", sid)
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
