package auth

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/model"
)

func TestRefresh_Success(t *testing.T) {
	te := newTestEnv()

	// Setup: signup, verify, login to get a refresh token.
	te.do("POST", "/auth/signup", `{"email":"alice@acme.com","password":"securepass123"}`)
	code := te.mailer.lastCode()
	te.do("POST", "/auth/verify-email", `{"email":"alice@acme.com","code":"`+code+`"}`)
	loginResp := te.do("POST", "/auth/login", `{"email":"alice@acme.com","password":"securepass123"}`)

	// Extract refresh token from Set-Cookie.
	var refreshToken string
	for _, cookie := range loginResp.Result().Cookies() {
		if cookie.Name == "refresh_token" {
			refreshToken = cookie.Value
		}
	}
	if refreshToken == "" {
		t.Fatal("no refresh_token cookie in login response")
	}

	initialSessions := te.sessions.count()

	// Refresh via cookie.
	w := te.doWithCookie("POST", "/auth/refresh", "", "refresh_token", refreshToken)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["access_token"] == "" {
		t.Error("expected access_token in response")
	}

	// Old session should be deleted and new one created (rotation).
	if te.sessions.count() != initialSessions {
		t.Errorf("session count = %d, want %d (rotation should keep count stable)", te.sessions.count(), initialSessions)
	}

	// New token should be valid.
	claims, err := te.issuer.Validate(resp["access_token"])
	if err != nil {
		t.Fatalf("new token invalid: %v", err)
	}
	if claims.Email != "alice@acme.com" {
		t.Errorf("email = %q", claims.Email)
	}
}

func TestRefresh_ViaJSONBody(t *testing.T) {
	te := newTestEnv()

	te.do("POST", "/auth/signup", `{"email":"alice@acme.com","password":"securepass123"}`)
	code := te.mailer.lastCode()
	te.do("POST", "/auth/verify-email", `{"email":"alice@acme.com","code":"`+code+`"}`)
	loginResp := te.do("POST", "/auth/login", `{"email":"alice@acme.com","password":"securepass123"}`)

	var refreshToken string
	for _, cookie := range loginResp.Result().Cookies() {
		if cookie.Name == "refresh_token" {
			refreshToken = cookie.Value
		}
	}

	// Refresh via JSON body instead of cookie.
	w := te.do("POST", "/auth/refresh", `{"refresh_token":"`+refreshToken+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestRefresh_InvalidToken(t *testing.T) {
	te := newTestEnv()
	w := te.doWithCookie("POST", "/auth/refresh", "", "refresh_token", "bogus-token")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestRefresh_MissingToken(t *testing.T) {
	te := newTestEnv()
	w := te.do("POST", "/auth/refresh", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestRefresh_ExpiredToken(t *testing.T) {
	te := newTestEnv()

	// Manually create an expired session.
	user := model.User{
		ID: "usr_test", EnterpriseID: "ent_test", Email: "alice@acme.com",
		Role: model.UserRoleOwner, Status: model.UserStatusActive, AuthProvider: "email",
	}
	te.users.Create(t.Context(), user)

	refreshRaw, refreshHash, _ := GenerateRefreshToken()
	expired := model.Session{
		ID:               "ses_expired",
		UserID:           user.ID,
		TokenHash:        "old-token-hash",
		RefreshTokenHash: refreshHash,
		ExpiresAt:        time.Now().Add(-1 * time.Hour), // expired
		CreatedAt:        time.Now().Add(-2 * time.Hour),
	}
	te.sessions.Create(t.Context(), expired)

	w := te.doWithCookie("POST", "/auth/refresh", "", "refresh_token", refreshRaw)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

// --- Logout handler tests ---

func TestLogout_ClearsSession(t *testing.T) {
	te := newTestEnv()

	// Login to create a session.
	te.do("POST", "/auth/signup", `{"email":"alice@acme.com","password":"securepass123"}`)
	code := te.mailer.lastCode()
	te.do("POST", "/auth/verify-email", `{"email":"alice@acme.com","code":"`+code+`"}`)
	loginResp := te.do("POST", "/auth/login", `{"email":"alice@acme.com","password":"securepass123"}`)

	var refreshToken string
	for _, cookie := range loginResp.Result().Cookies() {
		if cookie.Name == "refresh_token" {
			refreshToken = cookie.Value
		}
	}

	if te.sessions.count() != 1 {
		t.Fatalf("expected 1 session before logout, got %d", te.sessions.count())
	}

	// Logout with refresh token cookie.
	w := te.doWithCookie("POST", "/auth/logout", "", "refresh_token", refreshToken)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Session should be deleted.
	if te.sessions.count() != 0 {
		t.Errorf("expected 0 sessions after logout, got %d", te.sessions.count())
	}

	// Response should clear cookies.
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "access_token" && cookie.MaxAge >= 0 {
			t.Error("expected access_token cookie to be cleared")
		}
		if cookie.Name == "refresh_token" && cookie.MaxAge >= 0 {
			t.Error("expected refresh_token cookie to be cleared")
		}
	}
}

func TestLogout_WithoutCookie(t *testing.T) {
	te := newTestEnv()
	// Logout without any cookie should still succeed (idempotent).
	w := te.do("POST", "/auth/logout", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}
