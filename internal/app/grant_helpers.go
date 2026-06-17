package app

import (
	"context"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/auth"
)

// newExecutionGrant builds an execution grant for an approved intent. The grant
// records the connector and bounded parameters; credentials are resolved and
// decrypted at execution time via the direct host path (vault.Get + user KEK).
func (s *apiServer) newExecutionGrant(_ context.Context, intent api.IntentEnvelope, grantID string, expiresAt time.Time, bounded *map[string]interface{}, _ string) (api.ExecutionGrant, error) {
	grant := api.ExecutionGrant{
		GrantId:           grantID,
		IntentId:          intent.IntentId,
		Status:            api.ExecutionGrantStatusActive,
		ExpiresAt:         expiresAt,
		BoundedParameters: bounded,
	}
	connType, provider := resolveConnector(intent.Action.Type)
	if connType != "" && provider != "" {
		connectorID := connType + "/" + provider
		grant.ConnectorId = &connectorID
	}
	return grant, nil
}

// userIDFromContext extracts the authenticated user ID from request context.
// Returns the empty string when no claims are present (unauthenticated path).
func userIDFromContext(ctx context.Context) string {
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		return claims.Subject
	}
	return ""
}

// contextWithUser returns a context carrying the given user ID as auth claims.
// Used to scope vault lookups (e.g. connected-account credentials) to a user.
func contextWithUser(ctx context.Context, userID string) context.Context {
	claims := &auth.Claims{}
	claims.Subject = userID
	return auth.ContextWithClaims(ctx, claims)
}
