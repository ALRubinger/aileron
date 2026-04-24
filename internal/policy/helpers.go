package policy

import (
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
)

// ActiveStatus returns the active policy status value.
func ActiveStatus() api.PolicyStatus {
	return api.PolicyStatusActive
}

// MakePolicy creates an api.Policy with the given parameters.
func MakePolicy(id, workspaceID string, rules []api.PolicyRule, status api.PolicyStatus) api.Policy {
	now := time.Now().UTC()
	return api.Policy{
		PolicyId:    id,
		WorkspaceId: workspaceID,
		Name:        id,
		Version:     1,
		Status:      status,
		Rules:       rules,
		CreatedAt:   &now,
		UpdatedAt:   &now,
	}
}
