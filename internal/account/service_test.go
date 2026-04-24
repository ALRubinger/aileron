package account

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	net_url "net/url"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/store"
	"github.com/ALRubinger/aileron/internal/store/mem"
	"github.com/ALRubinger/aileron/internal/vault"
	"golang.org/x/oauth2"
)

func TestGoogleService_Providers(t *testing.T) {
	svc := NewGoogleService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	providers := svc.Providers()
	if len(providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(providers))
	}
	if providers[0] != model.ConnectedAccountProviderGmail {
		t.Errorf("expected gmail, got %s", providers[0])
	}
	if providers[1] != model.ConnectedAccountProviderGoogleCalendar {
		t.Errorf("expected google_calendar, got %s", providers[1])
	}
}

func TestGoogleService_AuthorizationURL(t *testing.T) {
	svc := NewGoogleService("test-client-id", "test-secret", mem.NewConnectedAccountStore(), vault.NewMemVault())

	result, err := svc.AuthorizationURL(context.Background(), model.ConnectedAccountProviderGmail, "test-state", "http://localhost:8080/callback")
	if err != nil {
		t.Fatal(err)
	}
	if result.URL == "" {
		t.Fatal("expected non-empty URL")
	}
	if !strings.Contains(result.URL, "gmail.readonly") {
		t.Errorf("URL should contain gmail scope, got: %s", result.URL)
	}
	if !strings.Contains(result.URL, "test-client-id") {
		t.Errorf("URL should contain client ID, got: %s", result.URL)
	}
	if !strings.Contains(result.URL, "test-state") {
		t.Errorf("URL should contain state, got: %s", result.URL)
	}
}

func TestGoogleService_AuthorizationURL_CalendarScopes(t *testing.T) {
	svc := NewGoogleService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())

	result, err := svc.AuthorizationURL(context.Background(), model.ConnectedAccountProviderGoogleCalendar, "state", "http://localhost/callback")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.URL, "calendar") {
		t.Errorf("URL should contain calendar scope, got: %s", result.URL)
	}
}

func TestGoogleService_AuthorizationURL_UnsupportedProvider(t *testing.T) {
	svc := NewGoogleService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())

	_, err := svc.AuthorizationURL(context.Background(), model.ConnectedAccountProviderOutlook, "state", "http://localhost/callback")
	if err == nil {
		t.Fatal("expected error for unsupported provider")
	}
}

func TestGoogleService_HandleCallback_Success(t *testing.T) {
	accountStore := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()
	svc := NewGoogleService("id", "secret", accountStore, v)

	// Inject test hooks to avoid real HTTP calls.
	svc.exchangeToken = func(_ context.Context, _ *oauth2.Config, code string) (*oauth2.Token, error) {
		if code != "valid-code" {
			return nil, fmt.Errorf("invalid code")
		}
		return &oauth2.Token{
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
			Expiry:       time.Now().Add(time.Hour),
		}, nil
	}
	svc.fetchEmail = func(_ context.Context, _ *oauth2.Config, _ *oauth2.Token) (string, error) {
		return "user@example.com", nil
	}

	ctx := context.Background()
	result, err := svc.HandleCallback(ctx, model.ConnectedAccountProviderGmail, CallbackRequest{
		Code:        "valid-code",
		State:       "state",
		RedirectURL: "http://localhost/callback",
		UserID:      "usr_1",
	})
	if err != nil {
		t.Fatal(err)
	}

	if result.Account.ExternalUserID != "user@example.com" {
		t.Errorf("expected user@example.com, got %s", result.Account.ExternalUserID)
	}
	if result.Account.Provider != model.ConnectedAccountProviderGmail {
		t.Errorf("expected gmail, got %s", result.Account.Provider)
	}
	if result.Account.UserID != "usr_1" {
		t.Errorf("expected usr_1, got %s", result.Account.UserID)
	}
	if result.Account.Status != model.ConnectedAccountStatusActive {
		t.Errorf("expected active, got %s", result.Account.Status)
	}
	if !strings.HasPrefix(result.Account.ID, "conn_") {
		t.Errorf("expected conn_ prefix, got %s", result.Account.ID)
	}

	// Verify account was stored.
	accounts, _ := accountStore.List(ctx, store.ConnectedAccountFilter{UserID: "usr_1"})
	if len(accounts) != 1 {
		t.Fatalf("expected 1 stored account, got %d", len(accounts))
	}

	// Verify token was stored in vault.
	secret, err := v.Get(ctx, result.Account.VaultPath())
	if err != nil {
		t.Fatalf("expected token in vault: %v", err)
	}
	if len(secret.Value) == 0 {
		t.Error("expected non-empty vault value")
	}
}

