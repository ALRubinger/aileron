package app

import (
	"net/http"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/auth"
)

// Auth and account endpoints are handled by the auth.Handler registered
// directly on the mux. These stubs satisfy the generated ServerInterface.
// When auth is disabled they return 501; when auth is enabled the auth
// handler's mux routes take priority over these.

func (s *apiServer) AuthLogin(w http.ResponseWriter, r *http.Request, provider string) {
	http.Error(w, `{"error":"auth not enabled"}`, http.StatusNotImplemented)
}

func (s *apiServer) AuthCallback(w http.ResponseWriter, r *http.Request, provider string, params api.AuthCallbackParams) {
	http.Error(w, `{"error":"auth not enabled"}`, http.StatusNotImplemented)
}

func (s *apiServer) AuthSignup(w http.ResponseWriter, r *http.Request) {
	http.Error(w, `{"error":"auth not enabled"}`, http.StatusNotImplemented)
}

func (s *apiServer) AuthVerifyEmail(w http.ResponseWriter, r *http.Request) {
	http.Error(w, `{"error":"auth not enabled"}`, http.StatusNotImplemented)
}

func (s *apiServer) AuthEmailLogin(w http.ResponseWriter, r *http.Request) {
	http.Error(w, `{"error":"auth not enabled"}`, http.StatusNotImplemented)
}

func (s *apiServer) AuthRefresh(w http.ResponseWriter, r *http.Request) {
	http.Error(w, `{"error":"auth not enabled"}`, http.StatusNotImplemented)
}

func (s *apiServer) AuthLogout(w http.ResponseWriter, r *http.Request) {
	http.Error(w, `{"error":"auth not enabled"}`, http.StatusNotImplemented)
}

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
	writeJSON(w, http.StatusOK, ent)
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
	writeJSON(w, http.StatusOK, user)
}
