package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/vault"
)

// /v1/vault/agents/{name}/credentials contract (ADR-0025, U2):
//
//   - GET round-trips the bytes a prior PUT wrote, including metadata.
//   - GET on an empty entry returns 404 with body code `vault_not_found`.
//   - GET while the vault is locked returns 423 with body code `vault_locked`.
//   - PUT with an empty body returns 400 (generated validation surface).
//   - PUT while the vault is locked returns 423 with body code `vault_locked`.
//   - The handler refuses requests when the daemon has no vault wired.

func newAgentCredentialsServer(t *testing.T, v vault.Vault) *apiServer {
	t.Helper()
	return &apiServer{
		log:   slog.Default(),
		vault: v,
	}
}

func TestPutAndGetAgentCredentials_RoundTrip(t *testing.T) {
	v := vault.NewMemVault()
	s := newAgentCredentialsServer(t, v)

	putBody := api.AgentCredentials{
		Value: []byte(`{"claudeAiOauth":{"accessToken":"tok","refreshToken":"rt"}}`),
		Metadata: &api.AgentCredentialsMetadata{
			Type: strPtr("oauth_refresh_token"),
		},
	}
	raw, err := json.Marshal(putBody)
	if err != nil {
		t.Fatalf("marshal put: %v", err)
	}

	putReq := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/claude/credentials",
		bytes.NewReader(raw))
	putReq.Header.Set("Content-Type", "application/json")
	putRec := httptest.NewRecorder()
	s.PutAgentCredentials(putRec, putReq, "claude")
	assertStatus(t, putRec, http.StatusNoContent)

	getReq := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/claude/credentials", nil)
	getRec := httptest.NewRecorder()
	s.GetAgentCredentials(getRec, getReq, "claude")
	assertStatus(t, getRec, http.StatusOK)

	var got api.AgentCredentials
	mustDecode(t, getRec.Body, &got)
	if string(got.Value) != string(putBody.Value) {
		t.Errorf("Value = %q, want %q", got.Value, putBody.Value)
	}
	if got.Metadata == nil || got.Metadata.Type == nil || *got.Metadata.Type != "oauth_refresh_token" {
		t.Errorf("Metadata.Type = %v, want oauth_refresh_token", got.Metadata)
	}
}

func TestGetAgentCredentials_MissingEntryReturnsNotFound(t *testing.T) {
	s := newAgentCredentialsServer(t, vault.NewMemVault())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/claude/credentials", nil)
	s.GetAgentCredentials(rec, req, "claude")
	assertStatus(t, rec, http.StatusNotFound)
	assertErrorCode(t, rec, "vault_not_found")
}

func TestGetAgentCredentials_LockedVaultReturns423(t *testing.T) {
	// LockableVault with no inner vault behaves as locked: every Get
	// returns ErrCredentialUnavailable. Matches the daemon's locked-
	// state semantics per ADR-0011.
	lv := vault.NewLockableVault()
	s := newAgentCredentialsServer(t, lv)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/codex/credentials", nil)
	s.GetAgentCredentials(rec, req, "codex")
	assertStatus(t, rec, http.StatusLocked)
	assertErrorCode(t, rec, "vault_locked")
}

func TestPutAgentCredentials_LockedVaultReturns423(t *testing.T) {
	lv := vault.NewLockableVault()
	s := newAgentCredentialsServer(t, lv)
	rec := httptest.NewRecorder()
	body := []byte(`{"value":"YWJj"}`) // base64("abc")
	req := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/claude/credentials",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.PutAgentCredentials(rec, req, "claude")
	assertStatus(t, rec, http.StatusLocked)
	assertErrorCode(t, rec, "vault_locked")
}

func TestPutAgentCredentials_EmptyValueReturns400(t *testing.T) {
	s := newAgentCredentialsServer(t, vault.NewMemVault())
	rec := httptest.NewRecorder()
	body := []byte(`{"value":""}`)
	req := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/claude/credentials",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.PutAgentCredentials(rec, req, "claude")
	assertStatus(t, rec, http.StatusBadRequest)
}

func TestAgentCredentials_NoVaultReturnsServiceUnavailable(t *testing.T) {
	s := &apiServer{log: slog.Default()}

	getRec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/claude/credentials", nil)
	s.GetAgentCredentials(getRec, getReq, "claude")
	assertStatus(t, getRec, http.StatusServiceUnavailable)

	putRec := httptest.NewRecorder()
	putReq := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/claude/credentials",
		bytes.NewReader([]byte(`{"value":"YWJj"}`)))
	putReq.Header.Set("Content-Type", "application/json")
	s.PutAgentCredentials(putRec, putReq, "claude")
	assertStatus(t, putRec, http.StatusServiceUnavailable)
}

func TestAgentCredentials_VaultPathIsNamespaceScoped(t *testing.T) {
	// A PUT at agent name "claude" must land at the documented
	// `agents/claude/oauth` vault path — the routing layer cannot
	// be used to reach other vault paths. We assert this by reading
	// the in-memory vault directly after the PUT.
	v := vault.NewMemVault()
	s := newAgentCredentialsServer(t, v)
	body := []byte(`{"value":"YWJj"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/claude/credentials",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.PutAgentCredentials(rec, req, "claude")
	assertStatus(t, rec, http.StatusNoContent)

	got, err := v.Get(context.Background(), "agents/claude/oauth")
	if err != nil {
		t.Fatalf("vault.Get agents/claude/oauth: %v", err)
	}
	if string(got.Value) != "abc" {
		t.Errorf("vault value = %q, want \"abc\"", got.Value)
	}

	// And the wrong path returns nothing.
	if _, err := v.Get(context.Background(), "claude/oauth"); !vault.IsNotFound(err) {
		t.Errorf("vault.Get claude/oauth: err = %v, want IsNotFound", err)
	}
}

func strPtr(s string) *string { return &s }
