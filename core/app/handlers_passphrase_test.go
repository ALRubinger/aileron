package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/auth"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
	"github.com/ALRubinger/aileron/enclave"
	"github.com/golang-jwt/jwt/v5"
)

// failingTransmitClient is an enclave client whose TransmitKEK always fails.
type failingTransmitClient struct {
	err error
}

func (c *failingTransmitClient) Attest(_ context.Context, _ enclave.AttestationRequest) (enclave.AttestationResponse, error) {
	return enclave.AttestationResponse{}, nil
}
func (c *failingTransmitClient) EstablishSession(_ context.Context, _ enclave.SessionRequest) (enclave.SessionResponse, error) {
	return enclave.SessionResponse{}, nil
}
func (c *failingTransmitClient) TransmitKEK(_ context.Context, _ enclave.TransmitKEKRequest) (enclave.TransmitKEKResponse, error) {
	return enclave.TransmitKEKResponse{}, c.err
}
func (c *failingTransmitClient) OAuthExchange(_ context.Context, _ enclave.OAuthExchangeRequest) (enclave.OAuthExchangeResponse, error) {
	return enclave.OAuthExchangeResponse{}, nil
}
func (c *failingTransmitClient) Execute(_ context.Context, _ enclave.ExecuteRequest) (enclave.ExecuteResponse, error) {
	return enclave.ExecuteResponse{}, nil
}
func (c *failingTransmitClient) EscrowStore(_ context.Context, _ enclave.EscrowStoreRequest) (enclave.EscrowStoreResponse, error) {
	return enclave.EscrowStoreResponse{}, nil
}
func (c *failingTransmitClient) EscrowRevoke(_ context.Context, _ enclave.EscrowRevokeRequest) error {
	return nil
}
func (c *failingTransmitClient) Close() error { return nil }

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

// mockKeyMaterialStore is a mock UserKeyMaterialStore that lets tests inject
// errors for each operation independently.
type mockKeyMaterialStore struct {
	inner    *mem.UserKeyMaterialStore
	getErr   error // if non-nil, Get always returns this
	createErr error // if non-nil, Create always returns this
	updateErr error // if non-nil, Update always returns this
}

func newMockKeyMaterialStore() *mockKeyMaterialStore {
	return &mockKeyMaterialStore{inner: mem.NewUserKeyMaterialStore()}
}

func (m *mockKeyMaterialStore) Create(ctx context.Context, mat model.UserKeyMaterial) error {
	if m.createErr != nil {
		return m.createErr
	}
	return m.inner.Create(ctx, mat)
}

func (m *mockKeyMaterialStore) Get(ctx context.Context, userID string) (model.UserKeyMaterial, error) {
	if m.getErr != nil {
		return model.UserKeyMaterial{}, m.getErr
	}
	return m.inner.Get(ctx, userID)
}

func (m *mockKeyMaterialStore) Update(ctx context.Context, mat model.UserKeyMaterial) error {
	if m.updateErr != nil {
		return m.updateErr
	}
	return m.inner.Update(ctx, mat)
}

// --- SetPassphrase tests ---

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

func TestSetPassphrase_CreateStoreError(t *testing.T) {
	mock := newMockKeyMaterialStore()
	mock.createErr = errors.New("db connection lost")
	srv := &apiServer{userKeyMaterials: mock}

	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"passphrase":"correct horse battery staple"}`, testClaims)
	w := httptest.NewRecorder()
	srv.SetPassphrase(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

func TestSetPassphrase_GetStoreNonNotFoundError(t *testing.T) {
	mock := newMockKeyMaterialStore()
	mock.getErr = errors.New("db timeout")
	srv := &apiServer{userKeyMaterials: mock}

	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"passphrase":"correct horse battery staple"}`, testClaims)
	w := httptest.NewRecorder()
	srv.SetPassphrase(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
}

