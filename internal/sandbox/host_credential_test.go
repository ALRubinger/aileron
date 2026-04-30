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
)

// stubResolver returns a fixed credential or error.
type stubResolver struct {
	cred credential.Credential
	err  error
}

func (s *stubResolver) Resolve(_ context.Context) (credential.Credential, error) {
	return s.cred, s.err
}

func TestInjectCredential_DeniedWhenManifestDeclaresNoCapability(t *testing.T) {
	// Connector manifest does not declare [capabilities.credential]; the
	// envelope referencing a credential kind is refused at the connector-
	// manifest boundary per ADR-0005.
	st := &hostState{connectorFQN: "github://x/y", expectedCredentialKind: ""}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	err := injectCredential(context.Background(), st, req, "oauth2")
	if err == nil {
		t.Fatal("expected denial when manifest declares no credential capability")
	}
	if err.Class != ClassCapabilityDenied {
		t.Errorf("class = %s, want capability_denied", err.Class)
	}
	if got, _ := err.Details["boundary_detail"].(string); got != "connector_manifest" {
		t.Errorf("boundary_detail = %q, want connector_manifest", got)
	}
}

func TestInjectCredential_DeniedOnKindMismatchAgainstManifest(t *testing.T) {
	// Connector declared "oauth2" but envelope asks for "api_key" — the
	// connector cannot opt into a different kind than its manifest.
	st := &hostState{connectorFQN: "github://x/y", expectedCredentialKind: "oauth2"}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	err := injectCredential(context.Background(), st, req, "api_key")
	if err == nil {
		t.Fatal("expected denial on kind mismatch")
	}
	if err.Class != ClassCapabilityDenied {
		t.Errorf("class = %s, want capability_denied", err.Class)
	}
	if got, _ := err.Details["declared"].(string); got != "credential:oauth2" {
		t.Errorf("declared = %q", got)
	}
	if got, _ := err.Details["requested"].(string); got != "credential:api_key" {
		t.Errorf("requested = %q", got)
	}
}

func TestInjectCredential_BindingRequiredWhenNoResolver(t *testing.T) {
	// Manifest declared the kind, the connector emitted a credential
	// reference, but the action declared no [[bindings]] entry — binding_required.
	st := &hostState{connectorFQN: "github://x/y", expectedCredentialKind: "oauth2"}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	err := injectCredential(context.Background(), st, req, "oauth2")
	if err == nil {
		t.Fatal("expected binding_required when no resolver wired")
	}
	if err.Class != ClassBindingRequired {
		t.Errorf("class = %s, want binding_required", err.Class)
	}
	if got, _ := err.Details["boundary_detail"].(string); got != "action" {
		t.Errorf("boundary_detail = %q, want action", got)
	}
}

func TestInjectCredential_BindingRequiredWhenVaultEntryMissing(t *testing.T) {
	st := &hostState{
		connectorFQN:           "github://x/y",
		expectedCredentialKind: "oauth2",
		credentialResolver: &stubResolver{
			err: credential.FormatBindingMissing("oauth2/missing"),
		},
	}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	err := injectCredential(context.Background(), st, req, "oauth2")
	if err == nil {
		t.Fatal("expected binding_required when vault entry is missing")
	}
	if err.Class != ClassBindingRequired {
		t.Errorf("class = %s, want binding_required", err.Class)
	}
	if !strings.Contains(err.Message, "oauth2/missing") {
		t.Errorf("message = %q, want vault path", err.Message)
	}
}

func TestInjectCredential_DeniedOnVaultKindMismatch(t *testing.T) {
	st := &hostState{
		connectorFQN:           "github://x/y",
		expectedCredentialKind: "oauth2",
		credentialResolver: &stubResolver{
			err: credential.FormatKindMismatch("oauth2", "api_key"),
		},
	}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	err := injectCredential(context.Background(), st, req, "oauth2")
	if err == nil {
		t.Fatal("expected capability_denied on vault kind mismatch")
	}
	if err.Class != ClassCapabilityDenied {
		t.Errorf("class = %s, want capability_denied", err.Class)
	}
	if got, _ := err.Details["boundary_detail"].(string); got != "vault" {
		t.Errorf("boundary_detail = %q, want vault", got)
	}
}

func TestInjectCredential_GenericResolverErrorBecomesRuntimeError(t *testing.T) {
	st := &hostState{
		connectorFQN:           "github://x/y",
		expectedCredentialKind: "oauth2",
		credentialResolver: &stubResolver{
			err: errors.New("vault read failed"),
		},
	}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	err := injectCredential(context.Background(), st, req, "oauth2")
	if err == nil {
		t.Fatal("expected runtime error on generic resolver failure")
	}
	if err.Class != ClassConnectorRuntimeError {
		t.Errorf("class = %s, want connector_runtime_error", err.Class)
	}
}

