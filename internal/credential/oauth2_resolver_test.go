package credential_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/vault"
)

// fixedNow returns t. Used to drive deterministic expiry checks.
func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

// putToken writes an OAuth2 token envelope to the vault as the
// resolver expects to find it.
func putToken(t *testing.T, v vault.Vault, path string, tok credential.OAuth2Token) {
	t.Helper()
	body, _ := json.Marshal(tok)
	if err := v.Put(context.Background(), path, body, vault.Metadata{Type: "oauth2"}); err != nil {
		t.Fatalf("vault.Put: %v", err)
	}
}

func TestOAuth2VaultResolver_FreshTokenIsReturnedAsIs(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	v := vault.NewMemVault()
	putToken(t, v, "oauth2/google/work", credential.OAuth2Token{
		AccessToken:  "fresh-access-token",
		RefreshToken: "stored-refresh",
		ExpiresAt:    now.Add(1 * time.Hour),
		ClientID:     "cid",
		TokenURL:     "https://example.test/token",
	})

	r := &credential.OAuth2VaultResolver{
		Vault:     v,
		VaultPath: "oauth2/google/work",
		Now:       fixedNow(now),
	}
	cred, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(cred.Value) != "fresh-access-token" {
		t.Errorf("Value = %q, want fresh-access-token", cred.Value)
	}
	if cred.Kind != "oauth2" {
		t.Errorf("Kind = %q", cred.Kind)
	}
}

