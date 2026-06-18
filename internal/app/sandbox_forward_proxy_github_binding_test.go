package app

import (
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/audit"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/vault"
)

// End-to-end contract for the two built-in GitHub host bindings (#1195),
// driving a decrypted request through the real forward-proxy boundary:
//
//   - github.com gets Authorization: Basic base64(x-access-token:<token>)
//     injected (the git-over-HTTPS convention).
//   - api.github.com gets Authorization: Bearer <token> (the gh/REST
//     convention).
//   - a locked/absent vault fails closed: no Authorization header reaches
//     the upstream, and no secret leaks into the response.
//
// The credential at user/github is stored with metadata Type "user" so
// the daemon's VaultResolver kind check (keyed on the credential-ref's
// first segment) passes, matching what `aileron auth github` writes.

func newGitHubBindingServer(t *testing.T, v vault.Vault, captureAuth *string) *apiServer {
	t.Helper()
	ghBindings, err := gitHubHostBindings()
	if err != nil {
		t.Fatalf("gitHubHostBindings: %v", err)
	}
	auditStore := audit.NewMemStore()
	return &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-github-binding" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         v,
		hostBindings:  ghBindings,
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if captureAuth != nil {
				*captureAuth = req.Header.Get("Authorization")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		})},
	}
}

func TestGitHubBinding_DotComGetsBasicAuth(t *testing.T) {
	const token = "ghp_clone_secret"
	v := mustVaultWith(t, "user/github", "user", []byte(token))
	var upstreamAuth string
	srv := newGitHubBindingServer(t, v, &upstreamAuth)
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://github.com/octocat/repo.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	if upstreamAuth != wantAuth {
		t.Fatalf("upstream Authorization = %q, want %q", upstreamAuth, wantAuth)
	}
	// The in-container client never sees the raw token.
	if strings.Contains(string(body), token) {
		t.Error("response body leaked the token")
	}
	for k, vs := range resp.Header {
		for _, val := range vs {
			if strings.Contains(val, token) {
				t.Errorf("response header %s leaked the token: %q", k, val)
			}
		}
	}
}

func TestGitHubBinding_APIGetsBearerAuth(t *testing.T) {
	const token = "ghp_rest_secret"
	v := mustVaultWith(t, "user/github", "user", []byte(token))
	var upstreamAuth string
	srv := newGitHubBindingServer(t, v, &upstreamAuth)
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://api.github.com/user")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if upstreamAuth != "Bearer "+token {
		t.Fatalf("upstream Authorization = %q, want Bearer %s", upstreamAuth, token)
	}
	if strings.Contains(string(body), token) {
		t.Error("response body leaked the token")
	}
}

func TestGitHubBinding_LockedVaultFailsClosedNoSecret(t *testing.T) {
	// An empty LockableVault is locked: Get returns
	// vault.ErrCredentialUnavailable, so the binding resolves to nothing.
	const token = "ghp_locked_secret"
	v := vault.NewLockableVault()

	var upstreamAuth string
	upstreamCalled := false
	srv := newGitHubBindingServer(t, v, &upstreamAuth)
	// Replace the transport to record whether the upstream was dialed.
	srv.sandboxProxyClient = &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		upstreamCalled = true
		upstreamAuth = req.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		}, nil
	})}
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://api.github.com/user")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Fail-closed: a bound host whose credential is unavailable is NOT
	// passed through; it gets an error status with no secret.
	if resp.StatusCode == http.StatusOK {
		t.Errorf("status = 200 on locked vault; want fail-closed error")
	}
	if upstreamCalled {
		t.Errorf("upstream was dialed on locked vault; want fail-closed before dial")
	}
	if strings.Contains(string(body), token) || strings.Contains(upstreamAuth, token) {
		t.Error("token leaked on locked-vault path")
	}
}
