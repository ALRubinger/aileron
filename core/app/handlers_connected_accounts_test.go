package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/account"
	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/auth"
	"github.com/ALRubinger/aileron/core/crypto"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
	"github.com/ALRubinger/aileron/enclave"
	"github.com/ALRubinger/aileron/enclave/local"
)

func newConnectedAccountServer() *apiServer {
	return &apiServer{
		log:               slog.Default(),
		connectedAccounts: mem.NewConnectedAccountStore(),
		vault:             vault.NewMemVault(),
		users:             nil, // auth disabled
		newID:             func() string { return "test-id" },
	}
}

func newConnectedAccountServerWithAuth() *apiServer {
	srv := newConnectedAccountServer()
	srv.users = &stubUserStore{}
	return srv
}

// newConnectedAccountServerWithVault creates a server with auth, passphrase,
// and an unlocked vault — required for tests that hit the direct-mode callback.
// The KEK session is unlocked for userAClaims (usr_a).
func newConnectedAccountServerWithVault() (*apiServer, []byte) {
	srv := newConnectedAccountServerWithAuth()
	srv.userKeyMaterials = mem.NewUserKeyMaterialStore()

	// Set up passphrase for the test user (usr_a).
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	verification, _ := crypto.Encrypt([]byte("aileron-kek-ok"), kek)
	srv.userKeyMaterials.Create(context.Background(), model.UserKeyMaterial{
		UserID:          "usr_a",
		Salt:            make([]byte, 16),
		KEKVerification: verification,
	})

	cache := auth.NewKEKSessionCache(24 * time.Hour)
	cache.Set("usr_a", kek)
	srv.kekSessionCache = cache

	return srv, kek
}

func newGoogleAccountRegistry(accounts store.ConnectedAccountStore, v vault.Vault) *account.Registry {
	r := account.NewRegistry()
	r.Register(account.NewGoogleService("id", "secret", accounts, v))
	return r
}

func seedConnectedAccount(ctx context.Context, s store.ConnectedAccountStore, id, userID string, provider model.ConnectedAccountProvider) {
	s.Create(ctx, model.ConnectedAccount{
		ID:       id,
		UserID:   userID,
		Provider: provider,
		Scopes:   []string{"gmail.readonly"},
		Status:   model.ConnectedAccountStatusActive,
	})
}

func TestListConnectedAccounts_Empty(t *testing.T) {
	srv := newConnectedAccountServer()
	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connected-accounts", "", nil)
	srv.ListConnectedAccounts(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Items []api.ConnectedAccount `json:"items"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Items) != 0 {
		t.Fatalf("expected empty list, got %d", len(resp.Items))
	}
}

func TestListConnectedAccounts_UserScoping(t *testing.T) {
	srv := newConnectedAccountServerWithAuth()
	ctx := context.Background()
	seedConnectedAccount(ctx, srv.connectedAccounts, "conn_1", "usr_a", model.ConnectedAccountProviderGmail)
	seedConnectedAccount(ctx, srv.connectedAccounts, "conn_2", "usr_b", model.ConnectedAccountProviderGmail)

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connected-accounts", "", userAClaims)
	srv.ListConnectedAccounts(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Items []api.ConnectedAccount `json:"items"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Items) != 1 {
		t.Fatalf("expected 1 account for usr_a, got %d", len(resp.Items))
	}
	if *resp.Items[0].Id != "conn_1" {
		t.Fatalf("expected conn_1, got %s", *resp.Items[0].Id)
	}
}

func TestGetConnectedAccount_Success(t *testing.T) {
	srv := newConnectedAccountServerWithAuth()
	ctx := context.Background()
	seedConnectedAccount(ctx, srv.connectedAccounts, "conn_1", "usr_a", model.ConnectedAccountProviderGmail)

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connected-accounts/conn_1", "", userAClaims)
	srv.GetConnectedAccount(w, r, "conn_1")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var acc api.ConnectedAccount
	json.NewDecoder(w.Body).Decode(&acc)
	if *acc.Id != "conn_1" {
		t.Fatalf("expected conn_1, got %s", *acc.Id)
	}
}

