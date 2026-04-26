package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchAttestationTokenLocal(t *testing.T) {
	token, err := fetchAttestationToken("local", "test-audience", nil)
	if err != nil {
		t.Fatalf("fetchAttestationToken: %v", err)
	}
	if token != "dev-ok" {
		t.Fatalf("expected dev-ok, got %q", token)
	}
}

func TestFetchAttestationTokenGCE(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify it's a proper metadata request.
		if r.Header.Get("Metadata-Flavor") != "Google" {
			t.Errorf("expected Metadata-Flavor: Google, got %q", r.Header.Get("Metadata-Flavor"))
		}
		if r.URL.Query().Get("audience") != "my-audience" {
			t.Errorf("expected audience=my-audience, got %q", r.URL.Query().Get("audience"))
		}
		if r.URL.Query().Get("format") != "full" {
			t.Errorf("expected format=full, got %q", r.URL.Query().Get("format"))
		}
		w.Write([]byte("mock-oidc-token-12345"))
	}))
	defer mockServer.Close()

	orig := metadataBaseURL
	metadataBaseURL = mockServer.URL
	defer func() { metadataBaseURL = orig }()

	token, err := fetchAttestationToken("confidential-space", "my-audience", nil)
	if err != nil {
		t.Fatalf("fetchAttestationToken: %v", err)
	}
	if token != "mock-oidc-token-12345" {
		t.Fatalf("expected mock-oidc-token-12345, got %q", token)
	}
}

func TestFetchAttestationTokenGCEDefaultAudience(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("audience") != "aileron-enclave" {
			t.Errorf("expected default audience aileron-enclave, got %q", r.URL.Query().Get("audience"))
		}
		w.Write([]byte("token-default"))
	}))
	defer mockServer.Close()

	orig := metadataBaseURL
	metadataBaseURL = mockServer.URL
	defer func() { metadataBaseURL = orig }()

	token, err := fetchAttestationToken("confidential-space", "", nil)
	if err != nil {
		t.Fatalf("fetchAttestationToken: %v", err)
	}
	if token != "token-default" {
		t.Fatalf("expected token-default, got %q", token)
	}
}

func TestFetchAttestationTokenGCEError(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("metadata service unavailable"))
	}))
	defer mockServer.Close()

	orig := metadataBaseURL
	metadataBaseURL = mockServer.URL
	defer func() { metadataBaseURL = orig }()

	_, err := fetchAttestationToken("confidential-space", "test", nil)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestFetchAttestationTokenWithNonces(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonces := r.URL.Query().Get("nonces")
		if nonces != "nonce1,nonce2" {
			t.Errorf("expected nonces=nonce1,nonce2, got %q", nonces)
		}
		w.Write([]byte("token-with-nonces"))
	}))
	defer mockServer.Close()

	orig := metadataBaseURL
	metadataBaseURL = mockServer.URL
	defer func() { metadataBaseURL = orig }()

	token, err := fetchAttestationToken("confidential-space", "aud", []string{"nonce1", "nonce2"})
	if err != nil {
		t.Fatalf("fetchAttestationToken: %v", err)
	}
	if token != "token-with-nonces" {
		t.Fatalf("expected token-with-nonces, got %q", token)
	}
}

func TestFetchAttestationTokenGCEUnreachable(t *testing.T) {
	orig := metadataBaseURL
	metadataBaseURL = "http://127.0.0.1:1" // unreachable
	defer func() { metadataBaseURL = orig }()

	_, err := fetchAttestationToken("confidential-space", "test", nil)
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}
