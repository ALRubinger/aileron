//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALRubinger/aileron/internal/app"
	"github.com/ALRubinger/aileron/internal/vault"
)

// TestAuthGitHub_VaultPathReachesRealDaemonRoute pins the user-credential
// route contract that the `aileron auth github` CLI depends on (issue
// #1157, the residual /cw-sweep item from #1148).
//
// The CLI sends `PUT /vault/user/github/credentials` (the literal at
// cmd/aileron/auth_github.go). The genuine daemon route is
// `/v1/vault/user/{service}/credentials`, registered through the
// generated mux (api.HandlerFromMux in internal/app/app.go) and backed by
// userCredentialVaultPath in internal/app/handlers_local_vault_user.go.
//
// The existing unit tests in cmd/aileron/auth_github_test.go drive a FAKE
// vault server (fakeVaultServer) whose handler echoes whatever path the
// CLI sends, so they pass even if the CLI's literal and the real route
// diverge. The unit test in
// internal/app/handlers_local_vault_user_test.go calls PutUserCredentials
// directly, bypassing routing entirely. Neither exercises the real route.
//
// This test stands up the production handler (app.NewHandlerWithConfig
// with a real MemVault — the route comes from the generated mux, not a
// test re-encoding) and round-trips a PUT then a GET through it. It locks
// in that the `/vault/user/github/credentials` shape the CLI uses is a
// live route on the real server. The end-to-end CLI-drift guard — driving
// the literal the command itself emits — is
// TestRunAuthGitHub_DrivesRealDaemonRoute below; this test is the
// supporting route-contract round-trip.
func TestAuthGitHub_VaultPathReachesRealDaemonRoute(t *testing.T) {
	// Build the production handler with a real in-memory vault. This is
	// the same harness app_test.go uses; the user-credential route is
	// mounted through api.HandlerFromMux, so the path the request must
	// hit is the genuine generated template, not a test re-encoding.
	handler, err := app.NewHandlerWithConfig(slog.Default(), app.Config{
		Vault: vault.NewMemVault(),
	})
	if err != nil {
		t.Fatalf("NewHandlerWithConfig: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Point the CLI's bindingAPIBaseURL escape hatch at the real handler.
	// The "/v1" suffix mirrors what bindingAPIBaseURL returns in
	// production (the daemon API is mounted under /v1), and matches the
	// vault_verbs_test.go precedent. setDaemonAuthorization reads
	// AILERON_TOKEN; clear it so the local (non-cloud) handler — which
	// runs no auth middleware — is exercised without a stray header.
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")
	t.Setenv("AILERON_TOKEN", "")

	token := []byte("gho_driftguardtoken1234567890")
	body, err := json.Marshal(agentCredentialsBody{Value: token})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	// Drive the CLI's literal request path through its real client helper.
	status, respBody, err := vaultDoRequest(http.MethodPut,
		"/vault/user/github/credentials", body)
	if err != nil {
		t.Fatalf("vaultDoRequest: %v", err)
	}

	// A 404 (no route matched) or 405 (path matched a different template's
	// segment count but not the method) is the route-miss signature: the
	// `/vault/user/github/credentials` shape is no longer a live PUT route.
	// Call it out explicitly so a future failure reads as "the path shape
	// the CLI uses and the daemon route diverged", not a generic status
	// mismatch.
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		t.Fatalf("PUT /vault/user/github/credentials returned %d against the real daemon: "+
			"the user-credential route (userCredentialVaultPath / api.HandlerFromMux) no longer "+
			"matches the shape the CLI sends in auth_github.go; body=%s",
			status, respBody)
	}
	if status != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 No Content; body=%s", status, respBody)
	}

	// The write landed where the CLI's path resolves; confirm a GET on the
	// same route round-trips the bytes, proving the PUT reached the real
	// handler's store rather than a 2xx from an unrelated route.
	getStatus, getBody, err := vaultDoRequest(http.MethodGet,
		"/vault/user/github/credentials", nil)
	if err != nil {
		t.Fatalf("vaultDoRequest GET: %v", err)
	}
	if getStatus != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", getStatus, getBody)
	}
	var got agentCredentialsBody
	if err := json.Unmarshal(getBody, &got); err != nil {
		t.Fatalf("decode GET body: %v", err)
	}
	if !bytes.Equal(got.Value, token) {
		t.Fatalf("round-tripped value = %q, want %q", got.Value, token)
	}
}

// TestRunAuthGitHub_DrivesRealDaemonRoute exercises the full
// runAuthGitHub entrypoint (not just vaultDoRequest) against the real
// daemon route, with the device flow stubbed so no container or GitHub
// round-trip is needed. It proves the captured-token store path that the
// command actually runs reaches the genuine generated route end to end.
func TestRunAuthGitHub_DrivesRealDaemonRoute(t *testing.T) {
	v := vault.NewMemVault()
	handler, err := app.NewHandlerWithConfig(slog.Default(), app.Config{Vault: v})
	if err != nil {
		t.Fatalf("NewHandlerWithConfig: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	t.Setenv("AILERON_API_URL", srv.URL+"/v1")
	t.Setenv("AILERON_TOKEN", "")

	token := []byte("gho_runauthgithubdrift0987654321")
	withStubDeviceFlow(t, stubDeviceFlow{token: token}, nil)

	var stdout, stderr bytes.Buffer
	code := runAuthGitHub(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runAuthGitHub exit = %d, want 0; stderr=%s", code, stderr.String())
	}

	// The success message only prints on the 204 path, which the CLI only
	// reaches when its hardcoded path matches the registered route. Confirm
	// the bytes actually landed in the vault under the user/github key.
	secret, err := v.Get(context.Background(), "user/github")
	if err != nil {
		t.Fatalf("vault.Get(user/github): %v — the captured token did not reach the real store via the CLI path", err)
	}
	if !bytes.Equal(secret.Value, token) {
		t.Fatalf("stored value = %q, want %q", secret.Value, token)
	}
}
