package auth

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ALRubinger/aileron/core/model"
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
	for _, c := range code {
		if c < '0' || c > '9' {
			t.Errorf("code contains non-digit: %c", c)
		}
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

// --- Signup handler tests ---

func TestSignup_Success(t *testing.T) {
	te := newTestEnv()
	w := te.do("POST", "/auth/signup", `{"email":"alice@acme.com","password":"securepass123","display_name":"Alice"}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "pending_verification" {
		t.Errorf("status = %q, want pending_verification", resp["status"])
	}
	if resp["user_id"] == "" {
		t.Error("expected user_id in response")
	}

	// Verify user was created with correct state.
	user, err := te.users.GetByEmail(t.Context(), "alice@acme.com")
	if err != nil {
		t.Fatalf("user not found: %v", err)
	}
	if user.Status != model.UserStatusPendingVerification {
		t.Errorf("user status = %q, want pending_verification", user.Status)
	}
	if user.AuthProvider != "email" {
		t.Errorf("auth_provider = %q, want email", user.AuthProvider)
	}
	if user.PasswordHash == "" {
		t.Error("expected password hash to be set")
	}

	// Verify enterprise was created (acme.com is organizational, not personal).
	ent, err := te.ents.Get(t.Context(), user.EnterpriseID)
	if err != nil {
		t.Fatalf("enterprise not found: %v", err)
	}
	if ent.Personal {
		t.Error("expected organizational enterprise for acme.com, got personal")
	}

	// Verify verification code was sent.
	if te.mailer.lastCode() == "" {
		t.Error("expected verification code to be sent")
	}
}

func TestSignup_OrgEmail(t *testing.T) {
	te := newTestEnv()
	w := te.do("POST", "/auth/signup", `{"email":"alice@bigcorp.com","password":"securepass123"}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusCreated)
	}

	user, _ := te.users.GetByEmail(t.Context(), "alice@bigcorp.com")
	ent, _ := te.ents.Get(t.Context(), user.EnterpriseID)
	if ent.Personal {
		t.Error("expected organizational enterprise for bigcorp.com")
	}
}

func TestSignup_PersonalGmail(t *testing.T) {
	te := newTestEnv()
	te.do("POST", "/auth/signup", `{"email":"alice@gmail.com","password":"securepass123"}`)

	user, _ := te.users.GetByEmail(t.Context(), "alice@gmail.com")
	ent, _ := te.ents.Get(t.Context(), user.EnterpriseID)
	if !ent.Personal {
		t.Error("expected personal enterprise for gmail.com")
	}
}

