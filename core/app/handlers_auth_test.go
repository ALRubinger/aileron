package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/auth"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
)

// --- in-memory stores for tests ---

type memUserStore struct {
	mu    sync.Mutex
	users map[string]model.User
}

func newMemUserStore() *memUserStore {
	return &memUserStore{users: make(map[string]model.User)}
}

func (s *memUserStore) Create(_ context.Context, u model.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[u.ID] = u
	return nil
}

func (s *memUserStore) Get(_ context.Context, id string) (model.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return model.User{}, &store.ErrNotFound{Entity: "user", ID: id}
	}
	return u, nil
}

func (s *memUserStore) GetByEmail(context.Context, string) (model.User, error) {
	return model.User{}, &store.ErrNotFound{Entity: "user"}
}

func (s *memUserStore) GetByProviderSubject(context.Context, string, string) (model.User, error) {
	return model.User{}, &store.ErrNotFound{Entity: "user"}
}

func (s *memUserStore) List(context.Context, store.UserFilter) ([]model.User, error) {
	return nil, nil
}

func (s *memUserStore) Update(_ context.Context, u model.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[u.ID]; !ok {
		return &store.ErrNotFound{Entity: "user", ID: u.ID}
	}
	s.users[u.ID] = u
	return nil
}

type memEnterpriseStore struct {
	mu   sync.Mutex
	ents map[string]model.Enterprise
}

func newMemEnterpriseStore() *memEnterpriseStore {
	return &memEnterpriseStore{ents: make(map[string]model.Enterprise)}
}

func (s *memEnterpriseStore) Create(_ context.Context, e model.Enterprise) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ents[e.ID] = e
	return nil
}

func (s *memEnterpriseStore) Get(_ context.Context, id string) (model.Enterprise, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.ents[id]
	if !ok {
		return model.Enterprise{}, &store.ErrNotFound{Entity: "enterprise", ID: id}
	}
	return e, nil
}

func (s *memEnterpriseStore) GetBySlug(context.Context, string) (model.Enterprise, error) {
	return model.Enterprise{}, &store.ErrNotFound{Entity: "enterprise"}
}

func (s *memEnterpriseStore) Update(_ context.Context, e model.Enterprise) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ents[e.ID]; !ok {
		return &store.ErrNotFound{Entity: "enterprise", ID: e.ID}
	}
	s.ents[e.ID] = e
	return nil
}

// --- test helpers ---

func newAuthServer(t *testing.T) (*apiServer, *memUserStore, *memEnterpriseStore) {
	t.Helper()
	us := newMemUserStore()
	es := newMemEnterpriseStore()

	now := time.Now().UTC()
	_ = us.Create(context.Background(), model.User{
		ID:           "usr_test1",
		EnterpriseID: "ent_test1",
		Email:        "alice@example.com",
		DisplayName:  "Alice",
		Role:         model.UserRoleOwner,
		Status:       model.UserStatusActive,
		AuthProvider: "google",
		CreatedAt:    now,
		UpdatedAt:    now,
	})
	_ = es.Create(context.Background(), model.Enterprise{
		ID:           "ent_test1",
		Name:         "Test Corp",
		Slug:         "test-corp",
		Plan:         model.EnterprisePlanFree,
		BillingEmail: "billing@example.com",
		CreatedAt:    now,
		UpdatedAt:    now,
	})

	return &apiServer{
		users:       us,
		enterprises: es,
	}, us, es
}

func authedRequest(method, path, body string, claims *auth.Claims) *http.Request {
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if claims != nil {
		ctx := auth.ContextWithClaims(req.Context(), claims)
		req = req.WithContext(ctx)
	}
	return req
}

var ownerClaims = &auth.Claims{
	EnterpriseID: "ent_test1",
	Email:        "alice@example.com",
	Role:         "owner",
}

var memberClaims = &auth.Claims{
	EnterpriseID: "ent_test1",
	Email:        "bob@example.com",
	Role:         "member",
}

func init() {
	ownerClaims.Subject = "usr_test1"
	memberClaims.Subject = "usr_test2"
}

// --- tests ---

