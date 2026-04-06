//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestGetCurrentUser_AuthProviders(t *testing.T) {
	resp := authedGet(t, apiURL()+"/v1/users/me")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Read body so we can both validate and decode.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	// Restore body for OpenAPI validation.
	resp.Body = io.NopCloser(bytes.NewReader(body))
	validateResponse(t, resp)

	var user map[string]any
	if err := json.Unmarshal(body, &user); err != nil {
		t.Fatalf("decoding user: %v", err)
	}

	// Email/password user should have has_password=true.
	if hp, ok := user["has_password"].(bool); !ok || !hp {
		t.Errorf("has_password = %v, want true", user["has_password"])
	}

	// Email/password user should have an empty auth_providers array (no OAuth linked).
	providers, ok := user["auth_providers"].([]any)
	if !ok {
		t.Fatalf("auth_providers is not an array: %T", user["auth_providers"])
	}
	if len(providers) != 0 {
		t.Errorf("auth_providers = %d, want 0 for email-only user", len(providers))
	}
}

func TestDisconnectAuthProvider_NotConnected(t *testing.T) {
	// With 0 OAuth providers and a password set, the last-method guard passes
	// (password exists), so the delete proceeds — returning 404 (not found).
	resp := authedDelete(t, apiURL()+"/v1/users/me/auth-providers/google")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 404, got %d: %s", resp.StatusCode, body)
	}
}
