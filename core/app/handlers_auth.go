package app

import (
	"net/http"

	"github.com/ALRubinger/aileron/core/auth"
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