func TestSignup_DuplicateEmail(t *testing.T) {
	te := newTestEnv()
	te.do("POST", "/auth/signup", `{"email":"alice@acme.com","password":"securepass123"}`)

	w := te.do("POST", "/auth/signup", `{"email":"alice@acme.com","password":"otherpass123"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
}

func TestSignup_InvalidEmail(t *testing.T) {
	te := newTestEnv()
	w := te.do("POST", "/auth/signup", `{"email":"notanemail","password":"securepass123"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestSignup_ShortPassword(t *testing.T) {
	te := newTestEnv()
	w := te.do("POST", "/auth/signup", `{"email":"alice@acme.com","password":"short"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestSignup_MissingBody(t *testing.T) {
	te := newTestEnv()
	w := te.do("POST", "/auth/signup", `{}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestSignup_DefaultDisplayName(t *testing.T) {
	te := newTestEnv()
	te.do("POST", "/auth/signup", `{"email":"bob@acme.com","password":"securepass123"}`)

	user, _ := te.users.GetByEmail(t.Context(), "bob@acme.com")
	if user.DisplayName != "bob" {
		t.Errorf("display_name = %q, want %q", user.DisplayName, "bob")
	}
}

// --- Verify email handler tests ---

func TestVerifyEmail_Success(t *testing.T) {
	te := newTestEnv()
	te.do("POST", "/auth/signup", `{"email":"alice@acme.com","password":"securepass123"}`)
	code := te.mailer.lastCode()

	w := te.do("POST", "/auth/verify-email", `{"email":"alice@acme.com","code":"`+code+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	user, _ := te.users.GetByEmail(t.Context(), "alice@acme.com")
	if user.Status != model.UserStatusActive {
		t.Errorf("user status = %q, want active", user.Status)
	}
}

func TestVerifyEmail_WrongCode(t *testing.T) {
	te := newTestEnv()
	te.do("POST", "/auth/signup", `{"email":"alice@acme.com","password":"securepass123"}`)

	w := te.do("POST", "/auth/verify-email", `{"email":"alice@acme.com","code":"000000"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}

	// User should still be pending.
	user, _ := te.users.GetByEmail(t.Context(), "alice@acme.com")
	if user.Status != model.UserStatusPendingVerification {
		t.Errorf("user status = %q, want pending_verification", user.Status)
	}
}

func TestVerifyEmail_AlreadyVerified(t *testing.T) {
	te := newTestEnv()
	te.do("POST", "/auth/signup", `{"email":"alice@acme.com","password":"securepass123"}`)
	code := te.mailer.lastCode()
	te.do("POST", "/auth/verify-email", `{"email":"alice@acme.com","code":"`+code+`"}`)

	// Second verification should fail.
	w := te.do("POST", "/auth/verify-email", `{"email":"alice@acme.com","code":"`+code+`"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
}

func TestVerifyEmail_UnknownUser(t *testing.T) {
	te := newTestEnv()
	w := te.do("POST", "/auth/verify-email", `{"email":"nobody@acme.com","code":"123456"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// --- Email login handler tests ---

func TestEmailLogin_Success(t *testing.T) {
	te := newTestEnv()

	// Signup and verify.
	te.do("POST", "/auth/signup", `{"email":"alice@acme.com","password":"securepass123"}`)
	code := te.mailer.lastCode()
	te.do("POST", "/auth/verify-email", `{"email":"alice@acme.com","code":"`+code+`"}`)

	// Login.
	w := te.do("POST", "/auth/login", `{"email":"alice@acme.com","password":"securepass123"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["access_token"] == "" {
		t.Error("expected access_token in response")
	}

	// Verify session was created.
	if te.sessions.count() != 1 {
		t.Errorf("sessions count = %d, want 1", te.sessions.count())
	}

	// Verify access token is valid.
	claims, err := te.issuer.Validate(resp["access_token"])
	if err != nil {
		t.Fatalf("token validation failed: %v", err)
	}
	if claims.Email != "alice@acme.com" {
		t.Errorf("claims.Email = %q, want alice@acme.com", claims.Email)
	}
}

func TestEmailLogin_WrongPassword(t *testing.T) {
	te := newTestEnv()
	te.do("POST", "/auth/signup", `{"email":"alice@acme.com","password":"securepass123"}`)
	code := te.mailer.lastCode()
	te.do("POST", "/auth/verify-email", `{"email":"alice@acme.com","code":"`+code+`"}`)

	w := te.do("POST", "/auth/login", `{"email":"alice@acme.com","password":"wrongpassword"}`)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestEmailLogin_NonexistentUser(t *testing.T) {
	te := newTestEnv()
	w := te.do("POST", "/auth/login", `{"email":"nobody@acme.com","password":"securepass123"}`)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestEmailLogin_UnverifiedAccount(t *testing.T) {
	te := newTestEnv()
	te.do("POST", "/auth/signup", `{"email":"alice@acme.com","password":"securepass123"}`)

	// Try to login without verifying.
	w := te.do("POST", "/auth/login", `{"email":"alice@acme.com","password":"securepass123"}`)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestEmailLogin_SuspendedAccount(t *testing.T) {
	te := newTestEnv()
	te.do("POST", "/auth/signup", `{"email":"alice@acme.com","password":"securepass123"}`)
	code := te.mailer.lastCode()
	te.do("POST", "/auth/verify-email", `{"email":"alice@acme.com","code":"`+code+`"}`)

	// Suspend the user.
	user, _ := te.users.GetByEmail(t.Context(), "alice@acme.com")
	user.Status = model.UserStatusSuspended
	te.users.Update(t.Context(), user)

	w := te.do("POST", "/auth/login", `{"email":"alice@acme.com","password":"securepass123"}`)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestEmailLogin_OAuthOnlyUser(t *testing.T) {
	te := newTestEnv()
	// Create a user without a password (OAuth-only).
	te.users.Create(t.Context(), model.User{
		ID:           "usr_oauth",
		Email:        "alice@acme.com",
		Status:       model.UserStatusActive,
		AuthProvider: "google",
	})

	w := te.do("POST", "/auth/login", `{"email":"alice@acme.com","password":"anypassword"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestEmailLogin_MissingFields(t *testing.T) {
	te := newTestEnv()
	w := te.do("POST", "/auth/login", `{"email":"alice@acme.com"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// --- Full lifecycle test ---

func TestFullEmailAuthLifecycle(t *testing.T) {
	te := newTestEnv()

	// 1. Signup.
	w := te.do("POST", "/auth/signup", `{"email":"alice@acme.com","password":"securepass123","display_name":"Alice"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("signup: status = %d; body: %s", w.Code, w.Body.String())
	}

	// 2. Login should fail (not verified).
	w = te.do("POST", "/auth/login", `{"email":"alice@acme.com","password":"securepass123"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("login before verify: status = %d, want 403", w.Code)
	}

	// 3. Verify email.
	code := te.mailer.lastCode()
	w = te.do("POST", "/auth/verify-email", `{"email":"alice@acme.com","code":"`+code+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("verify: status = %d; body: %s", w.Code, w.Body.String())
	}

	// 4. Login should succeed.
	w = te.do("POST", "/auth/login", `{"email":"alice@acme.com","password":"securepass123"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("login after verify: status = %d; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["access_token"] == "" {
		t.Fatal("expected access_token")
	}

	// 5. Token should be valid with correct claims.
	claims, _ := te.issuer.Validate(resp["access_token"])
	if claims.Email != "alice@acme.com" {
		t.Errorf("email = %q", claims.Email)
	}
	if claims.Role != "owner" {
		t.Errorf("role = %q, want owner", claims.Role)
	}
}