func TestGetConnectedAccount_OwnershipCheck(t *testing.T) {
	srv := newConnectedAccountServerWithAuth()
	ctx := context.Background()
	seedConnectedAccount(ctx, srv.connectedAccounts, "conn_1", "usr_a", model.ConnectedAccountProviderGmail)

	// User B tries to access User A's account.
	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connected-accounts/conn_1", "", userBClaims)
	srv.GetConnectedAccount(w, r, "conn_1")

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for ownership violation, got %d", w.Code)
	}
}

func TestGetConnectedAccount_NotFound(t *testing.T) {
	srv := newConnectedAccountServer()
	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connected-accounts/nonexistent", "", nil)
	srv.GetConnectedAccount(w, r, "nonexistent")

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDeleteConnectedAccount_Success(t *testing.T) {
	srv := newConnectedAccountServerWithAuth()
	ctx := context.Background()
	seedConnectedAccount(ctx, srv.connectedAccounts, "conn_1", "usr_a", model.ConnectedAccountProviderGmail)

	w := httptest.NewRecorder()
	r := mcpRequest("DELETE", "/v1/connected-accounts/conn_1", "", userAClaims)
	srv.DeleteConnectedAccount(w, r, "conn_1")

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}

	// Verify deleted.
	w2 := httptest.NewRecorder()
	r2 := mcpRequest("GET", "/v1/connected-accounts/conn_1", "", userAClaims)
	srv.GetConnectedAccount(w2, r2, "conn_1")
	if w2.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", w2.Code)
	}
}

func TestDeleteConnectedAccount_OwnershipCheck(t *testing.T) {
	srv := newConnectedAccountServerWithAuth()
	ctx := context.Background()
	seedConnectedAccount(ctx, srv.connectedAccounts, "conn_1", "usr_a", model.ConnectedAccountProviderGmail)

	w := httptest.NewRecorder()
	r := mcpRequest("DELETE", "/v1/connected-accounts/conn_1", "", userBClaims)
	srv.DeleteConnectedAccount(w, r, "conn_1")

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for ownership violation, got %d", w.Code)
	}
}