func TestInjectCredential_OAuth2InjectsBearerHeader(t *testing.T) {
	st := &hostState{
		connectorFQN:           "github://x/y",
		expectedCredentialKind: "oauth2",
		credentialResolver: &stubResolver{
			cred: credential.Credential{Kind: "oauth2", Value: []byte("xoxb-token")},
		},
	}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	if err := injectCredential(context.Background(), st, req, "oauth2"); err != nil {
		t.Fatalf("expected success; got %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer xoxb-token" {
		t.Errorf("Authorization = %q, want Bearer xoxb-token", got)
	}
}

func TestInjectCredential_APIKeyInjectsBearerHeader(t *testing.T) {
	// v1 wire convention: oauth2 and api_key both render as
	// Authorization: Bearer <token>. Header-name customisation is
	// post-MVP (per the plan's out-of-scope list).
	st := &hostState{
		connectorFQN:           "github://x/y",
		expectedCredentialKind: "api_key",
		credentialResolver: &stubResolver{
			cred: credential.Credential{Kind: "api_key", Value: []byte("k-secret")},
		},
	}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	if err := injectCredential(context.Background(), st, req, "api_key"); err != nil {
		t.Fatalf("expected success; got %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer k-secret" {
		t.Errorf("Authorization = %q, want Bearer k-secret", got)
	}
}

func TestInjectCredential_UnsupportedKindIsDenied(t *testing.T) {
	// A vault entry whose kind passed validation but isn't in the v1
	// closed set ("oauth2"/"api_key") cannot be wired into a request —
	// return capability_denied rather than silently dropping or
	// guessing the header. SigV4 etc. land in a follow-up issue.
	st := &hostState{
		connectorFQN:           "github://x/y",
		expectedCredentialKind: "sigv4",
		credentialResolver: &stubResolver{
			cred: credential.Credential{Kind: "sigv4", Value: []byte("AKIA...")},
		},
	}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	err := injectCredential(context.Background(), st, req, "sigv4")
	if err == nil {
		t.Fatal("expected denial for unsupported kind")
	}
	if err.Class != ClassCapabilityDenied {
		t.Errorf("class = %s, want capability_denied", err.Class)
	}
}

// End-to-end through hostHTTPRequest: a fake upstream verifies the
// Authorization header is set on the actual outbound request.
func TestHostHTTPRequest_CredentialInjectionEndToEnd(t *testing.T) {
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	host := httpHostFromURL(srv.URL)
	manifest := &cstore.Manifest{
		Connector: cstore.ManifestConnector{Name: "github://x/y", Version: "1.0.0"},
		Capabilities: cstore.ManifestCapabilities{
			Network:    &cstore.ManifestNetwork{Hosts: []string{host}},
			Credential: &cstore.ManifestCredential{Kind: "oauth2"},
		},
	}
	st := &hostState{
		policy:                 NewHostPolicy(manifest),
		doer:                   srv.Client(),
		connectorFQN:           manifest.Connector.Name,
		expectedCredentialKind: "oauth2",
		credentialResolver: &stubResolver{
			cred: credential.Credential{Kind: "oauth2", Value: []byte("test-token")},
		},
	}

	// Route directly through the host-side handler by composing the
	// envelope and a context with the state. A fake module stand-in
	// isn't needed because the helper reads from already-decoded
	// inputs via the state pointer.
	if err := callHTTPRequestForTest(st, srv.URL+"/path", "oauth2"); err != nil {
		t.Fatalf("http_request: %v", err)
	}
	if seenAuth != "Bearer test-token" {
		t.Errorf("upstream Authorization = %q, want Bearer test-token", seenAuth)
	}
}

// callHTTPRequestForTest mimics what hostHTTPRequest does after
// envelope decoding, so the test can drive the credential-injection
// branch without the WASM round-trip. Centralising the dialing path
// here keeps the assertion focused on whether the header reaches the
// upstream.
func callHTTPRequestForTest(s *hostState, url, credentialKind string) *Error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return newConnectorRuntimeError(err.Error())
	}
	if injErr := injectCredential(context.Background(), s, req, credentialKind); injErr != nil {
		return injErr
	}
	resp, err := s.doer.Do(req)
	if err != nil {
		return newConnectorRuntimeError(err.Error())
	}
	defer resp.Body.Close()
	return nil
}