func TestGoogleService_HandleCallback_ExchangeError(t *testing.T) {
	svc := NewGoogleService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	svc.exchangeToken = func(_ context.Context, _ *oauth2.Config, _ string) (*oauth2.Token, error) {
		return nil, fmt.Errorf("exchange failed")
	}

	_, err := svc.HandleCallback(context.Background(), model.ConnectedAccountProviderGmail, CallbackRequest{
		Code:        "bad",
		RedirectURL: "http://localhost/callback",
		UserID:      "usr_1",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "exchanging code") {
		t.Errorf("expected 'exchanging code' error, got: %s", err.Error())
	}
}

func TestGoogleService_HandleCallback_NoRefreshToken(t *testing.T) {
	svc := NewGoogleService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	svc.exchangeToken = func(_ context.Context, _ *oauth2.Config, _ string) (*oauth2.Token, error) {
		return &oauth2.Token{
			AccessToken:  "access",
			RefreshToken: "", // no refresh token
		}, nil
	}

	_, err := svc.HandleCallback(context.Background(), model.ConnectedAccountProviderGmail, CallbackRequest{
		Code:        "code",
		RedirectURL: "http://localhost/callback",
		UserID:      "usr_1",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "no refresh token") {
		t.Errorf("expected 'no refresh token' error, got: %s", err.Error())
	}
}

func TestGoogleService_HandleCallback_EmailFetchError(t *testing.T) {
	svc := NewGoogleService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	svc.exchangeToken = func(_ context.Context, _ *oauth2.Config, _ string) (*oauth2.Token, error) {
		return &oauth2.Token{AccessToken: "access", RefreshToken: "refresh"}, nil
	}
	svc.fetchEmail = func(_ context.Context, _ *oauth2.Config, _ *oauth2.Token) (string, error) {
		return "", fmt.Errorf("userinfo down")
	}

	_, err := svc.HandleCallback(context.Background(), model.ConnectedAccountProviderGmail, CallbackRequest{
		Code:        "code",
		RedirectURL: "http://localhost/callback",
		UserID:      "usr_1",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "fetching account email") {
		t.Errorf("expected 'fetching account email' error, got: %s", err.Error())
	}
}

func TestGoogleService_HandleCallback_UnsupportedProvider(t *testing.T) {
	svc := NewGoogleService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	_, err := svc.HandleCallback(context.Background(), model.ConnectedAccountProviderOutlook, CallbackRequest{
		Code:        "code",
		RedirectURL: "http://localhost/callback",
		UserID:      "usr_1",
	})
	if err == nil {
		t.Fatal("expected error for unsupported provider")
	}
}

func TestGoogleService_ListAndDisconnect(t *testing.T) {
	accountStore := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()
	svc := NewGoogleService("id", "secret", accountStore, v)

	ctx := context.Background()

	acc := model.ConnectedAccount{
		ID:       "conn_test",
		UserID:   "usr_1",
		Provider: model.ConnectedAccountProviderGmail,
		Scopes:   []string{"gmail.readonly"},
		Status:   model.ConnectedAccountStatusActive,
	}
	accountStore.Create(ctx, acc)
	v.Put(ctx, acc.VaultPath(), []byte(`{"token":"test"}`), vault.Metadata{Type: "oauth_refresh_token"})

	// List.
	accounts, err := svc.List(ctx, "usr_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}

	// Get.
	got, err := svc.Get(ctx, "conn_test")
	if err != nil {
		t.Fatal(err)
	}
	if got.ExternalUserID != "" {
		t.Errorf("expected empty ExternalUserID, got %s", got.ExternalUserID)
	}

	// Disconnect.
	if err := svc.Disconnect(ctx, "conn_test"); err != nil {
		t.Fatal(err)
	}

	// Verify deleted.
	accounts, _ = svc.List(ctx, "usr_1")
	if len(accounts) != 0 {
		t.Fatalf("expected 0 accounts after disconnect, got %d", len(accounts))
	}

	// Verify vault token removed.
	_, err = v.Get(ctx, acc.VaultPath())
	if err == nil {
		t.Fatal("expected vault token to be removed")
	}
}

func TestGoogleService_Disconnect_NotFound(t *testing.T) {
	svc := NewGoogleService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	err := svc.Disconnect(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestConnectedAccount_VaultPath(t *testing.T) {
	acc := model.ConnectedAccount{
		UserID:   "usr_abc",
		Provider: model.ConnectedAccountProviderGmail,
	}
	expected := "connected-accounts/usr_abc/gmail"
	if acc.VaultPath() != expected {
		t.Errorf("expected %s, got %s", expected, acc.VaultPath())
	}
}

func TestFetchGoogleEmail_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"email": "user@test.com"})
	}))
	defer ts.Close()

	// Override the userinfoURL by using the test server's client directly.
	client := ts.Client()
	// Patch the client's transport to redirect to test server.
	origURL := userinfoURL
	_ = origURL // userinfoURL is a package const, so we test fetchGoogleEmail with a custom client
	// fetchGoogleEmail uses client.Get(userinfoURL), so we need to intercept.
	// Instead, test via a round-tripper that redirects.
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req.URL = mustParseURL(ts.URL)
		return http.DefaultTransport.RoundTrip(req)
	})

	email, err := fetchGoogleEmail(client)
	if err != nil {
		t.Fatal(err)
	}
	if email != "user@test.com" {
		t.Errorf("expected user@test.com, got %s", email)
	}
}

