package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/vault"
)

// installFakeAPIKeyConnector writes an api_key-capability connector
// entry into the cstore at the path computed from the canonical hash.
// Returns the connector FQN string.
func installFakeAPIKeyConnector(t *testing.T, store *cstore.Store, fqn, version, kind string) string {
	t.Helper()
	manifestTOML := `[connector]
name = "` + fqn + `"
version = "` + version + `"
publisher = "test"

[capabilities.credential]
kind = "` + kind + `"
scope = "issues:write"
`
	tb := &cstore.Tarball{
		BinaryName: "connector.wasm",
		Binary:     []byte("FAKE-BINARY"),
		Manifest:   []byte(manifestTOML),
		Signature:  []byte("FAKE-SIG"),
	}
	hashHex := tb.CanonicalHashHex()
	dir := filepath.Join(store.Root(), "connectors", "sha256", hashHex)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "connector.wasm"), tb.Binary, 0o644); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.toml"), tb.Manifest, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	// Rebuild the index so LookupAnyVersion can find the connector.
	if err := store.RebuildIndex(); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	return fqn
}

// bindingTestServer assembles an apiServer with a wired binding store,
// an audit recorder backed by an in-memory store, and a cstore that
// already has one connector installed (api_key, kind=api_key).
func bindingTestServer(t *testing.T, opts ...func(*apiServer)) (*apiServer, *audit.MemStore) {
	t.Helper()
	store := cstore.NewStore(t.TempDir())
	installFakeAPIKeyConnector(t, store, "github://aileron/linear", "1.0.0", "api_key")

	auditStore := audit.NewMemStore()
	recorder := audit.NewRecorder(auditStore, nil, nil)

	srv := &apiServer{
		log:           slog.Default(),
		bindings:      &binding.VaultStore{Vault: vault.NewMemVault()},
		auditStore:    auditStore,
		auditRecorder: recorder,
		installer:     &cstore.Installer{Store: store},
	}
	for _, o := range opts {
		o(srv)
	}
	return srv, auditStore
}

