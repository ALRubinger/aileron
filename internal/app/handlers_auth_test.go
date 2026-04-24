package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/auth"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/store"
)

// --- in-memory stores for tests ---

// failingStore wraps a memUserStore but forces Update to fail.
type failingUserStore struct {
	*memUserStore
}

func (s *failingUserStore) Update(context.Context, model.User) error {
	return fmt.Errorf("simulated update failure")
}

// failingEnterpriseStore wraps a memEnterpriseStore but forces Update to fail.
type failingEnterpriseStore struct {
	*memEnterpriseStore
}

func (s *failingEnterpriseStore) Update(context.Context, model.Enterprise) error {
	return fmt.Errorf("simulated update failure")
}

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

// memUserAuthProviderStore is an in-memory user auth provider store for tests.
type memUserAuthProviderStore struct {
	mu    sync.Mutex
	links map[string]model.UserAuthProvider
}

func newMemUserAuthProviderStore() *memUserAuthProviderStore {
	return &memUserAuthProviderStore{links: make(map[string]model.UserAuthProvider)}
}

func (s *memUserAuthProviderStore) Create(_ context.Context, link model.UserAuthProvider) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.links[link.ID] = link
	return nil
}

func (s *memUserAuthProviderStore) GetByProviderSubject(_ context.Context, provider, subjectID string) (model.UserAuthProvider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, link := range s.links {
		if link.Provider == provider && link.SubjectID == subjectID {
			return link, nil
		}
	}
	return model.UserAuthProvider{}, &store.ErrNotFound{Entity: "user_auth_provider", ID: provider + "/" + subjectID}
}

func (s *memUserAuthProviderStore) ListByUser(_ context.Context, userID string) ([]model.UserAuthProvider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []model.UserAuthProvider
	for _, link := range s.links {
		if link.UserID == userID {
			result = append(result, link)
		}
	}
	return result, nil
}

func (s *memUserAuthProviderStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.links[id]; !ok {
		return &store.ErrNotFound{Entity: "user_auth_provider", ID: id}
	}
	delete(s.links, id)
	return nil
}

func (s *memUserAuthProviderStore) DeleteByUserAndProvider(_ context.Context, userID, provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, link := range s.links {
		if link.UserID == userID && link.Provider == provider {
			delete(s.links, id)
			return nil
		}
	}
	return &store.ErrNotFound{Entity: "user_auth_provider", ID: userID + "/" + provider}
}

// failingUserAuthProviderStore forces ListByUser to fail.
type failingUserAuthProviderStore struct {
	*memUserAuthProviderStore
}

func (s *failingUserAuthProviderStore) ListByUser(context.Context, string) ([]model.UserAuthProvider, error) {
	return nil, fmt.Errorf("simulated list failure")
}

// --- test helpers ---

type authServerEnv struct {
	srv  *apiServer
	us   *memUserStore
	es   *memEnterpriseStore
	uaps *memUserAuthProviderStore
}

func newAuthServer(t *testing.T) (*apiServer, *memUserStore, *memEnterpriseStore) {
	t.Helper()
	env := newAuthServerEnv(t)
	return env.srv, env.us, env.es
}

func newAuthServerEnv(t *testing.T) *authServerEnv {
	t.Helper()
	us := newMemUserStore()
	es := newMemEnterpriseStore()
	uaps := newMemUserAuthProviderStore()

	now := time.Now().UTC()
	_ = us.Create(context.Background(), model.User{
		ID:           "usr_test1",
		EnterpriseID: "ent_test1",
		Email:        "alice@example.com",
		DisplayName:  "Alice",
		Role:         model.UserRoleOwner,
		Status:       model.UserStatusActive,
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

	return &authServerEnv{
		srv: &apiServer{
			log:               slog.Default(),
			users:             us,
			enterprises:       es,
			userAuthProviders: uaps,
		},
		us:   us,
		es:   es,
		uaps: uaps,
	}
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

	t.Run("nil store", func(t *testing.T) {
		srv := &apiServer{users: nil, enterprises: nil}
		w := httptest.NewRecorder()
		srv.GetCurrentUser(w, authedRequest("GET", "/v1/users/me", "", ownerClaims))
		if w.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotImplemented)
		}
	})

	t.Run("user not found", func(t *testing.T) {
		srv, _, _ := newAuthServer(t)
		unknownClaims := &auth.Claims{
			EnterpriseID: "ent_test1",
			Email:        "unknown@example.com",
			Role:         "owner",
		}
		unknownClaims.Subject = "usr_nonexistent"

		w := httptest.NewRecorder()
		srv.GetCurrentUser(w, authedRequest("GET", "/v1/users/me", "", unknownClaims))
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
	})
}

