package github

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
	p := New("id", "secret")
	if p.Provider() != "github" {
		t.Errorf("Provider() = %q, want github", p.Provider())
	}
}

func TestProvider_AuthorizationURL(t *testing.T) {
	p := New("my-client-id", "my-secret")
	result, err := p.AuthorizationURL(context.Background(), "test-state-123", "https://example.com/callback")
	if err != nil {
		t.Fatalf("AuthorizationURL: %v", err)
	}
	url := result.URL
	if !strings.Contains(url, "client_id=my-client-id") {
		t.Errorf("URL missing client_id: %s", url)
	}
	if !strings.Contains(url, "state=test-state-123") {
		t.Errorf("URL missing state: %s", url)
	}
	if !strings.Contains(url, "redirect_uri=") {
		t.Errorf("URL missing redirect_uri: %s", url)
	}
	// Check for scopes (URL-encoded).
	if !strings.Contains(url, "user%3Aemail") && !strings.Contains(url, "user:email") {
		t.Errorf("URL missing user:email scope: %s", url)
	}
	if !strings.Contains(url, "read%3Auser") && !strings.Contains(url, "read:user") {
		t.Errorf("URL missing read:user scope: %s", url)
	}
	if result.ExtraState != "" {
		t.Errorf("ExtraState = %q, want empty (GitHub does not use PKCE)", result.ExtraState)
	}
}

func TestProvider_HandleCallback_Success(t *testing.T) {
	// Fake token endpoint.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
		})
	}))
	defer tokenServer.Close()

	// Fake user API returning public email.
	userServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer fake-access-token" {
			t.Errorf("expected Bearer token, got %q", authHeader)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":         12345,
			"login":      "alice",
			"name":       "Alice Smith",
			"email":      "alice@example.com",
			"avatar_url": "https://avatars.githubusercontent.com/u/12345",
		})
	}))
	defer userServer.Close()

	p := &Provider{
		clientID:     "test-client",
		clientSecret: "test-secret",
		endpoint: oauth2.Endpoint{
			TokenURL: tokenServer.URL,
		},
	}

	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{
		Transport: &fakeTransport{userServerURL: userServer.URL},
	})

	identity, err := p.HandleCallback(ctx, auth.CallbackRequest{
		Code:        "fake-auth-code",
		State:       "test-state",
		RedirectURL: "https://example.com/callback",
	})
	if err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}

	if identity.Subject != "12345" {
		t.Errorf("Subject = %q, want 12345", identity.Subject)
	}
	if identity.Email != "alice@example.com" {
		t.Errorf("Email = %q, want alice@example.com", identity.Email)
	}
	if identity.DisplayName != "Alice Smith" {
		t.Errorf("DisplayName = %q, want Alice Smith", identity.DisplayName)
	}
	if identity.AvatarURL != "https://avatars.githubusercontent.com/u/12345" {
		t.Errorf("AvatarURL = %q", identity.AvatarURL)
	}
	if identity.Provider != "github" {
		t.Errorf("Provider = %q, want github", identity.Provider)
	}
}

func TestProvider_HandleCallback_PrivateEmail(t *testing.T) {
	// Fake token endpoint.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
		})
	}))
	defer tokenServer.Close()

	// Fake user API returning null email (private).
	userServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":         67890,
			"login":      "bob",
			"name":       "",
			"email":      nil,
			"avatar_url": "https://avatars.githubusercontent.com/u/67890",
		})
	}))
	defer userServer.Close()

	// Fake emails API returning primary verified email.
	emailsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]any{
			{"email": "bob-noreply@users.noreply.github.com", "primary": false, "verified": true},
			{"email": "bob@company.com", "primary": true, "verified": true},
		})
	}))
	defer emailsServer.Close()

	p := &Provider{
		clientID:     "test-client",
		clientSecret: "test-secret",
		endpoint: oauth2.Endpoint{
			TokenURL: tokenServer.URL,
		},
	}

	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{
		Transport: &fakeTransport{
			userServerURL:   userServer.URL,
			emailsServerURL: emailsServer.URL,
		},
	})

	identity, err := p.HandleCallback(ctx, auth.CallbackRequest{
		Code: "fake-auth-code",
	})
	if err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}

	if identity.Email != "bob@company.com" {
		t.Errorf("Email = %q, want bob@company.com", identity.Email)
	}
	// Name is empty, should fall back to login.
	if identity.DisplayName != "bob" {
		t.Errorf("DisplayName = %q, want bob (login fallback)", identity.DisplayName)
	}
	if identity.Subject != "67890" {
		t.Errorf("Subject = %q, want 67890", identity.Subject)
	}
}

func TestProvider_HandleCallback_NoVerifiedEmail(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-token",
			"token_type":   "Bearer",
		})
	}))
	defer tokenServer.Close()

	// User API returns no email.
	userServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": 99, "login": "nomail", "email": nil,
		})
	}))
	defer userServer.Close()

	// Emails API returns no primary+verified entry.
	emailsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]any{
			{"email": "unverified@example.com", "primary": true, "verified": false},
			{"email": "secondary@example.com", "primary": false, "verified": true},
		})
	}))
	defer emailsServer.Close()

	p := &Provider{
		clientID:     "test-client",
		clientSecret: "test-secret",
		endpoint:     oauth2.Endpoint{TokenURL: tokenServer.URL},
	}

	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{
		Transport: &fakeTransport{
			userServerURL:   userServer.URL,
			emailsServerURL: emailsServer.URL,
		},
	})

	_, err := p.HandleCallback(ctx, auth.CallbackRequest{Code: "code"})
	if err == nil {
		t.Fatal("expected error when no primary verified email")
	}
	if !strings.Contains(err.Error(), "no primary verified email") {
		t.Errorf("error = %q, expected to mention no primary verified email", err.Error())
	}
}

