package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type escrowKeyProvider interface {
	Key(ctx context.Context) ([]byte, error)
}

type staticEscrowKeyProvider struct {
	key []byte
}

func (p staticEscrowKeyProvider) Key(context.Context) ([]byte, error) {
	return copyBytes(p.key), nil
}

type attestedEscrowKeyProvider struct {
	provider     string
	releaseURL   string
	audience     string
	httpClient   *http.Client
	tokenFetcher func(provider, audience string, nonces []string) (string, error)
}

type keyReleaseRequest struct {
	AttestationToken string `json:"attestation_token"`
}

type keyReleaseResponse struct {
	EscrowKey string `json:"escrow_key_b64"`
}

func (p attestedEscrowKeyProvider) Key(ctx context.Context) ([]byte, error) {
	if p.releaseURL == "" {
		return nil, fmt.Errorf("escrow key release URL required")
	}
	fetch := p.tokenFetcher
	if fetch == nil {
		fetch = fetchAttestationToken
	}
	token, err := fetch(p.provider, p.audience, nil)
	if err != nil {
		return nil, fmt.Errorf("fetching key-release attestation token: %w", err)
	}

	payload, err := json.Marshal(keyReleaseRequest{AttestationToken: token})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.releaseURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := p.httpClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("requesting escrow key release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("escrow key release returned %d", resp.StatusCode)
	}

	var out keyReleaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding escrow key release response: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(out.EscrowKey)
	if err != nil {
		return nil, fmt.Errorf("decoding released escrow key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("released escrow key: expected 32 bytes, got %d", len(key))
	}
	return key, nil
}