func TestSetPassphrase_UpdateStoreError(t *testing.T) {
	mock := newMockKeyMaterialStore()
	srv := &apiServer{userKeyMaterials: mock}

	// First, set a passphrase so the record exists.
	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"passphrase":"correct horse battery staple"}`, testClaims)
	w := httptest.NewRecorder()
	srv.SetPassphrase(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("initial set: status = %d", w.Code)
	}

	// Now inject update error and try rotation.
	mock.updateErr = errors.New("db write failed")
	req = authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"passphrase":"new passphrase for rotation"}`, testClaims)
	w = httptest.NewRecorder()
	srv.SetPassphrase(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

// --- VerifyPassphrase tests ---

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

func TestVerifyPassphrase_GetStoreError(t *testing.T) {
	mock := newMockKeyMaterialStore()
	mock.getErr = errors.New("db timeout")
	srv := &apiServer{userKeyMaterials: mock}

	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase/verify",
		`{"passphrase":"anything here"}`, testClaims)
	w := httptest.NewRecorder()
	srv.VerifyPassphrase(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
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

// --- GetPassphraseSalt tests ---

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

// TestVerifyPassphrase_TEE_TransmitKEKFails verifies the contract:
// Given a user with a valid passphrase and an active TEE session,
// when TransmitKEK to the enclave fails, then return 500 with enclave_error.
func TestVerifyPassphrase_TEE_TransmitKEKFails(t *testing.T) {
	srv := passphraseServer()
	srv.enclaveClient = &failingTransmitClient{err: errors.New("enclave unreachable")}
	srv.teeState = &teeState{sessionKey: make([]byte, 32)} // simulate active session

	// Set passphrase first.
	setReq := authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"passphrase":"a-valid-passphrase"}`, testClaims)
	w1 := httptest.NewRecorder()
	srv.SetPassphrase(w1, setReq)
	if w1.Code != http.StatusOK {
		t.Fatalf("SetPassphrase: expected 200, got %d", w1.Code)
	}

	// Verify passphrase — should attempt TransmitKEK and fail.
	verifyReq := authedRequest(http.MethodPost, "/v1/users/me/passphrase/verify",
		`{"passphrase":"a-valid-passphrase"}`, testClaims)
	w2 := httptest.NewRecorder()
	srv.VerifyPassphrase(w2, verifyReq)

	if w2.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w2.Code, w2.Body.String())
	}
	var errResp api.Error
	json.NewDecoder(w2.Body).Decode(&errResp)
	if errResp.Error.Code != "enclave_error" {
		t.Fatalf("expected error code enclave_error, got %q", errResp.Error.Code)
	}
}

// TestVerifyPassphrase_TEE_NoSessionFallsBackToCache verifies the contract:
// Given a user with a valid passphrase and TEE enabled but no session,
// when passphrase is verified, then KEK is cached locally (not transmitted).
func TestVerifyPassphrase_TEE_NoSessionFallsBackToCache(t *testing.T) {
	kekCache := auth.NewKEKSessionCache(30 * time.Minute)
	srv := passphraseServer()
	srv.enclaveClient = &failingTransmitClient{} // TEE enabled, but...
	srv.teeState = &teeState{}                   // ...no session key
	srv.kekCache = kekCache

	// Set passphrase.
	setReq := authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"passphrase":"a-valid-passphrase"}`, testClaims)
	w1 := httptest.NewRecorder()
	srv.SetPassphrase(w1, setReq)
	if w1.Code != http.StatusOK {
		t.Fatalf("SetPassphrase: expected 200, got %d", w1.Code)
	}

	// Verify passphrase — no TEE session, should fall back to kekCache.
	verifyReq := authedRequest(http.MethodPost, "/v1/users/me/passphrase/verify",
		`{"passphrase":"a-valid-passphrase"}`, testClaims)
	w2 := httptest.NewRecorder()
	srv.VerifyPassphrase(w2, verifyReq)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w2.Code, w2.Body.String())
	}
	var resp api.VerifyPassphraseResponse
	json.NewDecoder(w2.Body).Decode(&resp)
	if !resp.Valid {
		t.Fatal("expected valid=true")
	}

	// KEK should be in the local cache.
	kek := kekCache.Get(testClaims.Subject)
	if kek == nil {
		t.Fatal("expected KEK to be cached locally when TEE session is not active")
	}
}

