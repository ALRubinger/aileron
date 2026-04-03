package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/model"
)

// fakeProvider is a test AuthProvider that returns a fixed identity.
type fakeProvider struct {
	name     string
	identity *Identity
	err      error
}

func (f *fakeProvider) Provider() string { return f.name }

func (f *fakeProvider) AuthorizationURL(_ context.Context, state, _ string) (*AuthorizationResult, error) {
	return &AuthorizationResult{
		URL: "https://fake-provider.example.com/auth?state=" + state,
	}, nil
}

func (f *fakeProvider) HandleCallback(_ context.Context, _ CallbackRequest) (*Identity, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.identity, nil
}

// newOAuthTestEnv creates a testEnv with a fake OAuth provider registered.
func newOAuthTestEnv(identity *Identity) *testEnv {
	te := newTestEnv()
	te.handler.registry.Register(&fakeProvider{name: "fake", identity: identity})
	return te
}

// doOAuthCallback simulates a full OAuth callback request with state cookie.
func (te *testEnv) doOAuthCallback(provider, code, state string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/auth/"+provider+"/callback?code="+code+"&state="+state, nil)
	r.AddCookie(&http.Cookie{Name: "oauth_state", Value: state})
	w := httptest.NewRecorder()
	te.mux.ServeHTTP(w, r)
	return w
}

func TestOAuthCallback_NewUser(t *testing.T) {
	te := newOAuthTestEnv(&Identity{
		Subject:     "google-sub-123",
		Email:       "alice@acme.com",
		DisplayName: "Alice Smith",
		AvatarURL:   "https://example.com/avatar.jpg",
		Provider:    "fake",
	})

	w := te.doOAuthCallback("fake", "auth-code", "test-state")

	// Should redirect (307) on success.
	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusTemporaryRedirect, w.Body.String())
	}

	// User should be created.
	user, err := te.users.GetByEmail(t.Context(), "alice@acme.com")
	if err != nil {
		t.Fatalf("user not found: %v", err)
	}
	if user.DisplayName != "Alice Smith" {
		t.Errorf("display_name = %q, want Alice Smith", user.DisplayName)
	}
	if user.AuthProvider != "fake" {
		t.Errorf("auth_provider = %q, want fake", user.AuthProvider)
	}
	if user.AuthProviderSubjectID != "google-sub-123" {
		t.Errorf("subject = %q, want google-sub-123", user.AuthProviderSubjectID)
	}
	if user.Status != model.UserStatusActive {
		t.Errorf("status = %q, want active (OAuth skips verification)", user.Status)
	}

	// Enterprise should be created.
	ent, err := te.ents.Get(t.Context(), user.EnterpriseID)
	if err != nil {
		t.Fatalf("enterprise not found: %v", err)
	}
	if ent.Personal {
		t.Error("expected organizational enterprise for acme.com")
	}

	// Session should be created.
	if te.sessions.count() != 1 {
		t.Errorf("sessions = %d, want 1", te.sessions.count())
	}
}

func TestOAuthCallback_PersonalEmail(t *testing.T) {
	te := newOAuthTestEnv(&Identity{
		Subject:     "google-sub-456",
		Email:       "bob@gmail.com",
		DisplayName: "Bob Jones",
		Provider:    "fake",
	})

	te.doOAuthCallback("fake", "auth-code", "test-state")

	user, _ := te.users.GetByEmail(t.Context(), "bob@gmail.com")
	ent, _ := te.ents.Get(t.Context(), user.EnterpriseID)
	if !ent.Personal {
		t.Error("expected personal enterprise for gmail.com")
	}
}

func TestOAuthCallback_ReturningUser(t *testing.T) {
	te := newOAuthTestEnv(&Identity{
		Subject:     "google-sub-123",
		Email:       "alice@acme.com",
		DisplayName: "Alice Updated",
		AvatarURL:   "https://example.com/new-avatar.jpg",
		Provider:    "fake",
	})

	// First login creates user.
	te.doOAuthCallback("fake", "code1", "state1")
	user1, _ := te.users.GetByEmail(t.Context(), "alice@acme.com")

	// Second login should find existing user and update profile.
	te.doOAuthCallback("fake", "code2", "state2")
	user2, _ := te.users.GetByEmail(t.Context(), "alice@acme.com")

	if user2.ID != user1.ID {
		t.Error("expected same user ID for returning user")
	}
	if user2.DisplayName != "Alice Updated" {
		t.Errorf("display_name = %q, want Alice Updated", user2.DisplayName)
	}
	if user2.AvatarURL != "https://example.com/new-avatar.jpg" {
		t.Errorf("avatar not updated")
	}
}