func TestFetchGoogleEmail_Non200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("server error"))
	}))
	defer ts.Close()

	client := ts.Client()
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req.URL = mustParseURL(ts.URL)
		return http.DefaultTransport.RoundTrip(req)
	})

	_, err := fetchGoogleEmail(client)
	if err == nil {
		t.Fatal("expected error for non-200")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected 500 in error, got: %s", err.Error())
	}
}

func TestFetchGoogleEmail_BadJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer ts.Close()

	client := ts.Client()
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req.URL = mustParseURL(ts.URL)
		return http.DefaultTransport.RoundTrip(req)
	})

	_, err := fetchGoogleEmail(client)
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

func TestFetchGoogleEmail_NetworkError(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("network down")
		}),
	}

	_, err := fetchGoogleEmail(client)
	if err == nil {
		t.Fatal("expected error for network failure")
	}
}

// roundTripFunc allows using a function as an http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestGoogleService_WithVault(t *testing.T) {
	v1 := vault.NewMemVault()
	v2 := vault.NewMemVault()
	svc := NewGoogleService("id", "secret", mem.NewConnectedAccountStore(), v1)

	scoped := svc.WithVault(v2)

	// Original should still use v1.
	if svc.vault == v2 {
		t.Fatal("original service should keep its vault")
	}
	// Scoped copy is a ProviderService — verify it preserved credentials.
	if scoped.ClientID() != "id" || scoped.ClientSecret() != "secret" {
		t.Fatal("WithVault should preserve client credentials")
	}
}

