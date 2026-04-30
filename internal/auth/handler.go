package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/store"
	"golang.org/x/crypto/bcrypt"
)

// Handler serves the authentication HTTP routes.
type Handler struct {
	log               *slog.Logger
	registry          *Registry
	enforcer          Enforcer
	issuer            *TokenIssuer
	users             store.UserStore
	userAuthProviders store.UserAuthProviderStore
	enterprises       store.EnterpriseStore
	sessions          store.SessionStore
	verificationCodes store.VerificationCodeStore
	mailer            Mailer
	newID             func() string
	uiBaseURL         string // UI origin, e.g. "https://app.example.com"
	refreshTTL        time.Duration
	verificationTTL   time.Duration
	autoVerifyEmail   bool
	bcryptCost        int    // bcrypt cost parameter (default 12)
	dummyHash         string // pre-computed dummy hash for timing-safe rejection
}

// HandlerConfig configures the auth handler.
type HandlerConfig struct {
	Log               *slog.Logger
	Registry          *Registry
	Enforcer          Enforcer
	Issuer            *TokenIssuer
	Users             store.UserStore
	UserAuthProviders store.UserAuthProviderStore
	Enterprises       store.EnterpriseStore
	Sessions          store.SessionStore
	VerificationCodes store.VerificationCodeStore
	Mailer            Mailer
	NewID             func() string
	UIBaseURL         string        // UI origin, e.g. "http://localhost:5173" or "/"
	AutoVerifyEmail   bool          // skip email verification (dev/CI only)
	RefreshTTL        time.Duration // e.g. 7 * 24 * time.Hour
	VerificationTTL   time.Duration // e.g. 15 * time.Minute
	BcryptCost        int           // bcrypt cost (default 12; use bcrypt.MinCost in tests)
}

// NewHandler creates an auth handler.
func NewHandler(cfg HandlerConfig) *Handler {
	if cfg.RefreshTTL == 0 {
		cfg.RefreshTTL = 7 * 24 * time.Hour
	}
	if cfg.VerificationTTL == 0 {
		cfg.VerificationTTL = 15 * time.Minute
	}
	if cfg.UIBaseURL == "" {
		cfg.UIBaseURL = "/"
	}
	if cfg.BcryptCost == 0 {
		cfg.BcryptCost = 12
	}

	// Pre-compute a dummy hash at the configured cost for timing-safe rejection
	// of logins for nonexistent users (prevents email enumeration via timing).
	dummy, _ := bcrypt.GenerateFromPassword([]byte("dummy"), cfg.BcryptCost)

	return &Handler{
		log:               cfg.Log,
		registry:          cfg.Registry,
		enforcer:          cfg.Enforcer,
		issuer:            cfg.Issuer,
		users:             cfg.Users,
		userAuthProviders: cfg.UserAuthProviders,
		enterprises:       cfg.Enterprises,
		sessions:          cfg.Sessions,
		verificationCodes: cfg.VerificationCodes,
		mailer:            cfg.Mailer,
		newID:             cfg.NewID,
		uiBaseURL:         cfg.UIBaseURL,
		refreshTTL:        cfg.RefreshTTL,
		verificationTTL:   cfg.VerificationTTL,
		autoVerifyEmail:   cfg.AutoVerifyEmail,
		bcryptCost:        cfg.BcryptCost,
		dummyHash:         string(dummy),
	}
}

// RegisterRoutes registers auth routes on the given mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/{provider}/login", h.handleLogin)
	mux.HandleFunc("GET /auth/{provider}/callback", h.handleCallback)
	mux.HandleFunc("POST /auth/signup", h.handleSignup)
	mux.HandleFunc("POST /auth/verify-email", h.handleVerifyEmail)
	mux.HandleFunc("POST /auth/login", h.handleEmailLogin)
	mux.HandleFunc("POST /auth/refresh", h.handleRefresh)
	mux.HandleFunc("POST /auth/logout", h.handleLogout)
}

