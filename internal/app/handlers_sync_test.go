package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/action"
	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/vault"
)

// syncTestActionManifest is a minimal action manifest declaring one
// connector dep. The hash is intentionally not verified — sync v1
// trusts the resolver/verifier to produce the canonical hash; the
// action's declared hash is informational at this stage of the
// pipeline.
const syncTestActionManifest = `+++
name = "%s"
version = "1.0.0"
source = "hub://aileron/%s@1.0.0"

[[requires.connectors]]
name = "%s"
version = "%s"
hash = "sha256:placeholder"
capabilities = ["read"]

[match]
intent = "test"

[[execute]]
id = "step1"
connector = "%s"
op = "noop"
+++

# %s
`

// newSyncTestServer composes a fully-wired apiServer for sync tests.
// Callers control the action manifests on disk and which connectors
// the in-memory installer can fetch. Bindings start empty (memory
// vault); tests that exercise the bound/unbound boundary install
// bindings via VaultStore.Put before invoking Sync.
func newSyncTestServer(t *testing.T, actionManifests map[string]string, fetchableRefs ...cstore.Ref) (*apiServer, *binding.VaultStore) {
	t.Helper()

	// Action store on disk.
	actionsDir := t.TempDir()
	for name, body := range actionManifests {
		if err := os.WriteFile(filepath.Join(actionsDir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	actionStore := action.NewStore(actionsDir)
	if _, err := actionStore.Load(); err != nil {
		t.Fatalf("action.Load: %v", err)
	}

	// Build a tarball + URL fixture map for every connector the
	// installer should be able to fetch. Refs not listed here will
	// fail at fetch time — exactly the contract that drives
	// install_failures.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	bytesAt := map[string][]byte{}
	resolver := cstore.DefaultResolver()
	keyring := cstore.NewEd25519Keyring()
	for _, ref := range fetchableRefs {
		manifest := []byte(`[connector]
name = "` + ref.FQN.String() + `"
version = "` + ref.Version + `"
publisher = "test"

[capabilities.credential]
kind = "api_key"
scope = "test scope"
`)
		binary := []byte("BIN")
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

		url, err := resolver.ResolveTarball(ref)
		if err != nil {
			t.Fatalf("resolve %s: %v", ref.String(), err)
		}
		bytesAt[url] = buf.Bytes()
		keyring.Add(ref.FQN.Authority(), pub)
	}

	store := cstore.NewStore(t.TempDir())
	v := vault.NewMemVault()
	bs := &binding.VaultStore{Vault: v}

	srv := &apiServer{
		log: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		installer: &cstore.Installer{
			Resolver: resolver,
			Fetcher:  &fakeFetcher{bytesAt: bytesAt},
			Verifier: keyring,
			Store:    store,
		},
		actions:  actionStore,
		bindings: bs,
	}
	return srv, bs
}

// postSync POSTs an empty body to /v1/sync and decodes the response.
func postSync(t *testing.T, srv *apiServer) api.SyncResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Sync(rec, httptest.NewRequest(http.MethodPost, "/v1/sync", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("Sync status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var got api.SyncResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// TestSync_BareServerReturnsEmptyResults: a server with no installer,
// no actions, no bindings still produces a well-formed response. This
// guards the dev-mode harness that exercises sync before any wiring
// is in place.
func TestSync_BareServerReturnsEmptyResults(t *testing.T) {
	srv := &apiServer{}
	rec := httptest.NewRecorder()
	srv.Sync(rec, httptest.NewRequest(http.MethodPost, "/v1/sync", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got api.SyncResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.ActionsSeen != 0 || len(got.Required) != 0 || len(got.Installed) != 0 ||
		len(got.AlreadyInstalled) != 0 || len(got.InstallFailures) != 0 || len(got.Unbound) != 0 {
		t.Errorf("expected all-empty response; got %+v", got)
	}
}

// TestSync_InstallsMissingConnector: the canonical happy path. One
// action declares one connector dep. The connector isn't in the
// cstore. Sync drives the install pipeline and reports the connector
// in `installed`.
func TestSync_InstallsMissingConnector(t *testing.T) {
	ref := mustRef(t, "github://acme/widget@1.0.0")
	manifest := fmtManifest("post-update", "post-update", ref)
	srv, _ := newSyncTestServer(t, map[string]string{"post-update.md": manifest}, ref)

	got := postSync(t, srv)

	if got.ActionsSeen != 1 {
		t.Errorf("ActionsSeen = %d, want 1", got.ActionsSeen)
	}
	if len(got.Required) != 1 || got.Required[0].Fqn != "github://acme/widget" {
		t.Errorf("Required = %v, want one widget ref", got.Required)
	}
	if len(got.Installed) != 1 || got.Installed[0].Fqn != "github://acme/widget" {
		t.Errorf("Installed = %v, want widget", got.Installed)
	}
	if len(got.AlreadyInstalled) != 0 {
		t.Errorf("AlreadyInstalled = %v, want empty", got.AlreadyInstalled)
	}
	if len(got.InstallFailures) != 0 {
		t.Errorf("InstallFailures = %v, want empty", got.InstallFailures)
	}
}

// TestSync_AlreadyInstalledIdempotent: an action's connector is
// already in the cstore. Sync must produce zero new installs and
// surface the ref in `already_installed`. This is the idempotent
// re-run path operators rely on.
func TestSync_AlreadyInstalledIdempotent(t *testing.T) {
	ref := mustRef(t, "github://acme/widget@1.0.0")
	manifest := fmtManifest("post-update", "post-update", ref)
	srv, _ := newSyncTestServer(t, map[string]string{"post-update.md": manifest}, ref)

	// First sync installs it.
	postSync(t, srv)

	// Second sync: same ref already in store.
	got := postSync(t, srv)
	if len(got.Installed) != 0 {
		t.Errorf("Installed = %v, want empty on re-run", got.Installed)
	}
	if len(got.AlreadyInstalled) != 1 {
		t.Errorf("AlreadyInstalled = %v, want 1", got.AlreadyInstalled)
	}
}

// TestSync_DedupesAcrossActions: two actions reference the same
// (FQN, version). The required set must contain it exactly once and
// the install pipeline must run exactly once.
func TestSync_DedupesAcrossActions(t *testing.T) {
	ref := mustRef(t, "github://acme/widget@1.0.0")
	srv, _ := newSyncTestServer(t,
		map[string]string{
			"a.md": fmtManifest("a", "a", ref),
			"b.md": fmtManifest("b", "b", ref),
		},
		ref)

	got := postSync(t, srv)
	if got.ActionsSeen != 2 {
		t.Errorf("ActionsSeen = %d, want 2", got.ActionsSeen)
	}
	if len(got.Required) != 1 {
		t.Errorf("Required = %v, want 1 deduped ref", got.Required)
	}
	if len(got.Installed) != 1 {
		t.Errorf("Installed = %v, want 1 install", got.Installed)
	}
}

// TestSync_InstallFailureDoesNotAbort: action references a connector
// the fetcher can't produce → install fails. Sync surfaces the
// failure in `install_failures` and continues processing the next
// connector. Same "offline-failing per connector" contract that
// `connector check` honors.
func TestSync_InstallFailureDoesNotAbort(t *testing.T) {
	ok := mustRef(t, "github://acme/works@1.0.0")
	bad := mustRef(t, "github://acme/broken@1.0.0")
	srv, _ := newSyncTestServer(t, map[string]string{
		"a.md": fmtManifest("a", "a", bad),
		"b.md": fmtManifest("b", "b", ok),
	}, ok) // only `ok` is fetchable

	got := postSync(t, srv)
	if len(got.Installed) != 1 || got.Installed[0].Fqn != "github://acme/works" {
		t.Errorf("Installed = %v, want only works", got.Installed)
	}
	if len(got.InstallFailures) != 1 || got.InstallFailures[0].Fqn != "github://acme/broken" {
		t.Errorf("InstallFailures = %v, want broken entry", got.InstallFailures)
	}
}

// TestSync_UnboundCapabilitySurfaced: a connector with a
// `[capabilities.credential]` block and no binding in the vault must
// produce one `unbound` entry. This is the operator's signal to run
// `aileron binding setup <FQN>`.
func TestSync_UnboundCapabilitySurfaced(t *testing.T) {
	ref := mustRef(t, "github://acme/widget@1.0.0")
	manifest := fmtManifest("post-update", "post-update", ref)
	srv, _ := newSyncTestServer(t, map[string]string{"post-update.md": manifest}, ref)

	got := postSync(t, srv)
	if len(got.Unbound) != 1 {
		t.Fatalf("Unbound = %v, want 1 entry", got.Unbound)
	}
	u := got.Unbound[0]
	if u.ConnectorFqn != "github://acme/widget" {
		t.Errorf("Unbound[0].ConnectorFqn = %q, want github://acme/widget", u.ConnectorFqn)
	}
	if u.Kind != "api_key" {
		t.Errorf("Unbound[0].Kind = %q, want api_key", u.Kind)
	}
	if u.Scope == nil || *u.Scope != "test scope" {
		t.Errorf("Unbound[0].Scope = %v, want pointer to 'test scope'", u.Scope)
	}
}

// TestSync_BoundCapabilityNotInUnbound: a binding exists for the
// (connector, kind) pair → that capability must NOT appear in
// `unbound`. This is the "i already set this up" path operators see
// after their second sync.
func TestSync_BoundCapabilityNotInUnbound(t *testing.T) {
	ref := mustRef(t, "github://acme/widget@1.0.0")
	manifest := fmtManifest("post-update", "post-update", ref)
	srv, bs := newSyncTestServer(t, map[string]string{"post-update.md": manifest}, ref)

	// First sync installs the connector.
	postSync(t, srv)

	// Operator binds it.
	bn, err := binding.MakeName(cstore.CredentialKindAPIKey, "widget", "default")
	if err != nil {
		t.Fatalf("MakeName: %v", err)
	}
	b := binding.Binding{
		Name:         bn,
		Kind:         cstore.CredentialKindAPIKey,
		Service:      "widget",
		Identity:     "default",
		ConnectorFQN: "github://acme/widget",
		Status:       binding.StatusActive,
	}
	if err := bs.Put(context.Background(), b, []byte("token-bytes"), binding.PutCreate); err != nil {
		t.Fatalf("bs.Put: %v", err)
	}

	// Second sync should see it as bound.
	got := postSync(t, srv)
	if len(got.Unbound) != 0 {
		t.Errorf("Unbound = %v, want empty after binding created", got.Unbound)
	}
}

// TestSync_AutoInstallFlagAccepted: the `auto_install` request field
// is wired through the spec but is a no-op in v1 (no consent prompt).
// Asserting the handler accepts the body and runs the sweep is the
// guard against a future rename or removal that silently drops the
// field.
func TestSync_AutoInstallFlagAccepted(t *testing.T) {
	ref := mustRef(t, "github://acme/widget@1.0.0")
	manifest := fmtManifest("a", "a", ref)
	srv, _ := newSyncTestServer(t, map[string]string{"a.md": manifest}, ref)

	body := strings.NewReader(`{"auto_install":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/sync", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Sync(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestSync_BadRequestBody: a malformed request body returns 400 with
// a structured error. Guards CLI clients that send accidentally
// truncated payloads.
func TestSync_BadRequestBody(t *testing.T) {
	srv := &apiServer{}
	body := strings.NewReader(`{"auto_install":not_json}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/sync", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Sync(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// fmtManifest fills the action-manifest template with the provided
// action name and connector reference. The `name` parameter doubles
// as the action's id (used to sort List output) and as the title at
// the bottom of the markdown body.
func fmtManifest(name, source string, ref cstore.Ref) string {
	fqn := ref.FQN.String()
	return fmt.Sprintf(syncTestActionManifest, name, source, fqn, ref.Version, fqn, name)
}

// mustRef parses a "<FQN>@<version>" string and fatals on error.
// Mirrors cstore_test.go's helper but lives here so the app package's
// tests don't reach across the test boundary.
func mustRef(t *testing.T, s string) cstore.Ref {
	t.Helper()
	r, err := cstore.ParseRef(s)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", s, err)
	}
	return r
}