func TestGetCurrentUser(t *testing.T) {
	srv, _, _ := newAuthServer(t)

	t.Run("authenticated", func(t *testing.T) {
		w := httptest.NewRecorder()
		srv.GetCurrentUser(w, authedRequest("GET", "/v1/users/me", "", ownerClaims))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var u api.User
		json.NewDecoder(w.Body).Decode(&u)
		if u.Email != "alice@example.com" {
			t.Errorf("email = %q, want %q", u.Email, "alice@example.com")
		}
		if u.DisplayName != "Alice" {
			t.Errorf("display_name = %q, want %q", u.DisplayName, "Alice")
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		w := httptest.NewRecorder()
		srv.GetCurrentUser(w, authedRequest("GET", "/v1/users/me", "", nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
	})
}

func TestGetCurrentEnterprise(t *testing.T) {
	srv, _, _ := newAuthServer(t)

	w := httptest.NewRecorder()
	srv.GetCurrentEnterprise(w, authedRequest("GET", "/v1/enterprises/me", "", ownerClaims))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var e api.Enterprise
	json.NewDecoder(w.Body).Decode(&e)
	if e.Name != "Test Corp" {
		t.Errorf("name = %q, want %q", e.Name, "Test Corp")
	}
}

func TestUpdateCurrentUser(t *testing.T) {
	t.Run("update display name", func(t *testing.T) {
		srv, us, _ := newAuthServer(t)

		w := httptest.NewRecorder()
		srv.UpdateCurrentUser(w, authedRequest("PATCH", "/v1/users/me",
			`{"display_name":"Alice Smith"}`, ownerClaims))

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
		}

		var u api.User
		json.NewDecoder(w.Body).Decode(&u)
		if u.DisplayName != "Alice Smith" {
			t.Errorf("display_name = %q, want %q", u.DisplayName, "Alice Smith")
		}

		// Verify persisted.
		stored, _ := us.Get(context.Background(), "usr_test1")
		if stored.DisplayName != "Alice Smith" {
			t.Errorf("stored display_name = %q, want %q", stored.DisplayName, "Alice Smith")
		}
	})

	t.Run("empty body leaves fields unchanged", func(t *testing.T) {
		srv, _, _ := newAuthServer(t)

		w := httptest.NewRecorder()
		srv.UpdateCurrentUser(w, authedRequest("PATCH", "/v1/users/me", `{}`, ownerClaims))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}

		var u api.User
		json.NewDecoder(w.Body).Decode(&u)
		if u.DisplayName != "Alice" {
			t.Errorf("display_name = %q, want %q (should be unchanged)", u.DisplayName, "Alice")
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		srv, _, _ := newAuthServer(t)

		w := httptest.NewRecorder()
		srv.UpdateCurrentUser(w, authedRequest("PATCH", "/v1/users/me",
			`{"display_name":"X"}`, nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
	})
}

func TestUpdateCurrentEnterprise(t *testing.T) {
	t.Run("owner can update", func(t *testing.T) {
		srv, _, es := newAuthServer(t)

		w := httptest.NewRecorder()
		srv.UpdateCurrentEnterprise(w, authedRequest("PATCH", "/v1/enterprises/me",
			`{"name":"New Corp","billing_email":"new@example.com","sso_required":true,"allowed_auth_providers":["google","okta"],"allowed_email_domains":["example.com"]}`,
			ownerClaims))

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
		}

		var e api.Enterprise
		json.NewDecoder(w.Body).Decode(&e)
		if e.Name != "New Corp" {
			t.Errorf("name = %q, want %q", e.Name, "New Corp")
		}
		if e.BillingEmail != "new@example.com" {
			t.Errorf("billing_email = %q, want %q", e.BillingEmail, "new@example.com")
		}
		if e.SsoRequired == nil || !*e.SsoRequired {
			t.Error("sso_required should be true")
		}
		if e.AllowedAuthProviders == nil || len(*e.AllowedAuthProviders) != 2 {
			t.Errorf("allowed_auth_providers = %v, want [google okta]", e.AllowedAuthProviders)
		}

		// Verify persisted.
		stored, _ := es.Get(context.Background(), "ent_test1")
		if stored.Name != "New Corp" {
			t.Errorf("stored name = %q, want %q", stored.Name, "New Corp")
		}
	})

	t.Run("member is forbidden", func(t *testing.T) {
		srv, _, _ := newAuthServer(t)

		w := httptest.NewRecorder()
		srv.UpdateCurrentEnterprise(w, authedRequest("PATCH", "/v1/enterprises/me",
			`{"name":"Hacked"}`, memberClaims))
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		srv, _, _ := newAuthServer(t)

		w := httptest.NewRecorder()
		srv.UpdateCurrentEnterprise(w, authedRequest("PATCH", "/v1/enterprises/me",
			`{"name":"X"}`, nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
	})

	t.Run("partial update", func(t *testing.T) {
		srv, _, _ := newAuthServer(t)

		w := httptest.NewRecorder()
		srv.UpdateCurrentEnterprise(w, authedRequest("PATCH", "/v1/enterprises/me",
			`{"name":"Updated Only"}`, ownerClaims))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}

		var e api.Enterprise
		json.NewDecoder(w.Body).Decode(&e)
		if e.Name != "Updated Only" {
			t.Errorf("name = %q, want %q", e.Name, "Updated Only")
		}
		if e.BillingEmail != "billing@example.com" {
			t.Errorf("billing_email = %q, want %q (should be unchanged)", e.BillingEmail, "billing@example.com")
		}
	})
}

func TestUpdateCurrentEnterprise_AdminCanUpdate(t *testing.T) {
	srv, us, _ := newAuthServer(t)

	// Add an admin user.
	_ = us.Create(context.Background(), model.User{
		ID:           "usr_admin1",
		EnterpriseID: "ent_test1",
		Email:        "admin@example.com",
		DisplayName:  "Admin",
		Role:         model.UserRoleAdmin,
		Status:       model.UserStatusActive,
		AuthProvider: "google",
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	})

	adminClaims := &auth.Claims{
		EnterpriseID: "ent_test1",
		Email:        "admin@example.com",
		Role:         "admin",
	}
	adminClaims.Subject = "usr_admin1"

	w := httptest.NewRecorder()
	srv.UpdateCurrentEnterprise(w, authedRequest("PATCH", "/v1/enterprises/me",
		`{"name":"Admin Updated"}`, adminClaims))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var e api.Enterprise
	json.NewDecoder(w.Body).Decode(&e)
	if e.Name != "Admin Updated" {
		t.Errorf("name = %q, want %q", e.Name, "Admin Updated")
	}
}

func TestEnterpriseToAPI_SnakeCaseJSON(t *testing.T) {
	e := model.Enterprise{
		ID:                  "ent_1",
		Name:                "Test Corp",
		Slug:                "test-corp",
		Plan:                model.EnterprisePlanFree,
		Personal:            true,
		BillingEmail:        "billing@example.com",
		SSORequired:         true,
		AllowedAuthProviders: []string{"google", "okta"},
		AllowedEmailDomains:  []string{"example.com"},
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	b, _ := json.Marshal(enterpriseToAPI(e))
	m := make(map[string]any)
	json.Unmarshal(b, &m)

	for _, key := range []string{"id", "name", "slug", "plan", "personal", "billing_email", "sso_required", "allowed_auth_providers", "allowed_email_domains", "created_at", "updated_at"} {
		if _, ok := m[key]; !ok {
			t.Errorf("expected snake_case key %q in JSON output", key)
		}
	}
}

func TestEnterpriseToAPI_OmitsEmptyOptionals(t *testing.T) {
	e := model.Enterprise{
		ID:           "ent_1",
		Name:         "Test Corp",
		Slug:         "test-corp",
		Plan:         model.EnterprisePlanFree,
		BillingEmail: "billing@example.com",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	b, _ := json.Marshal(enterpriseToAPI(e))
	m := make(map[string]any)
	json.Unmarshal(b, &m)

	for _, key := range []string{"personal", "sso_required", "allowed_auth_providers", "allowed_email_domains"} {
		if _, ok := m[key]; ok {
			t.Errorf("expected key %q to be omitted when zero value, but it was present", key)
		}
	}
}

func TestUserToAPI_SnakeCaseJSON(t *testing.T) {
	u := model.User{
		ID:           "usr_1",
		EnterpriseID: "ent_1",
		Email:        "test@example.com",
		DisplayName:  "Test",
		Role:         model.UserRoleOwner,
		Status:       model.UserStatusActive,
		AuthProvider: "google",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	b, _ := json.Marshal(userToAPI(u))
	m := make(map[string]any)
	json.Unmarshal(b, &m)

	for _, key := range []string{"id", "enterprise_id", "email", "display_name", "role", "status", "auth_provider", "created_at", "updated_at"} {
		if _, ok := m[key]; !ok {
			t.Errorf("expected snake_case key %q in JSON output", key)
		}
	}
}
