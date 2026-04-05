// Package store defines persistence interfaces for the control plane.
//
// These interfaces decouple the API handlers from the storage backend.
// The in-memory implementations (in the mem sub-package) are suitable for
// development and testing. Production deployments swap in Postgres-backed
// implementations without changing business logic.
package store

import (
	"context"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/model"
)

// IntentStore persists and retrieves action intents.
type IntentStore interface {
	Create(ctx context.Context, intent api.IntentEnvelope) error
	Get(ctx context.Context, intentID string) (api.IntentEnvelope, error)
	List(ctx context.Context, filter IntentFilter) ([]api.IntentEnvelope, error)
	Update(ctx context.Context, intent api.IntentEnvelope) error
}

// IntentFilter scopes an intent list query.
type IntentFilter struct {
	WorkspaceID string
	Status      *api.IntentStatus
	AgentID     string
	PageSize    int
	PageToken   string
}

// ApprovalStore persists and retrieves approval requests.
type ApprovalStore interface {
	Create(ctx context.Context, approval api.Approval) error
	Get(ctx context.Context, approvalID string) (api.Approval, error)
	List(ctx context.Context, filter ApprovalFilter) ([]api.Approval, error)
	Update(ctx context.Context, approval api.Approval) error
}

// ApprovalFilter scopes an approval list query.
type ApprovalFilter struct {
	WorkspaceID string
	Status      *api.ApprovalStatus
	IntentID    string
	PageSize    int
	PageToken   string
}

// PolicyStore persists and retrieves policy definitions.
type PolicyStore interface {
	Create(ctx context.Context, policy api.Policy) error
	Get(ctx context.Context, policyID string) (api.Policy, error)
	List(ctx context.Context, filter PolicyFilter) ([]api.Policy, error)
	Update(ctx context.Context, policy api.Policy) error
}

// PolicyFilter scopes a policy list query.
type PolicyFilter struct {
	WorkspaceID string
	Status      *api.PolicyStatus
	PageSize    int
	PageToken   string
}

// GrantStore persists and retrieves execution grants.
type GrantStore interface {
	Create(ctx context.Context, grant api.ExecutionGrant) error
	Get(ctx context.Context, grantID string) (api.ExecutionGrant, error)
	Update(ctx context.Context, grant api.ExecutionGrant) error
}

// ExecutionStore persists and retrieves execution records.
type ExecutionStore interface {
	Create(ctx context.Context, execution api.Execution) error
	Get(ctx context.Context, executionID string) (api.Execution, error)
	Update(ctx context.Context, execution api.Execution) error
}

// ConnectorStore persists and retrieves connector registrations.
type ConnectorStore interface {
	Create(ctx context.Context, connector api.Connector) error
	Get(ctx context.Context, connectorID string) (api.Connector, error)
	List(ctx context.Context, filter ConnectorFilter) ([]api.Connector, error)
	Update(ctx context.Context, connector api.Connector) error
}

// ConnectorFilter scopes a connector list query.
type ConnectorFilter struct {
	WorkspaceID string
	Type        *api.ConnectorType
	PageSize    int
	PageToken   string
}

// CredentialStore persists and retrieves credential references (not secrets).
type CredentialStore interface {
	Create(ctx context.Context, cred api.CredentialReference) error
	Get(ctx context.Context, credentialID string) (api.CredentialReference, error)
	List(ctx context.Context, filter CredentialFilter) ([]api.CredentialReference, error)
}

// CredentialFilter scopes a credential list query.
type CredentialFilter struct {
	WorkspaceID string
	PageSize    int
	PageToken   string
}

// FundingSourceStore persists and retrieves funding sources.
type FundingSourceStore interface {
	Create(ctx context.Context, fs api.FundingSource) error
	Get(ctx context.Context, fundingSourceID string) (api.FundingSource, error)
	List(ctx context.Context, filter FundingSourceFilter) ([]api.FundingSource, error)
}

// FundingSourceFilter scopes a funding source list query.
type FundingSourceFilter struct {
	WorkspaceID string
	PageSize    int
	PageToken   string
}

