package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/auth"
	"github.com/ALRubinger/aileron/internal/crypto"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/store/mem"
	"github.com/ALRubinger/aileron/internal/vault"
	"github.com/ALRubinger/aileron/internal/enclave"
	"github.com/golang-jwt/jwt/v5"
)

// kekVerificationConstant is the known plaintext encrypted with the KEK.
// Duplicated here for test assertions — must match the client-side constant.
var testVerificationConstant = []byte("aileron-kek-verification-ok")

// mockEscrowClient records EscrowStore calls for testing auto-escrow.
type mockEscrowClient struct {
	escrowCalls  []enclave.EscrowStoreRequest
	escrowErr    error
	retrieveData map[string][]byte // escrow ID → plaintext credential
}

func (c *mockEscrowClient) Attest(_ context.Context, _ enclave.AttestationRequest) (enclave.AttestationResponse, error) {
	return enclave.AttestationResponse{}, nil
}
func (c *mockEscrowClient) EstablishSession(_ context.Context, _ enclave.SessionRequest) (enclave.SessionResponse, error) {
	return enclave.SessionResponse{SessionID: "sess_test", ExpiresAt: time.Now().Add(24 * time.Hour).Format(time.RFC3339)}, nil
}
func (c *mockEscrowClient) TransmitKEK(_ context.Context, _ enclave.TransmitKEKRequest) (enclave.TransmitKEKResponse, error) {
	return enclave.TransmitKEKResponse{Stored: true}, nil
}
func (c *mockEscrowClient) OAuthExchange(_ context.Context, _ enclave.OAuthExchangeRequest) (enclave.OAuthExchangeResponse, error) {
	return enclave.OAuthExchangeResponse{}, nil
}
func (c *mockEscrowClient) Execute(_ context.Context, _ enclave.ExecuteRequest) (enclave.ExecuteResponse, error) {
	return enclave.ExecuteResponse{}, nil
}
func (c *mockEscrowClient) EscrowStore(_ context.Context, req enclave.EscrowStoreRequest) (enclave.EscrowStoreResponse, error) {
	if c.escrowErr != nil {
		return enclave.EscrowStoreResponse{}, c.escrowErr
	}
	c.escrowCalls = append(c.escrowCalls, req)
	return enclave.EscrowStoreResponse{EscrowID: "esc_test_" + req.GrantID}, nil
}
func (c *mockEscrowClient) EscrowRetrieve(_ context.Context, req enclave.EscrowRetrieveRequest) (enclave.EscrowRetrieveResponse, error) {
	if c.retrieveData != nil {
		if data, ok := c.retrieveData[req.EscrowID]; ok {
			return enclave.EscrowRetrieveResponse{Credential: data}, nil
		}
	}
	return enclave.EscrowRetrieveResponse{}, enclave.ErrEscrowNotFound
}
func (c *mockEscrowClient) EscrowList(_ context.Context) (enclave.EscrowListResponse, error) {
	return enclave.EscrowListResponse{}, nil
}
func (c *mockEscrowClient) EscrowRevoke(_ context.Context, _ enclave.EscrowRevokeRequest) error {
	return nil
}
func (c *mockEscrowClient) Ready(_ context.Context) error { return nil }
func (c *mockEscrowClient) Close() error                  { return nil }

// failingTransmitClient is an enclave client whose TransmitKEK always fails.
type failingTransmitClient struct {
	mockEscrowClient
	err error
}

func (c *failingTransmitClient) TransmitKEK(_ context.Context, _ enclave.TransmitKEKRequest) (enclave.TransmitKEKResponse, error) {
	return enclave.TransmitKEKResponse{}, c.err
}

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
	inner     *mem.UserKeyMaterialStore
	getErr    error // if non-nil, Get always returns this
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

