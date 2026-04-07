package app

import (
	"net/http"

	"time"

	"github.com/ALRubinger/aileron/core/account"
	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/vault"
	"github.com/ALRubinger/aileron/enclave"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

// --- Connected Accounts ---

func (s *apiServer) ListConnectedAccounts(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	accounts, err := s.connectedAccounts.List(r.Context(), store.ConnectedAccountFilter{UserID: userID})
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

	if s.accountService != nil {
		if err := s.accountService.Disconnect(r.Context(), id); err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
	} else {
		if err := s.connectedAccounts.Delete(r.Context(), id); err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *apiServer) ConnectAccount(w http.ResponseWriter, r *http.Request, providerStr string) {
	if s.accountService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "connected accounts not configured")
		return
	}

	provider := model.ConnectedAccountProvider(providerStr)
	state := s.newID()

	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	redirectURL := scheme + "://" + r.Host + "/v1/connect/" + providerStr + "/callback"

	result, err := s.accountService.AuthorizationURL(r.Context(), provider, state, redirectURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_provider", err.Error())
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "aileron_connect_state",
		Value:    state,
		Path:     "/v1/connect/",
		MaxAge:   600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})

	http.Redirect(w, r, result.URL, http.StatusFound)
}

func (s *apiServer) ConnectAccountCallback(w http.ResponseWriter, r *http.Request, providerStr string, params api.ConnectAccountCallbackParams) {
	if s.accountService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "connected accounts not configured")
		return
	}

	cookie, err := r.Cookie("aileron_connect_state")
	if err != nil || cookie.Value != params.State {
		writeError(w, http.StatusBadRequest, "invalid_state", "state mismatch; possible CSRF")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:   "aileron_connect_state",
		Value:  "",
		Path:   "/auth/connect/",
		MaxAge: -1,
	})

	userID, _, ok := s.requireAuth(w, r)
	if !ok {
		return
	}

	provider := model.ConnectedAccountProvider(providerStr)

	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	redirectURL := scheme + "://" + r.Host + "/v1/connect/" + providerStr + "/callback"

	// TEE mode: forward the OAuth code to the enclave. The enclave exchanges
	// the code for tokens, encrypts them with the user's KEK, and returns
	// only the ciphertext. The server never sees the plaintext tokens.
	if s.enclaveClient != nil {
		oauthResp, oauthErr := s.enclaveClient.OAuthExchange(r.Context(), enclave.OAuthExchangeRequest{
			UserID:           userID,
			Provider:         providerStr,
			Code:             params.Code,
			RedirectURI:      redirectURL,
			ClientID:         s.accountService.ClientID(),
			ClientSecret:     s.accountService.ClientSecret(),
			Scopes:           s.accountService.ScopesFor(provider),
			TokenEndpoint:    s.accountService.TokenEndpointFor(provider),
			UserInfoEndpoint: s.accountService.UserInfoEndpoint(),
		})
		if oauthErr != nil {
			s.log.Error("enclave OAuth exchange failed", "error", oauthErr, "provider", providerStr)
			writeError(w, http.StatusInternalServerError, "callback_error", "failed to connect account: "+oauthErr.Error())
			return
		}

		// Store the encrypted token in the vault — server never saw plaintext.
		now := time.Now().UTC()
		acct := model.ConnectedAccount{
			ID:        "conn_" + s.newID(),
			UserID:    userID,
			Provider:  provider,
			Email:     oauthResp.Email,
			Scopes:    s.accountService.ScopesFor(provider),
			Status:    model.ConnectedAccountStatusActive,
			CreatedAt: now,
			UpdatedAt: now,
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
		if err := s.connectedAccounts.Create(r.Context(), acct); err != nil {
			writeError(w, http.StatusInternalServerError, "store_error", "failed to create account record")
			return
		}

		http.Redirect(w, r, "/settings", http.StatusFound)
		return
	}

	// Direct mode: exchange code on the host and optionally encrypt with KEK.
	svc := s.accountService
	if s.kekCache != nil {
		kek := s.kekCache.Get(userID)
		if kek != nil {
			svc = svc.WithVault(vault.NewUserScopedVault(s.vault, kek))
			defer zeroBytes(kek)
		}
	}

	_, err = svc.HandleCallback(r.Context(), provider, account.CallbackRequest{
		Code:        params.Code,
		State:       params.State,
		RedirectURL: redirectURL,
		UserID:      userID,
	})
	if err != nil {
		s.log.Error("connected account callback failed", "error", err, "provider", providerStr)
		writeError(w, http.StatusInternalServerError, "callback_error", "failed to connect account: "+err.Error())
		return
	}

	http.Redirect(w, r, "/settings", http.StatusFound)
}

func connectedAccountToAPI(a model.ConnectedAccount) api.ConnectedAccount {
	provider := api.ConnectedAccountProvider(a.Provider)
	status := api.ConnectedAccountStatus(a.Status)
	email := openapi_types.Email(a.Email)
	createdAt := a.CreatedAt
	updatedAt := a.UpdatedAt
	return api.ConnectedAccount{
		Id:        &a.ID,
		UserId:    &a.UserID,
		Provider:  &provider,
		Email:     &email,
		Scopes:    &a.Scopes,
		Status:    &status,
		CreatedAt: &createdAt,
		UpdatedAt: &updatedAt,
	}
}