func TestOAuthCallback_CrossProviderDedup(t *testing.T) {
	te := newTestEnv()

	// Register provider_a.
	te.handler.registry.Register(&fakeProvider{
		name: "provider_a",
		identity: &Identity{
			Subject: "provider-a-sub", Email: "alice@acme.com",
			DisplayName: "Alice", Provider: "provider_a",
		},
	})

	// First login via provider_a.
	r1 := httptest.NewRequest("GET", "/auth/provider_a/callback?code=c1&state=s1", nil)
	r1.AddCookie(&http.Cookie{Name: "oauth_state", Value: "s1"})
	w1 := httptest.NewRecorder()
	te.mux.ServeHTTP(w1, r1)

	user1, _ := te.users.GetByEmail(t.Context(), "alice@acme.com")

	// Register provider_b with same email but different subject.
	te.handler.registry.Register(&fakeProvider{
		name: "provider_b",
		identity: &Identity{
			Subject: "provider-b-sub", Email: "alice@acme.com",
			DisplayName: "Alice B", Provider: "provider_b",
		},
	})

	// Second login via provider_b — should find same user by email.
	r2 := httptest.NewRequest("GET", "/auth/provider_b/callback?code=c2&state=s2", nil)
	r2.AddCookie(&http.Cookie{Name: "oauth_state", Value: "s2"})
	w2 := httptest.NewRecorder()
	te.mux.ServeHTTP(w2, r2)

	if w2.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307; body: %s", w2.Code, w2.Body.String())
	}

	// Should be the same user (deduplicated by email).
	user2, _ := te.users.GetByEmail(t.Context(), "alice@acme.com")
	if user2.ID != user1.ID {
		t.Errorf("expected same user ID across providers: %q != %q", user2.ID, user1.ID)
	}
}

func TestOAuthCallback_ProviderNotAllowed(t *testing.T) {
	te := newOAuthTestEnv(&Identity{
		Subject: "sub-1", Email: "alice@acme.com",
		DisplayName: "Alice", Provider: "fake",
	})

	// First login creates user + enterprise.
	te.doOAuthCallback("fake", "code1", "state1")

	// Restrict enterprise to only allow "okta".
	user, _ := te.users.GetByEmail(t.Context(), "alice@acme.com")
	ent, _ := te.ents.Get(t.Context(), user.EnterpriseID)
	ent.AllowedAuthProviders = []string{"okta"}
	te.ents.Update(t.Context(), ent)

	// Second login with "fake" provider should be rejected.
	w := te.doOAuthCallback("fake", "code2", "state2")
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusForbidden, w.Body.String())
	}
}

