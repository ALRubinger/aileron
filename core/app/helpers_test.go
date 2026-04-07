package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/ALRubinger/aileron/core/auth"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
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