func TestOAuth2VaultResolver_NearExpiryTriggersRefreshAndPersists(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	// Stub provider serving the refresh endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)
		for _, want := range []string{"grant_type=refresh_token", "refresh_token=stored-refresh", "client_id=cid"} {
			if !strings.Contains(bodyStr, want) {
				t.Errorf("body missing %q: %s", want, bodyStr)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(srv.Close)

	v := vault.NewMemVault()
	putToken(t, v, "oauth2/google/work", credential.OAuth2Token{
		AccessToken:  "old-access",
		RefreshToken: "stored-refresh",
		ExpiresAt:    now.Add(10 * time.Second), // within 60s leeway
		ClientID:     "cid",
		TokenURL:     srv.URL + "/token",
		Scopes:       []string{"gmail.send"},
	})

	r := &credential.OAuth2VaultResolver{
		Vault:      v,
		VaultPath:  "oauth2/google/work",
		Now:        fixedNow(now),
		HTTPClient: srv.Client(),
	}
	cred, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(cred.Value) != "new-access" {
		t.Errorf("Value = %q, want new-access (refreshed)", cred.Value)
	}

	// Vault was updated with the new envelope including rotated refresh.
	stored, err := v.Get(context.Background(), "oauth2/google/work")
	if err != nil {
		t.Fatalf("vault.Get: %v", err)
	}
	var got credential.OAuth2Token
	_ = json.Unmarshal(stored.Value, &got)
	if got.AccessToken != "new-access" {
		t.Errorf("persisted AccessToken = %q", got.AccessToken)
	}
	if got.RefreshToken != "new-refresh" {
		t.Errorf("persisted RefreshToken = %q (rotation lost)", got.RefreshToken)
	}
	if !got.ExpiresAt.Equal(now.Add(3600 * time.Second)) {
		t.Errorf("persisted ExpiresAt = %v", got.ExpiresAt)
	}
	if len(got.Scopes) != 1 || got.Scopes[0] != "gmail.send" {
		t.Errorf("scopes lost on refresh: %+v", got.Scopes)
	}
}

func TestOAuth2VaultResolver_PreservesRefreshTokenWhenProviderOmitsIt(t *testing.T) {
	// Slack and others don't rotate refresh tokens; the response omits
	// the refresh_token field. The resolver must keep the existing one.
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"new-access","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)

	v := vault.NewMemVault()
	putToken(t, v, "oauth2/slack/work", credential.OAuth2Token{
		AccessToken:  "old",
		RefreshToken: "still-here",
		ExpiresAt:    now.Add(5 * time.Second),
		ClientID:     "cid",
		TokenURL:     srv.URL + "/token",
	})
	r := &credential.OAuth2VaultResolver{
		Vault: v, VaultPath: "oauth2/slack/work",
		Now: fixedNow(now), HTTPClient: srv.Client(),
	}
	if _, err := r.Resolve(context.Background()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	stored, _ := v.Get(context.Background(), "oauth2/slack/work")
	var got credential.OAuth2Token
	_ = json.Unmarshal(stored.Value, &got)
	if got.RefreshToken != "still-here" {
		t.Errorf("RefreshToken = %q, want still-here", got.RefreshToken)
	}
}

// TestOAuth2VaultResolver_RefreshForwardsClientSecretWhenSet asserts
// that an OAuth2Token envelope minted from a manifest declaring
// client_secret carries that secret on the refresh request as well.
// Mirrors exchangeOAuth2Code behavior — Google Desktop's token
// endpoint enforces client_secret on refresh, not just exchange.
func TestOAuth2VaultResolver_RefreshForwardsClientSecretWhenSet(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "client_secret=csecret") {
			t.Errorf("refresh body missing client_secret: %s", body)
		}
		_, _ = io.WriteString(w, `{"access_token":"new","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)

	v := vault.NewMemVault()
	putToken(t, v, "oauth2/google/work", credential.OAuth2Token{
		AccessToken:  "old",
		RefreshToken: "rt",
		ExpiresAt:    now.Add(5 * time.Second),
		ClientID:     "cid",
		ClientSecret: "csecret",
		TokenURL:     srv.URL,
	})
	r := &credential.OAuth2VaultResolver{
		Vault: v, VaultPath: "oauth2/google/work",
		Now: fixedNow(now), HTTPClient: srv.Client(),
	}
	if _, err := r.Resolve(context.Background()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	stored, _ := v.Get(context.Background(), "oauth2/google/work")
	var got credential.OAuth2Token
	_ = json.Unmarshal(stored.Value, &got)
	if got.ClientSecret != "csecret" {
		t.Errorf("persisted ClientSecret = %q, want csecret (lost on refresh persist)", got.ClientSecret)
	}
}

func TestOAuth2VaultResolver_RefreshFailureSurfacesAsErrOAuth2RefreshFailed(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
	}))
	t.Cleanup(srv.Close)

	v := vault.NewMemVault()
	putToken(t, v, "oauth2/x/y", credential.OAuth2Token{
		AccessToken:  "expired",
		RefreshToken: "revoked",
		ExpiresAt:    now.Add(-1 * time.Hour),
		ClientID:     "cid",
		TokenURL:     srv.URL,
	})
	r := &credential.OAuth2VaultResolver{
		Vault: v, VaultPath: "oauth2/x/y",
		Now: fixedNow(now), HTTPClient: srv.Client(),
	}
	_, err := r.Resolve(context.Background())
	if !errors.Is(err, credential.ErrOAuth2RefreshFailed) {
		t.Errorf("err = %v, want ErrOAuth2RefreshFailed", err)
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("err should include provider message: %v", err)
	}
}

func TestOAuth2VaultResolver_TokenWithoutExpiryDoesNotRefresh(t *testing.T) {
	// Some providers issue tokens without documented lifetime; we
	// don't gratuitously refresh those. The token comes back as-is.
	v := vault.NewMemVault()
	putToken(t, v, "oauth2/x/y", credential.OAuth2Token{
		AccessToken:  "long-lived",
		RefreshToken: "rt",
		ClientID:     "cid",
		TokenURL:     "https://wont.be.called",
	})
	r := &credential.OAuth2VaultResolver{Vault: v, VaultPath: "oauth2/x/y"}
	cred, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(cred.Value) != "long-lived" {
		t.Errorf("Value = %q", cred.Value)
	}
}

func TestOAuth2VaultResolver_TokenWithoutRefreshTokenDoesNotRefresh(t *testing.T) {
	// If we never got a refresh token (e.g. provider didn't issue
	// one), the resolver returns the access token as-is even when
	// it's near expiry. Upstream 401 then forces a manual rebind.
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	v := vault.NewMemVault()
	putToken(t, v, "oauth2/x/y", credential.OAuth2Token{
		AccessToken: "no-refresh",
		ExpiresAt:   now.Add(1 * time.Second),
		ClientID:    "cid",
		TokenURL:    "https://wont.be.called",
	})
	r := &credential.OAuth2VaultResolver{Vault: v, VaultPath: "oauth2/x/y", Now: fixedNow(now)}
	cred, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(cred.Value) != "no-refresh" {
		t.Errorf("Value = %q", cred.Value)
	}
}

func TestOAuth2VaultResolver_MissingBindingReturnsErrBindingMissing(t *testing.T) {
	r := &credential.OAuth2VaultResolver{
		Vault:     vault.NewMemVault(),
		VaultPath: "oauth2/missing/x",
	}
	_, err := r.Resolve(context.Background())
	if !errors.Is(err, credential.ErrBindingMissing) {
		t.Errorf("err = %v, want ErrBindingMissing", err)
	}
}

func TestOAuth2VaultResolver_KindMismatchSurfacesAsErrCredentialKindMismatch(t *testing.T) {
	v := vault.NewMemVault()
	if err := v.Put(context.Background(), "weird/path", []byte("{}"),
		vault.Metadata{Type: "api_key"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	r := &credential.OAuth2VaultResolver{Vault: v, VaultPath: "weird/path"}
	_, err := r.Resolve(context.Background())
	if !errors.Is(err, credential.ErrCredentialKindMismatch) {
		t.Errorf("err = %v, want ErrCredentialKindMismatch", err)
	}
}

func TestOAuth2VaultResolver_NilReceiverIsNoBindingResolver(t *testing.T) {
	var r *credential.OAuth2VaultResolver
	_, err := r.Resolve(context.Background())
	if !errors.Is(err, credential.ErrNoBindingResolver) {
		t.Errorf("err = %v, want ErrNoBindingResolver", err)
	}
}

func TestOAuth2VaultResolver_MalformedVaultPayloadIsAnError(t *testing.T) {
	v := vault.NewMemVault()
	_ = v.Put(context.Background(), "oauth2/x/y", []byte("not json"),
		vault.Metadata{Type: "oauth2"})
	r := &credential.OAuth2VaultResolver{Vault: v, VaultPath: "oauth2/x/y"}
	_, err := r.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error on malformed payload")
	}
}

// TestOAuth2VaultResolver_DefaultLeewayWhenZero exercises the
// `leeway == 0 → use defaultRefreshLeeway` branch in shouldRefresh.
func TestOAuth2VaultResolver_DefaultLeewayWhenZero(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"refreshed","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)

	v := vault.NewMemVault()
	putToken(t, v, "oauth2/x/y", credential.OAuth2Token{
		AccessToken:  "old",
		RefreshToken: "rt",
		// 30s before expiry; default leeway of 60s triggers refresh.
		ExpiresAt: now.Add(30 * time.Second),
		ClientID:  "cid",
		TokenURL:  srv.URL,
	})
	r := &credential.OAuth2VaultResolver{
		Vault: v, VaultPath: "oauth2/x/y",
		Now: fixedNow(now), HTTPClient: srv.Client(),
		// RefreshLeeway intentionally zero — exercises the default path.
	}
	cred, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(cred.Value) != "refreshed" {
		t.Errorf("Value = %q, want refreshed", cred.Value)
	}
}

// TestOAuth2VaultResolver_FallbackNowWhenNotInjected exercises the
// `r.Now == nil → time.Now()` branch.
func TestOAuth2VaultResolver_FallbackNowWhenNotInjected(t *testing.T) {
	v := vault.NewMemVault()
	// Token is fresh by wall-clock time; resolver returns it without
	// refreshing. No HTTPClient injected; if shouldRefresh fired we'd
	// see a network panic.
	putToken(t, v, "oauth2/x/y", credential.OAuth2Token{
		AccessToken: "wall-clock-fresh",
		ExpiresAt:   time.Now().Add(2 * time.Hour),
		ClientID:    "cid",
		TokenURL:    "https://wont.be.called",
	})
	r := &credential.OAuth2VaultResolver{Vault: v, VaultPath: "oauth2/x/y"}
	cred, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(cred.Value) != "wall-clock-fresh" {
		t.Errorf("Value = %q", cred.Value)
	}
}

// TestOAuth2VaultResolver_RefreshFailureFromNetworkErrorIsAlsoSurfaced
// exercises the dial-error path in refresh — the http.Client returns
// an error before the request even completes.
func TestOAuth2VaultResolver_RefreshFailureFromNetworkErrorIsAlsoSurfaced(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	v := vault.NewMemVault()
	putToken(t, v, "oauth2/x/y", credential.OAuth2Token{
		AccessToken:  "expired",
		RefreshToken: "rt",
		ExpiresAt:    now.Add(-1 * time.Hour),
		ClientID:     "cid",
		TokenURL:     "http://127.0.0.1:1", // closed port
	})
	r := &credential.OAuth2VaultResolver{
		Vault: v, VaultPath: "oauth2/x/y",
		Now:        fixedNow(now),
		HTTPClient: &http.Client{Timeout: 100 * time.Millisecond},
	}
	_, err := r.Resolve(context.Background())
	if !errors.Is(err, credential.ErrOAuth2RefreshFailed) {
		t.Errorf("err = %v, want ErrOAuth2RefreshFailed", err)
	}
}

// TestOAuth2VaultResolver_RefreshResponseMissingAccessToken exercises
// the response-missing-token branch.
func TestOAuth2VaultResolver_RefreshResponseMissingAccessToken(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"refresh_token":"only"}`)
	}))
	t.Cleanup(srv.Close)
	v := vault.NewMemVault()
	putToken(t, v, "oauth2/x/y", credential.OAuth2Token{
		AccessToken: "old", RefreshToken: "rt",
		ExpiresAt: now.Add(-1 * time.Hour),
		ClientID:  "cid", TokenURL: srv.URL,
	})
	r := &credential.OAuth2VaultResolver{
		Vault: v, VaultPath: "oauth2/x/y",
		Now: fixedNow(now), HTTPClient: srv.Client(),
	}
	_, err := r.Resolve(context.Background())
	if !errors.Is(err, credential.ErrOAuth2RefreshFailed) {
		t.Fatal("expected ErrOAuth2RefreshFailed")
	}
	if !strings.Contains(err.Error(), "access_token") {
		t.Errorf("err = %v", err)
	}
}

