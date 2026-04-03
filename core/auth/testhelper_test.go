package auth

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
)

// --- Stub stores ---

// stubEnterpriseStore is an in-memory enterprise store for tests.
type stubEnterpriseStore struct {
	mu          sync.RWMutex
	enterprises map[string]model.Enterprise
}

func newStubStore(ents ...model.Enterprise) *stubEnterpriseStore {
	m := make(map[string]model.Enterprise)
	for _, e := range ents {
		m[e.ID] = e
	}
	return &stubEnterpriseStore{enterprises: m}
}

func (s *stubEnterpriseStore) Create(_ context.Context, e model.Enterprise) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enterprises[e.ID] = e
	return nil
}

func (s *stubEnterpriseStore) Get(_ context.Context, id string) (model.Enterprise, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.enterprises[id]
	if !ok {
		return model.Enterprise{}, &store.ErrNotFound{Entity: "enterprise", ID: id}
	}
	return e, nil
}

func (s *stubEnterpriseStore) GetBySlug(_ context.Context, slug string) (model.Enterprise, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.enterprises {
		if e.Slug == slug {
			return e, nil
		}
	}
	return model.Enterprise{}, &store.ErrNotFound{Entity: "enterprise", ID: slug}
}

func (s *stubEnterpriseStore) Update(_ context.Context, e model.Enterprise) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enterprises[e.ID] = e
	return nil
}

// stubUserStore is an in-memory user store for tests.
type stubUserStore struct {
	mu    sync.RWMutex
	users map[string]model.User
}

func newStubUserStore() *stubUserStore {
	return &stubUserStore{users: make(map[string]model.User)}
}

func (s *stubUserStore) Create(_ context.Context, u model.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.users {
		if existing.Email == u.Email {
			return &store.ErrNotFound{Entity: "user", ID: u.Email}
		}
	}
	s.users[u.ID] = u
	return nil
}

func (s *stubUserStore) Get(_ context.Context, id string) (model.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[id]
	if !ok {
		return model.User{}, &store.ErrNotFound{Entity: "user", ID: id}
	}
	return u, nil
}

func (s *stubUserStore) GetByEmail(_ context.Context, email string) (model.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.users {
		if u.Email == email {
			return u, nil
		}
	}
	return model.User{}, &store.ErrNotFound{Entity: "user", ID: email}
}

func (s *stubUserStore) GetByProviderSubject(_ context.Context, provider, subject string) (model.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.users {
		if u.AuthProvider == provider && u.AuthProviderSubjectID == subject {
			return u, nil
		}
	}
	return model.User{}, &store.ErrNotFound{Entity: "user", ID: provider + "/" + subject}
}

func (s *stubUserStore) List(_ context.Context, _ store.UserFilter) ([]model.User, error) {
	return nil, nil
}

func (s *stubUserStore) Update(_ context.Context, u model.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[u.ID] = u
	return nil
}

// stubSessionStore is an in-memory session store for tests.
type stubSessionStore struct {
	mu       sync.RWMutex
	sessions map[string]model.Session
}

func newStubSessionStore() *stubSessionStore {
	return &stubSessionStore{sessions: make(map[string]model.Session)}
}

func (s *stubSessionStore) Create(_ context.Context, sess model.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID] = sess
	return nil
}

func (s *stubSessionStore) GetByTokenHash(_ context.Context, tokenHash string) (model.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sess := range s.sessions {
		if sess.TokenHash == tokenHash {
			return sess, nil
		}
	}
	return model.Session{}, &store.ErrNotFound{Entity: "session", ID: tokenHash}
}

func (s *stubSessionStore) GetByRefreshTokenHash(_ context.Context, refreshHash string) (model.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sess := range s.sessions {
		if sess.RefreshTokenHash == refreshHash {
			return sess, nil
		}
	}
	return model.Session{}, &store.ErrNotFound{Entity: "session", ID: refreshHash}
}