// clientSetPassphrase simulates the client-side flow: generate salt, derive
// KEK, encrypt verification constant, and send to server.
func clientSetPassphrase(t *testing.T, srv *apiServer, passphrase string) {
	t.Helper()
	salt, err := crypto.GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt: %v", err)
	}
	kek, err := crypto.DeriveKEK([]byte(passphrase), salt)
	if err != nil {
		t.Fatalf("DeriveKEK: %v", err)
	}
	verification, err := crypto.Encrypt(testVerificationConstant, kek)
	if err != nil {
		t.Fatalf("Encrypt verification: %v", err)
	}

	body := map[string]string{
		"salt":             base64.StdEncoding.EncodeToString(salt),
		"kek_verification": base64.StdEncoding.EncodeToString(verification),
	}
	bodyJSON, _ := json.Marshal(body)

	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase", string(bodyJSON), testClaims)
	w := httptest.NewRecorder()
	srv.SetPassphrase(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("SetPassphrase: status = %d; body: %s", w.Code, w.Body.String())
	}
}

// --- SetPassphrase tests ---

func TestSetPassphrase(t *testing.T) {
	srv := passphraseServer()
	salt := make([]byte, 16)
	verification := []byte("some-encrypted-blob")
	body := map[string]string{
		"salt":             base64.StdEncoding.EncodeToString(salt),
		"kek_verification": base64.StdEncoding.EncodeToString(verification),
	}
	bodyJSON, _ := json.Marshal(body)

	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase", string(bodyJSON), testClaims)
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

func TestSetPassphrase_InvalidSaltLength(t *testing.T) {
	srv := passphraseServer()
	salt := make([]byte, 8) // too short
	body := map[string]string{
		"salt":             base64.StdEncoding.EncodeToString(salt),
		"kek_verification": base64.StdEncoding.EncodeToString([]byte("blob")),
	}
	bodyJSON, _ := json.Marshal(body)

	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase", string(bodyJSON), testClaims)
	w := httptest.NewRecorder()
	srv.SetPassphrase(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestSetPassphrase_EmptyVerification(t *testing.T) {
	srv := passphraseServer()
	body := map[string]string{
		"salt":             base64.StdEncoding.EncodeToString(make([]byte, 16)),
		"kek_verification": "",
	}
	bodyJSON, _ := json.Marshal(body)

	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase", string(bodyJSON), testClaims)
	w := httptest.NewRecorder()
	srv.SetPassphrase(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestSetPassphrase_Rotation(t *testing.T) {
	srv := passphraseServer()

	// Set initial passphrase.
	clientSetPassphrase(t, srv, "first passphrase here")

	// Rotate.
	clientSetPassphrase(t, srv, "second passphrase here")
}

func TestSetPassphrase_Unauthenticated(t *testing.T) {
	srv := passphraseServer()
	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase",
		`{"salt":"AAAAAAAAAAAAAAAAAAAAAA==","kek_verification":"AAAA"}`, nil)
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
		`{"salt":"AAAAAAAAAAAAAAAAAAAAAA==","kek_verification":"AAAA"}`, testClaims)
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

	body := map[string]string{
		"salt":             base64.StdEncoding.EncodeToString(make([]byte, 16)),
		"kek_verification": base64.StdEncoding.EncodeToString([]byte("blob")),
	}
	bodyJSON, _ := json.Marshal(body)
	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase", string(bodyJSON), testClaims)
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

	body := map[string]string{
		"salt":             base64.StdEncoding.EncodeToString(make([]byte, 16)),
		"kek_verification": base64.StdEncoding.EncodeToString([]byte("blob")),
	}
	bodyJSON, _ := json.Marshal(body)
	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase", string(bodyJSON), testClaims)
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
	body := map[string]string{
		"salt":             base64.StdEncoding.EncodeToString(make([]byte, 16)),
		"kek_verification": base64.StdEncoding.EncodeToString([]byte("blob")),
	}
	bodyJSON, _ := json.Marshal(body)
	req := authedRequest(http.MethodPost, "/v1/users/me/passphrase", string(bodyJSON), testClaims)
	w := httptest.NewRecorder()
	srv.SetPassphrase(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("initial set: status = %d", w.Code)
	}

	// Now inject update error and try rotation.
	mock.updateErr = errors.New("db write failed")
	req = authedRequest(http.MethodPost, "/v1/users/me/passphrase", string(bodyJSON), testClaims)
	w = httptest.NewRecorder()
	srv.SetPassphrase(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

// --- GetPassphraseVerification tests ---

func TestGetPassphraseVerification_NoPassphrase(t *testing.T) {
	srv := passphraseServer()
	req := authedRequest(http.MethodGet, "/v1/users/me/passphrase/verification", "", testClaims)
	w := httptest.NewRecorder()

	srv.GetPassphraseVerification(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp api.PassphraseVerificationResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.HasPassphrase {
		t.Fatal("expected has_passphrase = false")
	}
	if resp.KekVerification != nil {
		t.Fatal("expected no kek_verification when no passphrase set")
	}
}

func TestGetPassphraseVerification_WithPassphrase(t *testing.T) {
	srv := passphraseServer()
	clientSetPassphrase(t, srv, "correct horse battery staple")

	req := authedRequest(http.MethodGet, "/v1/users/me/passphrase/verification", "", testClaims)
	w := httptest.NewRecorder()
	srv.GetPassphraseVerification(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp api.PassphraseVerificationResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.HasPassphrase {
		t.Fatal("expected has_passphrase = true")
	}
	if resp.KekVerification == nil || len(*resp.KekVerification) == 0 {
		t.Fatal("expected non-empty kek_verification")
	}
}

func TestGetPassphraseVerification_Unauthenticated(t *testing.T) {
	srv := passphraseServer()
	req := authedRequest(http.MethodGet, "/v1/users/me/passphrase/verification", "", nil)
	w := httptest.NewRecorder()
	srv.GetPassphraseVerification(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestGetPassphraseVerification_AuthNotEnabled(t *testing.T) {
	srv := &apiServer{}
	req := authedRequest(http.MethodGet, "/v1/users/me/passphrase/verification", "", testClaims)
	w := httptest.NewRecorder()
	srv.GetPassphraseVerification(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
}

func TestGetPassphraseVerification_GetStoreError(t *testing.T) {
	mock := newMockKeyMaterialStore()
	mock.getErr = errors.New("db timeout")
	srv := &apiServer{userKeyMaterials: mock}

	req := authedRequest(http.MethodGet, "/v1/users/me/passphrase/verification", "", testClaims)
	w := httptest.NewRecorder()
	srv.GetPassphraseVerification(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
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
	clientSetPassphrase(t, srv, "correct horse battery staple")

	req := authedRequest(http.MethodGet, "/v1/users/me/passphrase/salt", "", testClaims)
	w := httptest.NewRecorder()
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
	srv := &apiServer{}
	req := authedRequest(http.MethodGet, "/v1/users/me/passphrase/salt", "", testClaims)
	w := httptest.NewRecorder()
	srv.GetPassphraseSalt(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotImplemented)
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

// --- Auto-escrow tests ---

func TestAutoEscrowCredentials_EscrowsActiveAccounts(t *testing.T) {
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
	connAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_3", UserID: testClaims.Subject,
		Provider: model.ConnectedAccountProviderOutlook,
		Status:   model.ConnectedAccountStatusRevoked,
	})

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

	if escrowed != 2 {
		t.Fatalf("escrowed = %d, want 2", escrowed)
	}
	if len(mockClient.escrowCalls) != 2 {
		t.Fatalf("EscrowStore calls = %d, want 2", len(mockClient.escrowCalls))
	}

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
}

func TestAutoEscrowCredentials_SkipsUnencryptedCredentials(t *testing.T) {
	connAccounts := mem.NewConnectedAccountStore()
	ctx := context.Background()

	connAccounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_1", UserID: testClaims.Subject,
		Provider: model.ConnectedAccountProviderGmail,
		Status:   model.ConnectedAccountStatusActive,
	})

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

// --- UnlockVault / LockVault / GetVaultStatus tests ---

func unlockServer() *apiServer {
	srv := passphraseServer()
	srv.kekSessionCache = auth.NewKEKSessionCache(24 * time.Hour)
	srv.users = &stubUserStore{}

	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	verification, _ := crypto.Encrypt(testVerificationConstant, kek)
	srv.userKeyMaterials.Create(context.Background(), model.UserKeyMaterial{
		UserID:          testClaims.Subject,
		Salt:            make([]byte, 16),
		KEKVerification: verification,
	})
	return srv
}

func TestUnlockVault_Success(t *testing.T) {
	srv := unlockServer()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	body, _ := json.Marshal(api.UnlockVaultRequest{Kek: kek})

	w := httptest.NewRecorder()
	r := authedRequest(http.MethodPost, "/v1/users/me/passphrase/unlock", string(body), testClaims)
	srv.UnlockVault(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp api.VaultStatusResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Locked {
		t.Error("expected unlocked")
	}
	if !resp.HasPassphrase {
		t.Error("expected has_passphrase=true")
	}
	if resp.ExpiresAt == nil {
		t.Error("expected expires_at")
	}
	if srv.kekSessionCache.Get(testClaims.Subject) == nil {
		t.Error("expected KEK in session cache")
	}
}

func TestUnlockVault_BlockedWhenTEEEnabled(t *testing.T) {
	srv := unlockServer()
	srv.enclaveClient = &mockEscrowClient{} // non-nil = TEE enabled

	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	body, _ := json.Marshal(api.UnlockVaultRequest{Kek: kek})

	w := httptest.NewRecorder()
	r := authedRequest(http.MethodPost, "/v1/users/me/passphrase/unlock", string(body), testClaims)
	srv.UnlockVault(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when TEE enabled, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUnlockVault_WrongKEK(t *testing.T) {
	srv := unlockServer()
	body, _ := json.Marshal(api.UnlockVaultRequest{Kek: make([]byte, 32)})

	w := httptest.NewRecorder()
	r := authedRequest(http.MethodPost, "/v1/users/me/passphrase/unlock", string(body), testClaims)
	srv.UnlockVault(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUnlockVault_NoPassphraseSet(t *testing.T) {
	srv := passphraseServer()
	srv.kekSessionCache = auth.NewKEKSessionCache(24 * time.Hour)
	body, _ := json.Marshal(api.UnlockVaultRequest{Kek: make([]byte, 32)})

	w := httptest.NewRecorder()
	r := authedRequest(http.MethodPost, "/v1/users/me/passphrase/unlock", string(body), testClaims)
	srv.UnlockVault(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUnlockVault_BadKEKLength(t *testing.T) {
	srv := unlockServer()
	body, _ := json.Marshal(api.UnlockVaultRequest{Kek: []byte("short")})

	w := httptest.NewRecorder()
	r := authedRequest(http.MethodPost, "/v1/users/me/passphrase/unlock", string(body), testClaims)
	srv.UnlockVault(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUnlockVault_Unauthenticated(t *testing.T) {
	srv := unlockServer()
	w := httptest.NewRecorder()
	r := authedRequest(http.MethodPost, "/v1/users/me/passphrase/unlock", "{}", nil)
	srv.UnlockVault(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestLockVault_ClearsSession(t *testing.T) {
	srv := unlockServer()
	srv.kekSessionCache.Set(testClaims.Subject, make([]byte, 32))

	w := httptest.NewRecorder()
	r := authedRequest(http.MethodPost, "/v1/users/me/passphrase/lock", "", testClaims)
	srv.LockVault(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp api.VaultStatusResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.Locked {
		t.Error("expected locked=true")
	}
	if srv.kekSessionCache.Get(testClaims.Subject) != nil {
		t.Error("expected KEK cleared")
	}
}

func TestGetVaultStatus_Locked(t *testing.T) {
	srv := unlockServer()

	w := httptest.NewRecorder()
	r := authedRequest(http.MethodGet, "/v1/users/me/vault/status", "", testClaims)
	srv.GetVaultStatus(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp api.VaultStatusResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.Locked {
		t.Error("expected locked=true")
	}
	if !resp.HasPassphrase {
		t.Error("expected has_passphrase=true")
	}
}

func TestGetVaultStatus_Unlocked(t *testing.T) {
	srv := unlockServer()
	srv.kekSessionCache.Set(testClaims.Subject, make([]byte, 32))

	w := httptest.NewRecorder()
	r := authedRequest(http.MethodGet, "/v1/users/me/vault/status", "", testClaims)
	srv.GetVaultStatus(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp api.VaultStatusResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Locked {
		t.Error("expected locked=false")
	}
	if resp.ExpiresAt == nil {
		t.Error("expected expires_at")
	}
}

func TestGetVaultStatus_TEESessionUnlocked(t *testing.T) {
	// When TEE has an active session, vault should report unlocked even
	// though the server-side KEK cache is empty.
	srv := unlockServer()
	// Don't put KEK in session cache — simulates TEE-only unlock.

	// Set up active TEE session.
	srv.teeState = newTeeState()
	srv.teeState.userSessions[testClaims.Subject] = time.Now().Add(1 * time.Hour)

	w := httptest.NewRecorder()
	r := authedRequest(http.MethodGet, "/v1/users/me/vault/status", "", testClaims)
	srv.GetVaultStatus(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp api.VaultStatusResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Locked {
		t.Error("expected locked=false with active TEE session")
	}
	if resp.ExpiresAt == nil {
		t.Error("expected expires_at from TEE session")
	}
}

// TestGetVaultStatus_PassphraseSurvivesNewServerInstance verifies that a
// passphrase set on one apiServer is visible from a second apiServer backed
// by the same store. This is the contract that was broken when the
// userKeyMaterialStore used an in-memory store in production wiring —
// each server restart created a fresh empty store, so has_passphrase was
// always false after a redeploy.
func TestGetVaultStatus_PassphraseSurvivesNewServerInstance(t *testing.T) {
	// Shared backing store — in production this is PostgreSQL.
	sharedStore := mem.NewUserKeyMaterialStore()

	// Server 1: user sets a passphrase.
	srv1 := &apiServer{
		userKeyMaterials: sharedStore,
		kekSessionCache:  auth.NewKEKSessionCache(24 * time.Hour),
	}
	clientSetPassphrase(t, srv1, "correct horse battery staple")

	// Server 2: simulate a restart — new apiServer, same backing store.
	srv2 := &apiServer{
		userKeyMaterials: sharedStore,
		kekSessionCache:  auth.NewKEKSessionCache(24 * time.Hour),
	}

	w := httptest.NewRecorder()
	r := authedRequest(http.MethodGet, "/v1/users/me/vault/status", "", testClaims)
	srv2.GetVaultStatus(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp api.VaultStatusResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.HasPassphrase {
		t.Error("expected has_passphrase=true after server restart with same backing store")
	}
	if !resp.Locked {
		t.Error("expected locked=true (KEK session cache is fresh)")
	}
}

func TestGetVaultStatus_NoPassphrase(t *testing.T) {
	srv := passphraseServer()
	srv.kekSessionCache = auth.NewKEKSessionCache(24 * time.Hour)

	w := httptest.NewRecorder()
	r := authedRequest(http.MethodGet, "/v1/users/me/vault/status", "", testClaims)
	srv.GetVaultStatus(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp api.VaultStatusResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.HasPassphrase {
		t.Error("expected has_passphrase=false")
	}
}

func TestUnlockVault_AuthDisabled(t *testing.T) {
	srv := &apiServer{} // no kekSessionCache, no userKeyMaterials
	w := httptest.NewRecorder()
	r := authedRequest(http.MethodPost, "/v1/users/me/passphrase/unlock", "{}", testClaims)
	srv.UnlockVault(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestUnlockVault_InvalidBody(t *testing.T) {
	srv := unlockServer()
	w := httptest.NewRecorder()
	r := authedRequest(http.MethodPost, "/v1/users/me/passphrase/unlock", "not-json", testClaims)
	srv.UnlockVault(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestLockVault_AuthDisabled(t *testing.T) {
	srv := &apiServer{}
	w := httptest.NewRecorder()
	r := authedRequest(http.MethodPost, "/v1/users/me/passphrase/lock", "", testClaims)
	srv.LockVault(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestLockVault_Unauthenticated(t *testing.T) {
	srv := unlockServer()
	w := httptest.NewRecorder()
	r := authedRequest(http.MethodPost, "/v1/users/me/passphrase/lock", "", nil)
	srv.LockVault(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestGetVaultStatus_AuthDisabled(t *testing.T) {
	srv := &apiServer{}
	w := httptest.NewRecorder()
	r := authedRequest(http.MethodGet, "/v1/users/me/vault/status", "", testClaims)
	srv.GetVaultStatus(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestGetVaultStatus_Unauthenticated(t *testing.T) {
	srv := unlockServer()
	w := httptest.NewRecorder()
	r := authedRequest(http.MethodGet, "/v1/users/me/vault/status", "", nil)
	srv.GetVaultStatus(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestRequireKEK_AuthDisabled(t *testing.T) {
	srv := &apiServer{} // no kekSessionCache, no userKeyMaterials
	srv.users = &stubUserStore{}
	w := httptest.NewRecorder()
	r := authedRequest(http.MethodGet, "/test", "", testClaims)
	_, ok := srv.requireKEK(w, r)
	if ok {
		t.Fatal("expected failure when auth disabled")
	}
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestRequireKEK_InternalError(t *testing.T) {
	srv := unlockServer()
	srv.userKeyMaterials = &mockKeyMaterialStore{
		inner:  mem.NewUserKeyMaterialStore(),
		getErr: errors.New("db connection lost"),
	}

	w := httptest.NewRecorder()
	r := authedRequest(http.MethodGet, "/test", "", testClaims)
	_, ok := srv.requireKEK(w, r)
	if ok {
		t.Fatal("expected failure on internal error")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestUserVault_EscrowFallback(t *testing.T) {
	// Tier 2: KEK session cache exists but is empty for this user (vault
	// locked). Escrow index has an entry and the enclave client can retrieve
	// the plaintext. userVault should return an EscrowVault that succeeds.
	plaintext := []byte(`{"access_token":"xoxp-escrowed"}`)
	escrowID := "esc_test_slack"
	vaultPath := "connected-accounts/usr_escrow/slack"

	mockClient := &mockEscrowClient{
		retrieveData: map[string][]byte{escrowID: plaintext},
	}

	srv := &apiServer{
		vault:           vault.NewMemVault(),
		kekSessionCache: auth.NewKEKSessionCache(24 * time.Hour), // auth enabled, no KEK cached
		enclaveClient:   mockClient,
	}
	// Populate escrow index.
	srv.escrowIndex.Store(vaultPath, escrowID)

	vlt := srv.userVault("usr_escrow")

	// Get should retrieve the credential from escrow, not the vault.
	secret, err := vlt.Get(context.Background(), vaultPath)
	if err != nil {
		t.Fatalf("userVault.Get: %v", err)
	}
	if string(secret.Value) != string(plaintext) {
		t.Errorf("Value = %q, want %q", secret.Value, plaintext)
	}
}

func TestUserVault_NoEscrow_ReturnsLockedVault(t *testing.T) {
	// Tier 3: KEK session cache exists but empty, no enclave client.
	// userVault should return a vault that fails on Get.
	srv := &apiServer{
		vault:           vault.NewMemVault(),
		kekSessionCache: auth.NewKEKSessionCache(24 * time.Hour),
	}

	vlt := srv.userVault("usr_locked")
	_, err := vlt.Get(context.Background(), "connected-accounts/usr_locked/slack")
	if err == nil {
		t.Fatal("expected error from locked vault")
	}
}