// TestOAuth2VaultResolver_RefreshResponseMalformedJSON.
func TestOAuth2VaultResolver_RefreshResponseMalformedJSON(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{not json`)
	}))
	t.Cleanup(srv.Close)
	v := vault.NewMemVault()
	putToken(t, v, "oauth2/x/y", credential.OAuth2Token{
		AccessToken: "old", RefreshToken: "rt",
		ExpiresAt: now.Add(-1 * time.Hour),
		ClientID:  "cid", TokenURL: srv.URL,
	})
	r := &credential.OAuth2VaultResolver{
		Vault: v, VaultPath: "oauth2/x/y",
		Now: fixedNow(now), HTTPClient: srv.Client(),
	}
	_, err := r.Resolve(context.Background())
	if !errors.Is(err, credential.ErrOAuth2RefreshFailed) {
		t.Errorf("err = %v, want ErrOAuth2RefreshFailed", err)
	}
}

// TestOAuth2VaultResolver_RefreshWithoutExpiresInPreservesPreviousExpiry
// exercises the `parsed.ExpiresIn == 0` branch.
func TestOAuth2VaultResolver_RefreshWithoutExpiresInPreservesPreviousExpiry(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	prevExpiry := now.Add(30 * time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Response omits expires_in; resolver preserves the previous
		// expiry rather than treating the new token as immortal.
		_, _ = io.WriteString(w, `{"access_token":"new"}`)
	}))
	t.Cleanup(srv.Close)
	v := vault.NewMemVault()
	putToken(t, v, "oauth2/x/y", credential.OAuth2Token{
		AccessToken: "old", RefreshToken: "rt",
		ExpiresAt: prevExpiry,
		ClientID:  "cid", TokenURL: srv.URL,
	})
	r := &credential.OAuth2VaultResolver{
		Vault: v, VaultPath: "oauth2/x/y",
		Now: fixedNow(now), HTTPClient: srv.Client(),
	}
	if _, err := r.Resolve(context.Background()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	stored, _ := v.Get(context.Background(), "oauth2/x/y")
	var got credential.OAuth2Token
	_ = json.Unmarshal(stored.Value, &got)
	if !got.ExpiresAt.Equal(prevExpiry) {
		t.Errorf("ExpiresAt = %v, want %v (preserved from prior)", got.ExpiresAt, prevExpiry)
	}
}

// TestFormatBindingMissing_WrapsSentinel covers the helper used by
// callers to build wrapped sentinels with a vault-path context.
func TestFormatBindingMissing_WrapsSentinelOAuth2(t *testing.T) {
	err := credential.FormatBindingMissing("oauth2/google/work")
	if !errors.Is(err, credential.ErrBindingMissing) {
		t.Errorf("err = %v, not Is ErrBindingMissing", err)
	}
	if !strings.Contains(err.Error(), "oauth2/google/work") {
		t.Errorf("err missing path: %v", err)
	}
}
