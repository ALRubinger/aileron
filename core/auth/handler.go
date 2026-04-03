package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
)

// Handler serves the authentication HTTP routes.
type Handler struct {
	log               *slog.Logger
	registry          *Registry
	enforcer          Enforcer
	issuer            *TokenIssuer
	users             store.UserStore
	enterprises       store.EnterpriseStore
	sessions          store.SessionStore
	verificationCodes store.VerificationCodeStore
	mailer            Mailer
	newID             func() string
	uiRedirect        string // URL to redirect to after successful auth
	refreshTTL        time.Duration
	verificationTTL   time.Duration
	autoVerifyEmail   bool
	// callbackBaseURL, when non-empty, is used as the base URL for all OAuth
	// callback redirects. If set, the originating host is encoded in the OAuth
	// state parameter so the stable domain can relay the callback back.
	callbackBaseURL string
	// trustedOrigins is the set of origin patterns allowed as relay targets.
	trustedOrigins []string
}

// HandlerConfig configures the auth handler.
type HandlerConfig struct {
	Log               *slog.Logger
	Registry          *Registry
	Enforcer          Enforcer
	Issuer            *TokenIssuer
	Users             store.UserStore
	Enterprises       store.EnterpriseStore
	Sessions          store.SessionStore
	VerificationCodes store.VerificationCodeStore
	Mailer            Mailer
	NewID             func() string
	UIRedirect        string        // e.g. "http://localhost:5173"
	AutoVerifyEmail   bool          // skip email verification (dev/CI only)
	RefreshTTL        time.Duration // e.g. 7 * 24 * time.Hour
	VerificationTTL   time.Duration // e.g. 15 * time.Minute
	// CallbackBaseURL, when set, overrides the dynamically derived callback URL
	// with a stable domain. Used to support OAuth on Railway branch deployments.
	// e.g. "https://auth.withaileron.ai"
	CallbackBaseURL string
	// TrustedOrigins lists origin patterns (e.g. "*.up.railway.app") that are
	// allowed as relay targets in the stable-domain callback flow.
	TrustedOrigins []string
}