func TestGetPassphraseSalt_GetStoreError(t *testing.T) {
	mock := newMockKeyMaterialStore()
	mock.getErr = errors.New("db timeout")
	srv := &apiServer{userKeyMaterials: mock}

	req := authedRequest(http.MethodGet, "/v1/users/me/passphrase/salt", "", testClaims)
	w := httptest.NewRecorder()
	srv.GetPassphraseSalt(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

// --- GetPassphraseSession tests ---

func TestGetPassphraseSession_Active(t *testing.T) {
	cache := auth.NewKEKSessionCache(5 * time.Minute)
	srv := &apiServer{kekCache: cache}

	cache.Set(testClaims.Subject, []byte("0123456789abcdef0123456789abcdef"))

	req := authedRequest(http.MethodGet, "/v1/users/me/passphrase/session", "", testClaims)
	w := httptest.NewRecorder()
	srv.GetPassphraseSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp api.KEKSessionStatus
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.Active {
		t.Fatal("expected active = true")
	}
	if resp.ExpiresAt == nil {
		t.Fatal("expected expires_at when active")
	}
}

func TestGetPassphraseSession_Inactive(t *testing.T) {
	cache := auth.NewKEKSessionCache(5 * time.Minute)
	srv := &apiServer{kekCache: cache}

	req := authedRequest(http.MethodGet, "/v1/users/me/passphrase/session", "", testClaims)
	w := httptest.NewRecorder()
	srv.GetPassphraseSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp api.KEKSessionStatus
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Active {
		t.Fatal("expected active = false")
	}
	if resp.ExpiresAt != nil {
		t.Fatal("expected no expires_at when inactive")
	}
}

func TestGetPassphraseSession_NoCacheConfigured(t *testing.T) {
	srv := &apiServer{} // no kekCache

	req := authedRequest(http.MethodGet, "/v1/users/me/passphrase/session", "", testClaims)
	w := httptest.NewRecorder()
	srv.GetPassphraseSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp api.KEKSessionStatus
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Active {
		t.Fatal("expected active = false when no cache configured")
	}
}

func TestGetPassphraseSession_Unauthenticated(t *testing.T) {
	srv := &apiServer{}
	req := authedRequest(http.MethodGet, "/v1/users/me/passphrase/session", "", nil)
	w := httptest.NewRecorder()
	srv.GetPassphraseSession(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

// --- VerifyPassphrase session metadata tests ---

func TestVerifyPassphrase_ReturnsSessionExpiresAt(t *testing.T) {
	cache := auth.NewKEKSessionCache(5 * time.Minute)
	srv := &apiServer{
		userKeyMaterials: mem.NewUserKeyMaterialStore(),
		kekCache:         cache,
	}
	passphrase := "correct horse battery staple"

	// Set passphrase.
	setReq := authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"passphrase":"`+passphrase+`"}`, testClaims)
	w := httptest.NewRecorder()
	srv.SetPassphrase(w, setReq)
	if w.Code != http.StatusOK {
		t.Fatalf("set: status = %d", w.Code)
	}

	// Verify.
	before := time.Now()
	verifyReq := authedRequest(http.MethodPost, "/v1/users/me/passphrase/verify",
		`{"passphrase":"`+passphrase+`"}`, testClaims)
	w = httptest.NewRecorder()
	srv.VerifyPassphrase(w, verifyReq)
	if w.Code != http.StatusOK {
		t.Fatalf("verify: status = %d", w.Code)
	}

	var resp api.VerifyPassphraseResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.Valid {
		t.Fatal("expected valid = true")
	}
	if resp.SessionExpiresAt == nil {
		t.Fatal("expected session_expires_at in response")
	}

	// Should be ~5 minutes from now.
	expectedMin := before.Add(5 * time.Minute)
	if resp.SessionExpiresAt.Before(expectedMin.Add(-time.Second)) {
		t.Fatalf("session_expires_at %v is too early (expected ~%v)", *resp.SessionExpiresAt, expectedMin)
	}
}

// --- Auto-escrow tests ---

// mockEscrowClient records EscrowStore calls for testing auto-escrow.
type mockEscrowClient struct {
	failingTransmitClient
	escrowCalls []enclave.EscrowStoreRequest
	escrowErr   error
}

func (c *mockEscrowClient) TransmitKEK(_ context.Context, _ enclave.TransmitKEKRequest) (enclave.TransmitKEKResponse, error) {
	return enclave.TransmitKEKResponse{Stored: true}, nil
}

func (c *mockEscrowClient) EscrowStore(_ context.Context, req enclave.EscrowStoreRequest) (enclave.EscrowStoreResponse, error) {
	if c.escrowErr != nil {
		return enclave.EscrowStoreResponse{}, c.escrowErr
	}
	c.escrowCalls = append(c.escrowCalls, req)
	return enclave.EscrowStoreResponse{EscrowID: "esc_test_" + req.GrantID}, nil
}

func TestAutoEscrowCredentials_EscrowsActiveAccounts(t *testing.T) {
	connAccounts := mem.NewConnectedAccountStore()
	ctx := context.Background()

	// Create two active accounts and one revoked.
	connAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_1", UserID: testClaims.Subject,
		Provider: model.ConnectedAccountProviderGmail,
		Status:   model.ConnectedAccountStatusActive,
	})
	connAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_2", UserID: testClaims.Subject,
		Provider: model.ConnectedAccountProviderGoogleCalendar,
		Status:   model.ConnectedAccountStatusActive,
	})
	connAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_3", UserID: testClaims.Subject,
		Provider: model.ConnectedAccountProviderOutlook,
		Status:   model.ConnectedAccountStatusRevoked,
	})

	// Seed vault with encrypted credentials.
	v := vault.NewMemVault()
	encMeta := vault.Metadata{Type: "oauth_refresh_token", Labels: map[string]string{vault.EncryptedLabel: "true"}}
	v.Put(ctx, "connected-accounts/"+testClaims.Subject+"/gmail", []byte("encrypted-gmail"), encMeta)
	v.Put(ctx, "connected-accounts/"+testClaims.Subject+"/google_calendar", []byte("encrypted-gcal"), encMeta)
	v.Put(ctx, "connected-accounts/"+testClaims.Subject+"/outlook", []byte("encrypted-outlook"), encMeta)

	mockClient := &mockEscrowClient{}
	srv := &apiServer{
		connectedAccounts: connAccounts,
		vault:             v,
		enclaveClient:     mockClient,
		escrowTTL:         7 * 24 * time.Hour,
	}

	escrowed := srv.autoEscrowCredentials(ctx, testClaims.Subject)

	// Should escrow 2 active accounts (not the revoked one).
	if escrowed != 2 {
		t.Fatalf("escrowed = %d, want 2", escrowed)
	}
	if len(mockClient.escrowCalls) != 2 {
		t.Fatalf("EscrowStore calls = %d, want 2", len(mockClient.escrowCalls))
	}

	// Verify escrow index was populated.
	if _, ok := srv.escrowIndex.Load("connected-accounts/" + testClaims.Subject + "/gmail"); !ok {
		t.Fatal("expected escrow index entry for gmail")
	}
	if _, ok := srv.escrowIndex.Load("connected-accounts/" + testClaims.Subject + "/google_calendar"); !ok {
		t.Fatal("expected escrow index entry for google_calendar")
	}
}

func TestAutoEscrowCredentials_NoConnectedAccounts(t *testing.T) {
	connAccounts := mem.NewConnectedAccountStore()
	mockClient := &mockEscrowClient{}
	srv := &apiServer{
		connectedAccounts: connAccounts,
		vault:             vault.NewMemVault(),
		enclaveClient:     mockClient,
		escrowTTL:         time.Hour,
	}

	escrowed := srv.autoEscrowCredentials(context.Background(), testClaims.Subject)
	if escrowed != 0 {
		t.Fatalf("escrowed = %d, want 0", escrowed)
	}
	if len(mockClient.escrowCalls) != 0 {
		t.Fatalf("EscrowStore calls = %d, want 0", len(mockClient.escrowCalls))
	}
}

func TestAutoEscrowCredentials_SkipsUnencryptedCredentials(t *testing.T) {
	connAccounts := mem.NewConnectedAccountStore()
	ctx := context.Background()

	connAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_1", UserID: testClaims.Subject,
		Provider: model.ConnectedAccountProviderGmail,
		Status:   model.ConnectedAccountStatusActive,
	})

	// Vault entry without encrypted label.
	v := vault.NewMemVault()
	v.Put(ctx, "connected-accounts/"+testClaims.Subject+"/gmail", []byte("plaintext"), vault.Metadata{Type: "oauth_refresh_token"})

	mockClient := &mockEscrowClient{}
	srv := &apiServer{
		connectedAccounts: connAccounts,
		vault:             v,
		enclaveClient:     mockClient,
		escrowTTL:         time.Hour,
	}

	escrowed := srv.autoEscrowCredentials(ctx, testClaims.Subject)
	if escrowed != 0 {
		t.Fatalf("escrowed = %d, want 0 (unencrypted should be skipped)", escrowed)
	}
}

func TestAutoEscrowCredentials_PartialFailure(t *testing.T) {
	connAccounts := mem.NewConnectedAccountStore()
	ctx := context.Background()

	connAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_1", UserID: testClaims.Subject,
		Provider: model.ConnectedAccountProviderGmail,
		Status:   model.ConnectedAccountStatusActive,
	})
	connAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_2", UserID: testClaims.Subject,
		Provider: model.ConnectedAccountProviderGoogleCalendar,
		Status:   model.ConnectedAccountStatusActive,
	})

	v := vault.NewMemVault()
	encMeta := vault.Metadata{Type: "oauth_refresh_token", Labels: map[string]string{vault.EncryptedLabel: "true"}}
	v.Put(ctx, "connected-accounts/"+testClaims.Subject+"/gmail", []byte("enc-gmail"), encMeta)
	// No vault entry for google_calendar — should fail gracefully.

	mockClient := &mockEscrowClient{}
	srv := &apiServer{
		connectedAccounts: connAccounts,
		vault:             v,
		enclaveClient:     mockClient,
		escrowTTL:         time.Hour,
	}

	escrowed := srv.autoEscrowCredentials(ctx, testClaims.Subject)
	if escrowed != 1 {
		t.Fatalf("escrowed = %d, want 1 (partial success)", escrowed)
	}
}

func TestActionTypesForProvider(t *testing.T) {
	tests := []struct {
		provider model.ConnectedAccountProvider
		want     []string
	}{
		{model.ConnectedAccountProviderGmail, []string{"email.send"}},
		{model.ConnectedAccountProviderGoogleCalendar, []string{"calendar.create"}},
		{model.ConnectedAccountProviderOutlook, []string{"email.send"}},
		{model.ConnectedAccountProviderMicrosoftCalendar, []string{"calendar.create"}},
		{"unknown_provider", nil},
	}
	for _, tt := range tests {
		got := actionTypesForProvider(tt.provider)
		if len(got) != len(tt.want) {
			t.Errorf("actionTypesForProvider(%q) = %v, want %v", tt.provider, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("actionTypesForProvider(%q)[%d] = %q, want %q", tt.provider, i, got[i], tt.want[i])
			}
		}
	}
}
