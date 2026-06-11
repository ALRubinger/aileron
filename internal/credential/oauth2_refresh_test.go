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

	"github.com/ALRubinger/aileron/internal/credential"
)

// DoRefresh contract (U5):
//
//   - Happy path: parses access_token / refresh_token / expires_in
//     from a 2xx provider response.
//   - Error path: 4xx with RFC 6749 §5.2 `error` field surfaces the
//     standard error code (invalid_grant, etc.) but NOT the raw
//     response body. ADR-0025 / R21: token hints must not leak.
//   - Validation: empty refresh_token / token_url fail without an
//     HTTP round-trip.

func TestDoRefresh_SuccessReturnsParsedFields(t *testing.T) {
	gotForm := make(chan map[string]string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		flat := map[string]string{}
		for k, v := range r.PostForm {
			if len(v) > 0 {
				flat[k] = v[0]
			}
		}
		gotForm <- flat
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	}))
	defer srv.Close()

	resp, err := credential.DoRefresh(context.Background(), credential.RefreshTokenParams{
		RefreshToken: "old-refresh",
		ClientID:     "client-x",
		TokenURL:     srv.URL,
	})
	if err != nil {
		t.Fatalf("DoRefresh: %v", err)
	}
	if resp.AccessToken != "new-access" {
		t.Errorf("AccessToken = %q, want new-access", resp.AccessToken)
	}
	if resp.RefreshToken != "new-refresh" {
		t.Errorf("RefreshToken = %q, want new-refresh", resp.RefreshToken)
	}
	if resp.ExpiresIn != 3600 {
		t.Errorf("ExpiresIn = %d, want 3600", resp.ExpiresIn)
	}

	form := <-gotForm
	if form["grant_type"] != "refresh_token" {
		t.Errorf("grant_type = %q, want refresh_token", form["grant_type"])
	}
	if form["refresh_token"] != "old-refresh" {
		t.Errorf("refresh_token = %q, want old-refresh", form["refresh_token"])
	}
	if form["client_id"] != "client-x" {
		t.Errorf("client_id = %q, want client-x", form["client_id"])
	}
}

func TestDoRefresh_4xxErrorStripsBodyKeepsStandardErrorCode(t *testing.T) {
	// R21: raw provider body must not appear in user-facing errors.
	// The standard `error` code IS surfaced because it is a small
	// well-typed set (invalid_grant, invalid_request, etc.) without
	// token material.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"refresh token abc123def revoked"}`)
	}))
	defer srv.Close()

	_, err := credential.DoRefresh(context.Background(), credential.RefreshTokenParams{
		RefreshToken: "r",
		TokenURL:     srv.URL,
	})
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("err = %v, want mention of invalid_grant", err)
	}
	if strings.Contains(err.Error(), "abc123def") {
		t.Errorf("err leaked raw provider body: %v", err)
	}
	if !errors.Is(err, credential.ErrRefreshFailed) {
		t.Errorf("err = %v, want errors.Is(ErrRefreshFailed)", err)
	}
}

func TestDoRefresh_4xxErrorWithoutStandardCodeOmitsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "some opaque body")
	}))
	defer srv.Close()

	_, err := credential.DoRefresh(context.Background(), credential.RefreshTokenParams{
		RefreshToken: "r",
		TokenURL:     srv.URL,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "some opaque body") {
		t.Errorf("err leaked opaque body: %v", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("err should mention status: %v", err)
	}
}

func TestDoRefresh_RejectsEmptyRefreshToken(t *testing.T) {
	_, err := credential.DoRefresh(context.Background(), credential.RefreshTokenParams{
		RefreshToken: "",
		TokenURL:     "https://example.test/token",
	})
	if err == nil {
		t.Fatal("expected error for empty refresh_token")
	}
}

func TestDoRefresh_RejectsEmptyTokenURL(t *testing.T) {
	_, err := credential.DoRefresh(context.Background(), credential.RefreshTokenParams{
		RefreshToken: "r",
		TokenURL:     "",
	})
	if err == nil {
		t.Fatal("expected error for empty token_url")
	}
}

func TestDoRefresh_ResponseMissingAccessTokenFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"refresh_token": "r",
			// access_token missing
		})
	}))
	defer srv.Close()

	_, err := credential.DoRefresh(context.Background(), credential.RefreshTokenParams{
		RefreshToken: "r",
		TokenURL:     srv.URL,
	})
	if err == nil {
		t.Fatal("expected error for response missing access_token")
	}
}

func TestDoRefresh_ForwardsClientSecret(t *testing.T) {
	gotForm := make(chan map[string]string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		flat := map[string]string{}
		for k, v := range r.PostForm {
			if len(v) > 0 {
				flat[k] = v[0]
			}
		}
		gotForm <- flat
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "x"})
	}))
	defer srv.Close()

	_, err := credential.DoRefresh(context.Background(), credential.RefreshTokenParams{
		RefreshToken: "r",
		ClientID:     "c",
		ClientSecret: "s",
		TokenURL:     srv.URL,
	})
	if err != nil {
		t.Fatalf("DoRefresh: %v", err)
	}
	form := <-gotForm
	if form["client_secret"] != "s" {
		t.Errorf("client_secret = %q, want forwarded value", form["client_secret"])
	}
}