func TestGoogleService_ClientID(t *testing.T) {
	svc := NewGoogleService("my-client-id", "my-secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	if got := svc.ClientID(); got != "my-client-id" {
		t.Errorf("expected my-client-id, got %s", got)
	}
}

func TestGoogleService_ClientSecret(t *testing.T) {
	svc := NewGoogleService("id", "my-client-secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	if got := svc.ClientSecret(); got != "my-client-secret" {
		t.Errorf("expected my-client-secret, got %s", got)
	}
}

func TestGoogleService_ScopesFor(t *testing.T) {
	svc := NewGoogleService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())

	// Gmail scopes.
	gmailScopes := svc.ScopesFor(model.ConnectedAccountProviderGmail)
	if len(gmailScopes) == 0 {
		t.Fatal("expected non-empty scopes for Gmail")
	}
	found := false
	for _, s := range gmailScopes {
		if strings.Contains(s, "gmail") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a gmail scope in Gmail scopes")
	}

	// Calendar scopes.
	calScopes := svc.ScopesFor(model.ConnectedAccountProviderGoogleCalendar)
	if len(calScopes) == 0 {
		t.Fatal("expected non-empty scopes for Google Calendar")
	}
	foundCal := false
	for _, s := range calScopes {
		if strings.Contains(s, "calendar") {
			foundCal = true
			break
		}
	}
	if !foundCal {
		t.Error("expected a calendar scope in Google Calendar scopes")
	}

	// Unknown provider returns nil.
	if scopes := svc.ScopesFor(model.ConnectedAccountProviderOutlook); scopes != nil {
		t.Errorf("expected nil scopes for unsupported provider, got %v", scopes)
	}
}

func TestGoogleService_TokenEndpointFor(t *testing.T) {
	svc := NewGoogleService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())

	// Gmail endpoint should be non-empty.
	ep := svc.TokenEndpointFor(model.ConnectedAccountProviderGmail)
	if ep == "" {
		t.Fatal("expected non-empty token endpoint for Gmail")
	}

	// Calendar endpoint should be the same Google endpoint.
	epCal := svc.TokenEndpointFor(model.ConnectedAccountProviderGoogleCalendar)
	if epCal == "" {
		t.Fatal("expected non-empty token endpoint for Google Calendar")
	}
	if ep != epCal {
		t.Errorf("expected same Google endpoint for Gmail and Calendar, got %q vs %q", ep, epCal)
	}

	// Unknown provider returns empty string.
	if got := svc.TokenEndpointFor(model.ConnectedAccountProviderOutlook); got != "" {
		t.Errorf("expected empty token endpoint for unsupported provider, got %s", got)
	}
}

func TestGoogleService_UserInfoEndpoint(t *testing.T) {
	svc := NewGoogleService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	ep := svc.UserInfoEndpoint()
	if ep == "" {
		t.Fatal("expected non-empty userinfo endpoint")
	}
	if !strings.Contains(ep, "googleapis.com") {
		t.Errorf("expected googleapis.com in endpoint, got %s", ep)
	}
}

func TestGoogleService_WithEndpoints(t *testing.T) {
	svc := NewGoogleService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	overridden := svc.WithEndpoints("http://token.test", "http://userinfo.test")

	if overridden.TokenEndpointFor(model.ConnectedAccountProviderGmail) != "http://token.test" {
		t.Fatalf("expected overridden token endpoint, got %s", overridden.TokenEndpointFor(model.ConnectedAccountProviderGmail))
	}
	if overridden.UserInfoEndpoint() != "http://userinfo.test" {
		t.Fatalf("expected overridden userinfo endpoint, got %s", overridden.UserInfoEndpoint())
	}

	// Original should be unchanged.
	if svc.TokenEndpointFor(model.ConnectedAccountProviderGmail) == "http://token.test" {
		t.Fatal("original service should not be modified")
	}
}

func mustParseURL(raw string) *net_url.URL {
	u, err := net_url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}
