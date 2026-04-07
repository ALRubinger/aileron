package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/auth"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/golang-jwt/jwt/v5"
)

func passphraseServer() *apiServer {
	return &apiServer{
		userKeyMaterials: mem.NewUserKeyMaterialStore(),
	}
}

var testClaims = &auth.Claims{
	RegisteredClaims: jwt.RegisteredClaims{Subject: "usr_test123"},
	EnterpriseID:     "ent_test",
	Email:            "test@example.com",
	Role:             "owner",
}

func TestSetPassphrase(t *testing.T) {
	srv := passphraseServer()
	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"passphrase":"correct horse battery staple"}`, testClaims)
	w := httptest.NewRecorder()

	srv.SetPassphrase(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp api.PassphraseSaltResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if !resp.HasPassphrase {
		t.Fatal("expected has_passphrase = true")
	}
	if resp.Salt == nil || len(*resp.Salt) == 0 {
		t.Fatal("expected non-empty salt")
	}
}

func TestSetPassphrase_TooShort(t *testing.T) {
	srv := passphraseServer()
	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"passphrase":"short"}`, testClaims)
	w := httptest.NewRecorder()

	srv.SetPassphrase(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestSetPassphrase_Rotation(t *testing.T) {
	srv := passphraseServer()

	// Set initial passphrase.
	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"passphrase":"first passphrase here"}`, testClaims)
	w := httptest.NewRecorder()
	srv.SetPassphrase(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first set: status = %d", w.Code)
	}

	// Rotate to new passphrase.
	req = authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"passphrase":"second passphrase here"}`, testClaims)
	w = httptest.NewRecorder()
	srv.SetPassphrase(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("rotation: status = %d; body: %s", w.Code, w.Body.String())
	}
}

func TestVerifyPassphrase_Correct(t *testing.T) {
	srv := passphraseServer()
	passphrase := "correct horse battery staple"

	// Set passphrase first.
	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"passphrase":"`+passphrase+`"}`, testClaims)
	w := httptest.NewRecorder()
	srv.SetPassphrase(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("set: status = %d", w.Code)
	}

	// Verify with correct passphrase.
	req = authedRequest(http.MethodPost, "/v1/users/me/passphrase/verify",
		`{"passphrase":"`+passphrase+`"}`, testClaims)
	w = httptest.NewRecorder()
	srv.VerifyPassphrase(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("verify: status = %d", w.Code)
	}

	var resp api.VerifyPassphraseResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !resp.Valid {
		t.Fatal("expected valid = true")
	}
	if resp.Salt == nil {
		t.Fatal("expected salt in response when valid")
	}
}

func TestVerifyPassphrase_Wrong(t *testing.T) {
	srv := passphraseServer()

	// Set passphrase.
	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"passphrase":"correct horse battery staple"}`, testClaims)
	w := httptest.NewRecorder()
	srv.SetPassphrase(w, req)

	// Verify with wrong passphrase.
	req = authedRequest(http.MethodPost, "/v1/users/me/passphrase/verify",
		`{"passphrase":"wrong passphrase entirely"}`, testClaims)
	w = httptest.NewRecorder()
	srv.VerifyPassphrase(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("verify: status = %d", w.Code)
	}

	var resp api.VerifyPassphraseResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Valid {
		t.Fatal("expected valid = false for wrong passphrase")
	}
	if resp.Salt != nil {
		t.Fatal("expected no salt for invalid passphrase")
	}
}

