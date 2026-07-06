package app

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/audit"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
)

// TestSandboxForwardProxy_HostBindingReloadsOnFileEdit is the end-to-end #1887
// regression at the actual proxy boundary: a host that is unbound when the
// daemon's proxy first sees it (passthrough, no injection) becomes bound and
// injected on a later request once its descriptor is written to the user file —
// with no reconstruction of the server or the binding table ("restart"). Before
// the reload holder, the proxy read a table assembled once at boot and the
// second request would still pass through un-injected.
func TestSandboxForwardProxy_HostBindingReloadsOnFileEdit(t *testing.T) {
	descPath := filepath.Join(t.TempDir(), "binding-descriptors.yaml")

	auditStore := audit.NewMemStore()
	var upstreamAuth string
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-reload" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "api_key/reloaded/octocat", "api_key", []byte("reloaded_secret")),
		// The proxy reads through the reloader (production wiring), not the
		// static hostBindings field, so a descriptor edit is observed live.
		hostBindingsReloader: primeHostBindingsReloader(t, descPath, nil),
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			upstreamAuth = req.Header.Get("Authorization")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		})},
	}
	setup := newHostBindingProxySetup(t, srv)

	// First request: reload.example.test is unbound, so it passes through with
	// no credential injected.
	resp1, err := setup.client.Get("https://reload.example.test/v1/resource")
	if err != nil {
		t.Fatalf("first GET: %v", err)
	}
	io.Copy(io.Discard, resp1.Body)
	resp1.Body.Close()
	if upstreamAuth != "" {
		t.Fatalf("first request must pass through un-injected; upstream Authorization = %q", upstreamAuth)
	}

	// Operator (or `aileron skill bind`) writes the binding out of band.
	writeUserDescriptor(t, descPath, "reload.example.test", "api_key/reloaded/octocat")

	// Sanity: the daemon's live table observes the edit (proves the reload
	// itself, independent of proxy connection pooling).
	if _, ok := srv.currentHostBindings().Match("reload.example.test"); !ok {
		t.Fatal("live host-binding table must observe the descriptor edit")
	}

	// Force a fresh CONNECT tunnel: each intercepted tunnel serves a single
	// request, but the client may pool the tunnel, which would skip re-matching.
	setup.client.CloseIdleConnections()

	// Second request: the daemon reloads the descriptor from the file and now
	// injects the bound credential — no restart.
	resp2, err := setup.client.Get("https://reload.example.test/v1/resource")
	if err != nil {
		t.Fatalf("second GET: %v", err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if upstreamAuth != "Bearer reloaded_secret" {
		t.Fatalf("second request must inject the newly-bound credential; upstream Authorization = %q, want Bearer reloaded_secret", upstreamAuth)
	}
}
