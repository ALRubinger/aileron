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

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
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

	if result.Account.Email != "user@example.com" {
		t.Errorf("expected user@example.com, got %s", result.Account.Email)
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
		Email:    "test@example.com",
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
	if got.Email != "test@example.com" {
		t.Errorf("expected test@example.com, got %s", got.Email)
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

func mustParseURL(raw string) *net_url.URL {
	u, err := net_url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}
