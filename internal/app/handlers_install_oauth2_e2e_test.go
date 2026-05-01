package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/action"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/sandbox"
	"github.com/ALRubinger/aileron/internal/vault"
)

// TestOAuth2E2E_RefreshHappenedTransparently is the load-bearing
// assertion for #388 + ADR-0005's credential mediation rule applied
// to OAuth: when an action runs and the stored access token is near
// expiry, the runtime refreshes transparently in the resolver before
// injecting `Authorization: Bearer <new-token>` into the connector's
// outbound HTTP request. The connector never sees the refresh.
//
// Stands up:
//   - A real WazeroRuntime executing the github-prs-test fixture
//     connector with `credential: "oauth2"` on its http_request
//     envelope.
//   - A fake OAuth provider serving a `/token` endpoint that accepts
//     refresh-token requests.
//   - A fake upstream that records the Authorization header it sees.
//   - A pre-populated oauth2 binding whose access token is already
//     past its expiry, so the resolver MUST refresh.
//
// Asserts:
//   - The fake provider received exactly one refresh request.
//   - The fake upstream saw `Authorization: Bearer new-access-token`
//     (the post-refresh token), not the stale one.
//   - The vault now stores the refreshed envelope (so subsequent
//     calls don't re-refresh until that one expires).
//   - The audit log records the binding access without leaking any
//     token bytes anywhere in any payload.
func TestOAuth2E2E_RefreshHappenedTransparently(t *testing.T) {
	const (
		connectorFQN = "github://aileron-test/aileron-connector-google"
		connectorVer = "0.1.0"
		bindingName  = "oauth2/aileron-connector-google/work"
		oldAccess    = "old-access-token-expired"
		newAccess    = "new-access-token-fresh"
		refreshTok   = "stored-refresh-token"
	)

	// --- Fake upstream (impersonates api.example.com) ---
	var seenAuth atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"items":[]}`)
	}))
	t.Cleanup(upstream.Close)
	upstreamHost := stripScheme(upstream.URL)

	// --- Fake OAuth provider (only the token endpoint matters here) ---
	var refreshCount int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		atomic.AddInt32(&refreshCount, 1)
		if r.PostForm.Get("grant_type") != "refresh_token" {
			t.Errorf("grant_type = %q, want refresh_token", r.PostForm.Get("grant_type"))
		}
		if r.PostForm.Get("refresh_token") != refreshTok {
			t.Errorf("refresh_token = %q", r.PostForm.Get("refresh_token"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"access_token":"`+newAccess+`",
			"refresh_token":"`+refreshTok+`",
			"expires_in":3600,
			"token_type":"Bearer"
		}`)
	}))
	t.Cleanup(provider.Close)

	// --- Build + sign + install the connector that emits credential: "oauth2" ---
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tarball, connectorHash := buildOAuth2ConnectorTarball(t, connectorFQN, connectorVer,
		upstreamHost, provider.URL+"/authorize", provider.URL+"/token", priv)

	resolver := cstore.DefaultResolver()
	connectorRef, _ := cstore.ParseRef(connectorFQN + "@" + connectorVer)
	connectorURL, _ := resolver.ResolveTarball(connectorRef)
	keyring := cstore.NewEd25519Keyring()
	keyring.Add(connectorRef.FQN.Authority(), pub)

	rt, err := sandbox.NewWazeroRuntime(context.Background(),
		sandbox.WithHTTPDoer(upstream.Client()))
	if err != nil {
		t.Fatalf("NewWazeroRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	cstoreStore := cstore.NewStore(t.TempDir())
	v := vault.NewMemVault()
	bindingStore := &binding.VaultStore{Vault: v}
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		log:           slog.Default(),
		actions:       action.NewStore(t.TempDir()),
		bindings:      bindingStore,
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, nil),
		installer: &cstore.Installer{
			Resolver: resolver,
			Fetcher:  &fakeFetcher{bytesAt: map[string][]byte{connectorURL: tarball}},
			Verifier: keyring,
			Store:    cstoreStore,
		},
	}
	if _, lerr := srv.actions.Load(); lerr != nil {
		t.Fatalf("actions.Load: %v", lerr)
	}

	// Install the connector via the API so the cstore index is
	// populated like a real install would.
	rec := postInstall(srv, `{"fqn":"`+connectorFQN+`","version":"`+connectorVer+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("connector install: %d, %s", rec.Code, rec.Body.String())
	}

	// --- Install the action that uses the connector ---
	actionMD := buildOAuth2ActionMD(connectorFQN, connectorVer, connectorHash, upstream.URL)
	actionRef, _ := cstore.ParseRef("github://aileron-test/aileron-connector-google/actions/list-emails@0.1.0")
	actionURL, _ := resolver.ResolveTarball(actionRef)
	srv.installer.Fetcher = &fakeFetcher{bytesAt: map[string][]byte{
		connectorURL: tarball,
		actionURL:    buildActionTarball(t, actionMD, priv),
	}}
	rec = postInstallAction(srv, `{"fqn":"github://aileron-test/aileron-connector-google/actions/list-emails","version":"0.1.0"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("action install: %d, %s", rec.Code, rec.Body.String())
	}

	// --- Pre-populate an OAuth binding whose access token is expired ---
	tok := credential.OAuth2Token{
		AccessToken:  oldAccess,
		RefreshToken: refreshTok,
		ExpiresAt:    time.Now().Add(-1 * time.Hour), // already expired
		TokenType:    "Bearer",
		ClientID:     "test-client-id",
		TokenURL:     provider.URL + "/token",
		Scopes:       []string{"read"},
	}
	envelope, _ := json.Marshal(tok)
	if err := bindingStore.Put(context.Background(), binding.Binding{
		Name:                bindingName,
		Kind:                "oauth2",
		Service:             "aileron-connector-google",
		Identity:            "work",
		ConnectorFQN:        connectorFQN,
		Status:              binding.StatusActive,
		RefreshTokenPresent: true,
	}, envelope, binding.PutCreate); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	// --- Execute the action ---
	exec := action.NewSandboxExecutor(srv.actions, srv.installer.Store, rt, bindingStore)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	// We need to set up an HTTP client for the OAuth2VaultResolver
	// to use during refresh. The default resolver constructed by
	// VaultStore.ResolverFor uses http.DefaultClient; for the test
	// we want to use the upstream's client (which trusts the test
	// servers' TLS configs). The cleanest way is to re-test the
	// resolver in isolation here: the binding store + executor will
	// use the default client which works fine for httptest's plain
	// HTTP servers (no TLS).
	res, execErr := exec.Execute(context.Background(), "list-emails", map[string]any{})
	if execErr != nil {
		t.Fatalf("Execute: %v", execErr)
	}
	if res.Failure != nil {
		t.Fatalf("expected success; got failure: %+v", res.Failure)
	}

	// --- Assertions ---
	// Provider saw exactly one refresh request.
	if got := atomic.LoadInt32(&refreshCount); got != 1 {
		t.Errorf("provider refresh requests = %d, want 1", got)
	}
	// Upstream saw the refreshed access token, not the stale one.
	gotAuth, _ := seenAuth.Load().(string)
	if gotAuth != "Bearer "+newAccess {
		t.Errorf("upstream Authorization = %q, want %q", gotAuth, "Bearer "+newAccess)
	}
	// Vault now holds the refreshed envelope.
	stored, err := bindingStore.Vault.Get(context.Background(), bindingName)
	if err != nil {
		t.Fatalf("vault.Get: %v", err)
	}
	var got credential.OAuth2Token
	_ = json.Unmarshal(stored.Value, &got)
	if got.AccessToken != newAccess {
		t.Errorf("persisted AccessToken = %q, want %q", got.AccessToken, newAccess)
	}
	// Audit invariant: no token bytes leak anywhere in any payload.
	traces, _ := auditStore.ListTraces(context.Background(), audit.Filter{})
	for _, tr := range traces {
		for _, e := range tr.Events {
			payload, _ := json.Marshal(e.Payload)
			for _, leak := range []string{oldAccess, newAccess, refreshTok} {
				if strings.Contains(string(payload), leak) {
					t.Errorf("audit event %q leaked token bytes: %s", e.EventType, payload)
				}
			}
		}
	}
}

// buildOAuth2ConnectorTarball wraps the existing test wasm with a
// manifest that declares oauth2 + the OAuth provider URLs.
func buildOAuth2ConnectorTarball(t *testing.T, fqn, version, upstreamHost, authURL, tokenURL string, priv ed25519.PrivateKey) ([]byte, string) {
	t.Helper()
	manifestTOML := `[connector]
name = "` + fqn + `"
version = "` + version + `"
publisher = "aileron-test"

[capabilities.network]
hosts = ["` + upstreamHost + `"]

[capabilities.credential]
kind = "oauth2"
scope = "Read your data"

[capabilities.credential.oauth2]
authorize_url = "` + authURL + `"
token_url = "` + tokenURL + `"
client_id = "test-client-id"
scopes = ["read"]
`
	binary := githubPRSTestWASM
	manifest := []byte(manifestTOML)
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

	tb := &cstore.Tarball{Binary: binary, Manifest: manifest}
	return buf.Bytes(), "sha256:" + tb.CanonicalHashHex()
}

// buildOAuth2ActionMD writes an action that calls the test connector's
// list_prs op against the fake upstream.
func buildOAuth2ActionMD(connectorFQN, connectorVer, connectorHash, upstreamURL string) []byte {
	return []byte(`+++
name = "list-emails"
version = "0.1.0"
source = "github://aileron-test/aileron-connector-google/actions/list-emails@0.1.0"

[[requires.connectors]]
name = "` + connectorFQN + `"
version = "` + connectorVer + `"
hash = "` + connectorHash + `"
capabilities = ["list_prs"]

[match]
intent = "list emails"

[[execute]]
id = "list"
connector = "` + connectorFQN + `"
op = "list_prs"

[execute.inputs]
url = "` + upstreamURL + `/api/list"
credential_kind = "oauth2"
+++

# List emails

Lists recent emails.
`)
}