func (s *stubSessionStore) Delete(_ context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
	return nil
}

func (s *stubSessionStore) DeleteAllForUser(_ context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.sessions {
		if sess.UserID == userID {
			delete(s.sessions, id)
		}
	}
	return nil
}

func (s *stubSessionStore) count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

// stubVerificationCodeStore is an in-memory verification code store for tests.
type stubVerificationCodeStore struct {
	mu    sync.RWMutex
	codes map[string]model.VerificationCode
}

func newStubVerificationCodeStore() *stubVerificationCodeStore {
	return &stubVerificationCodeStore{codes: make(map[string]model.VerificationCode)}
}

func (s *stubVerificationCodeStore) Create(_ context.Context, code model.VerificationCode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[code.ID] = code
	return nil
}

func (s *stubVerificationCodeStore) GetActiveByUserID(_ context.Context, userID string) (model.VerificationCode, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.codes {
		if c.UserID == userID && !c.Used {
			return c, nil
		}
	}
	return model.VerificationCode{}, &store.ErrNotFound{Entity: "verification_code", ID: userID}
}

func (s *stubVerificationCodeStore) MarkUsed(_ context.Context, codeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.codes[codeID]
	if !ok {
		return &store.ErrNotFound{Entity: "verification_code", ID: codeID}
	}
	c.Used = true
	s.codes[codeID] = c
	return nil
}

func (s *stubVerificationCodeStore) DeleteExpiredForUser(_ context.Context, _ string) error {
	return nil
}

// capturingMailer records sent codes for test assertions.
type capturingMailer struct {
	mu       sync.Mutex
	sentTo   string
	sentCode string
}

func (m *capturingMailer) SendVerificationCode(_ context.Context, to string, code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sentTo = to
	m.sentCode = code
	return nil
}

func (m *capturingMailer) lastCode() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sentCode
}

// --- Test environment ---

// testEnv bundles all stubs and the handler for a test scenario.
type testEnv struct {
	handler  *Handler
	mux      *http.ServeMux
	users    *stubUserStore
	ents     *stubEnterpriseStore
	sessions *stubSessionStore
	codes    *stubVerificationCodeStore
	mailer   *capturingMailer
	issuer   *TokenIssuer
}

func newTestEnv() *testEnv {
	users := newStubUserStore()
	ents := &stubEnterpriseStore{enterprises: make(map[string]model.Enterprise)}
	sessions := newStubSessionStore()
	codes := newStubVerificationCodeStore()
	mailer := &capturingMailer{}
	issuer := NewTokenIssuer([]byte("test-key-32-bytes-long-for-hmac!"), "test", 15*time.Minute)

	seq := 0
	idGen := func() string {
		seq++
		return strings.Repeat("0", 8) + string(rune('0'+seq))
	}

	h := NewHandler(HandlerConfig{
		Log:               slog.Default(),
		Registry:          NewRegistry(),
		Enforcer:          NewStoreEnforcer(ents),
		Issuer:            issuer,
		Users:             users,
		Enterprises:       ents,
		Sessions:          sessions,
		VerificationCodes: codes,
		Mailer:            mailer,
		NewID:             idGen,
		RefreshTTL:        7 * 24 * time.Hour,
		VerificationTTL:   15 * time.Minute,
	})

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	return &testEnv{
		handler:  h,
		mux:      mux,
		users:    users,
		ents:     ents,
		sessions: sessions,
		codes:    codes,
		mailer:   mailer,
		issuer:   issuer,
	}
}

func (te *testEnv) do(method, path, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	te.mux.ServeHTTP(w, r)
	return w
}

func (te *testEnv) doWithCookie(method, path, body, cookieName, cookieValue string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.AddCookie(&http.Cookie{Name: cookieName, Value: cookieValue})
	w := httptest.NewRecorder()
	te.mux.ServeHTTP(w, r)
	return w
}
