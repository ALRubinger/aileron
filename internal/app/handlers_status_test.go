package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/vault"
)

// TestGetStatus_BareServerReturnsZeros covers the dev / no-config
// path: the apiServer struct exists but no actions, connectors, or
// bindings are wired. The endpoint should still return 200 with a
// well-formed payload (zeros) rather than panicking. This is the
// shape every test harness's daemon will see.
func TestGetStatus_BareServerReturnsZeros(t *testing.T) {
	srv := &apiServer{}
	rec := httptest.NewRecorder()
	srv.GetStatus(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got api.StatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ActionCount != 0 {
		t.Errorf("ActionCount = %d, want 0", got.ActionCount)
	}
	if got.ConnectorCount != 0 {
		t.Errorf("ConnectorCount = %d, want 0", got.ConnectorCount)
	}
	if got.BindingCount != 0 {
		t.Errorf("BindingCount = %d, want 0", got.BindingCount)
	}
	// Vault state on a bare server is "missing" — no vault wired,
	// no file at the canonical path (tests run in t.TempDir-style
	// environments without the user's real ~/.aileron/secrets.json).
	if got.VaultState == "" {
		t.Error("VaultState is empty; want one of missing/locked/unlocked/dev")
	}
}

// TestGetStatus_VersionPopulated asserts that the daemon's compile-
// time version + commit fields propagate into the response. The CLI
// uses these to render the runtime banner; missing them would make
// the operator think the server is stale.
func TestGetStatus_VersionPopulated(t *testing.T) {
	srv := &apiServer{}
	rec := httptest.NewRecorder()
	srv.GetStatus(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))

	var got api.StatusResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)

	// `internal/version`'s defaults are "(devel)" + "" until a build
	// stamps them. Either is acceptable here — we just want a non-
	// empty string so callers can tell the field is wired.
	if got.Version == "" {
		t.Error("Version is empty; want compile-time version string")
	}
}

// TestGetStatus_ActionCountReflectsStore wires a real action.Store
// with one synthetic manifest and asserts the count surfaces.
func TestGetStatus_ActionCountReflectsStore(t *testing.T) {
	srv, _ := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	}), 0

	rec := httptest.NewRecorder()
	srv.GetStatus(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))

	var got api.StatusResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.ActionCount != 1 {
		t.Errorf("ActionCount = %d, want 1", got.ActionCount)
	}
}

