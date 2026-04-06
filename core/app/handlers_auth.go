package app

import (
	"net/http"
	"time"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/auth"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

// GetCurrentEnterprise and GetCurrentUser are generated ServerInterface
// methods for the Enterprises and Users tags. Auth endpoints (Auth tag)
// are excluded from code generation and handled by core/auth.Handler.

func (s *apiServer) GetCurrentEnterprise(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}

	if s.enterprises == nil {
		http.Error(w, `{"error":"auth not enabled"}`, http.StatusNotImplemented)
		return
	}

	ent, err := s.enterprises.Get(r.Context(), claims.EnterpriseID)
	if err != nil {
		http.Error(w, `{"error":"enterprise not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, enterpriseToAPI(ent))
}

func (s *apiServer) GetCurrentUser(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}

	if s.users == nil {
		http.Error(w, `{"error":"auth not enabled"}`, http.StatusNotImplemented)
		return
	}

	user, err := s.users.Get(r.Context(), claims.Subject)
	if err != nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}

	// Load connected auth providers.
	if s.userAuthProviders != nil {
		providers, err := s.userAuthProviders.ListByUser(r.Context(), user.ID)
		if err != nil {
			s.log.Error("failed to list auth providers", "error", err)
		} else {
			user.AuthProviders = providers
		}
	}

	writeJSON(w, http.StatusOK, userToAPI(user))
}

func (s *apiServer) UpdateCurrentUser(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "not authenticated")
		return
	}

	if s.users == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "auth not enabled")
		return
	}

	var req api.UpdateUserRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}

	user, err := s.users.Get(r.Context(), claims.Subject)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "user not found")
		return
	}

	if req.DisplayName != nil {
		user.DisplayName = *req.DisplayName
	}

	user.UpdatedAt = time.Now().UTC()
	if err := s.users.Update(r.Context(), user); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to update user")
		return
	}

	writeJSON(w, http.StatusOK, userToAPI(user))
}

func (s *apiServer) UpdateCurrentEnterprise(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "not authenticated")
		return
	}

	if s.enterprises == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "auth not enabled")
		return
	}

	if claims.Role != string(model.UserRoleOwner) && claims.Role != string(model.UserRoleAdmin) {
		writeError(w, http.StatusForbidden, "forbidden", "owner or admin role required")
		return
	}

	var req api.UpdateEnterpriseRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}

	ent, err := s.enterprises.Get(r.Context(), claims.EnterpriseID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "enterprise not found")
		return
	}

	if req.Name != nil {
		ent.Name = *req.Name
	}
	if req.BillingEmail != nil {
		ent.BillingEmail = string(*req.BillingEmail)
	}
	if req.SsoRequired != nil {
		ent.SSORequired = *req.SsoRequired
	}
	if req.AllowedAuthProviders != nil {
		ent.AllowedAuthProviders = *req.AllowedAuthProviders
	}
	if req.AllowedEmailDomains != nil {
		ent.AllowedEmailDomains = *req.AllowedEmailDomains
	}

	ent.UpdatedAt = time.Now().UTC()
	if err := s.enterprises.Update(r.Context(), ent); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to update enterprise")
		return
	}

	writeJSON(w, http.StatusOK, enterpriseToAPI(ent))
}

func (s *apiServer) DisconnectAuthProvider(w http.ResponseWriter, r *http.Request, provider string) {
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "not authenticated")
		return
	}

	if s.userAuthProviders == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "auth not enabled")
		return
	}

	user, err := s.users.Get(r.Context(), claims.Subject)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "user not found")
		return
	}

	// Load current providers to enforce last-method guard.
	providers, err := s.userAuthProviders.ListByUser(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list providers")
		return
	}

	// Cannot disconnect if this is the only auth method and no password is set.
	if len(providers) <= 1 && user.PasswordHash == "" {
		writeError(w, http.StatusBadRequest, "last_auth_method",
			"cannot disconnect the last auth provider without a password set")
		return
	}

	if err := s.userAuthProviders.DeleteByUserAndProvider(r.Context(), user.ID, provider); err != nil {
		if _, ok := err.(*store.ErrNotFound); ok {
			writeError(w, http.StatusNotFound, "not_found", "provider not connected")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "failed to disconnect provider")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- model → API type conversions ---

func userToAPI(u model.User) api.User {
	out := api.User{
		Id:           u.ID,
		EnterpriseId: u.EnterpriseID,
		Email:        openapi_types.Email(u.Email),
		DisplayName:  u.DisplayName,
		Role:         api.UserRole(u.Role),
		Status:       api.UserStatus(u.Status),
		HasPassword:  u.PasswordHash != "",
		LastLoginAt:  u.LastLoginAt,
		CreatedAt:    u.CreatedAt,
		UpdatedAt:    u.UpdatedAt,
	}
	if u.AvatarURL != "" {
		out.AvatarUrl = &u.AvatarURL
	}

	// Build auth_providers array from connected OAuth providers.
	var providers []api.UserAuthProviderLink
	for _, p := range u.AuthProviders {
		providers = append(providers, api.UserAuthProviderLink{
			Provider:    p.Provider,
			ConnectedAt: p.CreatedAt,
		})
	}
	if providers == nil {
		providers = []api.UserAuthProviderLink{}
	}
	out.AuthProviders = providers
	return out
}

func enterpriseToAPI(e model.Enterprise) api.Enterprise {
	out := api.Enterprise{
		Id:           e.ID,
		Name:         e.Name,
		Slug:         e.Slug,
		Plan:         api.EnterprisePlan(e.Plan),
		BillingEmail: openapi_types.Email(e.BillingEmail),
		CreatedAt:    e.CreatedAt,
		UpdatedAt:    e.UpdatedAt,
	}
	if e.Personal {
		out.Personal = &e.Personal
	}
	if e.SSORequired {
		out.SsoRequired = &e.SSORequired
	}
	if len(e.AllowedAuthProviders) > 0 {
		out.AllowedAuthProviders = &e.AllowedAuthProviders
	}
	if len(e.AllowedEmailDomains) > 0 {
		out.AllowedEmailDomains = &e.AllowedEmailDomains
	}
	return out
}
