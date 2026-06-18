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
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/vault"
)

// /v1/vault/user/{service}/credentials contract (#1147):
//
//   - PUT then GET round-trips the bytes and metadata at `user/<service>`.
//   - GET on an empty entry returns 404 with body code `vault_not_found`.
//   - GET/PUT/DELETE on a locked vault return 423 `vault_locked`.
//   - PUT with an empty value returns 400 `invalid_request`.
//   - No vault wired returns 503 `no_local_vault` for each verb.
//   - DELETE on an existing entry returns 204; on a miss returns 404
//     (proves the Get-before-Delete existence check, not a bare
//     idempotent Delete).
//   - An invalid or path-traversal `{service}` is rejected 400 before the
//     store is touched, and never reads/overwrites/deletes an unrelated
//     `agents/<name>/oauth` entry.
//   - Success Get/Put/Delete emit EventTypeVaultUserCredential* carrying
//     `aileron.vault.user.service` and never `aileron.vault.agent` nor the
//     credential bytes.

func newUserCredentialsServer(t *testing.T, v vault.Vault) *apiServer {
	t.Helper()
	return &apiServer{
		log:   slog.Default(),
		vault: v,
	}
}

func putUserCredential(t *testing.T, s *apiServer, service string, body api.AgentCredentials) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal put: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/v1/vault/user/"+service+"/credentials",
		bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.PutUserCredentials(rec, req, service)
	return rec
}

func TestPutAndGetUserCredentials_RoundTrip(t *testing.T) {
	v := vault.NewMemVault()
	s := newUserCredentialsServer(t, v)

	putBody := api.AgentCredentials{
		Value: []byte(`{"token":"gho_abc"}`),
		Metadata: &api.AgentCredentialsMetadata{
			Type:        strPtr("oauth_access_token"),
			Environment: strPtr("prod"),
			Labels:      &map[string]string{"scope": "repo"},
		},
	}
	putRec := putUserCredential(t, s, "github", putBody)
	assertStatus(t, putRec, http.StatusNoContent)

	getReq := httptest.NewRequest(http.MethodGet, "/v1/vault/user/github/credentials", nil)
	getRec := httptest.NewRecorder()
	s.GetUserCredentials(getRec, getReq, "github")
	assertStatus(t, getRec, http.StatusOK)

	var got api.AgentCredentials
	mustDecode(t, getRec.Body, &got)
	if string(got.Value) != string(putBody.Value) {
		t.Errorf("Value = %q, want %q", got.Value, putBody.Value)
	}
	if got.Metadata == nil {
		t.Fatal("Metadata is nil; round-trip lost the optional block")
	}
	if got.Metadata.Type == nil || *got.Metadata.Type != "oauth_access_token" {
		t.Errorf("Metadata.Type = %v, want oauth_access_token", got.Metadata.Type)
	}
	if got.Metadata.Environment == nil || *got.Metadata.Environment != "prod" {
		t.Errorf("Metadata.Environment = %v, want prod", got.Metadata.Environment)
	}
	if got.Metadata.Labels == nil || (*got.Metadata.Labels)["scope"] != "repo" {
		t.Errorf("Metadata.Labels = %v, want scope=repo", got.Metadata.Labels)
	}
}

func TestPutUserCredentials_StoresAtUserServicePath(t *testing.T) {
	v := vault.NewMemVault()
	s := newUserCredentialsServer(t, v)

	rec := putUserCredential(t, s, "github", api.AgentCredentials{Value: []byte("x")})
	assertStatus(t, rec, http.StatusNoContent)

	// The vault path is exactly `user/<service>` with no purpose suffix.
	if _, err := v.Get(context.Background(), "user/github"); err != nil {
		t.Fatalf("expected entry at user/github: %v", err)
	}
}

func TestGetUserCredentials_MissingEntryReturnsNotFound(t *testing.T) {
	s := newUserCredentialsServer(t, vault.NewMemVault())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/user/github/credentials", nil)
	s.GetUserCredentials(rec, req, "github")
	assertStatus(t, rec, http.StatusNotFound)
	assertErrorCode(t, rec, "vault_not_found")
}

