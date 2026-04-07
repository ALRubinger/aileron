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

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/auth"
	"github.com/ALRubinger/aileron/core/crypto"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
	"github.com/ALRubinger/aileron/enclave"
	"github.com/golang-jwt/jwt/v5"
)

// kekVerificationConstant is the known plaintext encrypted with the KEK.
// Duplicated here for test assertions — must match the client-side constant.
var testVerificationConstant = []byte("aileron-kek-verification-ok")

// mockEscrowClient records EscrowStore calls for testing auto-escrow.
type mockEscrowClient struct {
	escrowCalls []enclave.EscrowStoreRequest
	escrowErr   error
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
func (c *mockEscrowClient) EscrowRevoke(_ context.Context, _ enclave.EscrowRevokeRequest) error {
	return nil
}
func (c *mockEscrowClient) Close() error { return nil }

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
