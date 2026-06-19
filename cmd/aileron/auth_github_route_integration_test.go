//go:build integration

package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/app"
	"github.com/ALRubinger/aileron/internal/vault"
)

// TestRunAuthGitHub_PUTReachesRealDaemonRoute is the path-drift guard the
// /cw-sweep decision (2026-06-18, issue #1157) called for. The CLI's
// production store closure (userCredentialStore) PUTs the captured token to
// /vault/<storeAt>/credentials, deriving the path from the descriptor's
// store_at ("user/github") plus the "/v1" base-URL prefix, while the daemon
// registers the route INDEPENDENTLY from the OpenAPI spec via
// api.HandlerFromMux ("/v1/vault/user/{service}/credentials", backed by
// userCredentialVaultPath in internal/app). The existing unit tests
// (auth_github_test.go) re-encode the same path into their fake server, so
// they cannot catch the two drifting out of lockstep.
//
// This test closes that blind spot: it stands up the REAL daemon handler
// (app.NewHandlerWithConfig, which registers the user-credentials route
// through the generated mux derived from the spec) over a MemVault, points
// the CLI at it, stubs the capture so no container/GitHub is touched, and
// asserts the descriptor-routed entrypoint returns exit 0 with the success
// message — i.e. the PUT resolved to the real PutUserCredentials handler
// and stored the credential, NOT a 404 from a route mismatch.
//
// Because the request reaches the handler through the spec-derived mux,
// changing userCredentialVaultPath or the spec's route template without
// updating the CLI's hardcoded path reddens this test. It is build-tagged
// (`integration`) so it stays out of the normal unit suite and runs in the
// dedicated integration step instead; it has no external prerequisites
// (pure in-process httptest), so it never self-skips.
func TestRunAuthGitHub_PUTReachesRealDaemonRoute(t *testing.T) {
	// The real daemon handler, with the user-credentials route registered
	// from the OpenAPI spec and an unlocked in-memory vault behind it.
	handler, err := app.NewHandlerWithConfig(
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		app.Config{Vault: vault.NewMemVault()},
	)
	if err != nil {
		t.Fatalf("NewHandlerWithConfig: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Point the CLI's daemon client at the real handler. The "/v1" prefix
	// lives in the base URL, matching production (bindingAPIBaseURL).
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")
	t.Setenv("AILERON_TOKEN", "test-token")

	// Stub the capture so the test drives only the store path: no
	// container, no GitHub. A real (non-empty) token so the empty-token
	// guard does not short-circuit before the PUT.
	withFakeCaptureDriver(t, &fakeCaptureRunner{token: "gho_routedrifttoken1234567890"}, nil)

	var stdout, stderr bytes.Buffer
	code := runAuthCapture("gh", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (the PUT must resolve to the real daemon route, not 404); stderr=%s",
			code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Stored user/github") {
		t.Errorf("stdout = %q, want the success message confirming the credential was stored", stdout.String())
	}

	// Independently confirm the daemon actually holds the credential at the
	// user/github route the CLI targeted: a GET against the same spec route
	// must now return 200, proving the PUT landed (not silently 404'd).
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodGet, srv.URL+"/v1/vault/user/github/credentials", nil)
	if err != nil {
		t.Fatalf("build GET request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET stored credential: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET user/github after store = %d, want 200 (the PUT did not land at the daemon route)",
			resp.StatusCode)
	}
}