func TestDeleteConnectedAccount_NotFound(t *testing.T) {
	srv := newConnectedAccountServer()
	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connected-accounts/nonexistent", "", nil)
	srv.DeleteConnectedAccount(w, r, "nonexistent")

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDeleteConnectedAccount_WithAccountService(t *testing.T) {
	srv := newConnectedAccountServerWithAuth()
	srv.accountService = newGoogleAccountRegistry(srv.connectedAccounts, srv.vault)
	ctx := context.Background()

	seedConnectedAccount(ctx, srv.connectedAccounts, "conn_1", "usr_a", model.ConnectedAccountProviderGmail)
	// Store a token in vault so Disconnect can remove it.
	srv.vault.Put(ctx, "connected-accounts/usr_a/gmail", []byte(`{"token":"test"}`), vault.Metadata{Type: "oauth_refresh_token"})

	w := httptest.NewRecorder()
	r := mcpRequest("DELETE", "/v1/connected-accounts/conn_1", "", userAClaims)
	srv.DeleteConnectedAccount(w, r, "conn_1")

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestConnectAccountCallback_ValidStateButBadCode(t *testing.T) {
	srv, _ := newConnectedAccountServerWithVault()
	srv.accountService = newGoogleAccountRegistry(srv.connectedAccounts, srv.vault)

	state := "test-state-123"
	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connect/gmail/callback?code=invalid&state="+state, "", userAClaims)
	r.AddCookie(&http.Cookie{Name: "aileron_connect_state", Value: state})
	srv.ConnectAccountCallback(w, r, "gmail", api.ConnectAccountCallbackParams{Code: "invalid", State: &state})

	// Should fail at the code exchange step (can't reach Google), returning 500.
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for invalid code exchange, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetConnectedAccount_Unauthorized(t *testing.T) {
	srv := newConnectedAccountServerWithAuth()
	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connected-accounts/conn_1", "", nil)
	srv.GetConnectedAccount(w, r, "conn_1")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestDeleteConnectedAccount_Unauthorized(t *testing.T) {
	srv := newConnectedAccountServerWithAuth()
	w := httptest.NewRecorder()
	r := mcpRequest("DELETE", "/v1/connected-accounts/conn_1", "", nil)
	srv.DeleteConnectedAccount(w, r, "conn_1")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestWriteJSON_MarshalError(t *testing.T) {
	w := httptest.NewRecorder()
	// A channel cannot be JSON-marshaled — triggers the error path.
	writeJSON(w, http.StatusOK, map[string]any{"bad": make(chan int)})

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on marshal failure, got %d", w.Code)
	}
	if w.Body.Len() == 0 {
		t.Fatal("expected error body, got empty")
	}
}

func TestWriteJSON_Success(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]any{"items": []string{}})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.Len() == 0 {
		t.Fatal("expected non-empty body")
	}
}

func TestListConnectedAccounts_SlackAccount(t *testing.T) {
	srv := newConnectedAccountServerWithAuth()
	ctx := context.Background()

	srv.connectedAccounts.Create(ctx, model.ConnectedAccount{
		ID:             "conn_slack",
		UserID:         "usr_a",
		Provider:       model.ConnectedAccountProviderSlack,
		Scopes:         []string{"channels:history"},
		Status:         model.ConnectedAccountStatusActive,
		ExternalUserID: "U123",
		ExternalTeamID: "T001",
	})

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connected-accounts", "", userAClaims)
	srv.ListConnectedAccounts(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.Len() == 0 {
		t.Fatal("expected non-empty response body")
	}

	var resp struct {
		Items []api.ConnectedAccount `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("expected 1 account, got %d", len(resp.Items))
	}
	if *resp.Items[0].Id != "conn_slack" {
		t.Errorf("expected conn_slack, got %s", *resp.Items[0].Id)
	}
	if resp.Items[0].ExternalUserId == nil || *resp.Items[0].ExternalUserId != "U123" {
		t.Errorf("expected ExternalUserId U123, got %v", resp.Items[0].ExternalUserId)
	}
}

func TestConnectAccount_RedirectsToGoogle(t *testing.T) {
	srv := newConnectedAccountServer()
	reg := account.NewRegistry()
	reg.Register(account.NewGoogleService("test-client-id", "secret", srv.connectedAccounts, srv.vault))
	srv.accountService = reg

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connect/gmail", "", nil)
	srv.ConnectAccount(w, r, "gmail", api.ConnectAccountParams{})

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc == "" {
		t.Fatal("expected Location header")
	}
	if !containsStr(loc, "accounts.google.com") {
		t.Errorf("expected google auth URL, got: %s", loc)
	}
	if !containsStr(loc, "gmail.readonly") {
		t.Errorf("expected gmail scope in URL, got: %s", loc)
	}
	// Should set state cookie.
	cookies := w.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == "aileron_connect_state" {
			found = true
		}
	}
	if !found {
		t.Error("expected aileron_connect_state cookie to be set")
	}
}

func TestConnectAccount_UnsupportedProvider(t *testing.T) {
	srv := newConnectedAccountServer()
	srv.accountService = newGoogleAccountRegistry(srv.connectedAccounts, srv.vault)

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connect/outlook", "", nil)
	srv.ConnectAccount(w, r, "outlook", api.ConnectAccountParams{})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsupported provider, got %d", w.Code)
	}
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestConnectAccount_NotConfigured(t *testing.T) {
	srv := newConnectedAccountServer()
	srv.accountService = nil

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connect/gmail", "", nil)
	srv.ConnectAccount(w, r, "gmail", api.ConnectAccountParams{})

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestConnectAccount_RequiresPassphrase(t *testing.T) {
	srv := newConnectedAccountServerWithAuth()
	srv.kekSessionCache = auth.NewKEKSessionCache(24 * time.Hour)
	srv.userKeyMaterials = mem.NewUserKeyMaterialStore()
	srv.accountService = newGoogleAccountRegistry(srv.connectedAccounts, srv.vault)

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connect/gmail", "", userAClaims)
	srv.ConnectAccount(w, r, "gmail", api.ConnectAccountParams{})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 passphrase_required, got %d: %s", w.Code, w.Body.String())
	}
}

func TestConnectAccount_VaultLocked(t *testing.T) {
	srv, _ := newConnectedAccountServerWithVault()
	srv.accountService = newGoogleAccountRegistry(srv.connectedAccounts, srv.vault)
	srv.kekSessionCache.Clear("usr_a") // lock the vault

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connect/gmail", "", userAClaims)
	srv.ConnectAccount(w, r, "gmail", api.ConnectAccountParams{})

	if w.Code != 423 {
		t.Fatalf("expected 423 vault_locked, got %d: %s", w.Code, w.Body.String())
	}
}

func TestConnectAccount_TEEMode_SkipsKEKCache(t *testing.T) {
	// In TEE mode, ConnectAccount should not require the KEK in the
	// server-side session cache — the enclave holds the KEK.
	srv, _ := newConnectedAccountServerWithVault()
	srv.accountService = newGoogleAccountRegistry(srv.connectedAccounts, srv.vault)
	srv.kekSessionCache.Clear("usr_a") // vault "locked" in server cache

	// Enable TEE mode.
	executeFn := func(_ context.Context, _ enclave.ExecuteRequest, _ []byte) (enclave.ExecuteResponse, error) {
		return enclave.ExecuteResponse{Status: "succeeded"}, nil
	}
	srv.enclaveClient = local.New(executeFn)
	srv.teeState = newTeeState()

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connect/gmail", "", userAClaims)
	srv.ConnectAccount(w, r, "gmail", api.ConnectAccountParams{})

	// Should redirect to Google OAuth, not return 423 vault_locked.
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d: %s", w.Code, w.Body.String())
	}
}

func TestConnectAccountCallback_NotConfigured(t *testing.T) {
	srv := newConnectedAccountServer()
	srv.accountService = nil

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connect/gmail/callback?code=x&state=y", "", nil)
	yState := "y"
	srv.ConnectAccountCallback(w, r, "gmail", api.ConnectAccountCallbackParams{Code: "x", State: &yState})

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestConnectAccountCallback_StateMismatch(t *testing.T) {
	srv := newConnectedAccountServer()
	srv.accountService = newGoogleAccountRegistry(srv.connectedAccounts, srv.vault)

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connect/gmail/callback?code=x&state=bad", "", nil)
	// No cookie set, so state will mismatch.
	badState := "bad"
	srv.ConnectAccountCallback(w, r, "gmail", api.ConnectAccountCallbackParams{Code: "x", State: &badState})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for state mismatch, got %d", w.Code)
	}
}

func TestConnectAccountCallback_RedirectsToReturnTo(t *testing.T) {
	// Regression test: after a successful OAuth callback the server must redirect
	// to the return_to URL set during ConnectAccount, not to a relative path
	// on the API host.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"authed_user": map[string]any{
				"id":           "U123ABC",
				"access_token": "xoxp-user-token-123",
				"scope":        "channels:history",
				"token_type":   "user",
			},
			"team": map[string]any{
				"id":   "T001TEAM",
				"name": "Test Workspace",
			},
		})
	}))
	defer tokenServer.Close()

	srv, _ := newConnectedAccountServerWithVault()
	slackSvc := account.NewSlackService("id", "secret", srv.connectedAccounts, srv.vault).
		WithEndpoints("https://slack.com/oauth/v2/authorize", tokenServer.URL)
	reg := account.NewRegistry()
	reg.Register(slackSvc)
	srv.accountService = reg

	state := "test-state-123"
	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connect/slack/callback?code=valid&state="+state, "", userAClaims)
	r.AddCookie(&http.Cookie{Name: "aileron_connect_state", Value: state})
	r.AddCookie(&http.Cookie{Name: "aileron_connect_return", Value: "https://app.withaileron.ai/settings/connected-accounts"})
	srv.ConnectAccountCallback(w, r, "slack", api.ConnectAccountCallbackParams{Code: "valid", State: &state})

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	want := "https://app.withaileron.ai/settings/connected-accounts"
	if loc != want {
		t.Errorf("Location = %q, want %q", loc, want)
	}
}

func TestConnectAccountCallback_DefaultRedirectWhenNoReturnTo(t *testing.T) {
	// When no return_to cookie is set, the redirect should use the default path.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"authed_user": map[string]any{
				"id":           "U123ABC",
				"access_token": "xoxp-user-token-123",
				"scope":        "channels:history",
				"token_type":   "user",
			},
			"team": map[string]any{
				"id":   "T001TEAM",
				"name": "Test Workspace",
			},
		})
	}))
	defer tokenServer.Close()

	srv, _ := newConnectedAccountServerWithVault()
	slackSvc := account.NewSlackService("id", "secret", srv.connectedAccounts, srv.vault).
		WithEndpoints("https://slack.com/oauth/v2/authorize", tokenServer.URL)
	reg := account.NewRegistry()
	reg.Register(slackSvc)
	srv.accountService = reg

	state := "test-state-456"
	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connect/slack/callback?code=valid&state="+state, "", userAClaims)
	r.AddCookie(&http.Cookie{Name: "aileron_connect_state", Value: state})
	// No aileron_connect_return cookie — should use default.
	srv.ConnectAccountCallback(w, r, "slack", api.ConnectAccountCallbackParams{Code: "valid", State: &state})

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	want := "/settings/connected-accounts"
	if loc != want {
		t.Errorf("Location = %q, want %q", loc, want)
	}
}

func TestConnectAccount_SetsReturnToCookie(t *testing.T) {
	srv := newConnectedAccountServer()
	reg := account.NewRegistry()
	reg.Register(account.NewGoogleService("test-client-id", "secret", srv.connectedAccounts, srv.vault))
	srv.accountService = reg

	returnTo := "https://app.withaileron.ai/settings/connected-accounts"
	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connect/gmail?return_to="+returnTo, "", nil)
	srv.ConnectAccount(w, r, "gmail", api.ConnectAccountParams{ReturnTo: &returnTo})

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	var found bool
	for _, c := range w.Result().Cookies() {
		if c.Name == "aileron_connect_return" {
			found = true
			if c.Value != returnTo {
				t.Errorf("aileron_connect_return = %q, want %q", c.Value, returnTo)
			}
		}
	}
	if !found {
		t.Error("expected aileron_connect_return cookie to be set")
	}
}

func TestConnectAccount_DefaultReturnToCookie(t *testing.T) {
	srv := newConnectedAccountServer()
	reg := account.NewRegistry()
	reg.Register(account.NewGoogleService("test-client-id", "secret", srv.connectedAccounts, srv.vault))
	srv.accountService = reg

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connect/gmail", "", nil)
	srv.ConnectAccount(w, r, "gmail", api.ConnectAccountParams{})

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "aileron_connect_return" {
			if c.Value != "/settings/connected-accounts" {
				t.Errorf("aileron_connect_return = %q, want default", c.Value)
			}
			return
		}
	}
	t.Error("expected aileron_connect_return cookie to be set")
}

func TestListConnectedAccounts_Unauthorized(t *testing.T) {
	srv := newConnectedAccountServerWithAuth()
	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connected-accounts", "", nil) // no claims
	srv.ListConnectedAccounts(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestRequestScheme_XForwardedProto(t *testing.T) {
	r := httptest.NewRequest("GET", "/test", nil)
	r.Header.Set("X-Forwarded-Proto", "https")

	if got := requestScheme(r); got != "https" {
		t.Errorf("expected https from X-Forwarded-Proto, got %s", got)
	}
}

func TestRequestScheme_XForwardedProtoHTTP(t *testing.T) {
	r := httptest.NewRequest("GET", "/test", nil)
	r.Header.Set("X-Forwarded-Proto", "http")

	if got := requestScheme(r); got != "http" {
		t.Errorf("expected http from X-Forwarded-Proto, got %s", got)
	}
}

func TestRequestScheme_TLSDirect(t *testing.T) {
	r := httptest.NewRequest("GET", "/test", nil)
	r.TLS = &tls.ConnectionState{} // simulate direct TLS connection
	if got := requestScheme(r); got != "https" {
		t.Errorf("expected https from TLS, got %s", got)
	}
}

func TestRequestScheme_NoHeader_NoTLS(t *testing.T) {
	r := httptest.NewRequest("GET", "/test", nil)
	// No X-Forwarded-Proto, no TLS → http
	if got := requestScheme(r); got != "http" {
		t.Errorf("expected http without proxy or TLS, got %s", got)
	}
}

func TestConnectAccount_RedirectURL_UsesHTTPS_BehindProxy(t *testing.T) {
	srv := newConnectedAccountServer()
	reg := account.NewRegistry()
	reg.Register(account.NewGoogleService("test-client-id", "secret", srv.connectedAccounts, srv.vault))
	srv.accountService = reg

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connect/gmail", "", nil)
	// Simulate reverse proxy (Railway, Cloudflare) setting X-Forwarded-Proto.
	r.Header.Set("X-Forwarded-Proto", "https")
	srv.ConnectAccount(w, r, "gmail", api.ConnectAccountParams{})

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	// The redirect_uri parameter in the OAuth URL should be https://.
	if !containsStr(loc, "redirect_uri=https") {
		t.Errorf("expected https redirect_uri behind proxy, got: %s", loc)
	}
}

func TestConnectAccount_RedirectURL_HTTP_WithoutProxy(t *testing.T) {
	srv := newConnectedAccountServer()
	reg := account.NewRegistry()
	reg.Register(account.NewGoogleService("test-client-id", "secret", srv.connectedAccounts, srv.vault))
	srv.accountService = reg

	w := httptest.NewRecorder()
	r := mcpRequest("GET", "/v1/connect/gmail", "", nil)
	// No X-Forwarded-Proto, no TLS → http (local dev).
	srv.ConnectAccount(w, r, "gmail", api.ConnectAccountParams{})

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	// Without a proxy, redirect_uri should be http:// (local dev).
	if !containsStr(loc, "redirect_uri=http") {
		t.Errorf("expected http redirect_uri without proxy, got: %s", loc)
	}
}

func TestConnectAccountCallback_SlackInstallDelegation(t *testing.T) {
	// When an unauthenticated Slack request hits the connect callback
	// without a state cookie (e.g. Slack Marketplace install), it should
	// delegate to handleSlackInstall instead of returning a 401 or CSRF error.
	srv := newConnectedAccountServer()
	srv.systemVault = vault.NewMemVault()
	srv.slackClientID = "test-client-id"
	srv.slackClientSecret = "test-client-secret"
	srv.slackBotExchanger = func(clientID, clientSecret, code, redirectURI string) (*slackBotInstallResponse, error) {
		if code != "marketplace-code" {
			t.Errorf("code = %q, want marketplace-code", code)
		}
		return &slackBotInstallResponse{
			OK:          true,
			AccessToken: "xoxb-marketplace-token",
			BotUserID:   "U_BOT",
			Team: struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}{"T_MKT", "marketplace-ws"},
		}, nil
	}
	// Enable auth so requireAuth would reject unauthenticated requests.
	srv.users = mem.NewUserStore()

	// No auth cookies/headers, no state cookie — simulates a Slack Marketplace redirect.
	req := httptest.NewRequest("GET", "/v1/connect/slack/callback?code=marketplace-code&state=", nil)
	w := httptest.NewRecorder()
	srv.ConnectAccountCallback(w, req, "slack", api.ConnectAccountCallbackParams{Code: "marketplace-code", State: nil})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (install success), got %d: %s", w.Code, w.Body.String())
	}

	// Verify the bot token was stored.
	secret, err := srv.systemVault.Get(context.Background(), "slack-workspaces/T_MKT/bot-token")
	if err != nil {
		t.Fatalf("expected bot token in system vault: %v", err)
	}
	if string(secret.Value) != "xoxb-marketplace-token" {
		t.Errorf("stored token = %q, want xoxb-marketplace-token", secret.Value)
	}
}

func TestConnectAccountCallback_SlackInstallDelegation_Authenticated(t *testing.T) {
	// Regression: an admin logged into Aileron clicks the Slack-generated
	// install URL. There's no state cookie (the flow wasn't started from
	// Aileron's UI), but there ARE auth claims from the session. The handler
	// must still delegate to handleSlackInstall.
	srv := newConnectedAccountServer()
	srv.systemVault = vault.NewMemVault()
	srv.slackClientID = "test-client-id"
	srv.slackClientSecret = "test-client-secret"
	srv.slackBotExchanger = func(_, _, code, _ string) (*slackBotInstallResponse, error) {
		if code != "admin-install-code" {
			t.Errorf("code = %q, want admin-install-code", code)
		}
		return &slackBotInstallResponse{
			OK:          true,
			AccessToken: "xoxb-admin-install",
			BotUserID:   "U_BOT",
			Team: struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}{"T_ADMIN", "admin-ws"},
		}, nil
	}

	req := httptest.NewRequest("GET", "/v1/connect/slack/callback?code=admin-install-code", nil)
	// Authenticated user — no state cookie.
	ctx := auth.ContextWithClaims(req.Context(), &auth.Claims{})
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	srv.ConnectAccountCallback(w, req, "slack", api.ConnectAccountCallbackParams{Code: "admin-install-code", State: nil})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (install delegation), got %d: %s", w.Code, w.Body.String())
	}

	secret, err := srv.systemVault.Get(context.Background(), "slack-workspaces/T_ADMIN/bot-token")
	if err != nil {
		t.Fatalf("expected bot token in system vault: %v", err)
	}
	if string(secret.Value) != "xoxb-admin-install" {
		t.Errorf("stored token = %q, want xoxb-admin-install", secret.Value)
	}
}

func TestConnectAccountCallback_SlackWithAuth_DoesNotDelegate(t *testing.T) {
	// When an authenticated Slack request hits the connect callback with a
	// state cookie, it should follow the normal user account connection flow,
	// not the bot install flow.
	srv := newConnectedAccountServer()
	srv.systemVault = vault.NewMemVault()
	srv.slackClientID = "test-client-id"
	srv.slackClientSecret = "test-client-secret"
	srv.slackBotExchanger = func(_, _, _, _ string) (*slackBotInstallResponse, error) {
		t.Error("install handler should not be called for authenticated requests with state cookie")
		return nil, nil
	}
	srv.accountService = newGoogleAccountRegistry(srv.connectedAccounts, srv.vault)

	req := httptest.NewRequest("GET", "/v1/connect/slack/callback?code=user-code&state=valid-state", nil)
	req.AddCookie(&http.Cookie{Name: "aileron_connect_state", Value: "valid-state"})
	// Add auth claims to context (simulates authenticated user).
	ctx := auth.ContextWithClaims(req.Context(), &auth.Claims{})
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	validState := "valid-state"
	srv.ConnectAccountCallback(w, req, "slack", api.ConnectAccountCallbackParams{Code: "user-code", State: &validState})

	// Should NOT have called the install exchanger — the state cookie is present,
	// so it proceeds with the normal account connection flow.
}

func TestConnectedAccountToAPI(t *testing.T) {
	acc := model.ConnectedAccount{
		ID:             "conn_1",
		UserID:         "usr_1",
		Provider:       model.ConnectedAccountProviderGmail,
		Scopes:         []string{"gmail.readonly"},
		Status:         model.ConnectedAccountStatusActive,
		ExternalUserID: "test@example.com",
	}
	result := connectedAccountToAPI(acc)
	if *result.Id != "conn_1" {
		t.Errorf("expected conn_1, got %s", *result.Id)
	}
	if string(*result.Provider) != "gmail" {
		t.Errorf("expected gmail, got %s", *result.Provider)
	}
	if result.ExternalUserId == nil || *result.ExternalUserId != "test@example.com" {
		t.Errorf("expected test@example.com")
	}
}