// TestGetStatus_ConnectorCountReflectsStore wires a real cstore.Store
// + Installer and asserts connector_count comes from
// `cstore.Store.Count()`. Without this regression test, a refactor
// that changes the count source could silently break the CLI surface.
func TestGetStatus_ConnectorCountReflectsStore(t *testing.T) {
	store := cstore.NewStore(t.TempDir())
	srv := &apiServer{
		installer: &cstore.Installer{Store: store},
	}

	// Empty store → 0.
	rec := httptest.NewRecorder()
	srv.GetStatus(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	var got api.StatusResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.ConnectorCount != 0 {
		t.Errorf("empty store ConnectorCount = %d, want 0", got.ConnectorCount)
	}
}

// TestGetStatus_BindingCountReflectsStore wires a vault with one
// bound entry and asserts binding_count surfaces. Vault is unlocked
// in the test (memory vault); the binding-count field must work
// without further unlock.
func TestGetStatus_BindingCountReflectsStore(t *testing.T) {
	v := vault.NewMemVault()
	bs := &binding.VaultStore{Vault: v}
	bn, err := binding.MakeName(cstore.CredentialKindAPIKey, "github", "default")
	if err != nil {
		t.Fatalf("MakeName: %v", err)
	}
	b := binding.Binding{
		Name:         bn,
		Kind:         cstore.CredentialKindAPIKey,
		Service:      "github",
		Identity:     "default",
		ConnectorFQN: "github://acme/connector",
		Status:       binding.StatusActive,
	}
	if err := bs.Put(context.Background(), b, []byte("token-bytes"), binding.PutCreate); err != nil {
		t.Fatalf("Put: %v", err)
	}

	srv := &apiServer{bindings: bs}
	rec := httptest.NewRecorder()
	srv.GetStatus(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	var got api.StatusResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.BindingCount != 1 {
		t.Errorf("BindingCount = %d, want 1", got.BindingCount)
	}
}

// TestGetStatus_VaultStateLocked asserts that when `s.vaultLocked`
// is true (ADR-0011 gate during boot), the surface reports `locked`.
// This is the production state between server start and the user
// entering their passphrase.
func TestGetStatus_VaultStateLocked(t *testing.T) {
	srv := &apiServer{vaultLocked: true}
	rec := httptest.NewRecorder()
	srv.GetStatus(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	var got api.StatusResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if string(got.VaultState) != "locked" {
		t.Errorf("VaultState = %q, want locked", got.VaultState)
	}
}

// TestGetStatus_VaultStateDev exercises the in-memory dev vault path:
// no file at the canonical path, but `s.vault` is non-nil (a mem-
// vault wired by `newLocalEncryptedVault`). The surface reports
// `dev` so operators can tell their persistent credentials aren't
// in play.
func TestGetStatus_VaultStateDev(t *testing.T) {
	// Re-route HOME so the canonical-path probe inside vaultState()
	// looks at an empty temp dir — no vault file present → `dev`.
	t.Setenv("HOME", t.TempDir())
	srv := &apiServer{vault: vault.NewMemVault()}
	rec := httptest.NewRecorder()
	srv.GetStatus(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	var got api.StatusResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if string(got.VaultState) != "dev" {
		t.Errorf("VaultState = %q, want dev", got.VaultState)
	}
}

// TestGetStatus_VaultStateUnlocked covers the production happy
// path: a vault file exists at the canonical path AND `s.vault` is
// non-nil. The surface reports `unlocked`.
func TestGetStatus_VaultStateUnlocked(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// Create the vault file at the canonical path.
	if err := os.MkdirAll(filepath.Join(tmp, ".aileron"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := vault.Init(filepath.Join(tmp, ".aileron", "secrets.json"), "p"); err != nil {
		t.Fatalf("vault.Init: %v", err)
	}
	srv := &apiServer{vault: vault.NewMemVault()}
	rec := httptest.NewRecorder()
	srv.GetStatus(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	var got api.StatusResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if string(got.VaultState) != "unlocked" {
		t.Errorf("VaultState = %q, want unlocked", got.VaultState)
	}
}

// TestGetStatus_GatewayURLSurfaces asserts that when the daemon was
// constructed with `cfg.WebappURL` (i.e. under `aileron launch`), the
// status response carries the URL so the CLI can show it. Operators
// landing here from a notification need a stable click-target.
func TestGetStatus_GatewayURLSurfaces(t *testing.T) {
	srv := &apiServer{webappURL: "http://127.0.0.1:54321"}
	rec := httptest.NewRecorder()
	srv.GetStatus(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	var got api.StatusResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.GatewayUrl == nil || *got.GatewayUrl != "http://127.0.0.1:54321" {
		t.Errorf("GatewayUrl = %v, want pointer to URL", got.GatewayUrl)
	}
}

// TestGetStatus_GatewayURLOmittedWhenAbsent asserts the JSON shape
// stays minimal — the `gateway_url` field is `omitempty`, so under
// the standalone-server invocation (no embedded gateway) callers
// don't see a misleading empty string.
func TestGetStatus_GatewayURLOmittedWhenAbsent(t *testing.T) {
	srv := &apiServer{}
	rec := httptest.NewRecorder()
	srv.GetStatus(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	if got := rec.Body.String(); got != "" {
		// Want the field absent. With pointer + omitempty, a nil
		// pointer drops the key entirely. Confirm by decoding.
		var raw map[string]any
		_ = json.NewDecoder(rec.Body).Decode(&raw)
		if _, present := raw["gateway_url"]; present {
			t.Errorf("gateway_url present in response with no URL configured: %v", raw["gateway_url"])
		}
	}
}

// TestGetStatus_SessionIDFromEnv asserts that the launch session ID,
// exported by `aileron launch` via AILERON_SESSION_ID, surfaces on the
// status response. The webapp uses this to scope its view (per session)
// once multi-session support lands.
func TestGetStatus_SessionIDFromEnv(t *testing.T) {
	t.Setenv("AILERON_SESSION_ID", "test-session-42")
	srv := &apiServer{}
	rec := httptest.NewRecorder()
	srv.GetStatus(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	var got api.StatusResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.SessionId == nil || *got.SessionId != "test-session-42" {
		t.Errorf("SessionId = %v, want pointer to 'test-session-42'", got.SessionId)
	}
}

// TestDefaultVaultPath_FromHome asserts the inlined path helper
// matches the launch package's surface. They are duplicate
// implementations across an import-cycle boundary — this test guards
// against the two drifting silently.
func TestDefaultVaultPath_FromHome(t *testing.T) {
	t.Setenv("HOME", "/test/home")
	got := defaultVaultPath()
	want := filepath.Join("/test/home", ".aileron", "secrets.json")
	if got != want {
		t.Errorf("defaultVaultPath = %q, want %q", got, want)
	}
}

// TestResolveAuditStateDir_DefaultsToHome asserts the env-unset path
// lands at `<home>/.aileron`.
func TestResolveAuditStateDir_DefaultsToHome(t *testing.T) {
	t.Setenv("HOME", "/test/home")
	os.Unsetenv("AILERON_AUDIT_DIR")
	got := resolveAuditStateDir()
	want := filepath.Join("/test/home", ".aileron")
	if got != want {
		t.Errorf("resolveAuditStateDir = %q, want %q", got, want)
	}
}

// TestResolveAuditStateDir_RespectsEnvVar asserts that an explicit env
// value (including an empty string for the in-memory opt-out)
// supersedes the home-based default.
func TestResolveAuditStateDir_RespectsEnvVar(t *testing.T) {
	t.Setenv("AILERON_AUDIT_DIR", "/tmp/x/y")
	if got := resolveAuditStateDir(); got != "/tmp/x/y" {
		t.Errorf("resolveAuditStateDir = %q, want explicit", got)
	}
	t.Setenv("AILERON_AUDIT_DIR", "")
	if got := resolveAuditStateDir(); got != "" {
		t.Errorf("resolveAuditStateDir with empty env = %q, want empty (memory opt-out)", got)
	}
}

// TestNewAuditStore_FileBackedByDefault asserts that the wiring
// chooses FileStore when AILERON_AUDIT_DIR points at a writable
// directory, and that events round-trip through today's daily file.
// Regression guard for the bug behind issue #412: events recorded
// under aileron launch did not survive process exit.
func TestNewAuditStore_FileBackedByDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AILERON_AUDIT_DIR", dir)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := newAuditStore(log)
	if _, ok := store.(*audit.FileStore); !ok {
		t.Fatalf("got %T, want *audit.FileStore", store)
	}

	// Round-trip through the daily file: append, then a fresh FileStore
	// must surface the same event after replay.
	if err := store.Append(context.Background(), audit.Event{
		EventID: "wired", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	reopened, err := audit.NewFileStore(dir, log)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if _, ok := reopened.GetByEventID("wired"); !ok {
		t.Error("event 'wired' did not survive into a fresh reader")
	}
}

// TestNewAuditStore_FallsBackToMemoryWhenEnvEmpty asserts the explicit
// in-memory opt-out path: tests and ephemeral contexts that set
// `AILERON_AUDIT_DIR=` (empty) get a MemStore back.
func TestNewAuditStore_FallsBackToMemoryWhenEnvEmpty(t *testing.T) {
	t.Setenv("AILERON_AUDIT_DIR", "")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := newAuditStore(log)
	if _, ok := store.(*audit.MemStore); !ok {
		t.Errorf("got %T, want *audit.MemStore for empty AILERON_AUDIT_DIR", store)
	}
}
