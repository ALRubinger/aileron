package app

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/ALRubinger/aileron/core/account"
	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/auth"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/vault"
	"github.com/ALRubinger/aileron/enclave"
)

// --- Connected Accounts ---

// handleCreateConnectedAccount creates a connected account directly
// (for programmatic/admin use and testing). The normal user flow is
// the OAuth connect path (/v1/connect/{provider}).
// POST /v1/connected-accounts
func (s *apiServer) handleCreateConnectedAccount(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	var req struct {
		Provider       string            `json:"provider"`
		Scopes         []string          `json:"scopes"`
		ExternalUserID string            `json:"external_user_id"`
		ExternalTeamID string            `json:"external_team_id"`
		Token          map[string]string `json:"token,omitempty"` // optional: stored in vault
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON request body")
		return
	}
	if req.Provider == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "provider is required")
		return
	}

	now := time.Now().UTC()
	acct := model.ConnectedAccount{
		ID:             "conn_" + s.newID(),
		UserID:         userID,
		Provider:       model.ConnectedAccountProvider(req.Provider),
		Scopes:         req.Scopes,
		Status:         model.ConnectedAccountStatusActive,
		ExternalUserID: req.ExternalUserID,
		ExternalTeamID: req.ExternalTeamID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	// Store token in vault if provided.
	if len(req.Token) > 0 {
		tokenJSON, _ := json.Marshal(req.Token)
		if err := s.vault.Put(r.Context(), acct.VaultPath(), tokenJSON, vault.Metadata{
			Type: "oauth_token",
			Labels: map[string]string{
				"provider": req.Provider,
				"user_id":  userID,
			},
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "vault_error", err.Error())
			return
		}
	}

	upserted, err := s.connectedAccounts.Upsert(r.Context(), acct)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, connectedAccountToAPI(upserted))
}