// handleLogin redirects to the provider's authorization URL.
func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	providerName := r.PathValue("provider")
	provider, ok := h.registry.Get(providerName)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown_provider", "unknown auth provider")
		return
	}

	state, err := generateState()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	// Store state in a short-lived cookie for CSRF validation.
	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_state",
		Value:    state,
		Path:     "/",
		MaxAge:   600, // 10 minutes
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})

	result, err := provider.AuthorizationURL(r.Context(), state, h.callbackURL(r, providerName))
	if err != nil {
		h.log.Error("failed to generate auth URL", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	// If the provider returned extra state (e.g. PKCE code_verifier), persist
	// it in a companion cookie so it survives the redirect round-trip.
	if result.ExtraState != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     "oauth_extra",
			Value:    result.ExtraState,
			Path:     "/",
			MaxAge:   600, // 10 minutes
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   r.TLS != nil,
		})
	}

	http.Redirect(w, r, result.URL, http.StatusTemporaryRedirect)
}

// callbackURL returns the OAuth callback URL for the given provider, derived
// from the incoming request. It respects X-Forwarded-Proto and X-Forwarded-Host.
func (h *Handler) callbackURL(r *http.Request, provider string) string {
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
		scheme = "http"
	}
	host := r.Host
	if fwdHost := r.Header.Get("X-Forwarded-Host"); fwdHost != "" {
		host = fwdHost
	}
	return fmt.Sprintf("%s://%s/auth/%s/callback", scheme, host, provider)
}