func TestOAuthCallback_DomainNotAllowed(t *testing.T) {
	te := newOAuthTestEnv(&Identity{
		Subject: "sub-1", Email: "alice@acme.com",
		DisplayName: "Alice", Provider: "fake",
	})

	// First login creates user + enterprise.
	te.doOAuthCallback("fake", "code1", "state1")

	// Restrict enterprise to only allow bigcorp.com.
	user, _ := te.users.GetByEmail(t.Context(), "alice@acme.com")
	ent, _ := te.ents.Get(t.Context(), user.EnterpriseID)
	ent.AllowedEmailDomains = []string{"bigcorp.com"}
	te.ents.Update(t.Context(), ent)

	// Second login with acme.com email should be rejected.
	w := te.doOAuthCallback("fake", "code2", "state2")
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestOAuthCallback_UnknownProvider(t *testing.T) {
	te := newTestEnv()
	w := te.doOAuthCallback("nonexistent", "code", "state")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestOAuthCallback_MissingStateCookie(t *testing.T) {
	te := newOAuthTestEnv(&Identity{
		Subject: "sub", Email: "a@b.com", Provider: "fake",
	})

	// Request without state cookie.
	r := httptest.NewRequest("GET", "/auth/fake/callback?code=c&state=s", nil)
	w := httptest.NewRecorder()
	te.mux.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestOAuthCallback_StateMismatch(t *testing.T) {
	te := newOAuthTestEnv(&Identity{
		Subject: "sub", Email: "a@b.com", Provider: "fake",
	})

	r := httptest.NewRequest("GET", "/auth/fake/callback?code=c&state=wrong", nil)
	r.AddCookie(&http.Cookie{Name: "oauth_state", Value: "correct"})
	w := httptest.NewRecorder()
	te.mux.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestOAuthCallback_ProviderError(t *testing.T) {
	te := newOAuthTestEnv(&Identity{
		Subject: "sub", Email: "a@b.com", Provider: "fake",
	})

	r := httptest.NewRequest("GET", "/auth/fake/callback?error=access_denied&state=s", nil)
	r.AddCookie(&http.Cookie{Name: "oauth_state", Value: "s"})
	w := httptest.NewRecorder()
	te.mux.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if !strings.Contains(w.Body.String(), "access_denied") {
		t.Error("expected error message to contain provider error")
	}
}

func TestOAuthLogin_Redirect(t *testing.T) {
	te := newOAuthTestEnv(&Identity{
		Subject: "sub", Email: "a@b.com", Provider: "fake",
	})

	w := te.do("GET", "/auth/fake/login", "")

	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusTemporaryRedirect)
	}

	location := w.Header().Get("Location")
	if !strings.Contains(location, "fake-provider.example.com") {
		t.Errorf("Location = %q, expected fake provider URL", location)
	}

	// Should set oauth_state cookie.
	var hasStateCookie bool
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "oauth_state" && cookie.Value != "" {
			hasStateCookie = true
		}
	}
	if !hasStateCookie {
		t.Error("expected oauth_state cookie to be set")
	}
}

func TestOAuthLogin_ExtraStateCookie(t *testing.T) {
	te := newTestEnv()

	// Register a provider that returns ExtraState (simulating PKCE).
	te.handler.registry.Register(&fakeProviderWithExtraState{
		name:       "pkce",
		extraState: "test-code-verifier-abc123",
		identity:   &Identity{Subject: "sub", Email: "a@b.com", Provider: "pkce"},
	})

	w := te.do("GET", "/auth/pkce/login", "")

	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusTemporaryRedirect)
	}

	// Should set oauth_extra cookie with the ExtraState value.
	var hasExtraCookie bool
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "oauth_extra" && cookie.Value == "test-code-verifier-abc123" {
			hasExtraCookie = true
		}
	}
	if !hasExtraCookie {
		t.Error("expected oauth_extra cookie to be set with ExtraState value")
	}
}

func TestOAuthCallback_ExtraStatePassedToProvider(t *testing.T) {
	te := newTestEnv()

	// Register a provider that captures the ExtraState from the callback.
	fp := &fakeProviderWithExtraState{
		name:       "pkce",
		extraState: "verifier-xyz",
		identity:   &Identity{Subject: "sub-1", Email: "alice@acme.com", DisplayName: "Alice", Provider: "pkce"},
	}
	te.handler.registry.Register(fp)

	// Simulate callback with both oauth_state and oauth_extra cookies.
	r := httptest.NewRequest("GET", "/auth/pkce/callback?code=c1&state=s1", nil)
	r.AddCookie(&http.Cookie{Name: "oauth_state", Value: "s1"})
	r.AddCookie(&http.Cookie{Name: "oauth_extra", Value: "verifier-xyz"})
	w := httptest.NewRecorder()
	te.mux.ServeHTTP(w, r)

	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusTemporaryRedirect, w.Body.String())
	}

	// Verify the provider received the ExtraState.
	if fp.receivedExtraState != "verifier-xyz" {
		t.Errorf("provider received ExtraState = %q, want verifier-xyz", fp.receivedExtraState)
	}

	// Verify oauth_extra cookie was cleared.
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "oauth_extra" && cookie.MaxAge == -1 {
			return // cleared as expected
		}
	}
	t.Error("expected oauth_extra cookie to be cleared")
}

// fakeProviderWithExtraState is a test AuthProvider that returns ExtraState
// from AuthorizationURL and captures it from HandleCallback.
type fakeProviderWithExtraState struct {
	name               string
	extraState         string
	identity           *Identity
	receivedExtraState string
}

func (f *fakeProviderWithExtraState) Provider() string { return f.name }

func (f *fakeProviderWithExtraState) AuthorizationURL(_ context.Context, state, _ string) (*AuthorizationResult, error) {
	return &AuthorizationResult{
		URL:        "https://fake-pkce.example.com/auth?state=" + state,
		ExtraState: f.extraState,
	}, nil
}

func (f *fakeProviderWithExtraState) HandleCallback(_ context.Context, req CallbackRequest) (*Identity, error) {
	f.receivedExtraState = req.ExtraState
	return f.identity, nil
}

func TestOAuthLogin_UnknownProvider(t *testing.T) {
	te := newTestEnv()
	w := te.do("GET", "/auth/nonexistent/login", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}
