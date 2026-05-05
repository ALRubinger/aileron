package app

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/vault"
)

// /v1/vault/status + /v1/vault/unlock contract (#429):
//
//   - status reports `unlocked` when the daemon was constructed with
//     a pre-unlocked vault (no LocalVaultPath).
//   - status reports `missing` when LocalVaultPath is set and no
//     vault file exists.
//   - status reports `locked` when LocalVaultPath is set and a vault
//     file exists but the daemon hasn't been unlocked.
//   - unlock with the right passphrase transitions to `unlocked` and
//     swaps the inner vault into the LockableVault wrapper.
//   - unlock with the wrong passphrase returns 401 wrong_passphrase.
//   - unlock against a missing vault file returns 404 no_local_vault_file.
//   - unlock when already unlocked returns 409 (idempotent / race-safe).
//   - unlock with empty passphrase returns 400.

func newLocalVaultServer(t *testing.T, vaultPath string) *apiServer {
	t.Helper()
	lv := vault.NewLockableVault()
	return &apiServer{
		log:            slog.Default(),
		vault:          lv,
		lockableVault:  lv,
		localVaultPath: vaultPath,
		vaultLocked:    true,
	}
}

func TestGetLocalVaultStatus_NoLocalVault_ReturnsUnlocked(t *testing.T) {
	// No LocalVaultPath: daemon is in dev / cloud mode, the local
	// vault gate doesn't apply. Status should report unlocked so
	// the webapp keeps the modal closed.
	s := &apiServer{log: slog.Default()}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/vault/status", nil)
	s.GetLocalVaultStatus(w, r)
	assertStatus(t, w, http.StatusOK)

	var got api.LocalVaultStatusResponse
	mustDecode(t, w.Body, &got)
	if got.Locked {
		t.Errorf("Locked = true, want false")
	}
	if got.State != api.LocalVaultStatusResponseStateUnlocked {
		t.Errorf("State = %q, want unlocked", got.State)
	}
}

func TestGetLocalVaultStatus_VaultMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json") // does not exist
	s := newLocalVaultServer(t, path)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/vault/status", nil)
	s.GetLocalVaultStatus(w, r)
	assertStatus(t, w, http.StatusOK)

	var got api.LocalVaultStatusResponse
	mustDecode(t, w.Body, &got)
	if !got.Locked {
		t.Errorf("Locked = false, want true (no vault file)")
	}
	if got.State != api.LocalVaultStatusResponseStateMissing {
		t.Errorf("State = %q, want missing", got.State)
	}
}

func TestGetLocalVaultStatus_VaultLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "open sesame"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	s := newLocalVaultServer(t, path)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/vault/status", nil)
	s.GetLocalVaultStatus(w, r)
	assertStatus(t, w, http.StatusOK)

	var got api.LocalVaultStatusResponse
	mustDecode(t, w.Body, &got)
	if !got.Locked {
		t.Errorf("Locked = false, want true")
	}
	if got.State != api.LocalVaultStatusResponseStateLocked {
		t.Errorf("State = %q, want locked", got.State)
	}
}

func TestGetLocalVaultStatus_VaultUnlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "open sesame"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	s := newLocalVaultServer(t, path)
	s.vaultLocked = false // simulate post-unlock state

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/vault/status", nil)
	s.GetLocalVaultStatus(w, r)
	assertStatus(t, w, http.StatusOK)

	var got api.LocalVaultStatusResponse
	mustDecode(t, w.Body, &got)
	if got.Locked {
		t.Errorf("Locked = true, want false")
	}
	if got.State != api.LocalVaultStatusResponseStateUnlocked {
		t.Errorf("State = %q, want unlocked", got.State)
	}
}

func TestUnlockLocalVault_Success(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "open sesame"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	s := newLocalVaultServer(t, path)

	w := httptest.NewRecorder()
	r := unlockRequest(`{"passphrase":"open sesame"}`)
	s.UnlockLocalVault(w, r)
	assertStatus(t, w, http.StatusOK)

	if s.vaultLocked {
		t.Error("vaultLocked still true after unlock")
	}
	if s.lockableVault.Inner() == nil {
		t.Error("lockableVault inner not set after unlock")
	}

	// Confirm the inner is usable: round-trip a secret.
	if err := s.vault.Put(r.Context(), "k", []byte("v"), vault.Metadata{}); err != nil {
		t.Errorf("Put after unlock: %v", err)
	}
}

func TestUnlockLocalVault_WrongPassphrase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "open sesame"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	s := newLocalVaultServer(t, path)

	w := httptest.NewRecorder()
	r := unlockRequest(`{"passphrase":"nope"}`)
	s.UnlockLocalVault(w, r)
	assertStatus(t, w, http.StatusUnauthorized)
	assertErrorCode(t, w, "wrong_passphrase")
	if !s.vaultLocked {
		t.Error("vaultLocked flipped to false on wrong passphrase")
	}
	if s.lockableVault.Inner() != nil {
		t.Error("lockableVault inner set on wrong passphrase")
	}
}

func TestUnlockLocalVault_MissingVaultFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json") // no Init call
	s := newLocalVaultServer(t, path)

	w := httptest.NewRecorder()
	r := unlockRequest(`{"passphrase":"any"}`)
	s.UnlockLocalVault(w, r)
	assertStatus(t, w, http.StatusNotFound)
	assertErrorCode(t, w, "no_local_vault_file")
}

func TestUnlockLocalVault_AlreadyUnlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "open sesame"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	s := newLocalVaultServer(t, path)
	s.vaultLocked = false

	w := httptest.NewRecorder()
	r := unlockRequest(`{"passphrase":"open sesame"}`)
	s.UnlockLocalVault(w, r)
	assertStatus(t, w, http.StatusConflict)

	var got api.LocalVaultStatusResponse
	mustDecode(t, w.Body, &got)
	if got.Locked || got.State != api.LocalVaultStatusResponseStateUnlocked {
		t.Errorf("body = %+v, want unlocked status", got)
	}
}

func TestUnlockLocalVault_EmptyPassphrase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "open sesame"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	s := newLocalVaultServer(t, path)

	w := httptest.NewRecorder()
	r := unlockRequest(`{"passphrase":""}`)
	s.UnlockLocalVault(w, r)
	assertStatus(t, w, http.StatusBadRequest)
	assertErrorCode(t, w, "missing_passphrase")
}

func TestUnlockLocalVault_NoLocalVaultConfigured(t *testing.T) {
	// Daemon constructed without a LocalVaultPath: /v1/vault/unlock
	// is meaningless and must surface that explicitly.
	s := &apiServer{log: slog.Default()}
	w := httptest.NewRecorder()
	r := unlockRequest(`{"passphrase":"any"}`)
	s.UnlockLocalVault(w, r)
	assertStatus(t, w, http.StatusServiceUnavailable)
	assertErrorCode(t, w, "no_local_vault")
}

// --- helpers ---

func unlockRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/vault/unlock", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func assertStatus(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status = %d, want %d, body = %s", w.Code, want, w.Body.String())
	}
}

func assertErrorCode(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body: %s)", err, w.Body.String())
	}
	if env.Error.Code != want {
		t.Errorf("error.code = %q, want %q", env.Error.Code, want)
	}
}

func mustDecode(t *testing.T, body io.Reader, into any) {
	t.Helper()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(buf.Bytes(), into); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, buf.String())
	}
}
