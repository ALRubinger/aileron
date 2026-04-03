package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMiddleware_ValidToken(t *testing.T) {
	issuer := NewTokenIssuer([]byte("test-secret-key-32-bytes-long!!!"), "test", 15*time.Minute)
	token, _ := issuer.Issue("usr_1", "ent_1", "alice@example.com", "owner")

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims := ClaimsFromContext(r.Context())
		if claims == nil {
			t.Error("expected claims in context")
			return
		}
		if claims.Subject != "usr_1" {
			t.Errorf("Subject = %q, want %q", claims.Subject, "usr_1")
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := Middleware(issuer, nil)(inner)
	req := httptest.NewRequest(http.MethodGet, "/v1/intents", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestMiddleware_MissingToken(t *testing.T) {
	issuer := NewTokenIssuer([]byte("test-secret-key-32-bytes-long!!!"), "test", 15*time.Minute)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	})

	handler := Middleware(issuer, nil)(inner)
	req := httptest.NewRequest(http.MethodGet, "/v1/intents", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestMiddleware_InvalidToken(t *testing.T) {
	issuer := NewTokenIssuer([]byte("test-secret-key-32-bytes-long!!!"), "test", 15*time.Minute)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	})

	handler := Middleware(issuer, nil)(inner)
	req := httptest.NewRequest(http.MethodGet, "/v1/intents", nil)
	req.Header.Set("Authorization", "Bearer invalid-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestMiddleware_SkipPaths(t *testing.T) {
	issuer := NewTokenIssuer([]byte("test-secret-key-32-bytes-long!!!"), "test", 15*time.Minute)
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	skipPaths := map[string]bool{"/v1/health": true}
	handler := Middleware(issuer, skipPaths)(inner)
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("handler should be called for skipped paths")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestMiddleware_SkipAuthRoutes(t *testing.T) {
	issuer := NewTokenIssuer([]byte("test-secret-key-32-bytes-long!!!"), "test", 15*time.Minute)
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := Middleware(issuer, nil)(inner)
	req := httptest.NewRequest(http.MethodGet, "/auth/google/login", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("handler should be called for /auth/ routes")
	}
}

func TestMiddleware_CookieToken(t *testing.T) {
	issuer := NewTokenIssuer([]byte("test-secret-key-32-bytes-long!!!"), "test", 15*time.Minute)
	token, _ := issuer.Issue("usr_1", "ent_1", "alice@example.com", "owner")

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims := ClaimsFromContext(r.Context())
		if claims == nil {
			t.Error("expected claims in context from cookie")
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := Middleware(issuer, nil)(inner)
	req := httptest.NewRequest(http.MethodGet, "/v1/intents", nil)
	req.AddCookie(&http.Cookie{Name: "access_token", Value: token})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}
