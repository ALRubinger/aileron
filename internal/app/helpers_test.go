package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/auth"
	"github.com/ALRubinger/aileron/internal/crypto"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/store"
	"github.com/ALRubinger/aileron/internal/store/mem"
	"github.com/ALRubinger/aileron/internal/vault"
)

// stubUserStore is a minimal non-nil UserStore to signal auth is enabled.
type stubUserStore struct{}

func (s *stubUserStore) Create(_ context.Context, _ model.User) error { return nil }
func (s *stubUserStore) Get(_ context.Context, _ string) (model.User, error) {
	return model.User{}, nil
}
func (s *stubUserStore) GetByEmail(_ context.Context, _ string) (model.User, error) {
	return model.User{}, nil
}
func (s *stubUserStore) List(_ context.Context, _ store.UserFilter) ([]model.User, error) {
	return nil, nil
}
func (s *stubUserStore) Update(_ context.Context, _ model.User) error { return nil }

func mcpRequest(method, path, body string, claims *auth.Claims) *http.Request {
	reader := strings.NewReader(body)
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if claims != nil {
		ctx := auth.ContextWithClaims(req.Context(), claims)
		req = req.WithContext(ctx)
	}
	return req
}

var userAClaims = &auth.Claims{
	EnterpriseID: "ent_1",
	Email:        "usera@example.com",
	Role:         "member",
}

var userBClaims = &auth.Claims{
	EnterpriseID: "ent_1",
	Email:        "userb@example.com",
	Role:         "member",
}

func init() {
	userAClaims.Subject = "usr_a"
	userBClaims.Subject = "usr_b"
}

// testKEK returns a deterministic 32-byte test KEK.
var testKEK = func() []byte {
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	return kek
}()

// enableVaultEncryption adds KEK session infrastructure to a test server
// and unlocks the vault for the given user. Tokens stored via the vault
// must use UserScopedVault to encrypt.
func enableVaultEncryption(srv *apiServer, userID string) {
	if srv.userKeyMaterials == nil {
		srv.userKeyMaterials = mem.NewUserKeyMaterialStore()
	}
	verification, _ := crypto.Encrypt([]byte("aileron-kek-ok"), testKEK)
	srv.userKeyMaterials.Create(context.Background(), model.UserKeyMaterial{
		UserID:          userID,
		Salt:            make([]byte, 16),
		KEKVerification: verification,
	})
	if srv.kekSessionCache == nil {
		srv.kekSessionCache = auth.NewKEKSessionCache(24 * time.Hour)
	}
	srv.kekSessionCache.Set(userID, testKEK)
}

// storeEncryptedToken stores an encrypted token in the vault for a connected account.
func storeEncryptedToken(srv *apiServer, path string, tokenJSON []byte) {
	ciphertext, _ := crypto.Encrypt(tokenJSON, testKEK)
	srv.vault.Put(context.Background(), path, ciphertext, vault.Metadata{
		Type:   "oauth_refresh_token",
		Labels: map[string]string{vault.EncryptedLabel: "true"},
	})
}
