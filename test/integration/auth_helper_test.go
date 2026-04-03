//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
)

var (
	authToken string
	authOnce  sync.Once
	authErr   error
)

// ensureAuth signs up a test user and logs in to get an access token.
// In CI and local dev, AILERON_AUTO_VERIFY_EMAIL=true so signup creates
// active accounts directly. This runs once per test process.
func ensureAuth(t *testing.T) string {
	t.Helper()
	authOnce.Do(func() {
		// Check if auth is enabled by trying an authenticated endpoint.
		resp, err := http.Get(apiURL() + "/v1/intents?workspace_id=default")
		if err != nil {
			authErr = fmt.Errorf("checking auth: %w", err)
			return
		}
		resp.Body.Close()

		// If we get 200, auth is not enabled — no token needed.
		if resp.StatusCode == http.StatusOK {
			authToken = ""
			return
		}

		// Auth is enabled. Sign up a test user.
		signupBody, _ := json.Marshal(map[string]string{
			"email":        "integration-test@aileron.test",
			"password":     "integration-test-password-123",
			"display_name": "Integration Test",
		})
		signupResp, err := http.Post(apiURL()+"/auth/signup", "application/json", bytes.NewReader(signupBody))
		if err != nil {
			authErr = fmt.Errorf("signup: %w", err)
			return
		}
		signupRespBody, _ := io.ReadAll(signupResp.Body)
		signupResp.Body.Close()

		// 409 means user already exists (from a previous test run) — that's fine.
		if signupResp.StatusCode != http.StatusCreated && signupResp.StatusCode != http.StatusConflict {
			authErr = fmt.Errorf("signup: expected 201 or 409, got %d: %s", signupResp.StatusCode, signupRespBody)
			return
		}

		// Log in.
		loginBody, _ := json.Marshal(map[string]string{
			"email":    "integration-test@aileron.test",
			"password": "integration-test-password-123",
		})
		loginResp, err := http.Post(apiURL()+"/auth/login", "application/json", bytes.NewReader(loginBody))
		if err != nil {
			authErr = fmt.Errorf("login: %w", err)
			return
		}
		defer loginResp.Body.Close()

		if loginResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(loginResp.Body)
			authErr = fmt.Errorf("login: expected 200, got %d: %s", loginResp.StatusCode, body)
			return
		}

		var loginResult map[string]string
		json.NewDecoder(loginResp.Body).Decode(&loginResult)
		authToken = loginResult["access_token"]
	})

	if authErr != nil {
		t.Fatalf("auth setup failed: %v", authErr)
	}
	return authToken
}

// authedPost sends an authenticated POST request with JSON body.
func authedPost(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	token := ensureAuth(t)
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// authedGet sends an authenticated GET request.
func authedGet(t *testing.T, url string) *http.Response {
	t.Helper()
	token := ensureAuth(t)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}
