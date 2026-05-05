package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/cstore"
)

// TestPreviewConnector_HappyPath: a well-formed request hits the
// preview endpoint and returns the manifest's metadata plus the
// computed hash. The store stays untouched — preview is fetch +
// verify + parse, never commit. This is the canonical path the CLI
// renders before showing the consent prompt.
func TestPreviewConnector_HappyPath(t *testing.T) {
	ref := mustRefForApp(t, "github://aileron/slack@1.2.0")
	srv, _ := installTestServer(t, ref)

	rec := postPreview(srv, `{"fqn":"`+ref.FQN.String()+`","version":"`+ref.Version+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var got api.ConnectorPreview
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Fqn != ref.FQN.String() {
		t.Errorf("Fqn = %q, want %q", got.Fqn, ref.FQN.String())
	}
	if got.Version != ref.Version {
		t.Errorf("Version = %q, want %q", got.Version, ref.Version)
	}
	if !strings.HasPrefix(got.Hash, "sha256:") {
		t.Errorf("Hash = %q, want sha256: prefix", got.Hash)
	}
	if got.SignatureStatus != api.ConnectorPreviewSignatureStatusVerified {
		t.Errorf("SignatureStatus = %q, want %q", got.SignatureStatus, api.ConnectorPreviewSignatureStatusVerified)
	}
	if got.AlreadyInstalled {
		t.Error("AlreadyInstalled = true on a fresh store, want false")
	}
	if got.Publisher == "" {
		t.Error("Publisher is empty; want either manifest publisher or FQN authority")
	}

	// Crucial: nothing in the store after preview.
	if srv.installer.Store.Count() != 0 {
		t.Errorf("Store.Count() = %d after preview; want 0 (preview must not commit)",
			srv.installer.Store.Count())
	}
}

// TestPreviewConnector_AlreadyInstalled: when the canonical hash is
// already in the store, preview short-circuits to AlreadyInstalled=
// true. The CLI consumes this to skip the consent prompt entirely
// and print "already installed" instead.
func TestPreviewConnector_AlreadyInstalled(t *testing.T) {
	ref := mustRefForApp(t, "github://aileron/slack@1.2.0")
	srv, expectedHash := installTestServer(t, ref)

	// Drive the install endpoint to populate the store first.
	if rec := postInstall(srv, `{"fqn":"`+ref.FQN.String()+`","version":"`+ref.Version+`"}`); rec.Code != http.StatusCreated {
		t.Fatalf("Install setup failed: %d %s", rec.Code, rec.Body.String())
	}

	// Now preview with the expected hash — short-circuit path.
	rec := postPreview(srv, `{"fqn":"`+ref.FQN.String()+`","version":"`+ref.Version+`","expected_hash":"`+expectedHash+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got api.ConnectorPreview
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if !got.AlreadyInstalled {
		t.Error("AlreadyInstalled = false after a real install of the same hash")
	}
}

// TestPreviewConnector_RejectsMalformedFQN asserts wire-level input
// validation: a malformed FQN returns 400, not a server-side panic.
func TestPreviewConnector_RejectsMalformedFQN(t *testing.T) {
	ref := mustRefForApp(t, "github://aileron/slack@1.2.0")
	srv, _ := installTestServer(t, ref)

	rec := postPreview(srv, `{"fqn":"not-a-valid-fqn","version":"1.0.0"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestPreviewConnector_RejectsMissingVersion asserts the empty-
// version path: a request with no version is a 400 rather than a
// server crash. Mirrors the same check in InstallConnector.
func TestPreviewConnector_RejectsMissingVersion(t *testing.T) {
	ref := mustRefForApp(t, "github://aileron/slack@1.2.0")
	srv, _ := installTestServer(t, ref)

	rec := postPreview(srv, `{"fqn":"github://aileron/slack","version":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestPreviewConnector_NoInstallerReturns503 covers the bare-server
// path: when no installer is wired (dev harness), the endpoint
// returns 503 rather than crashing. Same robustness contract as
// InstallConnector.
func TestPreviewConnector_NoInstallerReturns503(t *testing.T) {
	srv := &apiServer{}
	rec := httptest.NewRecorder()
	srv.PreviewConnector(rec, httptest.NewRequest(http.MethodPost, "/v1/connectors/preview",
		strings.NewReader(`{"fqn":"github://x/y","version":"1.0.0"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// TestPreviewConnector_CapabilitiesAbsentWhenManifestEmpty: a
// connector manifest with no `[capabilities.*]` blocks must
// surface as `Capabilities` with all sub-fields nil. The CLI
// renders only what's declared — this is the empty-manifest path,
// proving the renderer doesn't fabricate capabilities.
func TestPreviewConnector_CapabilitiesAbsentWhenManifestEmpty(t *testing.T) {
	ref := mustRefForApp(t, "github://aileron/slack@1.2.0")
	srv, _ := installTestServer(t, ref)

	rec := postPreview(srv, `{"fqn":"`+ref.FQN.String()+`","version":"`+ref.Version+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var got api.ConnectorPreview
	_ = json.NewDecoder(rec.Body).Decode(&got)

	// installTestServer's canonical manifest has no capabilities.
	// The preview's Capabilities object should reflect that.
	if got.Capabilities.Credential != nil {
		t.Errorf("Capabilities.Credential = %+v, want nil for empty manifest", got.Capabilities.Credential)
	}
	if got.Capabilities.NetworkHosts != nil {
		t.Errorf("Capabilities.NetworkHosts = %v, want nil for empty manifest", got.Capabilities.NetworkHosts)
	}
}

// TestPreviewConnector_CapabilitiesPropagatedFromManifest: a manifest
// that DOES declare capabilities flows them through. This is the
// load-bearing case — the operator's consent decision depends on
// these surfacing.
func TestPreviewConnector_CapabilitiesPropagatedFromManifest(t *testing.T) {
	ref := mustRefForApp(t, "github://aileron/slack@1.2.0")
	srv := newCapabilityPreviewTestServer(t, ref)

	rec := postPreview(srv, `{"fqn":"`+ref.FQN.String()+`","version":"`+ref.Version+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var got api.ConnectorPreview
	_ = json.NewDecoder(rec.Body).Decode(&got)

	if got.Capabilities.Credential == nil {
		t.Fatal("Capabilities.Credential is nil; manifest declared an api_key block")
	}
	if got.Capabilities.Credential.Kind != "api_key" {
		t.Errorf("Credential.Kind = %q, want api_key", got.Capabilities.Credential.Kind)
	}
	if got.Capabilities.Credential.Scope == nil || *got.Capabilities.Credential.Scope != "Send Slack messages" {
		t.Errorf("Credential.Scope = %v, want pointer to 'Send Slack messages'", got.Capabilities.Credential.Scope)
	}
	if got.Capabilities.NetworkHosts == nil || len(*got.Capabilities.NetworkHosts) == 0 {
		t.Fatal("Capabilities.NetworkHosts is empty; manifest declared slack.com:443")
	}
	if (*got.Capabilities.NetworkHosts)[0] != "slack.com:443" {
		t.Errorf("NetworkHosts[0] = %q, want slack.com:443", (*got.Capabilities.NetworkHosts)[0])
	}
}

// TestPreviewConnector_PublisherFromManifest: when the manifest
// declares a human-readable publisher (e.g. "Aileron Engineering"),
// the preview surfaces that string instead of the FQN authority.
// The CLI prefers the human name; falls back to the authority when
// the manifest is bare.
func TestPreviewConnector_PublisherFromManifest(t *testing.T) {
	ref := mustRefForApp(t, "github://aileron/slack@1.2.0")
	srv := newCapabilityPreviewTestServer(t, ref)

	rec := postPreview(srv, `{"fqn":"`+ref.FQN.String()+`","version":"`+ref.Version+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got api.ConnectorPreview
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.Publisher != "Aileron Engineering" {
		t.Errorf("Publisher = %q, want 'Aileron Engineering' from manifest", got.Publisher)
	}
}

// postPreview issues the preview request and returns the recorder.
func postPreview(srv *apiServer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/connectors/preview", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.PreviewConnector(rec, req)
	return rec
}

// mustRefForApp parses an "<FQN>@<version>" string and fatals on
// error. Mirror of the helper in handlers_sync_test.go but local to
// this file — Go's test scoping doesn't share helpers across the
// package's test files cleanly when both define different specs.
func mustRefForApp(t *testing.T, s string) cstore.Ref {
	t.Helper()
	r, err := cstore.ParseRef(s)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", s, err)
	}
	return r
}

// newCapabilityPreviewTestServer is a variant of installTestServer
// whose canonical manifest declares network and credential
// capabilities plus a human-readable publisher. Lets the preview
// tests exercise the load-bearing capabilities-propagation paths
// without modifying the canonical helper used by every other
// install test in the package.
func newCapabilityPreviewTestServer(t *testing.T, ref cstore.Ref) *apiServer {
	t.Helper()
	manifest := []byte(`[connector]
name = "` + ref.FQN.String() + `"
version = "` + ref.Version + `"
publisher = "Aileron Engineering"

[capabilities.network]
hosts = ["slack.com:443"]

[capabilities.credential]
kind = "api_key"
scope = "Send Slack messages"
`)
	binary := []byte("BIN")
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	payload := append(append([]byte{}, binary...), manifest...)
	sig := ed25519.Sign(priv, payload)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range []struct {
		name string
		body []byte
	}{
		{"connector.wasm", binary},
		{"manifest.toml", manifest},
		{"signature.sig", sig},
	} {
		_ = tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), ModTime: time.Unix(0, 0)})
		_, _ = tw.Write(e.body)
	}
	_ = tw.Close()
	_ = gz.Close()

	resolver := cstore.DefaultResolver()
	url, err := resolver.ResolveTarball(ref)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	keyring := cstore.NewEd25519Keyring()
	keyring.Add(ref.FQN.Authority(), pub)

	return &apiServer{
		log: slog.Default(),
		installer: &cstore.Installer{
			Resolver: resolver,
			Fetcher:  &fakeFetcher{bytesAt: map[string][]byte{url: buf.Bytes()}},
			Verifier: keyring,
			Store:    cstore.NewStore(t.TempDir()),
		},
	}
}
