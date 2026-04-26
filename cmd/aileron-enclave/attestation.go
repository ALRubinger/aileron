package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// metadataBaseURL is the GCE metadata endpoint for fetching attestation
// tokens. It is a variable so tests can override it with a mock server.
var metadataBaseURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity"

// fetchAttestationToken retrieves an attestation token appropriate for the
// TEE provider. For Confidential Space, it fetches an OIDC token from the
// GCE metadata service. Nonces are included in the token's eat_nonce claim
// and can be used to bind the ECDH public key to the attestation evidence.
// For local dev, it returns a static dev token.
func fetchAttestationToken(provider, audience string, nonces []string) (string, error) {
	if provider == "local" {
		return "dev-ok", nil
	}

	if audience == "" {
		audience = "aileron-enclave"
	}

	params := url.Values{
		"audience": {audience},
		"format":   {"full"},
	}
	if len(nonces) > 0 {
		params.Set("nonces", strings.Join(nonces, ","))
	}

	u := metadataBaseURL + "?" + params.Encode()

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("creating metadata request: %w", err)
	}
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching attestation token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("metadata service returned %d: %s", resp.StatusCode, string(body))
	}

	token, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading attestation token: %w", err)
	}
	return string(token), nil
}