func (s *apiServer) ListConnectedAccounts(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	filter := store.ConnectedAccountFilter{UserID: userID}
	if providerParam := r.URL.Query().Get("provider"); providerParam != "" {
		provider := model.ConnectedAccountProvider(providerParam)
		filter.Provider = &provider
	}
	if statusParam := r.URL.Query().Get("status"); statusParam != "" {
		status := model.ConnectedAccountStatus(statusParam)
		filter.Status = &status
	}
	if extUser := r.URL.Query().Get("external_user_id"); extUser != "" {
		filter.ExternalUserID = extUser
	}
	if extTeam := r.URL.Query().Get("external_team_id"); extTeam != "" {
		filter.ExternalTeamID = extTeam
	}

	accounts, err := s.connectedAccounts.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	items := make([]api.ConnectedAccount, 0, len(accounts))
	for _, a := range accounts {
		items = append(items, connectedAccountToAPI(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *apiServer) GetConnectedAccount(w http.ResponseWriter, r *http.Request, id string) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	acc, err := s.connectedAccounts.Get(r.Context(), id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", "connected account not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	if userID != "" && acc.UserID != userID {
		writeError(w, http.StatusNotFound, "not_found", "connected account not found")
		return
	}

	writeJSON(w, http.StatusOK, connectedAccountToAPI(acc))
}

func (s *apiServer) DeleteConnectedAccount(w http.ResponseWriter, r *http.Request, id string) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	acc, err := s.connectedAccounts.Get(r.Context(), id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", "connected account not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	if userID != "" && acc.UserID != userID {
		writeError(w, http.StatusNotFound, "not_found", "connected account not found")
		return
	}

	// Always clean up the vault secret if it exists.
	_ = s.vault.Delete(r.Context(), acc.VaultPath())

	if s.accountService != nil {
		if err := s.accountService.Disconnect(r.Context(), id); err != nil {
			// Disconnect may fail if the account was created without a vault token
			// (e.g. via POST /v1/connected-accounts without a token field).
			// Fall through to delete the account record directly.
			if err := s.connectedAccounts.Delete(r.Context(), id); err != nil {
				writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
				return
			}
		}
	} else {
		if err := s.connectedAccounts.Delete(r.Context(), id); err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// connectReturnDefault is the fallback redirect after a successful account connection.
const connectReturnDefault = "/settings/connected-accounts"

func (s *apiServer) ConnectAccount(w http.ResponseWriter, r *http.Request, providerStr string, params api.ConnectAccountParams) {
	if s.accountService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "connected accounts not configured")
		return
	}

	// Fail fast: require passphrase + unlocked vault before starting OAuth flow.
	// In TEE mode, the enclave holds the KEK — skip the server-side cache check
	// and verify only that a passphrase exists.
	if s.enclaveClient != nil {
		userID, _, ok := s.requireAuth(w, r)
		if !ok {
			return
		}
		if s.userKeyMaterials != nil {
			if _, err := s.userKeyMaterials.Get(r.Context(), userID); err != nil && isNotFound(err) {
				writeError(w, http.StatusBadRequest, "passphrase_required", "set a vault passphrase before connecting accounts")
				return
			}
		}
	} else if s.kekSessionCache != nil {
		kek, ok := s.requireKEK(w, r)
		if !ok {
			return
		}
		zeroBytes(kek)
	}

	provider := model.ConnectedAccountProvider(providerStr)
	state := s.newID()

	redirectURL := requestScheme(r) + "://" + r.Host + "/v1/connect/" + providerStr + "/callback"

	result, err := s.accountService.AuthorizationURL(r.Context(), provider, state, redirectURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_provider", err.Error())
		return
	}

	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"

	http.SetCookie(w, &http.Cookie{
		Name:     "aileron_connect_state",
		Value:    state,
		Path:     "/v1/connect/",
		MaxAge:   600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})

	// Persist the caller's desired return URL so the callback can redirect there.
	returnTo := connectReturnDefault
	if params.ReturnTo != nil && *params.ReturnTo != "" {
		returnTo = *params.ReturnTo
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "aileron_connect_return",
		Value:    returnTo,
		Path:     "/v1/connect/",
		MaxAge:   600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})

	http.Redirect(w, r, result.URL, http.StatusFound)
}

func (s *apiServer) ConnectAccountCallback(w http.ResponseWriter, r *http.Request, providerStr string, params api.ConnectAccountCallbackParams) {
	// Slack Marketplace and App Directory installs may redirect here instead
	// of /v1/slack/install/callback (we don't control the redirect URL Slack
	// chooses). Detect this case: provider is slack, no state cookie (the
	// cookie is only set when a user starts the connect flow from Aileron's
	// UI), and no authentication credentials.
	if providerStr == "slack" {
		if _, err := r.Cookie("aileron_connect_state"); err != nil {
			if auth.ClaimsFromContext(r.Context()) == nil {
				s.handleSlackInstall(w, r)
				return
			}
		}
	}

	if s.accountService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "connected accounts not configured")
		return
	}

	cookie, err := r.Cookie("aileron_connect_state")
	state := ""
	if params.State != nil {
		state = *params.State
	}
	if err != nil || cookie.Value != state {
		writeError(w, http.StatusBadRequest, "invalid_state", "state mismatch; possible CSRF")
		return
	}

	// Read the return URL set during ConnectAccount, falling back to the default.
	returnTo := connectReturnDefault
	if rc, err := r.Cookie("aileron_connect_return"); err == nil && rc.Value != "" {
		returnTo = rc.Value
	}

	// Clear both cookies.
	http.SetCookie(w, &http.Cookie{
		Name:   "aileron_connect_state",
		Value:  "",
		Path:   "/v1/connect/",
		MaxAge: -1,
	})
	http.SetCookie(w, &http.Cookie{
		Name:   "aileron_connect_return",
		Value:  "",
		Path:   "/v1/connect/",
		MaxAge: -1,
	})

	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	provider := model.ConnectedAccountProvider(providerStr)

	redirectURL := requestScheme(r) + "://" + r.Host + "/v1/connect/" + providerStr + "/callback"

	// TEE mode: forward the OAuth code to the enclave. The enclave exchanges
	// the code for tokens, encrypts them with the user's KEK, and returns
	// only the ciphertext. The server never sees the plaintext tokens.
	if s.enclaveClient != nil {
		// Verify passphrase exists (enclave has escrowed KEK).
		if s.userKeyMaterials != nil {
			if _, err := s.userKeyMaterials.Get(r.Context(), userID); err != nil && isNotFound(err) {
				writeError(w, http.StatusBadRequest, "passphrase_required", "set a vault passphrase before connecting accounts")
				return
			}
		}
		providerSvc, provErr := s.accountService.ProviderFor(provider)
		if provErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_provider", provErr.Error())
			return
		}
		oauthResp, oauthErr := s.enclaveClient.OAuthExchange(r.Context(), enclave.OAuthExchangeRequest{
			UserID:           userID,
			Provider:         providerStr,
			Code:             params.Code,
			RedirectURI:      redirectURL,
			ClientID:         providerSvc.ClientID(),
			ClientSecret:     providerSvc.ClientSecret(),
			Scopes:           providerSvc.ScopesFor(provider),
			TokenEndpoint:    providerSvc.TokenEndpointFor(provider),
			UserInfoEndpoint: providerSvc.UserInfoEndpoint(),
		})
		if oauthErr != nil {
			s.log.Error("enclave OAuth exchange failed", "error", oauthErr, "provider", providerStr)
			writeError(w, http.StatusInternalServerError, "callback_error", "failed to connect account: "+oauthErr.Error())
			return
		}

		// Store the encrypted token in the vault — server never saw plaintext.
		now := time.Now().UTC()

		// Use provider-specific external IDs when available (e.g. Slack
		// returns user ID + team ID in the token response). Fall back to
		// email for providers that use standard OAuth (Google, etc.).
		externalUserID := oauthResp.ExternalUserID
		if externalUserID == "" {
			externalUserID = oauthResp.Email
		}

		acct := model.ConnectedAccount{
			ID:             "conn_" + s.newID(),
			UserID:         userID,
			Provider:       provider,
			Scopes:         providerSvc.ScopesFor(provider),
			ExternalUserID: externalUserID,
			ExternalTeamID: oauthResp.ExternalTeamID,
			Status:         model.ConnectedAccountStatusActive,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := s.vault.Put(r.Context(), acct.VaultPath(), oauthResp.EncryptedToken, vault.Metadata{
			Type: "oauth_refresh_token",
			Labels: map[string]string{
				"provider":  providerStr,
				"user_id":   userID,
				"encrypted": "true",
			},
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "vault_error", "failed to store encrypted token")
			return
		}
		if _, err := s.connectedAccounts.Upsert(r.Context(), acct); err != nil {
			writeError(w, http.StatusInternalServerError, "store_error", "failed to upsert account record")
			return
		}

		http.Redirect(w, r, returnTo, http.StatusFound)
		return
	}

	// Direct mode: exchange code on the host, encrypt token with user's KEK.
	kek, ok := s.requireKEK(w, r)
	if !ok {
		return
	}
	defer zeroBytes(kek)

	encVault := vault.NewUserScopedVault(s.vault, kek)
	providerSvc, provErr := s.accountService.ProviderFor(provider)
	if provErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_provider", provErr.Error())
		return
	}
	scopedSvc := providerSvc.WithVault(encVault)

	_, err = scopedSvc.HandleCallback(r.Context(), provider, account.CallbackRequest{
		Code:        params.Code,
		State:       state,
		RedirectURL: redirectURL,
		UserID:      userID,
	})
	if err != nil {
		s.log.Error("connected account callback failed", "error", err, "provider", providerStr)
		writeError(w, http.StatusInternalServerError, "callback_error", "failed to connect account: "+err.Error())
		return
	}

	http.Redirect(w, r, returnTo, http.StatusFound)
}

// requestScheme returns "https" or "http" based on the request. It checks
// X-Forwarded-Proto first (set by reverse proxies like Railway, Cloudflare)
// then falls back to r.TLS. Defaults to "https" when behind a proxy that
// terminates TLS — the Go server sees plain HTTP but the client used HTTPS.
func requestScheme(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func connectedAccountToAPI(a model.ConnectedAccount) api.ConnectedAccount {
	provider := api.ConnectedAccountProvider(a.Provider)
	status := api.ConnectedAccountStatus(a.Status)
	createdAt := a.CreatedAt
	updatedAt := a.UpdatedAt
	result := api.ConnectedAccount{
		Id:             &a.ID,
		UserId:         &a.UserID,
		Provider:       &provider,
		Scopes:         &a.Scopes,
		Status:         &status,
		ExternalUserId: &a.ExternalUserID,
		CreatedAt:      &createdAt,
		UpdatedAt:      &updatedAt,
	}
	return result
}