func TestSetupBindings_HappyPathCreatesAPIKeyBinding(t *testing.T) {
	srv, auditStore := bindingTestServer(t)
	body := `{
		"connector_fqn": "github://aileron/linear",
		"bindings": [
			{
				"identity": "team",
				"source": {"kind": "api_key", "value": "lin_test"}
			}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.SetupBindings(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got api.BindingSetupResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Created) != 1 {
		t.Fatalf("Created = %+v", got.Created)
	}
	b := got.Created[0]
	if b.Name != "api_key/linear/team" {
		t.Errorf("Name = %q, want api_key/linear/team", b.Name)
	}
	if b.Kind != "api_key" || b.Service != "linear" || b.Identity != "team" {
		t.Errorf("segments = (%q,%q,%q)", b.Kind, b.Service, b.Identity)
	}
	if b.ConnectorFqn != "github://aileron/linear" {
		t.Errorf("ConnectorFqn = %q", b.ConnectorFqn)
	}
	// Audit event recorded.
	traces, _ := auditStore.ListTraces(context.Background(), audit.Filter{})
	_ = traces // we don't assert trace structure here; the recorder
	// is exercised through the audit_test and this binding contract
	// is "an event with type binding.created exists" — check via the
	// MemStore's events.
	events := dumpEvents(t, auditStore)
	if !containsEvent(events, "binding.created") {
		t.Errorf("expected binding.created event; got %v", events)
	}
}

func TestSetupBindings_RejectsOAuthSource(t *testing.T) {
	srv, _ := bindingTestServer(t)
	// Install an oauth2-kind connector for this test so the kind
	// itself matches; the oauth2 path is rejected because v1 doesn't
	// yet support OAuth setup (#388).
	installFakeAPIKeyConnector(t, srv.installer.Store, "github://aileron/slack", "1.2.0", "oauth2")
	body := `{
		"connector_fqn": "github://aileron/slack",
		"bindings": [
			{"identity": "work", "source": {"kind": "oauth2"}}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.SetupBindings(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "#388") {
		t.Errorf("body should reference #388: %s", rec.Body.String())
	}
}

func TestSetupBindings_KindMismatchIsBadRequest(t *testing.T) {
	srv, _ := bindingTestServer(t)
	body := `{
		"connector_fqn": "github://aileron/linear",
		"bindings": [
			{"identity": "work", "source": {"kind": "oauth2"}}
		]
	}`
	rec := httptest.NewRecorder()
	srv.SetupBindings(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "kind_mismatch") {
		t.Errorf("expected kind_mismatch error code: %s", rec.Body.String())
	}
}

func TestSetupBindings_UninstalledConnectorIs404(t *testing.T) {
	srv, _ := bindingTestServer(t)
	body := `{
		"connector_fqn": "github://aileron/missing",
		"bindings": [{"identity": "work", "source": {"kind": "api_key", "value": "x"}}]
	}`
	rec := httptest.NewRecorder()
	srv.SetupBindings(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(body)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestSetupBindings_VaultLockedReturns423(t *testing.T) {
	srv, _ := bindingTestServer(t, func(s *apiServer) { s.vaultLocked = true })
	body := `{
		"connector_fqn": "github://aileron/linear",
		"bindings": [{"identity": "work", "source": {"kind": "api_key", "value": "x"}}]
	}`
	rec := httptest.NewRecorder()
	srv.SetupBindings(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(body)))
	if rec.Code != http.StatusLocked {
		t.Errorf("status = %d, want 423", rec.Code)
	}
}

func TestSetupBindings_DuplicateNameSkippedByDefault(t *testing.T) {
	srv, _ := bindingTestServer(t)
	body := `{
		"connector_fqn": "github://aileron/linear",
		"bindings": [{"identity": "team", "source": {"kind": "api_key", "value": "v1"}}]
	}`
	rec := httptest.NewRecorder()
	srv.SetupBindings(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first status = %d", rec.Code)
	}
	// Second call hits ErrAlreadyExists; default is skip_existing=true.
	rec = httptest.NewRecorder()
	srv.SetupBindings(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("second status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got api.BindingSetupResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if len(got.Created) != 0 {
		t.Errorf("Created should be empty on duplicate skip; got %+v", got.Created)
	}
	if got.Skipped == nil || len(*got.Skipped) != 1 {
		t.Errorf("Skipped = %+v, want one entry", got.Skipped)
	}
}

func TestSetupBindings_DuplicateNameWithoutSkipReturns409(t *testing.T) {
	srv, _ := bindingTestServer(t)
	first := `{
		"connector_fqn": "github://aileron/linear",
		"bindings": [{"identity": "team", "source": {"kind": "api_key", "value": "v1"}}]
	}`
	second := `{
		"connector_fqn": "github://aileron/linear",
		"skip_existing": false,
		"bindings": [{"identity": "team", "source": {"kind": "api_key", "value": "v2"}}]
	}`
	rec := httptest.NewRecorder()
	srv.SetupBindings(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(first)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first status = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	srv.SetupBindings(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(second)))
	if rec.Code != http.StatusConflict {
		t.Errorf("conflict status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// --- list / inspect / revoke / rebind --------------------------------

// seedBinding creates a single api_key binding directly through the
// store so list/get/revoke tests don't need to drive setup first.
func seedBinding(t *testing.T, srv *apiServer, name, kind, connectorFQN, value string) {
	t.Helper()
	bn, _, service, identity, err := binding.Parse(name)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := srv.bindings.Put(context.Background(), binding.Binding{
		Name: bn, Kind: kind, Service: service, Identity: identity, ConnectorFQN: connectorFQN,
	}, []byte(value), binding.PutCreate); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
}

func TestListBindings_FiltersByConnectorAndKind(t *testing.T) {
	srv, _ := bindingTestServer(t)
	seedBinding(t, srv, "api_key/linear/team", "api_key", "github://aileron/linear", "v")
	seedBinding(t, srv, "api_key/slack/work", "api_key", "github://aileron/slack", "v")
	seedBinding(t, srv, "oauth2/slack/personal", "oauth2", "github://aileron/slack", "v")

	// No filters: all three.
	rec := httptest.NewRecorder()
	srv.ListBindings(rec, httptest.NewRequest(http.MethodGet, "/v1/bindings", nil), api.ListBindingsParams{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got api.BindingListResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if len(got.Items) != 3 {
		t.Errorf("len = %d, want 3", len(got.Items))
	}

	// Filter by connector_fqn.
	connector := "github://aileron/slack"
	rec = httptest.NewRecorder()
	srv.ListBindings(rec, httptest.NewRequest(http.MethodGet, "/v1/bindings", nil),
		api.ListBindingsParams{ConnectorFqn: &connector})
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if len(got.Items) != 2 {
		t.Errorf("connector filter len = %d, want 2", len(got.Items))
	}

	// Filter by kind.
	kind := "api_key"
	rec = httptest.NewRecorder()
	srv.ListBindings(rec, httptest.NewRequest(http.MethodGet, "/v1/bindings", nil),
		api.ListBindingsParams{Kind: &kind})
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if len(got.Items) != 2 {
		t.Errorf("kind filter len = %d, want 2", len(got.Items))
	}

	// Both filters.
	rec = httptest.NewRecorder()
	srv.ListBindings(rec, httptest.NewRequest(http.MethodGet, "/v1/bindings", nil),
		api.ListBindingsParams{ConnectorFqn: &connector, Kind: &kind})
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if len(got.Items) != 1 || got.Items[0].Name != "api_key/slack/work" {
		t.Errorf("dual filter result = %+v", got.Items)
	}
}

func TestGetBinding_NotFoundReturns404(t *testing.T) {
	srv, _ := bindingTestServer(t)
	rec := httptest.NewRecorder()
	srv.GetBinding(rec, httptest.NewRequest(http.MethodGet, "/v1/bindings/api_key/linear/missing", nil),
		"api_key/linear/missing")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestRevokeBinding_HappyPathDeletesAndAudits(t *testing.T) {
	srv, auditStore := bindingTestServer(t)
	seedBinding(t, srv, "api_key/linear/team", "api_key", "github://aileron/linear", "v")
	rec := httptest.NewRecorder()
	srv.RevokeBinding(rec, httptest.NewRequest(http.MethodDelete, "/v1/bindings/api_key/linear/team", nil),
		"api_key/linear/team")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !containsEvent(dumpEvents(t, auditStore), "binding.revoked") {
		t.Error("expected binding.revoked audit event")
	}
	// Subsequent get returns 404.
	rec = httptest.NewRecorder()
	srv.GetBinding(rec, httptest.NewRequest(http.MethodGet, "/v1/bindings/api_key/linear/team", nil),
		"api_key/linear/team")
	if rec.Code != http.StatusNotFound {
		t.Errorf("post-revoke get = %d, want 404", rec.Code)
	}
}

func TestRevokeBinding_NotFoundReturns404(t *testing.T) {
	srv, _ := bindingTestServer(t)
	rec := httptest.NewRecorder()
	srv.RevokeBinding(rec, httptest.NewRequest(http.MethodDelete, "/v1/bindings/api_key/x/y", nil),
		"api_key/x/y")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestRebind_HappyPathReplacesValue(t *testing.T) {
	srv, auditStore := bindingTestServer(t)
	seedBinding(t, srv, "api_key/linear/team", "api_key", "github://aileron/linear", "old")
	body := `{"source": {"kind": "api_key", "value": "new"}}`
	rec := httptest.NewRecorder()
	srv.RebindBinding(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/api_key/linear/team/rebind", strings.NewReader(body)),
		"api_key/linear/team")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !containsEvent(dumpEvents(t, auditStore), "binding.rebound") {
		t.Error("expected binding.rebound audit event")
	}
	// Confirm the underlying secret bytes changed.
	got, err := srv.bindings.Get(context.Background(), "api_key/linear/team")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	v, _ := srv.bindings.(*binding.VaultStore).Vault.Get(context.Background(), string(got.Name))
	if string(v.Value) != "new" {
		t.Errorf("value = %q, want new", v.Value)
	}
}

func TestRebind_NotFoundReturns404(t *testing.T) {
	srv, _ := bindingTestServer(t)
	body := `{"source": {"kind": "api_key", "value": "v"}}`
	rec := httptest.NewRecorder()
	srv.RebindBinding(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/api_key/x/y/rebind", strings.NewReader(body)),
		"api_key/x/y")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestRebind_VaultLockedReturns423(t *testing.T) {
	srv, _ := bindingTestServer(t, func(s *apiServer) { s.vaultLocked = true })
	body := `{"source": {"kind": "api_key", "value": "v"}}`
	rec := httptest.NewRecorder()
	srv.RebindBinding(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/api_key/x/y/rebind", strings.NewReader(body)),
		"api_key/x/y")
	if rec.Code != http.StatusLocked {
		t.Errorf("status = %d, want 423", rec.Code)
	}
}

func TestRebind_RejectsOAuthSource(t *testing.T) {
	srv, _ := bindingTestServer(t)
	seedBinding(t, srv, "api_key/linear/team", "api_key", "github://aileron/linear", "v")
	// kind on the binding matches but the source asks for a kind that
	// isn't api_key — we explicitly route oauth2 to the deferred-issue
	// error path and other unsupported kinds to a generic 400.
	srv2, _ := bindingTestServer(t)
	seedBinding(t, srv2, "oauth2/slack/work", "oauth2", "github://aileron/slack", "v")
	body := `{"source": {"kind": "oauth2"}}`
	rec := httptest.NewRecorder()
	srv2.RebindBinding(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/oauth2/slack/work/rebind", strings.NewReader(body)),
		"oauth2/slack/work")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "#388") {
		t.Errorf("body should reference #388: %s", rec.Body.String())
	}
}

func TestRebind_MissingValueReturns400(t *testing.T) {
	srv, _ := bindingTestServer(t)
	seedBinding(t, srv, "api_key/linear/team", "api_key", "github://aileron/linear", "v")
	body := `{"source": {"kind": "api_key"}}` // no value
	rec := httptest.NewRecorder()
	srv.RebindBinding(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/api_key/linear/team/rebind", strings.NewReader(body)),
		"api_key/linear/team")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestRebind_MalformedJSONReturns400(t *testing.T) {
	srv, _ := bindingTestServer(t)
	rec := httptest.NewRecorder()
	srv.RebindBinding(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/x/rebind", strings.NewReader(`{not json`)),
		"x")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestRevoke_VaultLockedReturns423(t *testing.T) {
	srv, _ := bindingTestServer(t, func(s *apiServer) { s.vaultLocked = true })
	rec := httptest.NewRecorder()
	srv.RevokeBinding(rec,
		httptest.NewRequest(http.MethodDelete, "/v1/bindings/api_key/x/y", nil), "api_key/x/y")
	if rec.Code != http.StatusLocked {
		t.Errorf("status = %d, want 423", rec.Code)
	}
}

func TestSetup_MalformedJSONReturns400(t *testing.T) {
	srv, _ := bindingTestServer(t)
	rec := httptest.NewRecorder()
	srv.SetupBindings(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(`{not json`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestSetup_MissingValueReturns400(t *testing.T) {
	srv, _ := bindingTestServer(t)
	body := `{
		"connector_fqn": "github://aileron/linear",
		"bindings": [{"identity": "team", "source": {"kind": "api_key"}}]
	}`
	rec := httptest.NewRecorder()
	srv.SetupBindings(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "missing_value") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestSetup_InvalidIdentityReturns400(t *testing.T) {
	srv, _ := bindingTestServer(t)
	body := `{
		"connector_fqn": "github://aileron/linear",
		"bindings": [{"identity": "BAD IDENTITY", "source": {"kind": "api_key", "value": "v"}}]
	}`
	rec := httptest.NewRecorder()
	srv.SetupBindings(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestSetup_ConnectorWithoutCredentialCapability400(t *testing.T) {
	// Install a connector that does NOT declare [capabilities.credential].
	store := cstore.NewStore(t.TempDir())
	manifestTOML := `[connector]
name = "github://aileron/echo"
version = "1.0.0"
publisher = "test"
`
	tb := &cstore.Tarball{
		BinaryName: "connector.wasm", Binary: []byte("BIN"),
		Manifest: []byte(manifestTOML), Signature: []byte("SIG"),
	}
	dir := filepath.Join(store.Root(), "connectors", "sha256", tb.CanonicalHashHex())
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "connector.wasm"), tb.Binary, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "manifest.toml"), tb.Manifest, 0o644)
	_ = store.RebuildIndex()

	auditStore := audit.NewMemStore()
	srv := &apiServer{
		log:           slog.Default(),
		bindings:      &binding.VaultStore{Vault: vault.NewMemVault()},
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, nil),
		installer:     &cstore.Installer{Store: store},
	}
	body := `{
		"connector_fqn": "github://aileron/echo",
		"bindings": [{"identity": "team", "source": {"kind": "api_key", "value": "v"}}]
	}`
	rec := httptest.NewRecorder()
	srv.SetupBindings(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestRebind_KindMismatchReturns400(t *testing.T) {
	srv, _ := bindingTestServer(t)
	seedBinding(t, srv, "api_key/linear/team", "api_key", "github://aileron/linear", "old")
	body := `{"source": {"kind": "oauth2"}}`
	rec := httptest.NewRecorder()
	srv.RebindBinding(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/api_key/linear/team/rebind", strings.NewReader(body)),
		"api_key/linear/team")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestBindings_Disabled503WhenStoreNil(t *testing.T) {
	// When no vault is wired (dev edge-case), all binding endpoints
	// short-circuit to 503 with a stable error code so callers know
	// the surface isn't available.
	srv := &apiServer{log: slog.Default()}
	for name, fn := range map[string]func(){
		"List": func() {
			rec := httptest.NewRecorder()
			srv.ListBindings(rec, httptest.NewRequest(http.MethodGet, "/v1/bindings", nil), api.ListBindingsParams{})
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("list code = %d", rec.Code)
			}
		},
		"Get": func() {
			rec := httptest.NewRecorder()
			srv.GetBinding(rec, httptest.NewRequest(http.MethodGet, "/v1/bindings/x", nil), "x")
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("get code = %d", rec.Code)
			}
		},
		"Setup": func() {
			rec := httptest.NewRecorder()
			srv.SetupBindings(rec, httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(`{}`)))
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("setup code = %d", rec.Code)
			}
		},
		"Revoke": func() {
			rec := httptest.NewRecorder()
			srv.RevokeBinding(rec, httptest.NewRequest(http.MethodDelete, "/v1/bindings/x", nil), "x")
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("revoke code = %d", rec.Code)
			}
		},
		"Rebind": func() {
			rec := httptest.NewRecorder()
			srv.RebindBinding(rec, httptest.NewRequest(http.MethodPost, "/v1/bindings/x/rebind", strings.NewReader(`{}`)), "x")
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("rebind code = %d", rec.Code)
			}
		},
	} {
		t.Run(name, func(t *testing.T) { fn() })
	}
}

// --- audit helpers ---

// dumpEvents reads every recorded event from the in-memory audit store.
func dumpEvents(t *testing.T, store *audit.MemStore) []audit.Event {
	t.Helper()
	traces, err := store.ListTraces(context.Background(), audit.Filter{})
	if err != nil {
		t.Fatalf("ListTraces: %v", err)
	}
	// MemStore puts each Append in a synthetic trace; flatten.
	var out []audit.Event
	for _, tr := range traces {
		out = append(out, tr.Events...)
	}
	return out
}

func containsEvent(events []audit.Event, kind string) bool {
	for _, e := range events {
		if string(e.EventType) == kind {
			return true
		}
	}
	return false
}
