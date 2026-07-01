package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/model"
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
	s.PutAgentCredentials(putRec, putReq, "claude", api.PutAgentCredentialsParams{})
	assertStatus(t, putRec, http.StatusNoContent)

	getReq := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/claude/credentials", nil)
	getRec := httptest.NewRecorder()
	s.GetAgentCredentials(getRec, getReq, "claude", api.GetAgentCredentialsParams{})
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
	s.GetAgentCredentials(rec, req, "claude", api.GetAgentCredentialsParams{})
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
	s.GetAgentCredentials(rec, req, "codex", api.GetAgentCredentialsParams{})
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
	s.PutAgentCredentials(rec, req, "claude", api.PutAgentCredentialsParams{})
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
	s.PutAgentCredentials(rec, req, "claude", api.PutAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusBadRequest)
}

func TestAgentCredentials_NoVaultReturnsServiceUnavailable(t *testing.T) {
	s := &apiServer{log: slog.Default()}

	getRec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/claude/credentials", nil)
	s.GetAgentCredentials(getRec, getReq, "claude", api.GetAgentCredentialsParams{})
	assertStatus(t, getRec, http.StatusServiceUnavailable)

	putRec := httptest.NewRecorder()
	putReq := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/claude/credentials",
		bytes.NewReader([]byte(`{"value":"YWJj"}`)))
	putReq.Header.Set("Content-Type", "application/json")
	s.PutAgentCredentials(putRec, putReq, "claude", api.PutAgentCredentialsParams{})
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
	s.PutAgentCredentials(rec, req, "claude", api.PutAgentCredentialsParams{})
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
	s.PutAgentCredentials(putRec, putReq, "claude", api.PutAgentCredentialsParams{})
	assertStatus(t, putRec, http.StatusNoContent)

	getRec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/claude/credentials", nil)
	s.GetAgentCredentials(getRec, getReq, "claude", api.GetAgentCredentialsParams{})
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
	s.PutAgentCredentials(rec, req, "", api.PutAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusBadRequest)
	assertErrorCode(t, rec, "invalid_request")
}

func TestGetAgentCredentials_EmptyAgentNameRejected(t *testing.T) {
	s := newAgentCredentialsServer(t, vault.NewMemVault())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/agents//credentials", nil)
	s.GetAgentCredentials(rec, req, "", api.GetAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusBadRequest)
	assertErrorCode(t, rec, "invalid_request")
}

func TestPutAgentCredentials_MalformedJSONRejected(t *testing.T) {
	s := newAgentCredentialsServer(t, vault.NewMemVault())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/claude/credentials",
		bytes.NewReader([]byte(`{not json`)))
	req.Header.Set("Content-Type", "application/json")
	s.PutAgentCredentials(rec, req, "claude", api.PutAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusBadRequest)
	assertErrorCode(t, rec, "invalid_request")
}

// DELETE + list contract (ADR-0025, #981):
//
//   - DELETE removes an entry a prior PUT wrote; a subsequent GET 404s.
//   - DELETE on a missing entry returns 404 `vault_not_found` (the
//     idempotent-Delete regression: the handler must existence-check
//     before deleting, not rely on Delete's nil return).
//   - DELETE while the vault is locked returns 423 `vault_locked`.
//   - DELETE with an empty name returns 400; with no vault wired, 503.
//   - GET /v1/vault/agents lists agent names + metadata only, never the
//     credential value; filters out non-agent paths; works empty; 503
//     when no vault is wired.

func TestDeleteAgentCredentials_RoundTrip(t *testing.T) {
	v := vault.NewMemVault()
	s := newAgentCredentialsServer(t, v)

	putReq := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/claude/credentials",
		bytes.NewReader([]byte(`{"value":"YWJj"}`)))
	putReq.Header.Set("Content-Type", "application/json")
	putRec := httptest.NewRecorder()
	s.PutAgentCredentials(putRec, putReq, "claude", api.PutAgentCredentialsParams{})
	assertStatus(t, putRec, http.StatusNoContent)

	delRec := httptest.NewRecorder()
	delReq := httptest.NewRequest(http.MethodDelete, "/v1/vault/agents/claude/credentials", nil)
	s.DeleteAgentCredentials(delRec, delReq, "claude", api.DeleteAgentCredentialsParams{})
	assertStatus(t, delRec, http.StatusNoContent)

	getRec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/claude/credentials", nil)
	s.GetAgentCredentials(getRec, getReq, "claude", api.GetAgentCredentialsParams{})
	assertStatus(t, getRec, http.StatusNotFound)
}

func TestDeleteAgentCredentials_MissingEntryReturnsNotFound(t *testing.T) {
	// Regression for the idempotent-Delete pitfall: MemVault.Delete
	// returns nil whether or not the path existed, so a handler that
	// trusts Delete's return would 204 on a miss. The contract is 404.
	s := newAgentCredentialsServer(t, vault.NewMemVault())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vault/agents/claude/credentials", nil)
	s.DeleteAgentCredentials(rec, req, "claude", api.DeleteAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusNotFound)
	assertErrorCode(t, rec, "vault_not_found")
}

func TestDeleteAgentCredentials_LockedVaultReturns423(t *testing.T) {
	lv := vault.NewLockableVault()
	s := newAgentCredentialsServer(t, lv)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vault/agents/codex/credentials", nil)
	s.DeleteAgentCredentials(rec, req, "codex", api.DeleteAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusLocked)
	assertErrorCode(t, rec, "vault_locked")
}

func TestDeleteAgentCredentials_EmptyAgentNameRejected(t *testing.T) {
	s := newAgentCredentialsServer(t, vault.NewMemVault())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vault/agents//credentials", nil)
	s.DeleteAgentCredentials(rec, req, "", api.DeleteAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusBadRequest)
	assertErrorCode(t, rec, "invalid_request")
}

