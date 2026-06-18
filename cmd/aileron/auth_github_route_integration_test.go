//go:build integration

package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALRubinger/aileron/internal/app"
	"github.com/ALRubinger/aileron/internal/vault"
)

// TestUserGitHubCredentialsVaultPathResolves is a drift guard for the
// coupling between the `auth github` CLI verb and the daemon's
// user-credentials vault route (#1157).
//
// The `auth github` verb PUTs the captured token to a *hardcoded* path
// (userGitHubCredentialsPath) that is deliberately not shared with the
// daemon's generated route. The route itself is generated from
// internal/api/openapi.yaml's `/v1/vault/user/{service}/credentials`
// entry and registered via api.HandlerFromMux inside
// app.NewHandlerWithConfig. Because the two ends are independent, a
// silent rename of either one (the CLI literal or the OpenAPI path)
// would make the CLI's request 404 in production while every stubbed
// unit test — which re-encodes the same literal — keeps passing.
//
// This test closes that gap by booting the REAL daemon handler (not a
// stub) over an in-memory vault, then driving the CLI's actual request
// helper (vaultDoRequest) with the CLI's actual path constant. If the
// registered route and the CLI path ever diverge, the PUT no longer
// reaches PutUserCredentials and this test fails on the 404.
func TestUserGitHubCredentialsVaultPathResolves(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// Boot the real local daemon handler with an unlocked in-memory
	// vault so a successful PUT round-trips to 204 rather than 423.
	handler, err := app.NewHandlerWithConfig(log, app.Config{
		Vault: vault.NewMemVault(),
	})
	if err != nil {
		t.Fatalf("NewHandlerWithConfig: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Point the CLI's daemon-base resolver at the live test server. In
	// production `auth github` resolves its base through spawn, which
	// returns the daemon URL WITH the /v1 prefix; the CLI literal path
	// carries no /v1 (that split is exactly what this guard protects).
	// The AILERON_API_URL escape hatch is returned as-is, so it must
	// include /v1 to reproduce the real request: vaultDoRequest then
	// builds <srv.URL>/v1 + userGitHubCredentialsPath, byte-for-byte
	// what production issues.
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	body, err := json.Marshal(agentCredentialsBody{Value: []byte("gho_drift_guard_token")})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	status, respBody, err := vaultDoRequest(http.MethodPut, userGitHubCredentialsPath, body)
	if err != nil {
		t.Fatalf("vaultDoRequest PUT %s: %v", userGitHubCredentialsPath, err)
	}

	if status == http.StatusNotFound {
		t.Fatalf("CLI path %q did not resolve to the registered daemon route (404): "+
			"the `auth github` hardcoded path has drifted from "+
			"/v1/vault/user/{service}/credentials — update one to match the other. body=%s",
			userGitHubCredentialsPath, respBody)
	}
	if status != http.StatusNoContent {
		t.Fatalf("PUT %s: status = %d, want %d (No Content); body=%s",
			userGitHubCredentialsPath, status, http.StatusNoContent, respBody)
	}
}

// TestDriftedVaultPathIsRejected is the negative control for the drift
// guard above: it proves the daemon does NOT answer the success 204 for
// a path that differs from the registered route. Without it, a server
// that returned 204 for everything would make the positive assertion
// meaningless. A drifted path is rejected — surfacing as 404 (no
// pattern matched) or 405 (a sibling subtree matched the path but not
// the PUT method); either way it is not the 204 the real route returns,
// which is the invariant that makes the positive test catch drift.
func TestDriftedVaultPathIsRejected(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	handler, err := app.NewHandlerWithConfig(log, app.Config{
		Vault: vault.NewMemVault(),
	})
	if err != nil {
		t.Fatalf("NewHandlerWithConfig: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	body, err := json.Marshal(agentCredentialsBody{Value: []byte("gho_drift_guard_token")})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	// A deliberately-wrong sibling of the real path. The real route's
	// final literal segment is `credentials`; this changes it, so the
	// PUT must not reach PutUserCredentials and must not return 204.
	const drifted = "/vault/user/github/nonexistent-credentials"
	status, _, err := vaultDoRequest(http.MethodPut, drifted, body)
	if err != nil {
		t.Fatalf("vaultDoRequest PUT %s: %v", drifted, err)
	}
	if status == http.StatusNoContent {
		t.Fatalf("PUT %s returned 204 — the daemon answers success for an "+
			"unregistered path, so the positive drift guard cannot "+
			"distinguish a resolving CLI path from a drifted one", drifted)
	}
}