func TestGetUserCredentials_LockedVaultReturns423(t *testing.T) {
	lv := vault.NewLockableVault()
	s := newUserCredentialsServer(t, lv)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/user/github/credentials", nil)
	s.GetUserCredentials(rec, req, "github")
	assertStatus(t, rec, http.StatusLocked)
	assertErrorCode(t, rec, "vault_locked")
}

func TestPutUserCredentials_LockedVaultReturns423(t *testing.T) {
	lv := vault.NewLockableVault()
	s := newUserCredentialsServer(t, lv)
	rec := putUserCredential(t, s, "github", api.AgentCredentials{Value: []byte("x")})
	assertStatus(t, rec, http.StatusLocked)
	assertErrorCode(t, rec, "vault_locked")
}

func TestDeleteUserCredentials_LockedVaultReturns423(t *testing.T) {
	lv := vault.NewLockableVault()
	s := newUserCredentialsServer(t, lv)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vault/user/github/credentials", nil)
	s.DeleteUserCredentials(rec, req, "github")
	assertStatus(t, rec, http.StatusLocked)
	assertErrorCode(t, rec, "vault_locked")
}

func TestPutUserCredentials_EmptyValueReturns400(t *testing.T) {
	s := newUserCredentialsServer(t, vault.NewMemVault())
	rec := putUserCredential(t, s, "github", api.AgentCredentials{Value: []byte{}})
	assertStatus(t, rec, http.StatusBadRequest)
	assertErrorCode(t, rec, "invalid_request")
}