func TestDeleteAgentCredentials_NoVaultReturnsServiceUnavailable(t *testing.T) {
	s := &apiServer{log: slog.Default()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vault/agents/claude/credentials", nil)
	s.DeleteAgentCredentials(rec, req, "claude", api.DeleteAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusServiceUnavailable)
}

func TestListAgentCredentials_RoundTripMetadataOnly(t *testing.T) {
	v := vault.NewMemVault()
	s := newAgentCredentialsServer(t, v)

	for _, name := range []string{"claude", "codex"} {
		body := api.AgentCredentials{
			Value:    []byte(`SECRETBYTES`),
			Metadata: &api.AgentCredentialsMetadata{Type: strPtr("oauth_refresh_token")},
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/"+name+"/credentials",
			bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.PutAgentCredentials(rec, req, name, api.PutAgentCredentialsParams{})
		assertStatus(t, rec, http.StatusNoContent)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/agents", nil)
	s.ListAgentCredentials(rec, req)
	assertStatus(t, rec, http.StatusOK)

	// The raw body must not leak credential bytes (ADR-0011).
	if strings.Contains(rec.Body.String(), "SECRETBYTES") ||
		strings.Contains(rec.Body.String(), "value") {
		t.Fatalf("list body leaked credential material: %s", rec.Body.String())
	}

	var got api.AgentCredentialsList
	mustDecode(t, rec.Body, &got)
	names := map[string]bool{}
	for _, a := range got.Agents {
		names[a.Name] = true
		if a.Metadata == nil || a.Metadata.Type == nil || *a.Metadata.Type != "oauth_refresh_token" {
			t.Errorf("agent %s metadata = %v, want type oauth_refresh_token", a.Name, a.Metadata)
		}
	}
	if !names["claude"] || !names["codex"] {
		t.Errorf("listed names = %v, want claude and codex", names)
	}
}

func TestListAgentCredentials_EmptyVaultReturnsEmptyArray(t *testing.T) {
	s := newAgentCredentialsServer(t, vault.NewMemVault())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/agents", nil)
	s.ListAgentCredentials(rec, req)
	assertStatus(t, rec, http.StatusOK)

	var got api.AgentCredentialsList
	mustDecode(t, rec.Body, &got)
	if got.Agents == nil {
		t.Fatal("agents must be a non-nil (empty) array")
	}
	if len(got.Agents) != 0 {
		t.Errorf("agents = %v, want empty", got.Agents)
	}
}

func TestListAgentCredentials_LockedVaultReturnsEmptyOK(t *testing.T) {
	// A locked LockableVault (no inner) returns an empty slice from List
	// rather than ErrCredentialUnavailable, because listing never
	// decrypts a stored value (see vault.LockableVault.List). Unlike
	// Get/Put/Delete, List does NOT yield 423 on a locked vault — it
	// returns 200 with an empty agents array. Pin that contract so a
	// future change cannot quietly add a 423 path to list.
	lv := vault.NewLockableVault()
	s := newAgentCredentialsServer(t, lv)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/agents", nil)
	s.ListAgentCredentials(rec, req)
	assertStatus(t, rec, http.StatusOK)

	var got api.AgentCredentialsList
	mustDecode(t, rec.Body, &got)
	if got.Agents == nil {
		t.Fatal("agents must be a non-nil (empty) array")
	}
	if len(got.Agents) != 0 {
		t.Errorf("agents = %v, want empty on locked vault", got.Agents)
	}
}

func TestListAgentCredentials_FiltersNonAgentPaths(t *testing.T) {
	v := vault.NewMemVault()
	s := newAgentCredentialsServer(t, v)
	// Seed a non-agent path directly (a secret-style key) plus one agent.
	if err := v.Put(context.Background(), "some/other/path", []byte("x"), vault.Metadata{}); err != nil {
		t.Fatal(err)
	}
	if err := v.Put(context.Background(), "agents/claude/oauth", []byte("y"), vault.Metadata{}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/agents", nil)
	s.ListAgentCredentials(rec, req)
	assertStatus(t, rec, http.StatusOK)

	var got api.AgentCredentialsList
	mustDecode(t, rec.Body, &got)
	if len(got.Agents) != 1 || got.Agents[0].Name != "claude" {
		t.Errorf("agents = %v, want only claude", got.Agents)
	}
	if got.Agents[0].Purpose == nil || *got.Agents[0].Purpose != "oauth" {
		t.Errorf("agent purpose = %v, want oauth", got.Agents[0].Purpose)
	}
}

// TestListAgentCredentials_SurfacesApiKeyPurpose pins the #1347 fix: an
// agent with both an oauth and an apikey credential yields two summaries,
// one per purpose. The old `/oauth`-suffix match dropped the apikey
// entry entirely.
func TestListAgentCredentials_SurfacesApiKeyPurpose(t *testing.T) {
	v := vault.NewMemVault()
	s := newAgentCredentialsServer(t, v)
	if err := v.Put(context.Background(), "agents/claude/oauth", []byte("o"), vault.Metadata{Type: "oauth_refresh_token"}); err != nil {
		t.Fatal(err)
	}
	if err := v.Put(context.Background(), "agents/claude/apikey", []byte("k"), vault.Metadata{Type: "api_key"}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/agents", nil)
	s.ListAgentCredentials(rec, req)
	assertStatus(t, rec, http.StatusOK)

	var got api.AgentCredentialsList
	mustDecode(t, rec.Body, &got)
	if len(got.Agents) != 2 {
		t.Fatalf("agents = %v, want 2 (oauth + apikey)", got.Agents)
	}
	purposes := map[string]string{} // purpose -> metadata type
	for _, a := range got.Agents {
		if a.Name != "claude" {
			t.Errorf("agent name = %q, want claude", a.Name)
		}
		if a.Purpose == nil {
			t.Fatalf("summary missing purpose: %+v", a)
		}
		typ := ""
		if a.Metadata != nil && a.Metadata.Type != nil {
			typ = *a.Metadata.Type
		}
		purposes[*a.Purpose] = typ
	}
	if purposes["oauth"] != "oauth_refresh_token" {
		t.Errorf("oauth summary type = %q, want oauth_refresh_token", purposes["oauth"])
	}
	if purposes["apikey"] != "api_key" {
		t.Errorf("apikey summary type = %q, want api_key", purposes["apikey"])
	}
}

// TestListAgentCredentials_ApiKeyOnlyAgentIsVisible is the direct
// regression for the bug: an agent that has ONLY an apikey credential
// (no oauth) was previously invisible because the list matched the
// `/oauth` suffix. It must now appear with purpose "apikey".
func TestListAgentCredentials_ApiKeyOnlyAgentIsVisible(t *testing.T) {
	v := vault.NewMemVault()
	s := newAgentCredentialsServer(t, v)
	if err := v.Put(context.Background(), "agents/claude/apikey", []byte("k"), vault.Metadata{Type: "api_key"}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/agents", nil)
	s.ListAgentCredentials(rec, req)
	assertStatus(t, rec, http.StatusOK)

	var got api.AgentCredentialsList
	mustDecode(t, rec.Body, &got)
	if len(got.Agents) != 1 {
		t.Fatalf("agents = %v, want 1 (apikey-only)", got.Agents)
	}
	if got.Agents[0].Name != "claude" {
		t.Errorf("name = %q, want claude", got.Agents[0].Name)
	}
	if got.Agents[0].Purpose == nil || *got.Agents[0].Purpose != "apikey" {
		t.Errorf("purpose = %v, want apikey", got.Agents[0].Purpose)
	}
}

func TestListAgentCredentials_NoVaultReturnsServiceUnavailable(t *testing.T) {
	s := &apiServer{log: slog.Default()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/agents", nil)
	s.ListAgentCredentials(rec, req)
	assertStatus(t, rec, http.StatusServiceUnavailable)
}

// getErrVault returns a generic (non-NotFound, non-Unavailable) error
// from Get so the DeleteAgentCredentials existence check hits its 500
// branch. Delete/List/Put are unreachable in that test.
type getErrVault struct{ vault.Vault }

func (getErrVault) Get(context.Context, string) (vault.Secret, error) {
	return vault.Secret{}, errGeneric
}

// deleteErrVault succeeds on the existence Get (returns a stored
// secret) but fails the Delete with a generic error, exercising the
// vault_delete_failed 500 branch.
type deleteErrVault struct{ vault.Vault }

func (deleteErrVault) Get(context.Context, string) (vault.Secret, error) {
	return vault.Secret{Value: []byte("x")}, nil
}
func (deleteErrVault) Delete(context.Context, string) error { return errGeneric }

// listErrVault fails List with a generic error for the
// vault_list_failed 500 branch.
type listErrVault struct{ vault.Vault }

func (listErrVault) List(context.Context) ([]vault.Entry, error) {
	return nil, errGeneric
}

var errGeneric = errGenericErr{}

type errGenericErr struct{}

func (errGenericErr) Error() string { return "boom" }

func TestDeleteAgentCredentials_GetErrorReturns500(t *testing.T) {
	s := newAgentCredentialsServer(t, getErrVault{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vault/agents/claude/credentials", nil)
	s.DeleteAgentCredentials(rec, req, "claude", api.DeleteAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusInternalServerError)
	assertErrorCode(t, rec, "vault_get_failed")
}

func TestDeleteAgentCredentials_DeleteErrorReturns500(t *testing.T) {
	s := newAgentCredentialsServer(t, deleteErrVault{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vault/agents/claude/credentials", nil)
	s.DeleteAgentCredentials(rec, req, "claude", api.DeleteAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusInternalServerError)
	assertErrorCode(t, rec, "vault_delete_failed")
}

func TestListAgentCredentials_ListErrorReturns500(t *testing.T) {
	s := newAgentCredentialsServer(t, listErrVault{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/agents", nil)
	s.ListAgentCredentials(rec, req)
	assertStatus(t, rec, http.StatusInternalServerError)
	assertErrorCode(t, rec, "vault_list_failed")
}

// agentNameAndPurposeFromVaultPath moved to internal/vaultscope (as
// AgentNameAndPurposeFromVaultPath) and is tested there.

func strPtr(s string) *string { return &s }

// Audit + session-log contract for the per-agent credential verbs
// (#985, ADR-0010 + ADR-0011):
//
//   - GET/PUT/DELETE each emit one audit event with the verb's
//     EventType, carrying `aileron.vault.agent` and never the bytes.
//   - The success path emits no `aileron.failure.class`; each error
//     branch emits the verb's EventType plus the error-code class.
//   - The credential value never appears in the audit payload JSON or
//     the captured slog buffer (the core leak-prevention assertion).
//   - A nil recorder never panics and still fires the slog line.
//   - The optional X-Aileron-Session-Id header attributes the operation
//     to that session; absent, the actor is the loopback service ref.

// vaultCredentialSecret is the credential body the audit tests PUT. The
// distinctive substrings below must never surface in any audit payload
// or session log line.
const vaultCredentialSecret = `{"claudeAiOauth":{"accessToken":"tok-SUPERSECRET","refreshToken":"rt-SUPERSECRET"}}`

var vaultCredentialForbidden = []string{"tok-SUPERSECRET", "rt-SUPERSECRET", "accessToken"}

// newAuditingAgentCredentialsServer builds a server with a real
// in-memory audit store and a slog logger that writes into logBuf, so a
// test can assert on both the recorded events and the session log.
func newAuditingAgentCredentialsServer(t *testing.T, v vault.Vault) (*apiServer, *audit.MemStore, *bytes.Buffer) {
	t.Helper()
	store := audit.NewMemStore()
	logBuf := &bytes.Buffer{}
	s := &apiServer{
		log:           slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		vault:         v,
		auditRecorder: audit.NewRecorder(store, nil, func() string { return "vault-cred-audit-id" }),
	}
	return s, store, logBuf
}

// assertNoCredentialLeak fails if any forbidden substring appears in the
// marshaled event payloads or the captured session log.
func assertNoCredentialLeak(t *testing.T, events []audit.Event, logBuf *bytes.Buffer) {
	t.Helper()
	for _, e := range events {
		raw, err := json.Marshal(e.Payload)
		if err != nil {
			t.Fatalf("marshal event payload: %v", err)
		}
		for _, forbidden := range vaultCredentialForbidden {
			if strings.Contains(string(raw), forbidden) {
				t.Errorf("audit payload leaked %q: %s", forbidden, string(raw))
			}
		}
	}
	logStr := logBuf.String()
	for _, forbidden := range vaultCredentialForbidden {
		if strings.Contains(logStr, forbidden) {
			t.Errorf("session log leaked %q: %s", forbidden, logStr)
		}
	}
}

// putAuditableCredential PUTs the distinctive secret for agent name.
func putAuditableCredential(t *testing.T, s *apiServer, name string) {
	t.Helper()
	body := api.AgentCredentials{Value: []byte(vaultCredentialSecret)}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/"+name+"/credentials",
		bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.PutAgentCredentials(rec, req, name, api.PutAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusNoContent)
}

func listVaultCredentialEvents(t *testing.T, store *audit.MemStore) []audit.Event {
	t.Helper()
	events, err := store.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	return events
}

func TestGetAgentCredentials_SuccessEmitsReadEvent(t *testing.T) {
	s, store, logBuf := newAuditingAgentCredentialsServer(t, vault.NewMemVault())
	putAuditableCredential(t, s, "claude")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/claude/credentials", nil)
	s.GetAgentCredentials(rec, req, "claude", api.GetAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusOK)

	events := listVaultCredentialEvents(t, store)
	read := requireSingleEvent(t, events, model.EventTypeVaultCredentialRead)
	if read.Payload["aileron.vault.agent"] != "claude" {
		t.Errorf("agent = %v, want claude", read.Payload["aileron.vault.agent"])
	}
	if _, ok := read.Payload["aileron.failure.class"]; ok {
		t.Errorf("success event must not carry aileron.failure.class: %v", read.Payload)
	}
	assertNoCredentialLeak(t, events, logBuf)
}

func TestPutAgentCredentials_SuccessEmitsWriteEventWithoutSecret(t *testing.T) {
	s, store, logBuf := newAuditingAgentCredentialsServer(t, vault.NewMemVault())
	putAuditableCredential(t, s, "claude")

	events := listVaultCredentialEvents(t, store)
	write := requireSingleEvent(t, events, model.EventTypeVaultCredentialWrite)
	if write.Payload["aileron.vault.agent"] != "claude" {
		t.Errorf("agent = %v, want claude", write.Payload["aileron.vault.agent"])
	}
	// Core leak-prevention assertion: the PUT body never reaches audit
	// or the session log.
	assertNoCredentialLeak(t, events, logBuf)
}

func TestDeleteAgentCredentials_SuccessEmitsDeleteEvent(t *testing.T) {
	s, store, logBuf := newAuditingAgentCredentialsServer(t, vault.NewMemVault())
	putAuditableCredential(t, s, "claude")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vault/agents/claude/credentials", nil)
	s.DeleteAgentCredentials(rec, req, "claude", api.DeleteAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusNoContent)

	events := listVaultCredentialEvents(t, store)
	del := requireSingleEvent(t, events, model.EventTypeVaultCredentialDelete)
	if del.Payload["aileron.vault.agent"] != "claude" {
		t.Errorf("agent = %v, want claude", del.Payload["aileron.vault.agent"])
	}
	assertNoCredentialLeak(t, events, logBuf)
}

func TestGetAgentCredentials_MissingEntryEmitsFailureEvent(t *testing.T) {
	s, store, logBuf := newAuditingAgentCredentialsServer(t, vault.NewMemVault())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/claude/credentials", nil)
	s.GetAgentCredentials(rec, req, "claude", api.GetAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusNotFound)

	events := listVaultCredentialEvents(t, store)
	ev := requireSingleEvent(t, events, model.EventTypeVaultCredentialRead)
	assertFailureClass(t, ev, "vault_not_found")
	assertNoCredentialLeak(t, events, logBuf)
}

func TestGetAgentCredentials_LockedVaultEmitsFailureEvent(t *testing.T) {
	s, store, logBuf := newAuditingAgentCredentialsServer(t, vault.NewLockableVault())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/codex/credentials", nil)
	s.GetAgentCredentials(rec, req, "codex", api.GetAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusLocked)

	events := listVaultCredentialEvents(t, store)
	ev := requireSingleEvent(t, events, model.EventTypeVaultCredentialRead)
	assertFailureClass(t, ev, "vault_locked")
	assertNoCredentialLeak(t, events, logBuf)
}

func TestPutAgentCredentials_LockedVaultEmitsFailureEvent(t *testing.T) {
	s, store, logBuf := newAuditingAgentCredentialsServer(t, vault.NewLockableVault())
	body := api.AgentCredentials{Value: []byte(vaultCredentialSecret)}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/claude/credentials",
		bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	s.PutAgentCredentials(rec, req, "claude", api.PutAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusLocked)

	events := listVaultCredentialEvents(t, store)
	ev := requireSingleEvent(t, events, model.EventTypeVaultCredentialWrite)
	assertFailureClass(t, ev, "vault_locked")
	// Even a rejected PUT must not leak the body it tried to store.
	assertNoCredentialLeak(t, events, logBuf)
}

func TestDeleteAgentCredentials_LockedVaultEmitsFailureEvent(t *testing.T) {
	s, store, logBuf := newAuditingAgentCredentialsServer(t, vault.NewLockableVault())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vault/agents/codex/credentials", nil)
	s.DeleteAgentCredentials(rec, req, "codex", api.DeleteAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusLocked)

	events := listVaultCredentialEvents(t, store)
	ev := requireSingleEvent(t, events, model.EventTypeVaultCredentialDelete)
	assertFailureClass(t, ev, "vault_locked")
	assertNoCredentialLeak(t, events, logBuf)
}

func TestDeleteAgentCredentials_MissingEntryEmitsFailureEvent(t *testing.T) {
	s, store, logBuf := newAuditingAgentCredentialsServer(t, vault.NewMemVault())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vault/agents/claude/credentials", nil)
	s.DeleteAgentCredentials(rec, req, "claude", api.DeleteAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusNotFound)

	events := listVaultCredentialEvents(t, store)
	ev := requireSingleEvent(t, events, model.EventTypeVaultCredentialDelete)
	assertFailureClass(t, ev, "vault_not_found")
	assertNoCredentialLeak(t, events, logBuf)
}

func TestVaultCredentials_NilRecorderDoesNotPanic(t *testing.T) {
	// newAgentCredentialsServer leaves auditRecorder nil. Every verb
	// must still return the correct status and fire the slog line.
	v := vault.NewMemVault()
	s := newAgentCredentialsServer(t, v)
	logBuf := &bytes.Buffer{}
	s.log = slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	putAuditableCredential(t, s, "claude")

	getRec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/claude/credentials", nil)
	s.GetAgentCredentials(getRec, getReq, "claude", api.GetAgentCredentialsParams{})
	assertStatus(t, getRec, http.StatusOK)

	delRec := httptest.NewRecorder()
	delReq := httptest.NewRequest(http.MethodDelete, "/v1/vault/agents/claude/credentials", nil)
	s.DeleteAgentCredentials(delRec, delReq, "claude", api.DeleteAgentCredentialsParams{})
	assertStatus(t, delRec, http.StatusNoContent)

	// The slog line fired even with no recorder, and never leaked.
	if !strings.Contains(logBuf.String(), "vault.credential.read") {
		t.Errorf("expected a read session-log line; got %s", logBuf.String())
	}
	assertNoCredentialLeak(t, nil, logBuf)
}

func TestVaultCredentials_SessionHeaderAttributesActor(t *testing.T) {
	s, store, logBuf := newAuditingAgentCredentialsServer(t, vault.NewMemVault())

	body := api.AgentCredentials{Value: []byte(vaultCredentialSecret)}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/claude/credentials",
		bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Aileron-Session-Id", "sess-123")
	rec := httptest.NewRecorder()
	s.PutAgentCredentials(rec, req, "claude", api.PutAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusNoContent)

	events := listVaultCredentialEvents(t, store)
	ev := requireSingleEvent(t, events, model.EventTypeVaultCredentialWrite)
	if ev.Actor.Type != model.ActorTypeService || ev.Actor.ID != "sess-123" {
		t.Errorf("actor = %+v, want service/sess-123", ev.Actor)
	}
	if ev.Payload["aileron.session.id"] != "sess-123" {
		t.Errorf("session id = %v, want sess-123", ev.Payload["aileron.session.id"])
	}
	assertNoCredentialLeak(t, events, logBuf)
}

func TestVaultCredentials_NoSessionHeaderUsesLoopbackActor(t *testing.T) {
	s, store, _ := newAuditingAgentCredentialsServer(t, vault.NewMemVault())
	putAuditableCredential(t, s, "claude")

	events := listVaultCredentialEvents(t, store)
	ev := requireSingleEvent(t, events, model.EventTypeVaultCredentialWrite)
	if ev.Actor.Type != model.ActorTypeService || ev.Actor.ID != "loopback" {
		t.Errorf("actor = %+v, want service/loopback", ev.Actor)
	}
	if _, ok := ev.Payload["aileron.session.id"]; ok {
		t.Errorf("no session header => payload must omit aileron.session.id: %v", ev.Payload)
	}
}

// requireSingleEvent asserts exactly one event of the given type is
// present and returns it.
func requireSingleEvent(t *testing.T, events []audit.Event, want model.EventType) audit.Event {
	t.Helper()
	var matches []audit.Event
	for _, e := range events {
		if e.EventType == want {
			matches = append(matches, e)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("events of type %q = %d, want 1 (all events: %+v)", want, len(matches), events)
	}
	return matches[0]
}

// putErrVault fails Put with a generic (non-Unavailable) error so the
// PutAgentCredentials vault_put_failed 500 branch — and its failure
// emission — is exercised.
type putErrVault struct{ vault.Vault }

func (putErrVault) Put(context.Context, string, []byte, vault.Metadata) error { return errGeneric }

func TestPutAgentCredentials_PutErrorEmitsFailureEvent(t *testing.T) {
	s, store, logBuf := newAuditingAgentCredentialsServer(t, putErrVault{})
	body := api.AgentCredentials{Value: []byte(vaultCredentialSecret)}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/claude/credentials",
		bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	s.PutAgentCredentials(rec, req, "claude", api.PutAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusInternalServerError)

	events := listVaultCredentialEvents(t, store)
	ev := requireSingleEvent(t, events, model.EventTypeVaultCredentialWrite)
	assertFailureClass(t, ev, "vault_put_failed")
	assertNoCredentialLeak(t, events, logBuf)
}

func assertFailureClass(t *testing.T, e audit.Event, want string) {
	t.Helper()
	if got := e.Payload["aileron.failure.class"]; got != want {
		t.Errorf("aileron.failure.class = %v, want %q", got, want)
	}
}

// X-Aileron-Session-Id is untrusted (the loopback endpoint is reachable
// by any local process). The handler must sanitize it before it reaches
// the audit actor ID, the `aileron.session.id` payload, or the slog
// line: bound the length, restrict to a safe charset, and strip control
// bytes so a forged header cannot inject extra log fields or bloat the
// record. When sanitization empties the value, the actor falls back to
// the loopback service ref.

func putWithSessionHeader(t *testing.T, s *apiServer, name, header string) {
	t.Helper()
	body := api.AgentCredentials{Value: []byte(vaultCredentialSecret)}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/"+name+"/credentials",
		bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Aileron-Session-Id", header)
	rec := httptest.NewRecorder()
	s.PutAgentCredentials(rec, req, name, api.PutAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusNoContent)
}

func TestSanitizeSessionID(t *testing.T) {
	long := strings.Repeat("a", maxSessionIDLen+50)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", "   \t\n", ""},
		{"normal value unchanged", "sess-123", "sess-123"},
		{"allowed punctuation kept", "a-b_c.d", "a-b_c.d"},
		{"trims surrounding space", "  sess-123  ", "sess-123"},
		{"strips CR/LF injection", "sess-123\r\nactor=admin", "sess-123actoradmin"},
		{"strips control bytes", "se\x00ss\x07", "sess"},
		{"strips disallowed punctuation", "a=b c/d", "abcd"},
		{"only-disallowed becomes empty", "=/ \t\n@", ""},
		{"truncates to cap", long, strings.Repeat("a", maxSessionIDLen)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeSessionID(c.in); got != c.want {
				t.Errorf("sanitizeSessionID(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestVaultCredentials_ForgedSessionHeaderIsSanitized(t *testing.T) {
	s, store, logBuf := newAuditingAgentCredentialsServer(t, vault.NewMemVault())
	// A forged header injects a CR/LF plus a fake log field. After
	// sanitization only the allow-listed bytes survive, concatenated.
	putWithSessionHeader(t, s, "claude", "sess-1\r\nactor=admin")

	events := listVaultCredentialEvents(t, store)
	ev := requireSingleEvent(t, events, model.EventTypeVaultCredentialWrite)
	const wantID = "sess-1actoradmin"
	if ev.Actor.ID != wantID {
		t.Errorf("actor ID = %q, want %q", ev.Actor.ID, wantID)
	}
	if ev.Payload["aileron.session.id"] != wantID {
		t.Errorf("session id = %v, want %q", ev.Payload["aileron.session.id"], wantID)
	}
	// No raw CR/LF or a second forged log line reached the buffer/payload.
	if strings.Contains(logBuf.String(), "\nactor=admin") {
		t.Errorf("slog buffer contains an injected log line: %q", logBuf.String())
	}
	raw, err := json.Marshal(ev.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(string(raw), "\r\n") {
		t.Errorf("audit payload contains raw control bytes: %s", string(raw))
	}
	assertNoCredentialLeak(t, events, logBuf)
}

func TestVaultCredentials_OverLengthSessionHeaderTruncated(t *testing.T) {
	s, store, _ := newAuditingAgentCredentialsServer(t, vault.NewMemVault())
	header := strings.Repeat("x", maxSessionIDLen+25)
	putWithSessionHeader(t, s, "claude", header)

	events := listVaultCredentialEvents(t, store)
	ev := requireSingleEvent(t, events, model.EventTypeVaultCredentialWrite)
	want := strings.Repeat("x", maxSessionIDLen)
	if ev.Actor.ID != want {
		t.Errorf("actor ID len = %d, want %d", len(ev.Actor.ID), maxSessionIDLen)
	}
	if ev.Payload["aileron.session.id"] != want {
		t.Errorf("session id len = %d, want %d", len(ev.Payload["aileron.session.id"].(string)), maxSessionIDLen)
	}
}

func TestVaultCredentials_AllDisallowedSessionHeaderFallsBackToLoopback(t *testing.T) {
	s, store, _ := newAuditingAgentCredentialsServer(t, vault.NewMemVault())
	// Header of only-disallowed chars sanitizes to empty -> loopback.
	putWithSessionHeader(t, s, "claude", "=/@ \t")

	events := listVaultCredentialEvents(t, store)
	ev := requireSingleEvent(t, events, model.EventTypeVaultCredentialWrite)
	if ev.Actor.Type != model.ActorTypeService || ev.Actor.ID != "loopback" {
		t.Errorf("actor = %+v, want service/loopback", ev.Actor)
	}
	if _, ok := ev.Payload["aileron.session.id"]; ok {
		t.Errorf("sanitized-empty header must omit aileron.session.id: %v", ev.Payload)
	}
}

// DELETE existence-check guard (acceptance item 2): the secret loaded by
// the existence Get is discarded and must never surface in the audit
// payload or session log, even though it is decrypted in memory.

func TestDeleteAgentCredentials_LoadedSecretNeverLogged(t *testing.T) {
	s, store, logBuf := newAuditingAgentCredentialsServer(t, vault.NewMemVault())
	putAuditableCredential(t, s, "claude")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vault/agents/claude/credentials", nil)
	s.DeleteAgentCredentials(rec, req, "claude", api.DeleteAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusNoContent)

	events := listVaultCredentialEvents(t, store)
	requireSingleEvent(t, events, model.EventTypeVaultCredentialDelete)
	// The existence Get decrypted the stored secret; assert that none of
	// its bytes reached either surface even though it was loaded.
	assertNoCredentialLeak(t, events, logBuf)
}

// DELETE 500-branch auditing (acceptance items 1 & 3): both 500 branches
// emit a failure event with the right class and never leak credential
// bytes to the payload or slog.

func TestDeleteAgentCredentials_GetErrorEmitsFailureEvent(t *testing.T) {
	s, store, logBuf := newAuditingAgentCredentialsServer(t, getErrVault{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vault/agents/claude/credentials", nil)
	s.DeleteAgentCredentials(rec, req, "claude", api.DeleteAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusInternalServerError)

	events := listVaultCredentialEvents(t, store)
	ev := requireSingleEvent(t, events, model.EventTypeVaultCredentialDelete)
	assertFailureClass(t, ev, "vault_get_failed")
	assertNoCredentialLeak(t, events, logBuf)
}

// GET 500-branch auditing: a generic vault Get error on the read path emits
// a vault_get_failed failure event and never leaks credential bytes to the
// audit payload or the session log.

func TestGetAgentCredentials_GetErrorEmitsFailureEvent(t *testing.T) {
	s, store, logBuf := newAuditingAgentCredentialsServer(t, getErrVault{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/claude/credentials", nil)
	s.GetAgentCredentials(rec, req, "claude", api.GetAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusInternalServerError)

	events := listVaultCredentialEvents(t, store)
	ev := requireSingleEvent(t, events, model.EventTypeVaultCredentialRead)
	assertFailureClass(t, ev, "vault_get_failed")
	assertNoCredentialLeak(t, events, logBuf)
}

func TestDeleteAgentCredentials_DeleteErrorEmitsFailureEvent(t *testing.T) {
	// deleteErrVault.Get returns a secret value before Delete fails, so
	// this also reinforces the Unit 2 guard: the loaded secret must not
	// leak on the vault_delete_failed path.
	s, store, logBuf := newAuditingAgentCredentialsServer(t, deleteErrVault{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vault/agents/claude/credentials", nil)
	s.DeleteAgentCredentials(rec, req, "claude", api.DeleteAgentCredentialsParams{})
	assertStatus(t, rec, http.StatusInternalServerError)

	events := listVaultCredentialEvents(t, store)
	ev := requireSingleEvent(t, events, model.EventTypeVaultCredentialDelete)
	assertFailureClass(t, ev, "vault_delete_failed")
	assertNoCredentialLeak(t, events, logBuf)
}

// purposeParams is a tiny helper building a *string-backed purpose for
// the generated *Params structs used by the per-agent credential verbs.
func purposeParam(p string) *string { return &p }

// TestAgentCredentials_PurposeRoundTrip verifies a non-oauth purpose
// round-trips: a PUT with purpose=apikey is readable by a GET with the
// same purpose, returning the written bytes (acceptance: GET/PUT
// round-trip for a non-oauth purpose).
func TestAgentCredentials_PurposeRoundTrip(t *testing.T) {
	v := vault.NewMemVault()
	s := newAgentCredentialsServer(t, v)

	body := api.AgentCredentials{
		Value:    []byte(`{"apiKey":"sk-test"}`),
		Metadata: &api.AgentCredentialsMetadata{Type: strPtr("api_key")},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	putReq := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/claude/credentials",
		bytes.NewReader(raw))
	putReq.Header.Set("Content-Type", "application/json")
	putRec := httptest.NewRecorder()
	s.PutAgentCredentials(putRec, putReq, "claude",
		api.PutAgentCredentialsParams{Purpose: purposeParam("apikey")})
	assertStatus(t, putRec, http.StatusNoContent)

	getReq := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/claude/credentials", nil)
	getRec := httptest.NewRecorder()
	s.GetAgentCredentials(getRec, getReq, "claude",
		api.GetAgentCredentialsParams{Purpose: purposeParam("apikey")})
	assertStatus(t, getRec, http.StatusOK)

	var got api.AgentCredentials
	mustDecode(t, getRec.Body, &got)
	if string(got.Value) != string(body.Value) {
		t.Errorf("Value = %q, want %q", got.Value, body.Value)
	}
}

// TestAgentCredentials_OmittedPurposeRoutesToOauth verifies an absent
// purpose routes identically to the pre-purpose oauth behavior: a PUT
// with no purpose is readable at agents/<name>/oauth (acceptance:
// omitted purpose routes identically).
func TestAgentCredentials_OmittedPurposeRoutesToOauth(t *testing.T) {
	v := vault.NewMemVault()
	s := newAgentCredentialsServer(t, v)

	body := []byte(`{"value":"YWJj"}`) // base64("abc")
	putReq := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/claude/credentials",
		bytes.NewReader(body))
	putReq.Header.Set("Content-Type", "application/json")
	putRec := httptest.NewRecorder()
	// Omitted purpose: nil Purpose pointer.
	s.PutAgentCredentials(putRec, putReq, "claude", api.PutAgentCredentialsParams{})
	assertStatus(t, putRec, http.StatusNoContent)

	// The bytes must be readable at the canonical oauth path directly,
	// proving the default maps to agents/claude/oauth.
	secret, err := v.Get(context.Background(), "agents/claude/oauth")
	if err != nil {
		t.Fatalf("expected entry at agents/claude/oauth: %v", err)
	}
	if string(secret.Value) != "abc" {
		t.Errorf("stored value = %q, want abc", secret.Value)
	}

	// And a GET with omitted purpose returns it.
	getReq := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/claude/credentials", nil)
	getRec := httptest.NewRecorder()
	s.GetAgentCredentials(getRec, getReq, "claude", api.GetAgentCredentialsParams{})
	assertStatus(t, getRec, http.StatusOK)
}

// TestAgentCredentials_TwoPurposesIndependent verifies oauth and apikey
// for the same agent are stored independently with no collision: each
// GET returns the value written for that purpose (acceptance: two
// purposes for one agent independently readable).
func TestAgentCredentials_TwoPurposesIndependent(t *testing.T) {
	v := vault.NewMemVault()
	s := newAgentCredentialsServer(t, v)

	put := func(purpose string, value []byte) {
		t.Helper()
		raw, _ := json.Marshal(api.AgentCredentials{Value: value})
		req := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/claude/credentials",
			bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.PutAgentCredentials(rec, req, "claude",
			api.PutAgentCredentialsParams{Purpose: purposeParam(purpose)})
		assertStatus(t, rec, http.StatusNoContent)
	}
	get := func(purpose string) []byte {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/claude/credentials", nil)
		rec := httptest.NewRecorder()
		s.GetAgentCredentials(rec, req, "claude",
			api.GetAgentCredentialsParams{Purpose: purposeParam(purpose)})
		assertStatus(t, rec, http.StatusOK)
		var got api.AgentCredentials
		mustDecode(t, rec.Body, &got)
		return got.Value
	}

	oauthVal := []byte(`{"oauth":"o"}`)
	apikeyVal := []byte(`{"apikey":"a"}`)
	put("oauth", oauthVal)
	put("apikey", apikeyVal)

	if got := get("oauth"); string(got) != string(oauthVal) {
		t.Errorf("oauth value = %q, want %q", got, oauthVal)
	}
	if got := get("apikey"); string(got) != string(apikeyVal) {
		t.Errorf("apikey value = %q, want %q", got, apikeyVal)
	}
}

// TestAgentCredentials_MalformedPurposeReturns400 verifies a purpose
// that escapes the agents/<name>/ namespace is rejected with
// 400 invalid_request, mirroring the {service} validation on the user
// endpoint (acceptance: malformed purpose rejected with 400).
func TestAgentCredentials_MalformedPurposeReturns400(t *testing.T) {
	bad := []string{"../x", "a/b", "OAUTH", "-leading", "with space", ""}
	for _, p := range bad {
		// Empty string defaults to oauth (valid), so only assert 400 for
		// genuinely malformed non-empty values.
		if p == "" {
			continue
		}
		t.Run(p, func(t *testing.T) {
			s := newAgentCredentialsServer(t, vault.NewMemVault())

			getReq := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/claude/credentials", nil)
			getRec := httptest.NewRecorder()
			s.GetAgentCredentials(getRec, getReq, "claude",
				api.GetAgentCredentialsParams{Purpose: purposeParam(p)})
			assertStatus(t, getRec, http.StatusBadRequest)
			assertErrorCode(t, getRec, "invalid_request")

			body := []byte(`{"value":"YWJj"}`)
			putReq := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/claude/credentials",
				bytes.NewReader(body))
			putReq.Header.Set("Content-Type", "application/json")
			putRec := httptest.NewRecorder()
			s.PutAgentCredentials(putRec, putReq, "claude",
				api.PutAgentCredentialsParams{Purpose: purposeParam(p)})
			assertStatus(t, putRec, http.StatusBadRequest)
			assertErrorCode(t, putRec, "invalid_request")

			delReq := httptest.NewRequest(http.MethodDelete, "/v1/vault/agents/claude/credentials", nil)
			delRec := httptest.NewRecorder()
			s.DeleteAgentCredentials(delRec, delReq, "claude",
				api.DeleteAgentCredentialsParams{Purpose: purposeParam(p)})
			assertStatus(t, delRec, http.StatusBadRequest)
			assertErrorCode(t, delRec, "invalid_request")
		})
	}
}

// TestAgentCredentials_PurposeMissingEntryNotFound verifies the
// not-found contract holds for a non-oauth purpose: a GET on an unseeded
// purpose returns 404 vault_not_found even when the oauth purpose is
// populated (no cross-purpose leakage).
func TestAgentCredentials_PurposeMissingEntryNotFound(t *testing.T) {
	v := vault.NewMemVault()
	s := newAgentCredentialsServer(t, v)

	// Seed only oauth.
	raw, _ := json.Marshal(api.AgentCredentials{Value: []byte("oauth-bytes")})
	putReq := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/claude/credentials",
		bytes.NewReader(raw))
	putReq.Header.Set("Content-Type", "application/json")
	putRec := httptest.NewRecorder()
	s.PutAgentCredentials(putRec, putReq, "claude", api.PutAgentCredentialsParams{})
	assertStatus(t, putRec, http.StatusNoContent)

	// GET apikey must still be 404.
	getReq := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/claude/credentials", nil)
	getRec := httptest.NewRecorder()
	s.GetAgentCredentials(getRec, getReq, "claude",
		api.GetAgentCredentialsParams{Purpose: purposeParam("apikey")})
	assertStatus(t, getRec, http.StatusNotFound)
	assertErrorCode(t, getRec, "vault_not_found")
}

// TestAgentCredentials_PurposeLockedVault verifies the locked-vault
// contract holds for a non-oauth purpose on every verb.
func TestAgentCredentials_PurposeLockedVault(t *testing.T) {
	lv := vault.NewLockableVault()
	s := newAgentCredentialsServer(t, lv)

	getReq := httptest.NewRequest(http.MethodGet, "/v1/vault/agents/claude/credentials", nil)
	getRec := httptest.NewRecorder()
	s.GetAgentCredentials(getRec, getReq, "claude",
		api.GetAgentCredentialsParams{Purpose: purposeParam("apikey")})
	assertStatus(t, getRec, http.StatusLocked)
	assertErrorCode(t, getRec, "vault_locked")

	body := []byte(`{"value":"YWJj"}`)
	putReq := httptest.NewRequest(http.MethodPut, "/v1/vault/agents/claude/credentials",
		bytes.NewReader(body))
	putReq.Header.Set("Content-Type", "application/json")
	putRec := httptest.NewRecorder()
	s.PutAgentCredentials(putRec, putReq, "claude",
		api.PutAgentCredentialsParams{Purpose: purposeParam("apikey")})
	assertStatus(t, putRec, http.StatusLocked)
	assertErrorCode(t, putRec, "vault_locked")

	delReq := httptest.NewRequest(http.MethodDelete, "/v1/vault/agents/claude/credentials", nil)
	delRec := httptest.NewRecorder()
	s.DeleteAgentCredentials(delRec, delReq, "claude",
		api.DeleteAgentCredentialsParams{Purpose: purposeParam("apikey")})
	assertStatus(t, delRec, http.StatusLocked)
	assertErrorCode(t, delRec, "vault_locked")
}
