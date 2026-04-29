package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAttestedEscrowKeyProviderReleasesKey(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	var gotRequest keyReleaseRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("content type = %q, want application/json", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotRequest); err != nil {
			t.Fatalf("Decode request: %v", err)
		}
		if gotRequest.AttestationToken != "token-123" {
			t.Fatalf("attestation token = %q", gotRequest.AttestationToken)
		}
		if err := json.NewEncoder(w).Encode(keyReleaseResponse{
			EscrowKey: base64.StdEncoding.EncodeToString(key),
		}); err != nil {
			t.Fatalf("Encode response: %v", err)
		}
	}))
	defer server.Close()

	provider := attestedEscrowKeyProvider{
		provider:   "confidential-space",
		releaseURL: server.URL,
		audience:   "key-release-audience",
		httpClient: server.Client(),
		tokenFetcher: func(provider, audience string, nonces []string) (string, error) {
			if provider != "confidential-space" {
				t.Fatalf("provider = %q", provider)
			}
			if audience != "key-release-audience" {
				t.Fatalf("audience = %q", audience)
			}
			if len(nonces) != 0 {
				t.Fatalf("nonces = %v, want none", nonces)
			}
			return "token-123", nil
		},
	}

	got, err := provider.Key(context.Background())
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if string(got) != string(key) {
		t.Fatalf("key = %q, want %q", got, key)
	}
}

func TestAttestedEscrowKeyProviderRequiresReleaseURL(t *testing.T) {
	provider := attestedEscrowKeyProvider{}

	if _, err := provider.Key(context.Background()); err == nil {
		t.Fatal("expected missing release URL to fail")
	}
}

func TestAttestedEscrowKeyProviderReportsTokenFetchError(t *testing.T) {
	provider := attestedEscrowKeyProvider{
		releaseURL: "https://key-release.example.com/escrow",
		tokenFetcher: func(string, string, []string) (string, error) {
			return "", errors.New("token unavailable")
		},
	}

	if _, err := provider.Key(context.Background()); err == nil {
		t.Fatal("expected token fetch failure")
	}
}

func TestAttestedEscrowKeyProviderReportsReleaseStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer server.Close()

	provider := attestedEscrowKeyProvider{
		releaseURL:   server.URL,
		httpClient:   server.Client(),
		tokenFetcher: func(string, string, []string) (string, error) { return "token-123", nil },
	}

	if _, err := provider.Key(context.Background()); err == nil {
		t.Fatal("expected non-2xx release response to fail")
	}
}

func TestAttestedEscrowKeyProviderRejectsMalformedResponse(t *testing.T) {
	tests := []struct {
		name     string
		response string
	}{
		{
			name:     "invalid json",
			response: "{",
		},
		{
			name:     "invalid base64",
			response: `{"escrow_key_b64":"not base64"}`,
		},
		{
			name:     "wrong key size",
			response: `{"escrow_key_b64":"` + base64.StdEncoding.EncodeToString([]byte("short")) + `"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.response))
			}))
			defer server.Close()

			provider := attestedEscrowKeyProvider{
				releaseURL:   server.URL,
				httpClient:   server.Client(),
				tokenFetcher: func(string, string, []string) (string, error) { return "token-123", nil },
			}
			if _, err := provider.Key(context.Background()); err == nil {
				t.Fatal("expected malformed release response to fail")
			}
		})
	}
}