func TestPutUserCredentials_MalformedJSONReturns400(t *testing.T) {
	s := newUserCredentialsServer(t, vault.NewMemVault())
	req := httptest.NewRequest(http.MethodPut, "/v1/vault/user/github/credentials",
		bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.PutUserCredentials(rec, req, "github")
	assertStatus(t, rec, http.StatusBadRequest)
	assertErrorCode(t, rec, "invalid_request")
}

func TestUserCredentials_NoVaultReturnsServiceUnavailable(t *testing.T) {
	s := newUserCredentialsServer(t, nil)

	getReq := httptest.NewRequest(http.MethodGet, "/v1/vault/user/github/credentials", nil)
	getRec := httptest.NewRecorder()
	s.GetUserCredentials(getRec, getReq, "github")
	assertStatus(t, getRec, http.StatusServiceUnavailable)
	assertErrorCode(t, getRec, "no_local_vault")

	putRec := putUserCredential(t, s, "github", api.AgentCredentials{Value: []byte("x")})
	assertStatus(t, putRec, http.StatusServiceUnavailable)
	assertErrorCode(t, putRec, "no_local_vault")

	delReq := httptest.NewRequest(http.MethodDelete, "/v1/vault/user/github/credentials", nil)
	delRec := httptest.NewRecorder()
	s.DeleteUserCredentials(delRec, delReq, "github")
	assertStatus(t, delRec, http.StatusServiceUnavailable)
	assertErrorCode(t, delRec, "no_local_vault")
}

func TestDeleteUserCredentials_RoundTrip(t *testing.T) {
	v := vault.NewMemVault()
	s := newUserCredentialsServer(t, v)

	putRec := putUserCredential(t, s, "github", api.AgentCredentials{Value: []byte("x")})
	assertStatus(t, putRec, http.StatusNoContent)

	delReq := httptest.NewRequest(http.MethodDelete, "/v1/vault/user/github/credentials", nil)
	delRec := httptest.NewRecorder()
	s.DeleteUserCredentials(delRec, delReq, "github")
	assertStatus(t, delRec, http.StatusNoContent)

	// The entry is gone.
	getReq := httptest.NewRequest(http.MethodGet, "/v1/vault/user/github/credentials", nil)
	getRec := httptest.NewRecorder()
	s.GetUserCredentials(getRec, getReq, "github")
	assertStatus(t, getRec, http.StatusNotFound)
}

func TestDeleteUserCredentials_MissingEntryReturnsNotFound(t *testing.T) {
	// Proves the Get-before-Delete existence check: a bare idempotent
	// Delete would 204 on a miss and fail the contract.
	s := newUserCredentialsServer(t, vault.NewMemVault())
	delReq := httptest.NewRequest(http.MethodDelete, "/v1/vault/user/github/credentials", nil)
	delRec := httptest.NewRecorder()
	s.DeleteUserCredentials(delRec, delReq, "github")
	assertStatus(t, delRec, http.StatusNotFound)
	assertErrorCode(t, delRec, "vault_not_found")
}

func TestValidateUserService(t *testing.T) {
	cases := []struct {
		service string
		want    bool
	}{
		{"github", true},
		{"gitlab", true},
		{"g", true},
		{"git-hub", true},
		{"git_hub", true},
		{"github2", true},
		{"2github", true},
		{"", false},
		{"GitHub", false},        // uppercase
		{"-github", false},       // leading dash
		{"_github", false},       // leading underscore
		{"git hub", false},       // space
		{"git/hub", false},       // slash
		{"..", false},            // dot-dot
		{"../agents", false},     // traversal
		{"agents/claude/oauth", false},
		{"git.hub", false}, // dot not allowed
	}
	for _, c := range cases {
		if got := validateUserService(c.service); got != c.want {
			t.Errorf("validateUserService(%q) = %v, want %v", c.service, got, c.want)
		}
	}
}

func TestUserCredentialVaultPath(t *testing.T) {
	if got := userCredentialVaultPath("github"); got != "user/github" {
		t.Errorf("userCredentialVaultPath(github) = %q, want user/github", got)
	}
}

func TestUserCredentials_InvalidServiceRejected(t *testing.T) {
	s := newUserCredentialsServer(t, vault.NewMemVault())
	for _, service := range []string{"GitHub", "-github", ""} {
		getReq := httptest.NewRequest(http.MethodGet, "/v1/vault/user/x/credentials", nil)
		getRec := httptest.NewRecorder()
		s.GetUserCredentials(getRec, getReq, service)
		assertStatus(t, getRec, http.StatusBadRequest)
		assertErrorCode(t, getRec, "invalid_request")

		putRec := putUserCredential(t, s, service, api.AgentCredentials{Value: []byte("x")})
		assertStatus(t, putRec, http.StatusBadRequest)
		assertErrorCode(t, putRec, "invalid_request")

		delReq := httptest.NewRequest(http.MethodDelete, "/v1/vault/user/x/credentials", nil)
		delRec := httptest.NewRecorder()
		s.DeleteUserCredentials(delRec, delReq, service)
		assertStatus(t, delRec, http.StatusBadRequest)
		assertErrorCode(t, delRec, "invalid_request")
	}
}

// TestUserCredentials_PathTraversalBlocked is the P0 regression: a crafted
// `{service}` must be rejected 400 before any vault path is constructed,
// and must never read, overwrite, or delete a pre-seeded
// `agents/claude/oauth` entry.
func TestUserCredentials_PathTraversalBlocked(t *testing.T) {
	const agentPath = "agents/claude/oauth"
	const agentSecret = "AGENT-SECRET-DO-NOT-TOUCH"

	// Direct-handler traversal attempts (the validation gate, exercised
	// with values that could escape the `user/` namespace if they reached
	// the path builder).
	traversals := []string{
		"../agents/claude/oauth",
		"..%2Fagents%2Fclaude%2Foauth",
		"agents/claude/oauth",
		"..",
	}
	for _, service := range traversals {
		v := vault.NewMemVault()
		if err := v.Put(context.Background(), agentPath, []byte(agentSecret), vault.Metadata{}); err != nil {
			t.Fatalf("seed agent entry: %v", err)
		}
		s := newUserCredentialsServer(t, v)

		// GET must reject without reading the agent entry.
		getReq := httptest.NewRequest(http.MethodGet, "/v1/vault/user/x/credentials", nil)
		getRec := httptest.NewRecorder()
		s.GetUserCredentials(getRec, getReq, service)
		assertStatus(t, getRec, http.StatusBadRequest)
		assertErrorCode(t, getRec, "invalid_request")

		// PUT must reject without overwriting the agent entry.
		putRec := putUserCredential(t, s, service, api.AgentCredentials{Value: []byte("OVERWRITE")})
		assertStatus(t, putRec, http.StatusBadRequest)
		assertErrorCode(t, putRec, "invalid_request")

		// DELETE must reject without deleting the agent entry.
		delReq := httptest.NewRequest(http.MethodDelete, "/v1/vault/user/x/credentials", nil)
		delRec := httptest.NewRecorder()
		s.DeleteUserCredentials(delRec, delReq, service)
		assertStatus(t, delRec, http.StatusBadRequest)
		assertErrorCode(t, delRec, "invalid_request")

		// The pre-seeded agent entry is intact and unread.
		secret, err := v.Get(context.Background(), agentPath)
		if err != nil {
			t.Fatalf("agent entry vanished for service %q: %v", service, err)
		}
		if string(secret.Value) != agentSecret {
			t.Errorf("agent entry mutated for service %q: %q", service, secret.Value)
		}
	}
}

// TestUserCredentials_RouterRejectsEncodedTraversal drives the request
// through the generated router so the percent-encoding semantics are
// honored exactly as the daemon serves them. A `..%2F...` value must not
// resolve to the per-agent entry through any user verb.
func TestUserCredentials_RouterRejectsEncodedTraversal(t *testing.T) {
	const agentPath = "agents/claude/oauth"
	const agentSecret = "AGENT-SECRET-DO-NOT-TOUCH"

	v := vault.NewMemVault()
	if err := v.Put(context.Background(), agentPath, []byte(agentSecret), vault.Metadata{}); err != nil {
		t.Fatalf("seed agent entry: %v", err)
	}
	s := newUserCredentialsServer(t, v)
	mux := muxFor(s)

	// Encoded-slash traversal targets; whatever the router decodes them to,
	// the agent entry must survive untouched and no user verb may 2xx.
	targets := []string{
		"/v1/vault/user/..%2Fagents%2Fclaude%2Foauth/credentials",
		"/v1/vault/user/%2e%2e%2fagents%2fclaude%2foauth/credentials",
	}
	for _, target := range targets {
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			req := httptest.NewRequest(method, target, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code >= 200 && rec.Code < 300 {
				t.Errorf("%s %s returned 2xx (%d); traversal must not succeed", method, target, rec.Code)
			}
		}
		// PUT attempt that would overwrite if it escaped.
		body, _ := json.Marshal(api.AgentCredentials{Value: []byte("OVERWRITE")})
		req := httptest.NewRequest(http.MethodPut, target, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code >= 200 && rec.Code < 300 {
			t.Errorf("PUT %s returned 2xx (%d); traversal must not succeed", target, rec.Code)
		}
	}

	secret, err := v.Get(context.Background(), agentPath)
	if err != nil {
		t.Fatalf("agent entry vanished: %v", err)
	}
	if string(secret.Value) != agentSecret {
		t.Errorf("agent entry mutated: %q", secret.Value)
	}
}

// --- audit (P1) ---

func newAuditingUserCredentialsServer(t *testing.T, v vault.Vault) (*apiServer, *audit.MemStore, *bytes.Buffer) {
	t.Helper()
	store := audit.NewMemStore()
	logBuf := &bytes.Buffer{}
	s := &apiServer{
		log:           slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		vault:         v,
		auditRecorder: audit.NewRecorder(store, nil, func() string { return "vault-user-cred-audit-id" }),
	}
	return s, store, logBuf
}

func putAuditableUserCredential(t *testing.T, s *apiServer, service string) {
	t.Helper()
	body := api.AgentCredentials{Value: []byte(vaultCredentialSecret)}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/v1/vault/user/"+service+"/credentials",
		bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.PutUserCredentials(rec, req, service)
	assertStatus(t, rec, http.StatusNoContent)
}

func assertUserServiceEvent(t *testing.T, e audit.Event) {
	t.Helper()
	if e.Payload["aileron.vault.user.service"] != "github" {
		t.Errorf("aileron.vault.user.service = %v, want github", e.Payload["aileron.vault.user.service"])
	}
	if _, ok := e.Payload["aileron.vault.agent"]; ok {
		t.Errorf("user event must not carry aileron.vault.agent: %v", e.Payload)
	}
}

func TestPutUserCredentials_SuccessEmitsWriteEventWithoutSecret(t *testing.T) {
	s, store, logBuf := newAuditingUserCredentialsServer(t, vault.NewMemVault())
	putAuditableUserCredential(t, s, "github")

	events := listVaultCredentialEvents(t, store)
	write := requireSingleEvent(t, events, model.EventTypeVaultUserCredentialWrite)
	assertUserServiceEvent(t, write)
	if _, ok := write.Payload["aileron.failure.class"]; ok {
		t.Errorf("success event must not carry aileron.failure.class: %v", write.Payload)
	}
	assertNoCredentialLeak(t, events, logBuf)
}

func TestGetUserCredentials_SuccessEmitsReadEvent(t *testing.T) {
	s, store, logBuf := newAuditingUserCredentialsServer(t, vault.NewMemVault())
	putAuditableUserCredential(t, s, "github")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/user/github/credentials", nil)
	s.GetUserCredentials(rec, req, "github")
	assertStatus(t, rec, http.StatusOK)

	events := listVaultCredentialEvents(t, store)
	read := requireSingleEvent(t, events, model.EventTypeVaultUserCredentialRead)
	assertUserServiceEvent(t, read)
	assertNoCredentialLeak(t, events, logBuf)
}

func TestDeleteUserCredentials_SuccessEmitsDeleteEvent(t *testing.T) {
	s, store, logBuf := newAuditingUserCredentialsServer(t, vault.NewMemVault())
	putAuditableUserCredential(t, s, "github")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vault/user/github/credentials", nil)
	s.DeleteUserCredentials(rec, req, "github")
	assertStatus(t, rec, http.StatusNoContent)

	events := listVaultCredentialEvents(t, store)
	del := requireSingleEvent(t, events, model.EventTypeVaultUserCredentialDelete)
	assertUserServiceEvent(t, del)
	assertNoCredentialLeak(t, events, logBuf)
}

func TestUserCredentials_InvalidServiceEmitsFailureEvent(t *testing.T) {
	s, store, logBuf := newAuditingUserCredentialsServer(t, vault.NewMemVault())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/user/x/credentials", nil)
	s.GetUserCredentials(rec, req, "BAD")
	assertStatus(t, rec, http.StatusBadRequest)

	events := listVaultCredentialEvents(t, store)
	ev := requireSingleEvent(t, events, model.EventTypeVaultUserCredentialRead)
	assertFailureClass(t, ev, "invalid_request")
	assertNoCredentialLeak(t, events, logBuf)
}

// --- 500 branches ---

func TestGetUserCredentials_GetErrorReturns500(t *testing.T) {
	s := newUserCredentialsServer(t, getErrVault{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/vault/user/github/credentials", nil)
	s.GetUserCredentials(rec, req, "github")
	assertStatus(t, rec, http.StatusInternalServerError)
	assertErrorCode(t, rec, "vault_get_failed")
}

func TestPutUserCredentials_PutErrorReturns500(t *testing.T) {
	s := newUserCredentialsServer(t, putErrVault{})
	rec := putUserCredential(t, s, "github", api.AgentCredentials{Value: []byte("x")})
	assertStatus(t, rec, http.StatusInternalServerError)
	assertErrorCode(t, rec, "vault_put_failed")
}

func TestDeleteUserCredentials_GetErrorReturns500(t *testing.T) {
	s := newUserCredentialsServer(t, getErrVault{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vault/user/github/credentials", nil)
	s.DeleteUserCredentials(rec, req, "github")
	assertStatus(t, rec, http.StatusInternalServerError)
	assertErrorCode(t, rec, "vault_get_failed")
}

// deleteLockedVault succeeds on the existence Get but returns
// ErrCredentialUnavailable from Delete, exercising the
// locked-on-delete 423 branch (a vault that locks between the Get and
// the Delete).
type deleteLockedVault struct{ vault.Vault }

func (deleteLockedVault) Get(context.Context, string) (vault.Secret, error) {
	return vault.Secret{Value: []byte("x")}, nil
}
func (deleteLockedVault) Delete(context.Context, string) error {
	return vault.ErrCredentialUnavailable
}

func TestDeleteUserCredentials_DeleteLockedReturns423(t *testing.T) {
	s := newUserCredentialsServer(t, deleteLockedVault{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vault/user/github/credentials", nil)
	s.DeleteUserCredentials(rec, req, "github")
	assertStatus(t, rec, http.StatusLocked)
	assertErrorCode(t, rec, "vault_locked")
}

func TestDeleteUserCredentials_DeleteErrorReturns500(t *testing.T) {
	s := newUserCredentialsServer(t, deleteErrVault{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/vault/user/github/credentials", nil)
	s.DeleteUserCredentials(rec, req, "github")
	assertStatus(t, rec, http.StatusInternalServerError)
	assertErrorCode(t, rec, "vault_delete_failed")
}
