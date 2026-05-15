package sandbox

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/vault"
)

// oauthEchoManifest builds a connector manifest that declares both a
// network capability (the upstream host the fixture dials) and an
// oauth2 credential capability — the shape #360 introduces.
func oauthEchoManifest(host string) *cstore.Manifest {
	return &cstore.Manifest{
		Connector: cstore.ManifestConnector{
			Name:    "github://aileron-test/oauth-echo",
			Version: "1.0.0",
		},
		Capabilities: cstore.ManifestCapabilities{
			Network:    &cstore.ManifestNetwork{Hosts: []string{host}},
			Credential: &cstore.ManifestCredential{Kind: "oauth2"},
		},
	}
}

// TestCredentialMediation_EndToEnd_InjectsBearerHeader is the
// integration test the plan calls for: a vault entry at oauth2/test
// holds "test-token"; the connector's http_request envelope carries
// `credential: "oauth2"`; the fake upstream sees `Authorization:
// Bearer test-token` on the actual outbound request. The connector
// never touches the bytes.
func TestCredentialMediation_EndToEnd_InjectsBearerHeader(t *testing.T) {
	t.Parallel()
	var seenAuth string
	var seenURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenURL = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	host := httpHostFromURL(srv.URL)
	v := vault.NewMemVault()
	if err := v.Put(context.Background(), "oauth2/test",
		[]byte("test-token"), vault.Metadata{Type: "oauth2"}); err != nil {
		t.Fatalf("vault.Put: %v", err)
	}

	rt := newTestRuntime(t, srv.Client())
	conn, err := rt.Compile(context.Background(), oauthEchoManifest(host), echoWASM)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	resolver := &credential.VaultResolver{
		Vault:        v,
		VaultPath:    "oauth2/test",
		ExpectedKind: "oauth2",
	}

	res, invErr := conn.Invoke(context.Background(), Call{
		Op: "http_cred",
		Args: map[string]any{
			"url":        srv.URL + "/api/post",
			"credential": "oauth2",
		},
		CredentialResolver: resolver,
	})
	if invErr != nil {
		t.Fatalf("Invoke: %v", invErr)
	}

	// Upstream observed the injected header.
	if seenAuth != "Bearer test-token" {
		t.Errorf("upstream Authorization = %q, want Bearer test-token", seenAuth)
	}
	if seenURL != "/api/post" {
		t.Errorf("upstream URL = %q, want /api/post", seenURL)
	}

	// Connector saw the response status (200), but the credential
	// bytes are nowhere in its output — the host injected them
	// directly into the request. The connector's `body` field
	// reflects only the upstream payload ("ok").
	if got, _ := res.Output["status"].(float64); got != 200 {
		t.Errorf("status = %v, want 200", got)
	}
	if got, _ := res.Output["body"].(string); got != "ok" {
		t.Errorf("body = %q, want ok", got)
	}
	for k, v := range res.Output {
		if s, ok := v.(string); ok && strings.Contains(s, "test-token") {
			t.Errorf("connector output contained credential bytes at %q: %q", k, s)
		}
	}
}

// TestCredentialMediation_BindingMissing_ReturnsBindingRequired
// covers the second mutation in the plan: the binding points at a
// vault path with no entry. The host fails closed at credential
// reference time; the connector receives a binding_required
// structured error.
func TestCredentialMediation_BindingMissing_ReturnsBindingRequired(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("upstream should not have been dialed")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	rt := newTestRuntime(t, srv.Client())
	conn, err := rt.Compile(context.Background(), oauthEchoManifest(httpHostFromURL(srv.URL)), echoWASM)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	// Vault is empty; resolver points at a missing path.
	resolver := &credential.VaultResolver{
		Vault:        vault.NewMemVault(),
		VaultPath:    "oauth2/missing",
		ExpectedKind: "oauth2",
	}

	_, invErr := conn.Invoke(context.Background(), Call{
		Op: "http_cred",
		Args: map[string]any{
			"url":        srv.URL + "/",
			"credential": "oauth2",
		},
		CredentialResolver: resolver,
	})
	if invErr == nil {
		t.Fatal("expected binding_required error; got nil")
	}
	var serr *Error
	if !errors.As(invErr, &serr) || serr.Class != ClassBindingRequired {
		t.Errorf("err = %v, want sandbox.ClassBindingRequired", invErr)
	}
	if !strings.Contains(serr.Message, "oauth2/missing") {
		t.Errorf("message = %q, want vault path", serr.Message)
	}
}

// TestCredentialMediation_KindMismatch_ReturnsCapabilityDenied covers
// the third mutation: the connector emits a `credential` field that
// disagrees with its own manifest's declared kind. Refused at the
// connector-manifest boundary.
func TestCredentialMediation_KindMismatch_ReturnsCapabilityDenied(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("upstream should not have been dialed")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	v := vault.NewMemVault()
	if err := v.Put(context.Background(), "oauth2/test",
		[]byte("test-token"), vault.Metadata{Type: "oauth2"}); err != nil {
		t.Fatalf("vault.Put: %v", err)
	}

	rt := newTestRuntime(t, srv.Client())
	conn, err := rt.Compile(context.Background(), oauthEchoManifest(httpHostFromURL(srv.URL)), echoWASM)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	resolver := &credential.VaultResolver{
		Vault:        v,
		VaultPath:    "oauth2/test",
		ExpectedKind: "oauth2",
	}

	// Manifest declared "oauth2"; envelope asks for "api_key" → denied
	// at the connector-manifest boundary.
	_, invErr := conn.Invoke(context.Background(), Call{
		Op: "http_cred",
		Args: map[string]any{
			"url":        srv.URL + "/",
			"credential": "api_key",
		},
		CredentialResolver: resolver,
	})
	if invErr == nil {
		t.Fatal("expected capability_denied; got nil")
	}
	var serr *Error
	if !errors.As(invErr, &serr) || serr.Class != ClassCapabilityDenied {
		t.Errorf("err = %v, want capability_denied", invErr)
	}
	if got, _ := serr.Details["boundary_detail"].(string); got != "connector_manifest" {
		t.Errorf("boundary_detail = %q, want connector_manifest", got)
	}
}
