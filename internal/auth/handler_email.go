package auth

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/model"
	"golang.org/x/crypto/bcrypt"
)

// handleSignup creates a new account with email + password and sends a
// verification code. The account is created with status pending_verification
// and cannot be used until the code is submitted via /auth/verify-email.
func (h *Handler) handleSignup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email       string `json:"email"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid request body")
		return
	}

	body.Email = strings.TrimSpace(strings.ToLower(body.Email))
	if body.Email == "" || !strings.Contains(body.Email, "@") {
		writeError(w, http.StatusBadRequest, "invalid_email", "valid email is required")
		return
	}
	if len(body.Password) < 8 {
		writeError(w, http.StatusBadRequest, "invalid_password", "password must be at least 8 characters")
		return
	}
	if body.DisplayName == "" {
		body.DisplayName = strings.SplitN(body.Email, "@", 2)[0]
	}

	ctx := r.Context()

	// Check if email is already taken.
	if _, err := h.users.GetByEmail(ctx, body.Email); err == nil {
		writeError(w, http.StatusConflict, "email_exists", "email already registered")
		return
	}

	// Hash password with bcrypt.
	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), h.bcryptCost)
	if err != nil {
		h.log.Error("failed to hash password", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	now := time.Now()
	personal := isPersonalEmail(body.Email)

	slug := strings.SplitN(body.Email, "@", 2)[0]
	slug = strings.ToLower(strings.ReplaceAll(slug, ".", "-"))

	var name string
	if personal {
		name = body.DisplayName
	} else {
		name = body.DisplayName + "'s Organization"
	}

	enterprise := model.Enterprise{
		ID:           "ent_" + h.newID(),
		Name:         name,
		Slug:         slug + "-" + h.newID()[:8],
		Plan:         model.EnterprisePlanFree,
		Personal:     personal,
		BillingEmail: body.Email,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := h.enterprises.Create(ctx, enterprise); err != nil {
		h.log.Error("failed to create enterprise", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	initialStatus := model.UserStatusPendingVerification
	if h.autoVerifyEmail {
		initialStatus = model.UserStatusActive
	}

	user := model.User{
		ID:           "usr_" + h.newID(),
		EnterpriseID: enterprise.ID,
		Email:        body.Email,
		DisplayName:  body.DisplayName,
		Role:         model.UserRoleOwner,
		Status:       initialStatus,
		PasswordHash: string(hash),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := h.users.Create(ctx, user); err != nil {
		h.log.Error("failed to create user", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	// Send verification code unless auto-verify is enabled.
	if !h.autoVerifyEmail {
		if err := h.sendVerificationCode(ctx, user.ID, body.Email); err != nil {
			h.log.Error("failed to send verification code", "error", err)
			// Account created but code failed — user can request a new one.
		}
	}

	h.log.Info("user signed up",
		"user_id", user.ID,
		"email", user.Email,
		"personal", personal,
		"auto_verified", h.autoVerifyEmail,
	)

	message := "verification code sent to " + body.Email
	if h.autoVerifyEmail {
		message = "account created and activated"
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{
		"user_id": user.ID,
		"status":  string(initialStatus),
		"message": message,
	})
}

// handleVerifyEmail verifies the email verification code and activates the account.
func (h *Handler) handleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid request body")
		return
	}

	body.Email = strings.TrimSpace(strings.ToLower(body.Email))
	body.Code = strings.TrimSpace(body.Code)
	if body.Email == "" || body.Code == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "email and code are required")
		return
	}

	ctx := r.Context()

	user, err := h.users.GetByEmail(ctx, body.Email)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "user not found")
		return
	}

	if user.Status != model.UserStatusPendingVerification {
		writeError(w, http.StatusConflict, "already_verified", "account already verified")
		return
	}

	code, err := h.verificationCodes.GetActiveByUserID(ctx, user.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "no_verification_code", "no active verification code — request a new one")
		return
	}

	// Compare hashed code.
	if HashToken(body.Code) != code.CodeHash {
		writeError(w, http.StatusBadRequest, "invalid_verification", "invalid verification code")
		return
	}

	// Mark code as used and activate the account.
	_ = h.verificationCodes.MarkUsed(ctx, code.ID)
	_ = h.verificationCodes.DeleteExpiredForUser(ctx, user.ID)

	now := time.Now()
	user.Status = model.UserStatusActive
	user.UpdatedAt = now
	if err := h.users.Update(ctx, user); err != nil {
		h.log.Error("failed to activate user", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	h.log.Info("email verified", "user_id", user.ID, "email", user.Email)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "verified",
		"message": "account activated — you can now log in",
	})
}

// handleEmailLogin authenticates with email + password and issues tokens.
func (h *Handler) handleEmailLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid request body")
		return
	}

	body.Email = strings.TrimSpace(strings.ToLower(body.Email))
	if body.Email == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "email and password are required")
		return
	}

	ctx := r.Context()

	user, err := h.users.GetByEmail(ctx, body.Email)
	if err != nil {
		// Constant-time-ish: still run bcrypt comparison to prevent timing attacks.
		bcrypt.CompareHashAndPassword([]byte(h.dummyHash), []byte(body.Password))
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
		return
	}

	if user.PasswordHash == "" {
		// OAuth-only user — no password set.
		writeError(w, http.StatusBadRequest, "oauth_account", "this account uses OAuth sign-in")
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(body.Password)); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
		return
	}

	if user.Status == model.UserStatusPendingVerification {
		writeError(w, http.StatusForbidden, "email_unverified", "email not verified — check your inbox")
		return
	}
	if user.Status == model.UserStatusSuspended {
		writeError(w, http.StatusForbidden, "account_suspended", "account suspended")
		return
	}

	// Update last login.
	now := time.Now()
	user.LastLoginAt = &now
	user.UpdatedAt = now
	_ = h.users.Update(ctx, user)

	// Issue tokens.
	accessToken, err := h.issuer.Issue(user.ID, user.EnterpriseID, user.Email, string(user.Role))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	refreshRaw, refreshHash, err := GenerateRefreshToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	session := model.Session{
		ID:               "ses_" + h.newID(),
		UserID:           user.ID,
		TokenHash:        HashToken(accessToken),
		RefreshTokenHash: refreshHash,
		ExpiresAt:        now.Add(h.refreshTTL),
		CreatedAt:        now,
	}
	if err := h.sessions.Create(ctx, session); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	secure := r.TLS != nil
	http.SetCookie(w, &http.Cookie{
		Name:     "access_token",
		Value:    accessToken,
		Path:     "/",
		MaxAge:   900,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "refresh_token",
		Value:    refreshRaw,
		Path:     "/auth/refresh",
		MaxAge:   int(h.refreshTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
	})

	h.log.Info("user logged in", "user_id", user.ID, "provider", "email")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"access_token": accessToken,
	})
}

// sendVerificationCode generates a 6-digit code, stores its hash, and sends it.
func (h *Handler) sendVerificationCode(ctx context.Context, userID, email string) error {
	code, err := generateVerificationCode()
	if err != nil {
		return fmt.Errorf("generating code: %w", err)
	}

	now := time.Now()
	vc := model.VerificationCode{
		ID:        "vrc_" + h.newID(),
		UserID:    userID,
		CodeHash:  HashToken(code),
		ExpiresAt: now.Add(h.verificationTTL),
		CreatedAt: now,
	}
	if err := h.verificationCodes.Create(ctx, vc); err != nil {
		return fmt.Errorf("storing code: %w", err)
	}

	if err := h.mailer.SendVerificationCode(ctx, email, code); err != nil {
		return fmt.Errorf("sending code: %w", err)
	}

	return nil
}

// generateVerificationCode returns a cryptographically random 6-digit string.
func generateVerificationCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}