// MCPServerStore persists and retrieves MCP server configurations.
type MCPServerStore interface {
	Create(ctx context.Context, server api.MCPServerConfig) error
	Get(ctx context.Context, serverID string) (api.MCPServerConfig, error)
	List(ctx context.Context, filter MCPServerFilter) ([]api.MCPServerConfig, error)
	Update(ctx context.Context, server api.MCPServerConfig) error
	Delete(ctx context.Context, serverID string) error
}

// MCPServerFilter scopes an MCP server list query.
type MCPServerFilter struct {
	UserID   string
	Status   *api.MCPServerConfigStatus
	PageSize int
}

// EnterpriseMCPServerStore persists and retrieves enterprise-level MCP server configurations.
type EnterpriseMCPServerStore interface {
	Create(ctx context.Context, server api.EnterpriseMCPServer) error
	Get(ctx context.Context, serverID string) (api.EnterpriseMCPServer, error)
	List(ctx context.Context, filter EnterpriseMCPServerFilter) ([]api.EnterpriseMCPServer, error)
	Update(ctx context.Context, server api.EnterpriseMCPServer) error
	Delete(ctx context.Context, serverID string) error
}

// EnterpriseMCPServerFilter scopes an enterprise MCP server list query.
type EnterpriseMCPServerFilter struct {
	EnterpriseID string
	AutoEnabled  *bool
	PageSize     int
}

// EnterpriseStore persists and retrieves enterprises.
type EnterpriseStore interface {
	Create(ctx context.Context, enterprise model.Enterprise) error
	Get(ctx context.Context, enterpriseID string) (model.Enterprise, error)
	GetBySlug(ctx context.Context, slug string) (model.Enterprise, error)
	Update(ctx context.Context, enterprise model.Enterprise) error
}

// UserStore persists and retrieves users. The ID is a surrogate key (usr_ + UUID).
// Email is a unique column used for cross-provider deduplication.
type UserStore interface {
	Create(ctx context.Context, user model.User) error
	Get(ctx context.Context, userID string) (model.User, error)
	GetByEmail(ctx context.Context, email string) (model.User, error)
	GetByProviderSubject(ctx context.Context, provider, subjectID string) (model.User, error)
	List(ctx context.Context, filter UserFilter) ([]model.User, error)
	Update(ctx context.Context, user model.User) error
}

// UserFilter scopes a user list query.
type UserFilter struct {
	EnterpriseID string
	Status       *model.UserStatus
	PageSize     int
	PageToken    string
}

// SessionStore persists and retrieves login sessions.
type SessionStore interface {
	Create(ctx context.Context, session model.Session) error
	GetByTokenHash(ctx context.Context, tokenHash string) (model.Session, error)
	GetByRefreshTokenHash(ctx context.Context, refreshHash string) (model.Session, error)
	Delete(ctx context.Context, sessionID string) error
	DeleteAllForUser(ctx context.Context, userID string) error
}

// VerificationCodeStore persists and retrieves email verification codes.
type VerificationCodeStore interface {
	Create(ctx context.Context, code model.VerificationCode) error
	// GetActiveByUserID returns the most recent unused, unexpired code for the user.
	GetActiveByUserID(ctx context.Context, userID string) (model.VerificationCode, error)
	MarkUsed(ctx context.Context, codeID string) error
	DeleteExpiredForUser(ctx context.Context, userID string) error
}

// SSOConfigStore persists and retrieves per-enterprise SSO configurations.
type SSOConfigStore interface {
	Create(ctx context.Context, cfg model.SSOConfig) error
	Get(ctx context.Context, configID string) (model.SSOConfig, error)
	GetByEnterprise(ctx context.Context, enterpriseID string) ([]model.SSOConfig, error)
	Update(ctx context.Context, cfg model.SSOConfig) error
	Delete(ctx context.Context, configID string) error
}

// ErrNotFound is returned when a requested entity does not exist.
type ErrNotFound struct {
	Entity string
	ID     string
}

func (e *ErrNotFound) Error() string {
	return e.Entity + " not found: " + e.ID
}
