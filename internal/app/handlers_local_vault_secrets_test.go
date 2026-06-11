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

func TestAgentCredentials_RoundTripPreservesEnvironmentAndLabels(t *testing.T) {
	// agentCredentialsResponse maps every Metadata branch through
	// optional pointers; the prior round-trip test only exercised
	// Type. Make sure Environment and Labels survive a PUT/GET so a
	// future binding that uses them does not silently lose state.
	v := vault.NewMemVault()
	s := newAgentCredentialsServer(t, v)
	body := api.AgentCredentials{
		Value: []byte(`{"x":1}`),
		Metadata: &api.AgentCredentialsMetadata{
			Type:        strPtr("oauth_refresh_token"),
			Environment: strPtr("work"),
			Labels:      &map[string]string{"team": "infra", "purpose": "ci"},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	putReq := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/claude/credentials",
		bytes.NewReader(raw))
	putReq.Header.Set("Content-Type", "application/json")
	putRec := httptest.NewRecorder()
	s.PutAgentCredentials(putRec, putReq, "claude")
	assertStatus(t, putRec, http.StatusNoContent)

	getRec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/claude/credentials", nil)
	s.GetAgentCredentials(getRec, getReq, "claude")
	assertStatus(t, getRec, http.StatusOK)

	var got api.AgentCredentials
	mustDecode(t, getRec.Body, &got)
	if got.Metadata == nil {
		t.Fatal("Metadata is nil; full round-trip lost the optional block")
	}
	if got.Metadata.Environment == nil || *got.Metadata.Environment != "work" {
		t.Errorf("Environment = %v, want work", got.Metadata.Environment)
	}
	if got.Metadata.Labels == nil {
		t.Fatal("Labels round-trip lost the map")
	}
	if (*got.Metadata.Labels)["team"] != "infra" {
		t.Errorf("Labels[team] = %q, want infra", (*got.Metadata.Labels)["team"])
	}
	if (*got.Metadata.Labels)["purpose"] != "ci" {
		t.Errorf("Labels[purpose] = %q, want ci", (*got.Metadata.Labels)["purpose"])
	}
}

func TestPutAgentCredentials_EmptyAgentNameRejected(t *testing.T) {
	// The handler should reject an empty agent name explicitly even
	// when the router would normally bind a non-empty string —
	// defense in depth against a future routing change.
	s := newAgentCredentialsServer(t, vault.NewMemVault())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/vault/agents//credentials",
		bytes.NewReader([]byte(`{"value":"YWJj"}`)))
	req.Header.Set("Content-Type", "application/json")
	s.PutAgentCredentials(rec, req, "")
	assertStatus(t, rec, http.StatusBadRequest)
	assertErrorCode(t, rec, "invalid_request")
}

func TestGetAgentCredentials_EmptyAgentNameRejected(t *testing.T) {
	s := newAgentCredentialsServer(t, vault.NewMemVault())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/agents//credentials", nil)
	s.GetAgentCredentials(rec, req, "")
	assertStatus(t, rec, http.StatusBadRequest)
	assertErrorCode(t, rec, "invalid_request")
}

func TestPutAgentCredentials_MalformedJSONRejected(t *testing.T) {
	s := newAgentCredentialsServer(t, vault.NewMemVault())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/claude/credentials",
		bytes.NewReader([]byte(`{not json`)))
	req.Header.Set("Content-Type", "application/json")
	s.PutAgentCredentials(rec, req, "claude")
	assertStatus(t, rec, http.StatusBadRequest)
	assertErrorCode(t, rec, "invalid_request")
}

func strPtr(s string) *string { return &s }
