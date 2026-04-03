package auth

import (
	"context"
	"net/http"
	"strings"
)

type authContextKey int

const (
	claimsKey authContextKey = iota
)

// ClaimsFromContext returns the authenticated claims from the request context,
// or nil if the request is not authenticated.
func ClaimsFromContext(ctx context.Context) *Claims {
	c, _ := ctx.Value(claimsKey).(*Claims)
	return c
}

// ContextWithClaims returns a context with the given claims attached.
func ContextWithClaims(ctx context.Context, claims *Claims) context.Context {
	return context.WithValue(ctx, claimsKey, claims)
}

// Middleware returns HTTP middleware that validates Bearer JWTs and injects
// claims into the request context. Requests to paths in skipPaths bypass
// authentication (e.g. health checks, auth callbacks).
func Middleware(issuer *TokenIssuer, skipPaths map[string]bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip auth for explicitly excluded paths.
			if skipPaths[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}

			// Skip auth for auth routes (prefix match).
			if strings.HasPrefix(r.URL.Path, "/auth/") {
				next.ServeHTTP(w, r)
				return
			}

			token := extractBearerToken(r)
			if token == "" {
				http.Error(w, `{"error":"missing or invalid Authorization header"}`, http.StatusUnauthorized)
				return
			}

			claims, err := issuer.Validate(token)
			if err != nil {
				http.Error(w, `{"error":"invalid or expired token"}`, http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), claimsKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractBearerToken extracts the token from the Authorization header.
// It also checks for the access_token cookie as a fallback for browser flows.
func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	// Fallback: check cookie for browser-based flows.
	if c, err := r.Cookie("access_token"); err == nil {
		return c.Value
	}
	return ""
}