// handleCallback processes the OAuth callback and creates a session.
func (h *Handler) handleCallback(w http.ResponseWriter, r *http.Request) {
	providerName := r.PathValue("provider")
	provider, ok := h.registry.Get(providerName)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown_provider", "unknown auth provider")
		return
	}

	stateParam := r.URL.Query().Get("state")

	// Validate CSRF state.
	stateCookie, err := r.Cookie("oauth_state")
	if err != nil || stateCookie.Value == "" {
		writeError(w, http.StatusBadRequest, "missing_state", "missing state cookie")
		return
	}
	if stateParam != stateCookie.Value {
		writeError(w, http.StatusBadRequest, "state_mismatch", "state mismatch")
		return
	}
	// Clear the state cookie.
	http.SetCookie(w, &http.Cookie{
		Name:   "oauth_state",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})

	// Check for error from provider.
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		h.log.Warn("auth provider returned error", "provider", providerName, "error", errParam)
		writeError(w, http.StatusBadRequest, "provider_error", "provider error: "+errParam)
		return
	}

	// Read optional extra state (e.g. PKCE code_verifier) from companion cookie.
	var extraState string
	if extraCookie, err := r.Cookie("oauth_extra"); err == nil {
		extraState = extraCookie.Value
		// Clear the extra state cookie.
		http.SetCookie(w, &http.Cookie{
			Name:   "oauth_extra",
			Value:  "",
			Path:   "/",
			MaxAge: -1,
		})
	}

	// Exchange code for identity.
	identity, err := provider.HandleCallback(r.Context(), CallbackRequest{
		Code:        r.URL.Query().Get("code"),
		State:       stateParam,
		RedirectURL: h.callbackURL(r, providerName),
		ExtraState:  extraState,
	})
	if err != nil {
		h.log.Error("callback exchange failed", "provider", providerName, "error", err)
		writeError(w, http.StatusUnauthorized, "unauthenticated", "authentication failed")
		return
	}

	ctx := r.Context()

	// Look up user: first by provider+subject in the auth providers table
	// (fast path for returning users), then by email (handles sign-in via
	// a new provider for an existing account).
	var user model.User
	link, err := h.userAuthProviders.GetByProviderSubject(ctx, identity.Provider, identity.Subject)
	if err == nil {
		// Known provider link — fetch the user.
		user, err = h.users.Get(ctx, link.UserID)
		if err != nil {
			h.log.Error("user lookup by link failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
	} else if isNotFound(err) {
		// Provider subject not seen before — check if the email already exists.
		user, err = h.users.GetByEmail(ctx, identity.Email)
		if err != nil && !isNotFound(err) {
			h.log.Error("user lookup failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		if isNotFound(err) {
			// Entirely new user — auto-create enterprise and user.
			user, err = h.createEnterpriseAndUser(ctx, identity)
			if err != nil {
				h.log.Error("failed to create enterprise/user", "error", err)
				writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
				return
			}
			// createEnterpriseAndUser already created the auth provider link.
			goto issueTokens
		}
		// Existing user found by email — create a new auth provider link.
		newLink := model.UserAuthProvider{
			ID:        "uap_" + h.newID(),
			UserID:    user.ID,
			Provider:  identity.Provider,
			SubjectID: identity.Subject,
			CreatedAt: time.Now(),
		}
		if err := h.userAuthProviders.Create(ctx, newLink); err != nil {
			h.log.Error("failed to create auth provider link", "error", err)
			// Non-fatal: user can still log in, link just won't be saved.
		}
	} else {
		h.log.Error("auth provider lookup failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	{
		// Existing user — enforce SSO policies.
		allowed, err := h.enforcer.IsProviderAllowed(ctx, user.EnterpriseID, identity.Provider)
		if err != nil {
			h.log.Error("provider check failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		if !allowed {
			writeError(w, http.StatusForbidden, "provider_not_allowed", "auth provider not allowed for this enterprise")
			return
		}

		domainAllowed, err := h.enforcer.IsEmailDomainAllowed(ctx, user.EnterpriseID, identity.Email)
		if err != nil {
			h.log.Error("domain check failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		if !domainAllowed {
			writeError(w, http.StatusForbidden, "domain_not_allowed", "email domain not allowed for this enterprise")
			return
		}

		// Update last login.
		now := time.Now()
		user.LastLoginAt = &now
		user.DisplayName = identity.DisplayName
		user.AvatarURL = identity.AvatarURL
		user.UpdatedAt = now
		if err := h.users.Update(ctx, user); err != nil {
			h.log.Error("failed to update user", "error", err)
		}
	}

issueTokens:

	// Issue tokens.
	accessToken, err := h.issuer.Issue(user.ID, user.EnterpriseID, user.Email, string(user.Role))
	if err != nil {
		h.log.Error("failed to issue token", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	refreshRaw, refreshHash, err := GenerateRefreshToken()
	if err != nil {
		h.log.Error("failed to generate refresh token", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	now := time.Now()
	session := model.Session{
		ID:               "ses_" + h.newID(),
		UserID:           user.ID,
		TokenHash:        HashToken(accessToken),
		RefreshTokenHash: refreshHash,
		ExpiresAt:        now.Add(h.refreshTTL),
		CreatedAt:        now,
	}
	if err := h.sessions.Create(ctx, session); err != nil {
		h.log.Error("failed to create session", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	// Set cookies for browser flow.
	// Check both direct TLS and X-Forwarded-Proto (behind reverse proxy).
	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name:     "access_token",
		Value:    accessToken,
		Path:     "/",
		MaxAge:   900, // 15 minutes
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

	h.log.Info("user authenticated", "user_id", user.ID, "provider", providerName)
	http.Redirect(w, r, strings.TrimRight(h.uiBaseURL, "/")+"/auth/callback", http.StatusTemporaryRedirect)
}

// handleRefresh exchanges a refresh token for a new access token.
func (h *Handler) handleRefresh(w http.ResponseWriter, r *http.Request) {
	refreshToken := ""
	if c, err := r.Cookie("refresh_token"); err == nil {
		refreshToken = c.Value
	}
	if refreshToken == "" {
		// Try JSON body.
		var body struct {
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			refreshToken = body.RefreshToken
		}
	}
	if refreshToken == "" {
		writeError(w, http.StatusBadRequest, "missing_refresh_token", "missing refresh token")
		return
	}

	ctx := r.Context()
	hash := HashToken(refreshToken)
	session, err := h.sessions.GetByRefreshTokenHash(ctx, hash)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_refresh_token", "invalid refresh token")
		return
	}
	if time.Now().After(session.ExpiresAt) {
		_ = h.sessions.Delete(ctx, session.ID)
		writeError(w, http.StatusUnauthorized, "refresh_token_expired", "refresh token expired")
		return
	}

	user, err := h.users.Get(ctx, session.UserID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "user_not_found", "user not found")
		return
	}

	// Issue new access token.
	accessToken, err := h.issuer.Issue(user.ID, user.EnterpriseID, user.Email, string(user.Role))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	// Rotate refresh token.
	newRefreshRaw, newRefreshHash, err := GenerateRefreshToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	// Delete old session, create new one.
	_ = h.sessions.Delete(ctx, session.ID)
	now := time.Now()
	newSession := model.Session{
		ID:               "ses_" + h.newID(),
		UserID:           user.ID,
		TokenHash:        HashToken(accessToken),
		RefreshTokenHash: newRefreshHash,
		ExpiresAt:        now.Add(h.refreshTTL),
		CreatedAt:        now,
	}
	if err := h.sessions.Create(ctx, newSession); err != nil {
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
		Value:    newRefreshRaw,
		Path:     "/auth/refresh",
		MaxAge:   int(h.refreshTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"access_token": accessToken,
	})
}

// handleLogout deletes the session and clears cookies.
func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	// Try to find and delete the session via refresh token.
	if c, err := r.Cookie("refresh_token"); err == nil && c.Value != "" {
		hash := HashToken(c.Value)
		if sess, err := h.sessions.GetByRefreshTokenHash(r.Context(), hash); err == nil {
			_ = h.sessions.Delete(r.Context(), sess.ID)
		}
	}

	// Clear cookies.
	http.SetCookie(w, &http.Cookie{Name: "access_token", Value: "", Path: "/", MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: "refresh_token", Value: "", Path: "/auth/refresh", MaxAge: -1})

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"logged_out"}`))
}

// personalEmailDomains are well-known consumer email providers. Users signing
// in with these domains get a personal enterprise rather than an organizational one.
var personalEmailDomains = map[string]bool{
	"gmail.com":      true,
	"googlemail.com": true,
	"yahoo.com":      true,
	"hotmail.com":    true,
	"outlook.com":    true,
	"live.com":       true,
	"aol.com":        true,
	"icloud.com":     true,
	"me.com":         true,
	"mac.com":        true,
	"protonmail.com": true,
	"proton.me":      true,
}

// isPersonalEmail reports whether the email address belongs to a consumer
// email provider rather than an organization domain.
func isPersonalEmail(email string) bool {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return false
	}
	return personalEmailDomains[strings.ToLower(parts[1])]
}

// createEnterpriseAndUser auto-creates an enterprise and user on first sign-in.
// If the user signs in with a personal email (e.g. Gmail), a personal enterprise
// is created. Otherwise an organizational enterprise is created.
func (h *Handler) createEnterpriseAndUser(ctx context.Context, identity *Identity) (model.User, error) {
	now := time.Now()
	personal := isPersonalEmail(identity.Email)

	// Generate a slug from the email username.
	slug := strings.SplitN(identity.Email, "@", 2)[0]
	slug = strings.ToLower(strings.ReplaceAll(slug, ".", "-"))

	var name string
	if personal {
		name = identity.DisplayName
	} else {
		name = identity.DisplayName + "'s Organization"
	}

	enterprise := model.Enterprise{
		ID:           "ent_" + h.newID(),
		Name:         name,
		Slug:         slug + "-" + h.newID()[:8],
		Plan:         model.EnterprisePlanFree,
		Personal:     personal,
		BillingEmail: identity.Email,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := h.enterprises.Create(ctx, enterprise); err != nil {
		return model.User{}, fmt.Errorf("creating enterprise: %w", err)
	}

	user := model.User{
		ID:           "usr_" + h.newID(),
		EnterpriseID: enterprise.ID,
		Email:        identity.Email,
		DisplayName:  identity.DisplayName,
		AvatarURL:    identity.AvatarURL,
		Role:         model.UserRoleOwner,
		Status:       model.UserStatusActive,
		LastLoginAt:  &now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := h.users.Create(ctx, user); err != nil {
		return model.User{}, fmt.Errorf("creating user: %w", err)
	}

	// Create the auth provider link.
	link := model.UserAuthProvider{
		ID:        "uap_" + h.newID(),
		UserID:    user.ID,
		Provider:  identity.Provider,
		SubjectID: identity.Subject,
		CreatedAt: now,
	}
	if err := h.userAuthProviders.Create(ctx, link); err != nil {
		return model.User{}, fmt.Errorf("creating auth provider link: %w", err)
	}

	h.log.Info("auto-created enterprise and user",
		"enterprise_id", enterprise.ID,
		"user_id", user.ID,
		"email", user.Email,
		"personal", personal,
	)

	return user, nil
}

func generateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func isNotFound(err error) bool {
	_, ok := err.(*store.ErrNotFound)
	return ok
}
