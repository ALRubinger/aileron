package google

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/auth"
	"golang.org/x/oauth2"
)

func TestProvider_Provider(t *testing.T) {
	p := New("id", "secret", "https://example.com/callback")
	if p.Provider() != "google" {
		t.Errorf("Provider() = %q, want google", p.Provider())
	}
}

func TestProvider_AuthorizationURL(t *testing.T) {
	p := New("my-client-id", "my-secret", "https://example.com/callback")
	url, err := p.AuthorizationURL(context.Background(), "test-state-123")
	if err != nil {
		t.Fatalf("AuthorizationURL: %v", err)
	}
	if !strings.Contains(url, "client_id=my-client-id") {
		t.Errorf("URL missing client_id: %s", url)
	}
	if !strings.Contains(url, "state=test-state-123") {
		t.Errorf("URL missing state: %s", url)
	}
	if !strings.Contains(url, "redirect_uri=") {
		t.Errorf("URL missing redirect_uri: %s", url)
	}
	if !strings.Contains(url, "scope=openid+email+profile") &&
		!strings.Contains(url, "scope=openid%20email%20profile") {
		t.Errorf("URL missing expected scopes: %s", url)
	}
	if !strings.Contains(url, "access_type=offline") {
		t.Errorf("URL missing access_type=offline: %s", url)
	}
}

func TestProvider_HandleCallback_Success(t *testing.T) {
	// Fake token endpoint.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}))
	defer tokenServer.Close()

	// Fake userinfo endpoint.
	userinfoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the access token is passed.
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer fake-access-token" {
			t.Errorf("expected Bearer token, got %q", authHeader)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"sub":     "google-uid-12345",
			"email":   "alice@example.com",
			"name":    "Alice Smith",
			"picture": "https://lh3.example.com/photo.jpg",
		})
	}))
	defer userinfoServer.Close()

	p := &Provider{
		cfg: &oauth2.Config{
			ClientID:     "test-client",
			ClientSecret: "test-secret",
			Endpoint: oauth2.Endpoint{
				TokenURL: tokenServer.URL,
			},
		},
	}

	// Override the userinfo URL for this test.
	origURL := userinfoURL
	// We can't override the const, so we'll use a custom approach:
	// create the provider with a custom HTTP client that redirects
	// userinfo requests to our fake server. Actually, the simpler
	// approach is to test using the provider's internals.
	// Let's just test the full flow by having the token server
	// return a token, then manually calling the userinfo endpoint.

	// Actually, HandleCallback calls p.cfg.Client(ctx, token).Get(userinfoURL)
	// which goes to the real Google URL. We need to intercept that.
	// The cleanest way: override the HTTP transport.

	_ = origURL // we'll use a different approach

	// Create a transport that routes userinfo requests to our fake server.
	transport := &fakeTransport{
		userinfoURL:    userinfoServer.URL,
		userinfoServer: userinfoServer,
	}

	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{
		Transport: transport,
	})

	identity, err := p.HandleCallback(ctx, auth.CallbackRequest{
		Code:  "fake-auth-code",
		State: "test-state",
	})
	if err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}

	if identity.Subject != "google-uid-12345" {
		t.Errorf("Subject = %q, want google-uid-12345", identity.Subject)
	}
	if identity.Email != "alice@example.com" {
		t.Errorf("Email = %q, want alice@example.com", identity.Email)
	}
	if identity.DisplayName != "Alice Smith" {
		t.Errorf("DisplayName = %q, want Alice Smith", identity.DisplayName)
	}
	if identity.AvatarURL != "https://lh3.example.com/photo.jpg" {
		t.Errorf("AvatarURL = %q", identity.AvatarURL)
	}
	if identity.Provider != "google" {
		t.Errorf("Provider = %q, want google", identity.Provider)
	}
	if identity.RawClaims["sub"] != "google-uid-12345" {
		t.Error("RawClaims missing sub")
	}
}

func TestProvider_HandleCallback_TokenExchangeError(t *testing.T) {
	// Token endpoint returns an error.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer tokenServer.Close()

	p := &Provider{
		cfg: &oauth2.Config{
			ClientID:     "test-client",
			ClientSecret: "test-secret",
			Endpoint: oauth2.Endpoint{
				TokenURL: tokenServer.URL,
			},
		},
	}

	_, err := p.HandleCallback(context.Background(), auth.CallbackRequest{
		Code: "bad-code",
	})
	if err == nil {
		t.Fatal("expected error for invalid token exchange")
	}
	if !strings.Contains(err.Error(), "exchanging code") {
		t.Errorf("error = %q, expected to mention exchanging code", err.Error())
	}
}

func TestProvider_HandleCallback_UserinfoError(t *testing.T) {
	// Token endpoint succeeds.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-token",
			"token_type":   "Bearer",
		})
	}))
	defer tokenServer.Close()

	// Userinfo endpoint returns error.
	userinfoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer userinfoServer.Close()

	p := &Provider{
		cfg: &oauth2.Config{
			ClientID:     "test-client",
			ClientSecret: "test-secret",
			Endpoint: oauth2.Endpoint{
				TokenURL: tokenServer.URL,
			},
		},
	}

	transport := &fakeTransport{
		userinfoURL:    userinfoServer.URL,
		userinfoServer: userinfoServer,
	}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{
		Transport: transport,
	})

	_, err := p.HandleCallback(ctx, auth.CallbackRequest{Code: "code"})
	if err == nil {
		t.Fatal("expected error for userinfo failure")
	}
	if !strings.Contains(err.Error(), "userinfo returned") {
		t.Errorf("error = %q, expected to mention userinfo", err.Error())
	}
}

// fakeTransport intercepts requests to the Google userinfo URL and
// redirects them to a local test server.
type fakeTransport struct {
	userinfoURL    string
	userinfoServer *httptest.Server
}

func (t *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Redirect userinfo requests to fake server.
	if strings.Contains(req.URL.String(), "googleapis.com/oauth2") {
		newReq, _ := http.NewRequestWithContext(req.Context(), req.Method, t.userinfoURL, req.Body)
		for k, v := range req.Header {
			newReq.Header[k] = v
		}
		return http.DefaultTransport.RoundTrip(newReq)
	}
	// Everything else (token exchange) goes through normally.
	return http.DefaultTransport.RoundTrip(req)
}
