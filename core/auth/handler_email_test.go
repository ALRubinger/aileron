package auth

import (
	"context"
	"sync"
	"testing"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"golang.org/x/crypto/bcrypt"
)

func TestGenerateVerificationCode(t *testing.T) {
	code, err := generateVerificationCode()
	if err != nil {
		t.Fatalf("generateVerificationCode: %v", err)
	}
	if len(code) != 6 {
		t.Errorf("code length = %d, want 6", len(code))
	}
	// Should be all digits.
	for _, c := range code {
		if c < '0' || c > '9' {
			t.Errorf("code contains non-digit: %c", c)
		}
	}
	// Two codes should (almost certainly) differ.
	code2, _ := generateVerificationCode()
	if code == code2 {
		t.Log("warning: two consecutive codes were identical (unlikely but possible)")
	}
}

func TestBcryptHashAndCompare(t *testing.T) {
	password := "my-secure-password-123"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword(hash, []byte(password)); err != nil {
		t.Error("correct password should match")
	}
	if err := bcrypt.CompareHashAndPassword(hash, []byte("wrong-password")); err == nil {
		t.Error("wrong password should not match")
	}
}

func TestLogMailer(t *testing.T) {
	// LogMailer should not error — it just logs.
	m := NewLogMailer(nil)
	// We can't use nil logger in practice, but the interface should not panic
	// on a real logger. Just test it compiles and satisfies the interface.
	var _ Mailer = m
}

// --- Stub stores for handler tests ---

type stubUserStore struct {
	mu    sync.RWMutex
	users map[string]model.User // keyed by ID
}

func newStubUserStore() *stubUserStore {
	return &stubUserStore{users: make(map[string]model.User)}
}

func (s *stubUserStore) Create(_ context.Context, u model.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.users {
		if existing.Email == u.Email {
			return &store.ErrNotFound{Entity: "user", ID: u.Email} // simulate unique constraint
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
