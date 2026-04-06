//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"testing"
)

func TestGetCurrentUser_AuthProviders(t *testing.T) {
	resp := authedGet(t, apiURL()+"/v1/users/me")
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	validateResponse(t, resp)

	var user map[string]any
	json.NewDecoder(resp.Body).Decode(&user)

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
	// Try to disconnect a provider that was never connected.
	// Should get 400 (last method guard) since the user has no OAuth providers
	// and only one auth method (password).
	resp := authedDelete(t, apiURL()+"/v1/users/me/auth-providers/google")
	defer resp.Body.Close()

	// With 0 OAuth providers and a password, the guard sees len(providers) <= 1
	// and password IS set, so it proceeds to delete — which returns 404 (not found).
	if resp.StatusCode != 404 {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 404, got %d: %s", resp.StatusCode, body)
	}
}