func TestGetCurrentEnterprise(t *testing.T) {
	t.Run("authenticated", func(t *testing.T) {
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
	})

	t.Run("unauthenticated", func(t *testing.T) {
		srv, _, _ := newAuthServer(t)
		w := httptest.NewRecorder()
		srv.GetCurrentEnterprise(w, authedRequest("GET", "/v1/enterprises/me", "", nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
	})

	t.Run("nil store", func(t *testing.T) {
		srv := &apiServer{users: nil, enterprises: nil}
		w := httptest.NewRecorder()
		srv.GetCurrentEnterprise(w, authedRequest("GET", "/v1/enterprises/me", "", ownerClaims))
		if w.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotImplemented)
		}
	})

	t.Run("enterprise not found", func(t *testing.T) {
		srv, _, _ := newAuthServer(t)
		unknownClaims := &auth.Claims{
			EnterpriseID: "ent_nonexistent",
			Email:        "alice@example.com",
			Role:         "owner",
		}
		unknownClaims.Subject = "usr_test1"

		w := httptest.NewRecorder()
		srv.GetCurrentEnterprise(w, authedRequest("GET", "/v1/enterprises/me", "", unknownClaims))
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
	})
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

	t.Run("nil store", func(t *testing.T) {
		srv := &apiServer{users: nil, enterprises: nil}
		w := httptest.NewRecorder()
		srv.UpdateCurrentUser(w, authedRequest("PATCH", "/v1/users/me",
			`{"display_name":"X"}`, ownerClaims))
		if w.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotImplemented)
		}
	})

	t.Run("invalid body", func(t *testing.T) {
		srv, _, _ := newAuthServer(t)
		w := httptest.NewRecorder()
		req := authedRequest("PATCH", "/v1/users/me", "not json", ownerClaims)
		srv.UpdateCurrentUser(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("user not found", func(t *testing.T) {
		srv, _, _ := newAuthServer(t)
		unknownClaims := &auth.Claims{
			EnterpriseID: "ent_test1",
			Email:        "unknown@example.com",
			Role:         "owner",
		}
		unknownClaims.Subject = "usr_nonexistent"

		w := httptest.NewRecorder()
		srv.UpdateCurrentUser(w, authedRequest("PATCH", "/v1/users/me",
			`{"display_name":"X"}`, unknownClaims))
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
	})

	t.Run("update store failure", func(t *testing.T) {
		us := newMemUserStore()
		now := time.Now().UTC()
		_ = us.Create(context.Background(), model.User{
			ID: "usr_test1", EnterpriseID: "ent_test1", Email: "alice@example.com",
			DisplayName: "Alice", Role: model.UserRoleOwner, Status: model.UserStatusActive,
			CreatedAt: now, UpdatedAt: now,
		})
		srv := &apiServer{users: &failingUserStore{us}, enterprises: newMemEnterpriseStore()}

		w := httptest.NewRecorder()
		srv.UpdateCurrentUser(w, authedRequest("PATCH", "/v1/users/me",
			`{"display_name":"X"}`, ownerClaims))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
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

	t.Run("nil store", func(t *testing.T) {
		srv := &apiServer{users: nil, enterprises: nil}
		w := httptest.NewRecorder()
		srv.UpdateCurrentEnterprise(w, authedRequest("PATCH", "/v1/enterprises/me",
			`{"name":"X"}`, ownerClaims))
		if w.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotImplemented)
		}
	})

	t.Run("invalid body", func(t *testing.T) {
		srv, _, _ := newAuthServer(t)
		w := httptest.NewRecorder()
		srv.UpdateCurrentEnterprise(w, authedRequest("PATCH", "/v1/enterprises/me",
			"not json", ownerClaims))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("enterprise not found", func(t *testing.T) {
		srv, _, _ := newAuthServer(t)
		unknownClaims := &auth.Claims{
			EnterpriseID: "ent_nonexistent",
			Email:        "alice@example.com",
			Role:         "owner",
		}
		unknownClaims.Subject = "usr_test1"

		w := httptest.NewRecorder()
		srv.UpdateCurrentEnterprise(w, authedRequest("PATCH", "/v1/enterprises/me",
			`{"name":"X"}`, unknownClaims))
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
	})

	t.Run("update store failure", func(t *testing.T) {
		es := newMemEnterpriseStore()
		now := time.Now().UTC()
		_ = es.Create(context.Background(), model.Enterprise{
			ID: "ent_test1", Name: "Test Corp", Slug: "test-corp",
			Plan: model.EnterprisePlanFree, BillingEmail: "billing@example.com",
			CreatedAt: now, UpdatedAt: now,
		})
		srv := &apiServer{users: newMemUserStore(), enterprises: &failingEnterpriseStore{es}}

		w := httptest.NewRecorder()
		srv.UpdateCurrentEnterprise(w, authedRequest("PATCH", "/v1/enterprises/me",
			`{"name":"X"}`, ownerClaims))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
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

func TestUserToAPI_AvatarURL(t *testing.T) {
	t.Run("with avatar", func(t *testing.T) {
		u := model.User{
			ID: "usr_1", EnterpriseID: "ent_1", Email: "test@example.com",
			DisplayName: "Test", AvatarURL: "https://example.com/avatar.png",
			Role: model.UserRoleOwner, Status: model.UserStatusActive,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		out := userToAPI(u)
		if out.AvatarUrl == nil {
			t.Fatal("expected avatar_url to be set")
		}
		if *out.AvatarUrl != "https://example.com/avatar.png" {
			t.Errorf("avatar_url = %q, want %q", *out.AvatarUrl, "https://example.com/avatar.png")
		}
	})

	t.Run("without avatar", func(t *testing.T) {
		u := model.User{
			ID: "usr_1", EnterpriseID: "ent_1", Email: "test@example.com",
			DisplayName: "Test", Role: model.UserRoleOwner, Status: model.UserStatusActive,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		out := userToAPI(u)
		if out.AvatarUrl != nil {
			t.Errorf("expected avatar_url to be nil, got %q", *out.AvatarUrl)
		}
	})
}

func TestUserToAPI_SnakeCaseJSON(t *testing.T) {
	u := model.User{
		ID:           "usr_1",
		EnterpriseID: "ent_1",
		Email:        "test@example.com",
		DisplayName:  "Test",
		Role:         model.UserRoleOwner,
		Status:       model.UserStatusActive,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	b, _ := json.Marshal(userToAPI(u))
	m := make(map[string]any)
	json.Unmarshal(b, &m)

	for _, key := range []string{"id", "enterprise_id", "email", "display_name", "role", "status", "auth_providers", "has_password", "created_at", "updated_at"} {
		if _, ok := m[key]; !ok {
			t.Errorf("expected snake_case key %q in JSON output", key)
		}
	}
}

func TestUserToAPI_AuthProviders(t *testing.T) {
	t.Run("with providers", func(t *testing.T) {
		now := time.Now()
		u := model.User{
			ID: "usr_1", EnterpriseID: "ent_1", Email: "test@example.com",
			DisplayName: "Test", Role: model.UserRoleOwner, Status: model.UserStatusActive,
			CreatedAt: now, UpdatedAt: now,
			AuthProviders: []model.UserAuthProvider{
				{ID: "uap_1", UserID: "usr_1", Provider: "google", SubjectID: "g-123", CreatedAt: now},
				{ID: "uap_2", UserID: "usr_1", Provider: "github", SubjectID: "gh-456", CreatedAt: now},
			},
		}
		out := userToAPI(u)
		if len(out.AuthProviders) != 2 {
			t.Fatalf("auth_providers length = %d, want 2", len(out.AuthProviders))
		}
		if out.AuthProviders[0].Provider != "google" {
			t.Errorf("provider[0] = %q, want google", out.AuthProviders[0].Provider)
		}
		if out.AuthProviders[1].Provider != "github" {
			t.Errorf("provider[1] = %q, want github", out.AuthProviders[1].Provider)
		}
	})

	t.Run("empty providers returns empty array", func(t *testing.T) {
		u := model.User{
			ID: "usr_1", EnterpriseID: "ent_1", Email: "test@example.com",
			DisplayName: "Test", Role: model.UserRoleOwner, Status: model.UserStatusActive,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		out := userToAPI(u)
		if out.AuthProviders == nil {
			t.Fatal("auth_providers should not be nil")
		}
		if len(out.AuthProviders) != 0 {
			t.Errorf("auth_providers length = %d, want 0", len(out.AuthProviders))
		}
	})

	t.Run("has_password true when password set", func(t *testing.T) {
		u := model.User{
			ID: "usr_1", EnterpriseID: "ent_1", Email: "test@example.com",
			DisplayName: "Test", Role: model.UserRoleOwner, Status: model.UserStatusActive,
			PasswordHash: "$2a$12$xxx",
			CreatedAt:    time.Now(), UpdatedAt: time.Now(),
		}
		out := userToAPI(u)
		if !out.HasPassword {
			t.Error("has_password should be true when password hash is set")
		}
	})

	t.Run("has_password false when no password", func(t *testing.T) {
		u := model.User{
			ID: "usr_1", EnterpriseID: "ent_1", Email: "test@example.com",
			DisplayName: "Test", Role: model.UserRoleOwner, Status: model.UserStatusActive,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		out := userToAPI(u)
		if out.HasPassword {
			t.Error("has_password should be false when password hash is empty")
		}
	})
}

func TestGetCurrentUser_WithAuthProviders(t *testing.T) {
	env := newAuthServerEnv(t)

	// Add auth provider links for the test user.
	now := time.Now().UTC()
	_ = env.uaps.Create(context.Background(), model.UserAuthProvider{
		ID: "uap_1", UserID: "usr_test1", Provider: "google", SubjectID: "g-123", CreatedAt: now,
	})
	_ = env.uaps.Create(context.Background(), model.UserAuthProvider{
		ID: "uap_2", UserID: "usr_test1", Provider: "github", SubjectID: "gh-456", CreatedAt: now,
	})

	w := httptest.NewRecorder()
	env.srv.GetCurrentUser(w, authedRequest("GET", "/v1/users/me", "", ownerClaims))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var u api.User
	json.NewDecoder(w.Body).Decode(&u)
	if len(u.AuthProviders) != 2 {
		t.Fatalf("auth_providers = %d, want 2", len(u.AuthProviders))
	}
}

func TestDisconnectAuthProvider(t *testing.T) {
	t.Run("disconnect with multiple providers", func(t *testing.T) {
		env := newAuthServerEnv(t)
		now := time.Now().UTC()
		_ = env.uaps.Create(context.Background(), model.UserAuthProvider{
			ID: "uap_1", UserID: "usr_test1", Provider: "google", SubjectID: "g-123", CreatedAt: now,
		})
		_ = env.uaps.Create(context.Background(), model.UserAuthProvider{
			ID: "uap_2", UserID: "usr_test1", Provider: "github", SubjectID: "gh-456", CreatedAt: now,
		})

		w := httptest.NewRecorder()
		env.srv.DisconnectAuthProvider(w, authedRequest("DELETE", "/v1/users/me/auth-providers/google", "", ownerClaims), "google")
		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusNoContent, w.Body.String())
		}

		// Verify google was removed.
		links, _ := env.uaps.ListByUser(context.Background(), "usr_test1")
		if len(links) != 1 {
			t.Errorf("remaining links = %d, want 1", len(links))
		}
		if links[0].Provider != "github" {
			t.Errorf("remaining provider = %q, want github", links[0].Provider)
		}
	})

	t.Run("disconnect with password allows last provider removal", func(t *testing.T) {
		env := newAuthServerEnv(t)
		// Set password on user.
		u, _ := env.us.Get(context.Background(), "usr_test1")
		u.PasswordHash = "$2a$12$xxx"
		_ = env.us.Update(context.Background(), u)

		now := time.Now().UTC()
		_ = env.uaps.Create(context.Background(), model.UserAuthProvider{
			ID: "uap_1", UserID: "usr_test1", Provider: "google", SubjectID: "g-123", CreatedAt: now,
		})

		w := httptest.NewRecorder()
		env.srv.DisconnectAuthProvider(w, authedRequest("DELETE", "/v1/users/me/auth-providers/google", "", ownerClaims), "google")
		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusNoContent, w.Body.String())
		}
	})

	t.Run("cannot disconnect last provider without password", func(t *testing.T) {
		env := newAuthServerEnv(t)
		now := time.Now().UTC()
		_ = env.uaps.Create(context.Background(), model.UserAuthProvider{
			ID: "uap_1", UserID: "usr_test1", Provider: "google", SubjectID: "g-123", CreatedAt: now,
		})

		w := httptest.NewRecorder()
		env.srv.DisconnectAuthProvider(w, authedRequest("DELETE", "/v1/users/me/auth-providers/google", "", ownerClaims), "google")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
	})

	t.Run("provider not connected", func(t *testing.T) {
		env := newAuthServerEnv(t)
		now := time.Now().UTC()
		// Need at least 2 providers so the last-method guard passes.
		_ = env.uaps.Create(context.Background(), model.UserAuthProvider{
			ID: "uap_1", UserID: "usr_test1", Provider: "google", SubjectID: "g-123", CreatedAt: now,
		})
		_ = env.uaps.Create(context.Background(), model.UserAuthProvider{
			ID: "uap_2", UserID: "usr_test1", Provider: "github", SubjectID: "gh-456", CreatedAt: now,
		})

		w := httptest.NewRecorder()
		env.srv.DisconnectAuthProvider(w, authedRequest("DELETE", "/v1/users/me/auth-providers/nonexistent", "", ownerClaims), "nonexistent")
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		env := newAuthServerEnv(t)

		w := httptest.NewRecorder()
		env.srv.DisconnectAuthProvider(w, authedRequest("DELETE", "/v1/users/me/auth-providers/google", "", nil), "google")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
	})

	t.Run("nil store", func(t *testing.T) {
		srv := &apiServer{users: newMemUserStore(), enterprises: newMemEnterpriseStore(), userAuthProviders: nil}
		w := httptest.NewRecorder()
		srv.DisconnectAuthProvider(w, authedRequest("DELETE", "/v1/users/me/auth-providers/google", "", ownerClaims), "google")
		if w.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotImplemented)
		}
	})

	t.Run("user not found", func(t *testing.T) {
		env := newAuthServerEnv(t)
		unknownClaims := &auth.Claims{
			EnterpriseID: "ent_test1",
			Email:        "unknown@example.com",
			Role:         "owner",
		}
		unknownClaims.Subject = "usr_nonexistent"

		w := httptest.NewRecorder()
		env.srv.DisconnectAuthProvider(w, authedRequest("DELETE", "/v1/users/me/auth-providers/google", "", unknownClaims), "google")
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
	})

	t.Run("list providers failure", func(t *testing.T) {
		env := newAuthServerEnv(t)
		env.srv.userAuthProviders = &failingUserAuthProviderStore{env.uaps}

		w := httptest.NewRecorder()
		env.srv.DisconnectAuthProvider(w, authedRequest("DELETE", "/v1/users/me/auth-providers/google", "", ownerClaims), "google")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
		}
	})
}

func TestGetCurrentUser_AuthProviderStoreError(t *testing.T) {
	// When ListByUser fails, GetCurrentUser should still return the user
	// (just without auth providers).
	env := newAuthServerEnv(t)
	env.srv.userAuthProviders = &failingUserAuthProviderStore{env.uaps}

	w := httptest.NewRecorder()
	env.srv.GetCurrentUser(w, authedRequest("GET", "/v1/users/me", "", ownerClaims))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var u api.User
	json.NewDecoder(w.Body).Decode(&u)
	if len(u.AuthProviders) != 0 {
		t.Errorf("auth_providers = %d, want 0 when store fails", len(u.AuthProviders))
	}
}

func TestGetCurrentUser_NilAuthProviderStore(t *testing.T) {
	// When userAuthProviders is nil, GetCurrentUser should still work
	// and return an empty auth_providers array.
	srv, _, _ := newAuthServer(t)
	srv.userAuthProviders = nil

	w := httptest.NewRecorder()
	srv.GetCurrentUser(w, authedRequest("GET", "/v1/users/me", "", ownerClaims))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var u api.User
	json.NewDecoder(w.Body).Decode(&u)
	if len(u.AuthProviders) != 0 {
		t.Errorf("auth_providers = %d, want 0 when store is nil", len(u.AuthProviders))
	}
}