// NewHandler creates an auth handler.
func NewHandler(cfg HandlerConfig) *Handler {
	if cfg.RefreshTTL == 0 {
		cfg.RefreshTTL = 7 * 24 * time.Hour
	}
	if cfg.VerificationTTL == 0 {
		cfg.VerificationTTL = 15 * time.Minute
	}
	if cfg.UIRedirect == "" {
		cfg.UIRedirect = "/"
	}
	return &Handler{
		log:               cfg.Log,
		registry:          cfg.Registry,
		enforcer:          cfg.Enforcer,
		issuer:            cfg.Issuer,
		users:             cfg.Users,
		enterprises:       cfg.Enterprises,
		sessions:          cfg.Sessions,
		verificationCodes: cfg.VerificationCodes,
		mailer:            cfg.Mailer,
		newID:             cfg.NewID,
		uiRedirect:        cfg.UIRedirect,
		refreshTTL:        cfg.RefreshTTL,
		verificationTTL:   cfg.VerificationTTL,
		autoVerifyEmail:   cfg.AutoVerifyEmail,
		callbackBaseURL:   strings.TrimRight(cfg.CallbackBaseURL, "/"),
		trustedOrigins:    cfg.TrustedOrigins,
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
		http.Error(w, `{"error":"unknown auth provider"}`, http.StatusBadRequest)
		return
	}

	csrfToken, err := generateState()
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	// When a stable callback base URL is configured, encode the originating
	// host into the state so the relay can return to this deployment.
	state := csrfToken
	if h.callbackBaseURL != "" {
		if originatingHost := requestHost(r); originatingHost != stableHost(h.callbackBaseURL) {
			state = composeState(csrfToken, originatingHost)
		}
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
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
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

// callbackURL returns the OAuth callback URL for the given provider.
// When callbackBaseURL is set, that stable domain is used so that all OAuth
// providers only need one registered redirect URI. Otherwise the URL is derived
// from the incoming request, which works for single-domain deployments and local
// development. It respects X-Forwarded-Proto and X-Forwarded-Host.
func (h *Handler) callbackURL(r *http.Request, provider string) string {
	if h.callbackBaseURL != "" {
		return fmt.Sprintf("%s/auth/%s/callback", h.callbackBaseURL, provider)
	}
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
// When callbackBaseURL is configured and the state encodes an originating host
// that differs from the current host, this acts as a relay: it validates the
// originating host against trustedOrigins and redirects the code+state back to
// the originating deployment to complete the token exchange there.
func (h *Handler) handleCallback(w http.ResponseWriter, r *http.Request) {
	providerName := r.PathValue("provider")
	provider, ok := h.registry.Get(providerName)
	if !ok {
		http.Error(w, `{"error":"unknown auth provider"}`, http.StatusBadRequest)
		return
	}

	stateParam := r.URL.Query().Get("state")

	// Relay mode: if state encodes an originating host that differs from ours,
	// validate it and forward the callback there to complete the auth flow.
	if _, originatingHost := parseState(stateParam); originatingHost != "" && originatingHost != requestHost(r) {
		if !isTrustedOrigin(originatingHost, h.trustedOrigins) {
			h.log.Warn("relay rejected: untrusted originating host", "host", originatingHost)
			http.Error(w, `{"error":"untrusted originating host"}`, http.StatusBadRequest)
			return
		}

		// Check for provider error before relaying so we don't send bad codes.
		if errParam := r.URL.Query().Get("error"); errParam != "" {
			h.log.Warn("auth provider returned error during relay", "provider", providerName, "error", errParam)
			http.Error(w, fmt.Sprintf(`{"error":"provider error: %s"}`, errParam), http.StatusBadRequest)
			return
		}

		relayURL := fmt.Sprintf("https://%s/auth/%s/callback?code=%s&state=%s",
			originatingHost,
			providerName,
			url.QueryEscape(r.URL.Query().Get("code")),
			url.QueryEscape(stateParam),
		)
		h.log.Info("relaying OAuth callback to originating host", "host", originatingHost, "provider", providerName)
		http.Redirect(w, r, relayURL, http.StatusFound)
		return
	}

	// Validate CSRF state.
	stateCookie, err := r.Cookie("oauth_state")
	if err != nil || stateCookie.Value == "" {
		http.Error(w, `{"error":"missing state cookie"}`, http.StatusBadRequest)
		return
	}
	if stateParam != stateCookie.Value {
		http.Error(w, `{"error":"state mismatch"}`, http.StatusBadRequest)
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
		http.Error(w, fmt.Sprintf(`{"error":"provider error: %s"}`, errParam), http.StatusBadRequest)
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
		http.Error(w, `{"error":"authentication failed"}`, http.StatusUnauthorized)
		return
	}

	ctx := r.Context()

	// Look up user: first by provider+subject (fast path for returning users),
	// then by email (handles sign-in via a new provider for an existing account).
	user, err := h.users.GetByProviderSubject(ctx, identity.Provider, identity.Subject)
	if err != nil && isNotFound(err) {
		// Provider subject not seen before — check if the email already exists.
		user, err = h.users.GetByEmail(ctx, identity.Email)
	}
	if err != nil {
		if !isNotFound(err) {
			h.log.Error("user lookup failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}

		// Entirely new user — auto-create enterprise and user.
		user, err = h.createEnterpriseAndUser(ctx, identity)
		if err != nil {
			h.log.Error("failed to create enterprise/user", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
	} else {
		// Existing user — enforce SSO policies.
		allowed, err := h.enforcer.IsProviderAllowed(ctx, user.EnterpriseID, identity.Provider)
		if err != nil {
			h.log.Error("provider check failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		if !allowed {
			http.Error(w, `{"error":"auth provider not allowed for this enterprise"}`, http.StatusForbidden)
			return
		}

		domainAllowed, err := h.enforcer.IsEmailDomainAllowed(ctx, user.EnterpriseID, identity.Email)
		if err != nil {
			h.log.Error("domain check failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		if !domainAllowed {
			http.Error(w, `{"error":"email domain not allowed for this enterprise"}`, http.StatusForbidden)
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

	// Issue tokens.
	accessToken, err := h.issuer.Issue(user.ID, user.EnterpriseID, user.Email, string(user.Role))
	if err != nil {
		h.log.Error("failed to issue token", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	refreshRaw, refreshHash, err := GenerateRefreshToken()
	if err != nil {
		h.log.Error("failed to generate refresh token", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
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
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
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
	http.Redirect(w, r, h.uiRedirect, http.StatusTemporaryRedirect)
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
		http.Error(w, `{"error":"missing refresh token"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	hash := HashToken(refreshToken)
	session, err := h.sessions.GetByRefreshTokenHash(ctx, hash)
	if err != nil {
		http.Error(w, `{"error":"invalid refresh token"}`, http.StatusUnauthorized)
		return
	}
	if time.Now().After(session.ExpiresAt) {
		_ = h.sessions.Delete(ctx, session.ID)
		http.Error(w, `{"error":"refresh token expired"}`, http.StatusUnauthorized)
		return
	}

	user, err := h.users.Get(ctx, session.UserID)
	if err != nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusUnauthorized)
		return
	}

	// Issue new access token.
	accessToken, err := h.issuer.Issue(user.ID, user.EnterpriseID, user.Email, string(user.Role))
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	// Rotate refresh token.
	newRefreshRaw, newRefreshHash, err := GenerateRefreshToken()
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
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
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
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
		ID:                    "usr_" + h.newID(),
		EnterpriseID:          enterprise.ID,
		Email:                 identity.Email,
		DisplayName:           identity.DisplayName,
		AvatarURL:             identity.AvatarURL,
		Role:                  model.UserRoleOwner,
		Status:                model.UserStatusActive,
		AuthProvider:          identity.Provider,
		AuthProviderSubjectID: identity.Subject,
		LastLoginAt:           &now,
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	if err := h.users.Create(ctx, user); err != nil {
		return model.User{}, fmt.Errorf("creating user: %w", err)
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

// composeState builds a state value that embeds the originating host so the
// stable-domain relay can return the OAuth callback to the correct deployment.
// Format: "{csrf_token}.{base64url(originatingHost)}"
func composeState(csrfToken, originatingHost string) string {
	encoded := base64.RawURLEncoding.EncodeToString([]byte(originatingHost))
	return csrfToken + "." + encoded
}

// parseState splits a state value into its CSRF token and optional originating
// host. If the state has no embedded host, originatingHost is empty.
func parseState(state string) (csrfToken, originatingHost string) {
	idx := strings.Index(state, ".")
	if idx < 0 {
		return state, ""
	}
	csrf := state[:idx]
	decoded, err := base64.RawURLEncoding.DecodeString(state[idx+1:])
	if err != nil {
		// Not a valid compound state — treat the whole value as the CSRF token.
		return state, ""
	}
	return csrf, string(decoded)
}

// requestHost returns the effective host of the incoming request, respecting
// the X-Forwarded-Host header for reverse-proxied deployments.
func requestHost(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		return fwd
	}
	return r.Host
}

// stableHost extracts just the host portion from a base URL string.
// Returns an empty string if the URL cannot be parsed.
func stableHost(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// isTrustedOrigin reports whether host matches any of the provided patterns.
// Patterns may use a leading "*." wildcard to match any subdomain.
func isTrustedOrigin(host string, patterns []string) bool {
	for _, pattern := range patterns {
		if strings.HasPrefix(pattern, "*.") {
			suffix := pattern[1:] // e.g. ".up.railway.app"
			if strings.HasSuffix(host, suffix) {
				return true
			}
		} else if host == pattern {
			return true
		}
	}
	return false
}

func isNotFound(err error) bool {
	_, ok := err.(*store.ErrNotFound)
	return ok
}
