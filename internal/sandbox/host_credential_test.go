package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
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
	// reference, but no binding has been created for this connector —
	// binding_required. The message must name the connector FQN, the
	// credential kind, and point operators at `aileron binding setup`
	// (regression test for #414: the old wording falsely blamed a
	// "[[bindings]]" block in the action manifest, sending operators
	// chasing a non-existent file edit).
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
	if strings.Contains(err.Message, "[[bindings]]") {
		t.Errorf("message still references [[bindings]] (the misleading wording from #414): %q", err.Message)
	}
	for _, want := range []string{"github://x/y", "oauth2", "aileron binding setup"} {
		if !strings.Contains(err.Message, want) {
			t.Errorf("message = %q, want substring %q", err.Message, want)
		}
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

func TestInjectCredential_APIKeyDefaultsToBearerHeader(t *testing.T) {
	// Default api_key wire shape is `Authorization: Bearer <key>` —
	// preserves the pre-#917 behavior so existing connectors that don't
	// set the new `header` / `format` manifest fields are unchanged.
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

func TestInjectCredential_APIKeyRawFormat(t *testing.T) {
	// Linear's personal API keys go in as `Authorization: <key>` with
	// no Bearer prefix (verified empirically against api.linear.app —
	// see ALRubinger/aileron#917 for the curl evidence). The manifest
	// declares format = "{key}" and the runtime substitutes.
	st := &hostState{
		connectorFQN:           "github://ALRubinger/aileron-connector-linear",
		expectedCredentialKind: "api_key",
		credentialFormat:       "{key}",
		credentialResolver: &stubResolver{
			cred: credential.Credential{Kind: "api_key", Value: []byte("lin_api_xxxxxxxx")},
		},
	}
	req, _ := http.NewRequest("GET", "https://api.linear.app/graphql", nil)
	if err := injectCredential(context.Background(), st, req, "api_key"); err != nil {
		t.Fatalf("expected success; got %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "lin_api_xxxxxxxx" {
		t.Errorf("Authorization = %q, want raw key without prefix", got)
	}
}

func TestInjectCredential_APIKeyTokenFormat(t *testing.T) {
	// GitHub personal access tokens go in as `Authorization: token <key>`
	// (lower-case `token`, no Bearer). A representative non-Bearer
	// prefix — exercises the `format` template's prefix support.
	st := &hostState{
		connectorFQN:           "github://x/y",
		expectedCredentialKind: "api_key",
		credentialFormat:       "token {key}",
		credentialResolver: &stubResolver{
			cred: credential.Credential{Kind: "api_key", Value: []byte("ghp_abcdef")},
		},
	}
	req, _ := http.NewRequest("GET", "https://api.github.com", nil)
	if err := injectCredential(context.Background(), st, req, "api_key"); err != nil {
		t.Fatalf("expected success; got %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "token ghp_abcdef" {
		t.Errorf("Authorization = %q, want token ghp_abcdef", got)
	}
}

func TestInjectCredential_APIKeyCustomHeader(t *testing.T) {
	// X-API-Key is a common alternative header name. The manifest
	// declares header = "X-API-Key" and the runtime writes there
	// instead of Authorization. format defaults to Bearer {key}; for
	// custom-header connectors that's usually not what they want, so
	// they typically set both. This test exercises the header-only
	// override; the next test exercises both at once.
	st := &hostState{
		connectorFQN:           "github://x/y",
		expectedCredentialKind: "api_key",
		credentialHeader:       "X-API-Key",
		credentialFormat:       "{key}",
		credentialResolver: &stubResolver{
			cred: credential.Credential{Kind: "api_key", Value: []byte("sk-secret")},
		},
	}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	if err := injectCredential(context.Background(), st, req, "api_key"); err != nil {
		t.Fatalf("expected success; got %v", err)
	}
	if got := req.Header.Get("X-API-Key"); got != "sk-secret" {
		t.Errorf("X-API-Key = %q, want sk-secret", got)
	}
	// Authorization header must remain untouched — otherwise upstreams
	// that read both would see a stale or empty value masquerading as
	// an auth attempt.
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want empty (custom header path)", got)
	}
}

func TestInjectCredential_OAuth2IgnoresFormatOverride(t *testing.T) {
	// OAuth2 access tokens are RFC 6750–fixed at `Bearer <token>`. Even
	// if a manifest somehow declared a header/format for oauth2 kind
	// (manifest validation catches that at install, but defense in
	// depth at injection too), the runtime ignores them and emits the
	// RFC shape.
	st := &hostState{
		connectorFQN:           "github://x/y",
		expectedCredentialKind: "oauth2",
		credentialHeader:       "X-Should-Not-Use",
		credentialFormat:       "raw {key}",
		credentialResolver: &stubResolver{
			cred: credential.Credential{Kind: "oauth2", Value: []byte("xoxb-token")},
		},
	}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	if err := injectCredential(context.Background(), st, req, "oauth2"); err != nil {
		t.Fatalf("expected success; got %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer xoxb-token" {
		t.Errorf("Authorization = %q, want Bearer xoxb-token (oauth2 ignores format)", got)
	}
	if got := req.Header.Get("X-Should-Not-Use"); got != "" {
		t.Errorf("X-Should-Not-Use = %q, want empty (oauth2 ignores header field)", got)
	}
}

func TestInjectCredential_UnsupportedKindIsDenied(t *testing.T) {
	// A vault entry whose kind isn't in the host-injection closed set
	// ("oauth2"/"api_key"/"aws_sigv4") cannot be wired into a request —
	// return capability_denied rather than silently dropping or guessing
	// the header. "basic" is a recognized post-MVP kind that the host
	// injection path does not yet implement, so it exercises the
	// default-arm denial without colliding with a supported kind.
	st := &hostState{
		connectorFQN:           "github://x/y",
		expectedCredentialKind: "basic",
		credentialResolver: &stubResolver{
			cred: credential.Credential{Kind: "basic", Value: []byte("user:pass")},
		},
	}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	err := injectCredential(context.Background(), st, req, "basic")
	if err == nil {
		t.Fatal("expected denial for unsupported kind")
	}
	if err.Class != ClassCapabilityDenied {
		t.Errorf("class = %s, want capability_denied", err.Class)
	}
}

func TestInjectCredential_AWSSigV4SignsRequest(t *testing.T) {
	// aws_sigv4 with valid params and a bound secret access key produces
	// a structurally valid SigV4 Authorization header plus the canonical
	// X-Amz-Date / X-Amz-Content-Sha256 headers. Exact-vector signing is
	// covered by the inject package's own tests; here we assert the host
	// dispatch routes through the shared signer.
	st := &hostState{
		connectorFQN:           "github://acme/s3",
		expectedCredentialKind: cstore.CredentialKindAWSSigV4,
		credentialRegion:       "us-east-1",
		credentialService:      "s3",
		credentialAccessKeyID:  "AKIAIOSFODNN7EXAMPLE",
		credentialResolver: &stubResolver{
			cred: credential.Credential{
				Kind:  cstore.CredentialKindAWSSigV4,
				Value: []byte("wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"),
			},
		},
	}
	req, _ := http.NewRequest("GET", "https://s3.amazonaws.com/bucket/key", nil)
	if err := injectCredential(context.Background(), st, req, cstore.CredentialKindAWSSigV4); err != nil {
		t.Fatalf("expected success; got %v", err)
	}
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256") {
		t.Errorf("Authorization = %q, want AWS4-HMAC-SHA256 prefix", auth)
	}
	if !strings.Contains(auth, "Credential=AKIAIOSFODNN7EXAMPLE/") {
		t.Errorf("Authorization = %q, want it to carry the access key id in Credential=", auth)
	}
	if req.Header.Get("X-Amz-Date") == "" {
		t.Error("X-Amz-Date header not set")
	}
	if req.Header.Get("X-Amz-Content-Sha256") == "" {
		t.Error("X-Amz-Content-Sha256 header not set")
	}
}

func TestInjectCredential_AWSSigV4DeniedOnMissingParams(t *testing.T) {
	// When a required signing input is empty at injection time the host
	// fails closed with capability_denied, and the secret access key must
	// never appear in the error message or details.
	const secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	cases := []struct {
		name        string
		region      string
		service     string
		accessKeyID string
	}{
		{"missing region", "", "s3", "AKIA"},
		{"missing service", "us-east-1", "", "AKIA"},
		{"missing access key id", "us-east-1", "s3", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &hostState{
				connectorFQN:           "github://acme/s3",
				expectedCredentialKind: cstore.CredentialKindAWSSigV4,
				credentialRegion:       tc.region,
				credentialService:      tc.service,
				credentialAccessKeyID:  tc.accessKeyID,
				credentialResolver: &stubResolver{
					cred: credential.Credential{
						Kind:  cstore.CredentialKindAWSSigV4,
						Value: []byte(secret),
					},
				},
			}
			req, _ := http.NewRequest("GET", "https://s3.amazonaws.com/bucket/key", nil)
			err := injectCredential(context.Background(), st, req, cstore.CredentialKindAWSSigV4)
			if err == nil {
				t.Fatalf("expected denial for %s", tc.name)
			}
			if err.Class != ClassCapabilityDenied {
				t.Errorf("class = %s, want capability_denied", err.Class)
			}
			if strings.Contains(err.Message, secret) {
				t.Errorf("error message leaked the secret access key: %q", err.Message)
			}
			for k, v := range err.Details {
				if s, ok := v.(string); ok && strings.Contains(s, secret) {
					t.Errorf("error detail %q leaked the secret access key: %q", k, s)
				}
			}
			// The request must be left unsigned on failure.
			if req.Header.Get("Authorization") != "" {
				t.Errorf("Authorization set on failed signing: %q", req.Header.Get("Authorization"))
			}
		})
	}
}

func TestInjectCredential_AWSSigV4KindMismatchDenied(t *testing.T) {
	// The manifest declares aws_sigv4 but the envelope asks for a
	// different kind — the pre-switch guard denies before any signing.
	st := &hostState{
		connectorFQN:           "github://acme/s3",
		expectedCredentialKind: cstore.CredentialKindAWSSigV4,
		credentialRegion:       "us-east-1",
		credentialService:      "s3",
		credentialAccessKeyID:  "AKIA",
	}
	req, _ := http.NewRequest("GET", "https://s3.amazonaws.com", nil)
	err := injectCredential(context.Background(), st, req, "oauth2")
	if err == nil {
		t.Fatal("expected denial on kind mismatch")
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

// TestHostHTTPRequest_AWSSigV4BodySurvivesRewind is the rewind
// regression for the WASM connector SigV4 path. The shared signer reads
// req.Body fully to compute the payload hash, then restores it; this test
// proves (a) the upstream still receives the complete body after signing
// and (b) the X-Amz-Content-Sha256 the signer set equals SHA-256 of the
// body the upstream actually read — i.e. rewind happened and the payload
// hash is over the real bytes. Mirrors how hostHTTPRequest builds the
// request: a POST with a non-empty body via a re-readable bytes.Reader.
func TestHostHTTPRequest_AWSSigV4BodySurvivesRewind(t *testing.T) {
	const body = `{"Bucket":"my-bucket","Key":"objects/report.json"}`

	var (
		gotBody       []byte
		gotContentSha string
		gotAuth       string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotContentSha = r.Header.Get("X-Amz-Content-Sha256")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	host := httpHostFromURL(srv.URL)
	manifest := &cstore.Manifest{
		Connector: cstore.ManifestConnector{Name: "github://acme/s3", Version: "1.0.0"},
		Capabilities: cstore.ManifestCapabilities{
			Network: &cstore.ManifestNetwork{Hosts: []string{host}},
			Credential: &cstore.ManifestCredential{
				Kind:        cstore.CredentialKindAWSSigV4,
				Region:      "us-east-1",
				Service:     "s3",
				AccessKeyID: "AKIAIOSFODNN7EXAMPLE",
			},
		},
	}
	st := &hostState{
		policy:                 NewHostPolicy(manifest),
		doer:                   srv.Client(),
		connectorFQN:           manifest.Connector.Name,
		expectedCredentialKind: cstore.CredentialKindAWSSigV4,
		credentialRegion:       "us-east-1",
		credentialService:      "s3",
		credentialAccessKeyID:  "AKIAIOSFODNN7EXAMPLE",
		credentialResolver: &stubResolver{
			cred: credential.Credential{
				Kind:  cstore.CredentialKindAWSSigV4,
				Value: []byte("wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"),
			},
		},
	}

	if err := callHTTPRequestBodyForTest(st, http.MethodPost, srv.URL+"/bucket", body, cstore.CredentialKindAWSSigV4); err != nil {
		t.Fatalf("http_request: %v", err)
	}

	if string(gotBody) != body {
		t.Errorf("upstream body = %q, want %q (body did not survive signing rewind)", string(gotBody), body)
	}
	wantSha := fmt.Sprintf("%x", sha256.Sum256(gotBody))
	if gotContentSha != wantSha {
		t.Errorf("X-Amz-Content-Sha256 = %q, want sha256(body) = %q", gotContentSha, wantSha)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256") {
		t.Errorf("upstream Authorization = %q, want AWS4-HMAC-SHA256 prefix", gotAuth)
	}
}

// callHTTPRequestForTest mimics what hostHTTPRequest does after
// envelope decoding, so the test can drive the credential-injection
// branch without the WASM round-trip. Centralising the dialing path
// here keeps the assertion focused on whether the header reaches the
// upstream.
func callHTTPRequestForTest(s *hostState, url, credentialKind string) *Error {
	return callHTTPRequestBodyForTest(s, http.MethodGet, url, "", credentialKind)
}

// callHTTPRequestBodyForTest is callHTTPRequestForTest with an explicit
// method and request body, built the same way hostHTTPRequest builds it
// (a re-readable bytes.Reader over the body) so the credential-injection
// branch sees a body it can buffer and restore.
func callHTTPRequestBodyForTest(s *hostState, method, url, body, credentialKind string) *Error {
	var reqBody io.Reader
	if body != "" {
		reqBody = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, url, reqBody)
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