func TestVerifyPassphrase_NoPassphraseSet(t *testing.T) {
	srv := passphraseServer()

	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase/verify",
		`{"passphrase":"anything here"}`, testClaims)
	w := httptest.NewRecorder()
	srv.VerifyPassphrase(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestGetPassphraseSalt_NoPassphrase(t *testing.T) {
	srv := passphraseServer()
	req := authedRequest(http.MethodGet, "/v1/users/me/passphrase/salt", "", testClaims)
	w := httptest.NewRecorder()

	srv.GetPassphraseSalt(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp api.PassphraseSaltResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.HasPassphrase {
		t.Fatal("expected has_passphrase = false")
	}
	if resp.Salt != nil {
		t.Fatal("expected no salt when no passphrase set")
	}
}

func TestGetPassphraseSalt_WithPassphrase(t *testing.T) {
	srv := passphraseServer()

	// Set passphrase.
	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"passphrase":"correct horse battery staple"}`, testClaims)
	w := httptest.NewRecorder()
	srv.SetPassphrase(w, req)

	// Get salt.
	req = authedRequest(http.MethodGet, "/v1/users/me/passphrase/salt", "", testClaims)
	w = httptest.NewRecorder()
	srv.GetPassphraseSalt(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp api.PassphraseSaltResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.HasPassphrase {
		t.Fatal("expected has_passphrase = true")
	}
	if resp.Salt == nil || len(*resp.Salt) == 0 {
		t.Fatal("expected non-empty salt")
	}
}

func TestSetPassphrase_Unauthenticated(t *testing.T) {
	srv := passphraseServer()
	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"passphrase":"something"}`, nil)
	w := httptest.NewRecorder()

	srv.SetPassphrase(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestSetPassphrase_InvalidBody(t *testing.T) {
	srv := passphraseServer()
	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{invalid json`, testClaims)
	w := httptest.NewRecorder()

	srv.SetPassphrase(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestSetPassphrase_AuthNotEnabled(t *testing.T) {
	srv := &apiServer{} // no userKeyMaterials
	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"passphrase":"something long enough"}`, testClaims)
	w := httptest.NewRecorder()

	srv.SetPassphrase(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
}

func TestVerifyPassphrase_Unauthenticated(t *testing.T) {
	srv := passphraseServer()
	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase/verify",
		`{"passphrase":"anything"}`, nil)
	w := httptest.NewRecorder()

	srv.VerifyPassphrase(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestVerifyPassphrase_AuthNotEnabled(t *testing.T) {
	srv := &apiServer{} // no userKeyMaterials
	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase/verify",
		`{"passphrase":"anything"}`, testClaims)
	w := httptest.NewRecorder()

	srv.VerifyPassphrase(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
}

func TestVerifyPassphrase_InvalidBody(t *testing.T) {
	srv := passphraseServer()
	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase/verify",
		`{broken`, testClaims)
	w := httptest.NewRecorder()

	srv.VerifyPassphrase(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestGetPassphraseSalt_Unauthenticated(t *testing.T) {
	srv := passphraseServer()
	req := authedRequest(http.MethodGet, "/v1/users/me/passphrase/salt", "", nil)
	w := httptest.NewRecorder()

	srv.GetPassphraseSalt(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestGetPassphraseSalt_AuthNotEnabled(t *testing.T) {
	srv := &apiServer{} // no userKeyMaterials
	req := authedRequest(http.MethodGet, "/v1/users/me/passphrase/salt", "", testClaims)
	w := httptest.NewRecorder()

	srv.GetPassphraseSalt(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
}

func TestVerifyPassphrase_CachesKEK(t *testing.T) {
	cache := auth.NewKEKSessionCache(5 * time.Minute)
	srv := &apiServer{
		userKeyMaterials: mem.NewUserKeyMaterialStore(),
		kekCache:         cache,
	}
	passphrase := "correct horse battery staple"

	// Set passphrase.
	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"passphrase":"`+passphrase+`"}`, testClaims)
	w := httptest.NewRecorder()
	srv.SetPassphrase(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("set: status = %d", w.Code)
	}

	// Verify — should cache the KEK.
	req = authedRequest(http.MethodPost, "/v1/users/me/passphrase/verify",
		`{"passphrase":"`+passphrase+`"}`, testClaims)
	w = httptest.NewRecorder()
	srv.VerifyPassphrase(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("verify: status = %d", w.Code)
	}

	// Check that KEK was cached.
	kek := cache.Get("usr_test123")
	if kek == nil {
		t.Fatal("expected KEK to be cached after successful verify")
	}
}