func TestProvider_HandleCallback_TokenExchangeError(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad_verification_code"}`, http.StatusBadRequest)
	}))
	defer tokenServer.Close()

	p := &Provider{
		clientID:     "test-client",
		clientSecret: "test-secret",
		endpoint:     oauth2.Endpoint{TokenURL: tokenServer.URL},
	}

	_, err := p.HandleCallback(context.Background(), auth.CallbackRequest{Code: "bad-code"})
	if err == nil {
		t.Fatal("expected error for invalid token exchange")
	}
	if !strings.Contains(err.Error(), "exchanging code") {
		t.Errorf("error = %q, expected to mention exchanging code", err.Error())
	}
}

func TestProvider_HandleCallback_UserAPIError(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-token",
			"token_type":   "Bearer",
		})
	}))
	defer tokenServer.Close()

	userServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer userServer.Close()

	p := &Provider{
		clientID:     "test-client",
		clientSecret: "test-secret",
		endpoint:     oauth2.Endpoint{TokenURL: tokenServer.URL},
	}

	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{
		Transport: &fakeTransport{userServerURL: userServer.URL},
	})

	_, err := p.HandleCallback(ctx, auth.CallbackRequest{Code: "code"})
	if err == nil {
		t.Fatal("expected error for user API failure")
	}
	if !strings.Contains(err.Error(), "user API returned") {
		t.Errorf("error = %q, expected to mention user API", err.Error())
	}
}

func TestProvider_HandleCallback_UserDecodeError(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-token",
			"token_type":   "Bearer",
		})
	}))
	defer tokenServer.Close()

	// User API returns invalid JSON.
	userServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{not valid json`))
	}))
	defer userServer.Close()

	p := &Provider{
		clientID:     "test-client",
		clientSecret: "test-secret",
		endpoint:     oauth2.Endpoint{TokenURL: tokenServer.URL},
	}

	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{
		Transport: &fakeTransport{userServerURL: userServer.URL},
	})

	_, err := p.HandleCallback(ctx, auth.CallbackRequest{Code: "code"})
	if err == nil {
		t.Fatal("expected error for decode failure")
	}
	if !strings.Contains(err.Error(), "decoding user profile") {
		t.Errorf("error = %q, expected to mention decoding user profile", err.Error())
	}
}

func TestProvider_HandleCallback_EmailsAPIError(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-token",
			"token_type":   "Bearer",
		})
	}))
	defer tokenServer.Close()

	// User API returns no email (private).
	userServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": 1, "login": "x", "email": nil,
		})
	}))
	defer userServer.Close()

	// Emails API returns HTTP error.
	emailsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer emailsServer.Close()

	p := &Provider{
		clientID:     "test-client",
		clientSecret: "test-secret",
		endpoint:     oauth2.Endpoint{TokenURL: tokenServer.URL},
	}

	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{
		Transport: &fakeTransport{
			userServerURL:   userServer.URL,
			emailsServerURL: emailsServer.URL,
		},
	})

	_, err := p.HandleCallback(ctx, auth.CallbackRequest{Code: "code"})
	if err == nil {
		t.Fatal("expected error for emails API failure")
	}
	if !strings.Contains(err.Error(), "user emails API returned") {
		t.Errorf("error = %q, expected to mention user emails API", err.Error())
	}
}

func TestProvider_HandleCallback_EmailsDecodeError(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-token",
			"token_type":   "Bearer",
		})
	}))
	defer tokenServer.Close()

	userServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": 1, "login": "x", "email": nil,
		})
	}))
	defer userServer.Close()

	// Emails API returns invalid JSON.
	emailsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[not valid`))
	}))
	defer emailsServer.Close()

	p := &Provider{
		clientID:     "test-client",
		clientSecret: "test-secret",
		endpoint:     oauth2.Endpoint{TokenURL: tokenServer.URL},
	}

	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{
		Transport: &fakeTransport{
			userServerURL:   userServer.URL,
			emailsServerURL: emailsServer.URL,
		},
	})

	_, err := p.HandleCallback(ctx, auth.CallbackRequest{Code: "code"})
	if err == nil {
		t.Fatal("expected error for emails decode failure")
	}
	if !strings.Contains(err.Error(), "decoding user emails") {
		t.Errorf("error = %q, expected to mention decoding user emails", err.Error())
	}
}

// fakeTransport intercepts requests to the GitHub API URLs and
// redirects them to local test servers.
type fakeTransport struct {
	userServerURL   string
	emailsServerURL string
}

func (t *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case strings.Contains(req.URL.String(), "api.github.com/user/emails"):
		newReq, _ := http.NewRequestWithContext(req.Context(), req.Method, t.emailsServerURL, req.Body)
		for k, v := range req.Header {
			newReq.Header[k] = v
		}
		return http.DefaultTransport.RoundTrip(newReq)
	case strings.Contains(req.URL.String(), "api.github.com/user"):
		newReq, _ := http.NewRequestWithContext(req.Context(), req.Method, t.userServerURL, req.Body)
		for k, v := range req.Header {
			newReq.Header[k] = v
		}
		return http.DefaultTransport.RoundTrip(newReq)
	default:
		return http.DefaultTransport.RoundTrip(req)
	}
}
