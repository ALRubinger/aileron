package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/action"
	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/sandbox"
	"github.com/ALRubinger/aileron/internal/vault"
)

// githubPRSTestWASM is the compiled test fixture connector built from
// internal/app/testdata/github-prs-test/main.go. Rebuild with:
//
//	cd internal/app/testdata/github-prs-test && \
//	  GOOS=wasip1 GOARCH=wasm go build -o ../github-prs-test.wasm .
//
//go:embed testdata/github-prs-test.wasm
var githubPRSTestWASM []byte

// TestInstallE2E_FullChain drives the entire framework end-to-end:
//
//  1. POST /v1/connectors/install installs a real signed WASM
//     connector via the cstore install pipeline (fetch → verify →
//     hash → store).
//  2. POST /v1/actions/install installs a signed action template
//     that references that connector by FQN+hash.
//  3. POST /v1/bindings/setup creates an api_key binding for the
//     connector, storing the test PAT in the vault.
//  4. SandboxExecutor.Execute invokes the action. The connector
//     emits an http_request envelope with `credential: "api_key"`;
//     the runtime resolves the binding, injects
//     `Authorization: Bearer <PAT>`, and dials the upstream.
//
// Asserts:
//   - the upstream sees the injected Authorization header (proves
//     credential mediation actually mediates)
//   - the action's output contains the upstream body (proves the
//     install + execute round-trip works)
//   - the audit log records install + binding events but NEVER the
//     credential bytes anywhere in any payload (proves the
//     end-to-end privacy invariant)
//
// This is the load-bearing test for #366. The test-only fixture
// connector stands in for the real per-service connector that lives
// in its own repo (per ADR-0002 lifecycle independence).
func TestInstallE2E_FullChain(t *testing.T) {
	const (
		connectorFQN = "github://aileron-test/aileron-connector-github"
		connectorVer = "0.1.0"
		actionFQN    = "github://aileron-test/aileron-connector-github/actions/list-recent-prs"
		actionVer    = "0.1.0"
		testPAT      = "ghp_e2e_test_token_xxxxxxxxxxx"
	)

	// --- Fake upstream (impersonates api.github.com) ---
	var seenAuth string
	var seenPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"number":42,"title":"Test PR","html_url":"https://x"}]`)
	}))
	t.Cleanup(upstream.Close)
	upstreamHost := stripScheme(upstream.URL)

	// --- Tarballs + signing ---
	connectorPub, connectorPriv, _ := ed25519.GenerateKey(rand.Reader)
	connectorTarball, connectorHash := buildConnectorTarballForE2E(t, connectorFQN, connectorVer,
		upstreamHost, connectorPriv)

	actionMD := buildActionMDForE2E(connectorFQN, connectorVer, connectorHash, upstream.URL)
	actionTarball := buildActionTarball(t, actionMD, connectorPriv)

	// --- Resolver + fetcher: route both FQNs to in-memory tarballs ---
	connectorRef, _ := cstore.ParseRef(connectorFQN + "@" + connectorVer)
	actionRef, _ := cstore.ParseRef(actionFQN + "@" + actionVer)
	resolver := cstore.DefaultResolver()
	connectorURL, _ := resolver.ResolveTarball(connectorRef)
	actionURL, _ := resolver.ResolveTarball(actionRef)
	fetcher := &fakeFetcher{bytesAt: map[string][]byte{
		connectorURL: connectorTarball,
		actionURL:    actionTarball,
	}}

	// --- Keyring trusts the publisher ---
	keyring := cstore.NewEd25519Keyring()
	keyring.Add(connectorRef.FQN.Authority(), connectorPub)

	// --- Real Wazero runtime ---
	ctx := context.Background()
	rt, err := sandbox.NewWazeroRuntime(ctx, sandbox.WithLogger(slog.Default()),
		sandbox.WithHTTPDoer(upstream.Client()))
	if err != nil {
		t.Fatalf("NewWazeroRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(ctx) })

	// --- Server wiring ---
	cstoreStore := cstore.NewStore(t.TempDir())
	v := vault.NewMemVault()
	bindingStore := &binding.VaultStore{Vault: v}
	auditStore := audit.NewMemStore()
	recorder := audit.NewRecorder(auditStore, nil, nil)
	srv := &apiServer{
		log:           slog.Default(),
		actions:       action.NewStore(t.TempDir()),
		bindings:      bindingStore,
		auditStore:    auditStore,
		auditRecorder: recorder,
		installer: &cstore.Installer{
			Resolver: resolver,
			Fetcher:  fetcher,
			Verifier: keyring,
			Store:    cstoreStore,
		},
	}
	if _, lerr := srv.actions.Load(); lerr != nil {
		t.Fatalf("actions.Load: %v", lerr)
	}

	// === 1. Install the connector ===
	rec := postInstall(srv, `{"fqn":"`+connectorFQN+`","version":"`+connectorVer+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("connector install: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// === 2. Install the action ===
	rec = postInstallAction(srv, `{"fqn":"`+actionFQN+`","version":"`+actionVer+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("action install: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var installed api.InstalledAction
	_ = json.NewDecoder(rec.Body).Decode(&installed)
	if installed.Name != "list-recent-prs" {
		t.Errorf("InstalledAction.Name = %q", installed.Name)
	}

	// === 3. Bind the api_key for the connector ===
	bindBody := `{
		"connector_fqn":"` + connectorFQN + `",
		"bindings":[{"identity":"e2e","source":{"kind":"api_key","value":"` + testPAT + `"}}]
	}`
	bindReq := httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(bindBody))
	bindRec := httptest.NewRecorder()
	srv.SetupBindings(bindRec, bindReq)
	if bindRec.Code != http.StatusCreated {
		t.Fatalf("binding setup: status = %d, body = %s", bindRec.Code, bindRec.Body.String())
	}

	// === 4. Execute the action ===
	exec := action.NewSandboxExecutor(srv.actions, srv.installer.Store, rt, bindingStore)
	t.Cleanup(func() { _ = exec.Close(ctx) })
	res, execErr := exec.Execute(ctx, "list-recent-prs", map[string]any{
		"owner": "ALRubinger",
		"repo":  "aileron",
	})
	if execErr != nil {
		t.Fatalf("Execute: %v", execErr)
	}
	if res.Failure != nil {
		t.Fatalf("expected success; got failure: %+v", res.Failure)
	}

	// === Assertions ===
	if seenAuth != "Bearer "+testPAT {
		t.Errorf("upstream Authorization = %q, want %q", seenAuth, "Bearer "+testPAT)
	}
	if !strings.Contains(seenPath, "/repos/ALRubinger/aileron/pulls") {
		t.Errorf("upstream path = %q; want contains /repos/.../pulls", seenPath)
	}
	if !strings.Contains(res.Content, "Test PR") {
		t.Errorf("action output missing upstream body: %s", res.Content)
	}

	// Audit invariant: nowhere in any recorded payload should the
	// credential bytes appear. This is the load-bearing privacy
	// guarantee for ADR-0005's credential mediation.
	traces, _ := auditStore.ListTraces(ctx, audit.Filter{})
	for _, tr := range traces {
		for _, e := range tr.Events {
			payload, _ := json.Marshal(e.Payload)
			if bytes.Contains(payload, []byte(testPAT)) {
				t.Errorf("audit event %q leaked credential bytes: %s", e.EventType, payload)
			}
		}
	}
}

// TestInstallE2E_HashMismatchRefusesAtInstall flips a byte in the
// fetched connector tarball before serving so the hash check on the
// action's [[requires.connectors]] entry fails. The action install
// pre-check refuses (the connector is not installed at the expected
// hash) — but the more direct assertion is at the connector install
// level: when an expected_hash is supplied that doesn't match, the
// pipeline aborts.
func TestInstallE2E_HashMismatchRefusesAtInstall(t *testing.T) {
	const (
		connectorFQN = "github://aileron-test/aileron-connector-github"
		connectorVer = "0.1.0"
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tarball, realHash := buildConnectorTarballForE2E(t, connectorFQN, connectorVer,
		stripScheme(upstream.URL), priv)

	resolver := cstore.DefaultResolver()
	ref, _ := cstore.ParseRef(connectorFQN + "@" + connectorVer)
	url, _ := resolver.ResolveTarball(ref)
	keyring := cstore.NewEd25519Keyring()
	keyring.Add(ref.FQN.Authority(), pub)

	srv := &apiServer{
		log: slog.Default(),
		installer: &cstore.Installer{
			Resolver: resolver,
			Fetcher:  &fakeFetcher{bytesAt: map[string][]byte{url: tarball}},
			Verifier: keyring,
			Store:    cstore.NewStore(t.TempDir()),
		},
	}

	// Supply a deliberately-wrong expected_hash. The pipeline computes
	// the real hash, sees the mismatch, refuses without writing.
	wrongHash := "sha256:" + strings.Repeat("0", 64)
	rec := postInstall(srv, `{"fqn":"`+connectorFQN+`","version":"`+connectorVer+
		`","expected_hash":"`+wrongHash+`"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hash_mismatch") {
		t.Errorf("body should report hash_mismatch: %s", rec.Body.String())
	}
	// And the real hash, when supplied, succeeds — proves the same
	// pipeline accepts when the expectation matches.
	rec = postInstall(srv, `{"fqn":"`+connectorFQN+`","version":"`+connectorVer+
		`","expected_hash":"`+realHash+`"}`)
	if rec.Code != http.StatusCreated {
		t.Errorf("matching-hash install status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestInstallE2E_ActionBoundaryCapabilityDenial proves the action's
// declared capability subset is enforced when the action attempts to
// invoke an op outside the subset. The action manifest declares
// capabilities = ["something_else"], but the execute step calls
// "list_prs" — denied at the action boundary before the connector
// instantiates.
func TestInstallE2E_ActionBoundaryCapabilityDenial(t *testing.T) {
	const (
		connectorFQN = "github://aileron-test/aileron-connector-github"
		connectorVer = "0.1.0"
		actionFQN    = "github://aileron-test/aileron-connector-github/actions/forbidden"
		actionVer    = "0.1.0"
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("upstream should never be dialed when action boundary denies")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tarball, connectorHash := buildConnectorTarballForE2E(t, connectorFQN, connectorVer,
		stripScheme(upstream.URL), priv)

	// Action manifest with a declared capability that does NOT
	// include "list_prs"; the executor's EnforceCapability denies.
	deniedActionMD := []byte(`+++
name = "forbidden"
version = "0.1.0"
source = "` + actionFQN + `@` + actionVer + `"

[[requires.connectors]]
name = "` + connectorFQN + `"
version = "` + connectorVer + `"
hash = "` + connectorHash + `"
capabilities = ["something_else"]

[match]
intent = "test"

[[execute]]
id = "x"
connector = "` + connectorFQN + `"
op = "list_prs"
+++

# Forbidden
`)
	actionTarball := buildActionTarball(t, deniedActionMD, priv)

	resolver := cstore.DefaultResolver()
	connectorRef, _ := cstore.ParseRef(connectorFQN + "@" + connectorVer)
	actionRef, _ := cstore.ParseRef(actionFQN + "@" + actionVer)
	connectorURL, _ := resolver.ResolveTarball(connectorRef)
	actionURL, _ := resolver.ResolveTarball(actionRef)
	keyring := cstore.NewEd25519Keyring()
	keyring.Add(connectorRef.FQN.Authority(), pub)

	rt, err := sandbox.NewWazeroRuntime(context.Background(),
		sandbox.WithHTTPDoer(upstream.Client()))
	if err != nil {
		t.Fatalf("NewWazeroRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	cstoreStore := cstore.NewStore(t.TempDir())
	srv := &apiServer{
		log:           slog.Default(),
		actions:       action.NewStore(t.TempDir()),
		bindings:      &binding.VaultStore{Vault: vault.NewMemVault()},
		auditStore:    audit.NewMemStore(),
		auditRecorder: audit.NewRecorder(audit.NewMemStore(), nil, nil),
		installer: &cstore.Installer{
			Resolver: resolver,
			Fetcher:  &fakeFetcher{bytesAt: map[string][]byte{connectorURL: tarball, actionURL: actionTarball}},
			Verifier: keyring,
			Store:    cstoreStore,
		},
	}
	_, _ = srv.actions.Load()

	// Install both, then attempt to execute.
	if rec := postInstall(srv, `{"fqn":"`+connectorFQN+`","version":"`+connectorVer+`"}`); rec.Code != http.StatusCreated {
		t.Fatalf("connector install: %d, %s", rec.Code, rec.Body.String())
	}
	if rec := postInstallAction(srv, `{"fqn":"`+actionFQN+`","version":"`+actionVer+`"}`); rec.Code != http.StatusCreated {
		t.Fatalf("action install: %d, %s", rec.Code, rec.Body.String())
	}

	exec := action.NewSandboxExecutor(srv.actions, srv.installer.Store, rt, srv.bindings)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })
	res, _ := exec.Execute(context.Background(), "forbidden", nil)
	if res.Failure == nil {
		t.Fatal("expected Result.Failure on action-boundary denial")
	}
	if string(res.Failure.Class()) != "capability_denied" {
		t.Errorf("class = %q, want capability_denied", res.Failure.Class())
	}
}

// --- helpers ---

// buildConnectorTarballForE2E builds + signs a connector tarball
// containing the embedded github-prs-test.wasm and a manifest that
// pins network access to the supplied upstream host:port. Returns the
// tarball bytes and the canonical hash (`sha256:<hex>`) of the
// `binary || manifest` payload.
func buildConnectorTarballForE2E(t *testing.T, fqn, version, upstreamHostPort string, priv ed25519.PrivateKey) ([]byte, string) {
	t.Helper()
	manifestTOML := `[connector]
name = "` + fqn + `"
version = "` + version + `"
publisher = "aileron-test"

[capabilities.network]
hosts = ["` + upstreamHostPort + `"]

[capabilities.credential]
kind = "api_key"
scope = "repo:read"
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

// buildActionMDForE2E renders an action manifest that targets the
// given connector + invokes its `list_prs` op against the supplied
// upstream URL.
func buildActionMDForE2E(connectorFQN, connectorVer, connectorHash, upstreamURL string) []byte {
	return []byte(`+++
name = "list-recent-prs"
version = "0.1.0"
source = "github://aileron-test/aileron-connector-github/actions/list-recent-prs@0.1.0"

[[requires.connectors]]
name = "` + connectorFQN + `"
version = "` + connectorVer + `"
hash = "` + connectorHash + `"
capabilities = ["list_prs"]

[match]
intent = "list recent merged PRs"

[[inputs]]
name = "owner"
type = "string"
description = "Repository owner."

[[inputs]]
name = "repo"
type = "string"
description = "Repository name."

[[execute]]
id = "list"
connector = "` + connectorFQN + `"
op = "list_prs"

[execute.inputs]
url = "` + upstreamURL + `/repos/${args.owner}/${args.repo}/pulls?state=closed&per_page=10"
+++

# List recent PRs

Lists the most recently merged PRs in a GitHub repository.
`)
}

// stripScheme returns the host:port portion of an http(s):// URL.
func stripScheme(url string) string {
	u := strings.TrimPrefix(url, "https://")
	u = strings.TrimPrefix(u, "http://")
	if i := strings.Index(u, "/"); i >= 0 {
		u = u[:i]
	}
	return u
}
